#!/usr/bin/env bash
# Kind leg for aether#1136: a proxy roll that CHANGES the Envoy worker count
# (proxy.concurrency) under continuous east-west HTTP/3 load.
#
# A hot restart between different worker counts re-steers the parent's QUIC
# connections by the child's count: the child attaches its connection-ID
# steering program (SO_ATTACH_REUSEPORT_CBPF, `CID % concurrency`) to the
# sockets it inherits, and that program belongs to the whole reuse-port group.
# On a 4 -> 2 change about half of the parent's live connections then land on a
# parent worker that does not own them (`Mismatched worker index. expected 3,
# actual 1`, then a stateless reset); on 2 -> 4 the child's NEW sockets for
# workers 2-3 are not paused and take about half of them. The #1136 supervisor
# does not hot-restart across a count change: it drains the predecessor, stops
# it, and starts a fresh Envoy at epoch 0.
#
# Two nodes (EWQ_WORKER=1): the sources run on the control plane, the
# destinations on the worker, so every request crosses an east-west QUIC twin
# between two different node proxies.
#
#   C0  the mesh runs at proxy.concurrency=PCC_FROM, the proxy DaemonSet settled
#   C1  PCC_LOOPS curl loops per (source, destination) pair, from client-a and
#       client-b to quic-a and quic-b, for PCC_SECONDS
#   C2  PCC_LEAD_S into the load, `helm upgrade` to proxy.concurrency=PCC_TO:
#       only the proxy DaemonSet rolls (one node at a time, maxSurge 1)
#   C3  every proxy pod's log is snapshotted once a second while the roll runs
#       (an old pod's log is gone once it is deleted), then counted
#
# Reported: `Mismatched worker index` lines (Envoy logs them at error with a
# power-of-two rate limit, so a line stands for a doubling of events), the
# #1133 remap lines (`udp worker index ... is not one of this instance's`), the
# supervisor's handoff decisions, and the client side: requests, non-200s by
# code, and the seconds in which any request failed.
#
# Red-then-green (main vs the #1136 supervisor image, PCC_TAG):
#   red    Mismatched worker index > 0 on 4 -> 2; supervisors log `live
#          predecessor confirmed; starting cross-pod hot restart`
#   green  0 Mismatched; supervisors log `envoy worker count changes across
#          this handoff; NOT hot-restarting` and start fresh at epoch 0; the
#          client-side failure seconds are the cost of the fresh start (the
#          node has no listeners while its fresh Envoy initializes)
#
# Usage: e2e/proxy-concurrency-change.sh {up|run|down}   (bare = up + run)
# Env: PCC_FROM (4), PCC_TO (2), PCC_LOOPS (4), PCC_SECONDS (180),
#      PCC_LEAD_S (15), PCC_REQUIRE (none|red|green: make the verdict fatal),
#      plus every EWQ_* knob of e2e/eastwest-quic.sh (EWQ_SKIP_BUILD=1 with
#      EWQ_IMAGE_TAG to run pre-built images; `run` re-installs with that tag
#      first, so red and green can share one cluster).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export EWQ_CLUSTER="${EWQ_CLUSTER:-aether-1136}"
export EWQ_WORKER=1
PCC_FROM="${PCC_FROM:-4}"
PCC_TO="${PCC_TO:-2}"
PCC_LOOPS="${PCC_LOOPS:-4}"
PCC_SECONDS="${PCC_SECONDS:-180}"
PCC_LEAD_S="${PCC_LEAD_S:-15}"
PCC_REQUIRE="${PCC_REQUIRE:-none}"
EWQ_EXTRA_HELM_ARGS=(--set "proxy.concurrency=$PCC_FROM")
# shellcheck source=e2e/eastwest-quic.sh
. "$HERE/eastwest-quic.sh"

PCC_SRCS=(client-a client-b)
PCC_DSTS=(quic-a quic-b)

proxy_pods() {
	kc -n "$NS" get pods -l app.kubernetes.io/component=proxy --no-headers \
		-o custom-columns=N:.metadata.name 2>/dev/null
}

# install_with N — the same install as `up`, at proxy.concurrency=N.
install_with() {
	EWQ_EXTRA_HELM_ARGS=(--set "proxy.concurrency=$1")
	install_aether
}

wait_proxy_settled() {
	kc -n "$NS" rollout status ds/aether-proxy --timeout=600s >/dev/null || die "the proxy DaemonSet never settled"
	local i n
	for i in $(seq 1 120); do
		n="$(kc -n "$NS" get pods -l app.kubernetes.io/component=proxy --no-headers 2>/dev/null | wc -l)"
		[ "$n" = 2 ] && return 0
		sleep 2
	done
	die "the proxy never settled to one pod per node"
}

# concurrency_on NODE — the worker count the node's Envoy reports.
concurrency_on() {
	docker exec "$1" curl -s --max-time 2 http://127.0.0.1:9901/server_info 2>/dev/null |
		tr -d ' \n' | { grep -o '"concurrency":[0-9]*' || true; } | cut -d: -f2
}

# load_loop SRC DST SECONDS OUT — one curl loop in SRC's pod: "<unix s> <code>
# <seconds>" per request.
load_loop() {
	local src="$1" dst="$2" secs="$3" out="$4" pod
	pod="$(pod_of "$src")"
	# shellcheck disable=SC2016  # evaluated by the POD's shell
	kc -n "$TEST_NS" exec "$pod" -c curl -- sh -c '
		url="$1"; end=$(( $(date +%s) + $2 ))
		while [ "$(date +%s)" -lt "$end" ]; do
			c=$(curl -s -o /dev/null --max-time 5 -w "%{http_code} %{time_total}" "$url" 2>/dev/null)
			echo "$(date +%s) ${c:-000 5}"
		done
	' sh "http://$(fqdn "$dst"):$OUTBOUND_PORT/hostname" "$secs" >"$out" 2>/dev/null || true
}

# snapshot_logs DIR STOPFILE — keep the latest log of every proxy pod in DIR.
snapshot_logs() {
	local dir="$1" stop="$2" p
	while [ ! -e "$stop" ]; do
		for p in $(proxy_pods); do
			kc -n "$NS" logs "$p" -c proxy >"$dir/$p.log.new" 2>/dev/null && mv "$dir/$p.log.new" "$dir/$p.log"
		done
		sleep 1
	done
}

# since T0ISO DIR — every proxy log line stamped at or after T0ISO (UTC): the
# supervisor's `timestamp` and Envoy's `time` are both ISO-8601 UTC, so their
# first 19 characters compare as strings. Leaves out the C0 install's own roll.
since() {
	cat "$2"/*.log 2>/dev/null | awk -v t0="$1" '
		match($0, /"(time|timestamp)":"[0-9T:-]+/) {
			ts = substr($0, RSTART, RLENGTH)
			sub(/^"[a-z]+":"/, "", ts)
			if (substr(ts, 1, 19) >= t0) print
		}'
}

count_in() { grep -c -- "$2" "$1" || true; }

report() {
	local logs="$1" load="$2" t0 after
	t0="$(cat "$load/t0")"
	after="$logs/after-upgrade.txt"
	since "$(date -u -d "@$t0" +%Y-%m-%dT%H:%M:%S)" "$logs" >"$after"
	log "C3 results ($PCC_FROM -> $PCC_TO, supervisor image tag $IMAGE_TAG)"
	local mis remap fresh hot inconc
	mis="$(count_in "$after" 'Mismatched worker index')"
	remap="$(count_in "$after" "is not one of this instance's")"
	fresh="$(count_in "$after" 'NOT hot-restarting')"
	hot="$(count_in "$after" 'live predecessor confirmed; starting cross-pod hot restart')"
	inconc="$(count_in "$after" 'worker-count check inconclusive')"
	echo "  proxy logs since the upgrade: Mismatched_worker_index=$mis udp_index_remap=$remap"
	echo "  supervisor: fresh_after_drain=$fresh hot_restart=$hot check_inconclusive=$inconc"
	grep -h -E 'worker count changes|predecessor drained|starting envoy|pod ready|live predecessor confirmed|Mismatched worker index' \
		"$after" | sed -E 's/"args":\[[^]]*\],?//' | cut -c1-330 | sed 's/^/    /' || true
	local total bad secs slow max
	total="$(cat "$load"/*.out | wc -l)"
	bad="$(cat "$load"/*.out | awk '$2 != "200"' | wc -l)"
	secs="$(cat "$load"/*.out | awk '$2 != "200" { print $1 }' | sort -u | wc -l)"
	slow="$(cat "$load"/*.out | awk '$3 >= 1' | wc -l)"
	max="$(cat "$load"/*.out | awk '$3 > m { m = $3 } END { printf "%.3f", m }')"
	echo "  client: requests=$total non200=$bad failing_seconds=$secs slower_than_1s=$slow max_latency=${max}s"
	echo "  codes: $(cat "$load"/*.out | awk '{ print $2 }' | sort | uniq -c | tr '\n' ' ')"
	echo "  failed or >=1s requests per second (relative to the helm upgrade):"
	cat "$load"/*.out | awk -v t0="$t0" '
		$2 != "200" { f[$1 - t0]++; a[$1 - t0] = 1 }
		$3 >= 1 { s[$1 - t0]++; a[$1 - t0] = 1 }
		END { for (k in a) print k, f[k] + 0, s[k] + 0 }' |
		sort -n | awk '{ printf "    t=%+ds failed=%d slow=%d\n", $1, $2, $3 }'
	echo "PCC FROM=$PCC_FROM TO=$PCC_TO TAG=$IMAGE_TAG MISMATCHED=$mis REMAP=$remap FRESH=$fresh HOT=$hot REQUESTS=$total NON200=$bad FAILING_SECONDS=$secs SLOW_GE_1S=$slow MAX_LATENCY_S=$max"
	case "$PCC_REQUIRE" in
	red) [ "$mis" -gt 0 ] || [ "$bad" -gt 0 ] || die "PCC_REQUIRE=red but the roll left no mis-steer and no failure" ;;
	green)
		[ "$mis" = 0 ] || die "PCC_REQUIRE=green but $mis Mismatched worker index lines"
		[ "$fresh" -gt 0 ] || die "PCC_REQUIRE=green but no supervisor took the fresh-after-drain path"
		;;
	none) ;;
	*) die "PCC_REQUIRE must be none, red or green" ;;
	esac
}

run() {
	log "C0 mesh at proxy.concurrency=$PCC_FROM (image tag $IMAGE_TAG)"
	load_images
	install_with "$PCC_FROM"
	wait_proxy_settled
	local n
	for n in "$NODE" "$DST_NODE"; do
		[ "$(concurrency_on "$n")" = "$PCC_FROM" ] || die "C0: $n runs $(concurrency_on "$n") workers, not $PCC_FROM"
	done
	ok "both node proxies run $PCC_FROM workers"

	local work logs load stop s d i snap
	local pids=()
	work="$(mktemp -d)"
	logs="$work/logs"
	load="$work/load"
	stop="$work/stop"
	mkdir -p "$logs" "$load"
	log "C1 load: $PCC_LOOPS loops per pair, ${PCC_SRCS[*]} -> ${PCC_DSTS[*]}, ${PCC_SECONDS}s"
	for s in "${PCC_SRCS[@]}"; do
		for d in "${PCC_DSTS[@]}"; do
			for i in $(seq 1 "$PCC_LOOPS"); do
				load_loop "$s" "$d" "$PCC_SECONDS" "$load/$s.$d.$i.out" &
				pids+=("$!")
			done
		done
	done
	snapshot_logs "$logs" "$stop" &
	snap="$!"
	sleep "$PCC_LEAD_S"

	log "C2 helm upgrade to proxy.concurrency=$PCC_TO"
	date +%s >"$load/t0"
	install_with "$PCC_TO"
	wait_proxy_settled
	for n in "$NODE" "$DST_NODE"; do
		echo "  $n now runs $(concurrency_on "$n") workers"
	done
	ok "roll done after $(($(date +%s) - $(cat "$load/t0")))s; waiting for the load to finish"
	wait "${pids[@]}" || true
	touch "$stop"
	wait "$snap" || true
	report "$logs" "$load"
	echo "  artifacts: $work"
}

case "${1:-}" in
up) up ;;
run) run ;;
down) down ;;
"") up && run ;;
*) die "usage: $0 {up|run|down}" ;;
esac
