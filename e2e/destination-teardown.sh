#!/usr/bin/env bash
# aether#1103: a request a stale source sends to a destination pod that is being
# torn down must come back fast and retriable, so the source's retry policy
# lands it on another endpoint -- and it must never reach an application twice.
#
# THE SOAK SIGNATURE (2026-10-01 TRIPLE). Five requests to svc-3 pods whose
# source agents were restarting (so the source proxies still selected the
# draining endpoint) were DELIVERED to the destination node proxy. Its access
# log shows, per pod, the same sequence:
#   app gone          a few `503 UF ... Connection refused` in 2-62 ms
#   then              `503 UF upstream_reset_before_response_started{connection_timeout}`
#                     after 5.00 s (Envoy's default connect_timeout)
#   CNI DEL           the pod's listeners removed
#   ~2.2 s later      `Server: Write failed ... (Network is unreachable)`: the veth is gone
# The 5 s answers were written into a netns with no route out; the sources
# heard nothing until their own liveness timer.
#
# WHY THE CONNECT HANGS. containerd's StopPodSandbox stops the containers, then
# tears the network down through go-cni, which DELs its networks in order: the
# built-in loopback network first (the loopback plugin's DEL is `ip link set lo
# down`), then the pod's conflist (aether, then the primary CNI, which deletes
# the veth). Between the two, the node proxy's inbound listener still accepts
# requests and its app hop dials 127.0.0.1 through a DOWN loopback: the SYN is
# dropped, not refused. aether's own DEL then holds ~2 s (its listener-gone probe
# cannot be refused either with lo DOWN) before the primary CNI removes the
# veth. The fix bounds the app connect (meshconst.AppConnectTimeout, 1 s) so the
# 503 UF -- which never reached the app -- leaves while the veth still exists.
#
# THE HARNESS. EWQ_WORKER=1 shape of e2e/eastwest-quic.sh (source client-a on
# the control plane, destination quic-a x2 on the worker). Per run:
#   - the SOURCE node's agent is frozen (SIGSTOP), so the source proxy keeps
#     the victim in its EDS for the whole leg, as a source whose agent is
#     restarting does;
#   - 6 sequential GET loops + one POST every 0.5 s, unpinned, for DT_SECONDS;
#   - DT_LEAD s in: the victim's app process is stopped (SIGTERM, waited for),
#     its lo is set DOWN at once (the loopback CNI DEL step, compressed so the
#     source cannot first eject the pod on the "connection refused" 503s, as
#     it can on kind when nothing else is moving), and the pod is deleted;
#     the real CNI DEL then runs.
# Gates per run:
#   green  every request 200, none slower than DT_BOUND (AppConnectTimeout +
#          retry slack), and the destination proxy's app cluster counted at
#          least one upstream_cx_connect_timeout (the leg really hit the window)
#   red    at least one request failed or exceeded DT_BOUND (the #1103 hang)
#   N      no request id at more than one application (a retry after the
#          request reached an app would show up twice)
#
# Usage:
#   EWQ_IMAGE_TAG=<tag> EWQ_SKIP_BUILD=1 e2e/destination-teardown.sh up
#   DT_EXPECT=red   e2e/destination-teardown.sh verify     # agent from main
#   e2e/destination-teardown.sh swap-agent <tag>           # roll the agent image
#   DT_EXPECT=green e2e/destination-teardown.sh verify     # agent with the fix
#   e2e/destination-teardown.sh down
set -euo pipefail
if [ -n "${DT_TRACE:-}" ]; then set -x; fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export EWQ_CLUSTER="${EWQ_CLUSTER:-aether-1103}"
export EWQ_WORKER=1

# shellcheck source=e2e/eastwest-quic.sh
. "$HERE/eastwest-quic.sh"

DT_EXPECT="${DT_EXPECT:-green}"
DT_RUNS="${DT_RUNS:-3}"
DT_SECONDS="${DT_SECONDS:-25}"
DT_LEAD="${DT_LEAD:-4}"
DT_BOUND="${DT_BOUND:-2.5}"
DT_OUT="${DT_OUT:-$(mktemp -d)}"
DT_DST="quic-a"
DT_SRC="client-a"

# --- the source agent freeze (as e2e/eastwest-quic-deadpeer.sh) -----------------

agent_pids() { docker exec "$NODE" pgrep -x agent || true; }

freeze_agent() {
	local pids
	pids="$(agent_pids)"
	[ -n "$pids" ] || die "no agent process on $NODE to freeze"
	# shellcheck disable=SC2086 # one PID per word
	docker exec "$NODE" kill -STOP $pids
}

thaw_agent() {
	local pids
	pids="$(agent_pids)"
	# shellcheck disable=SC2086
	[ -z "$pids" ] || docker exec "$NODE" kill -CONT $pids || true
}

# --- the destination ---------------------------------------------------------------

dst_admin() { docker exec "$DST_NODE" curl -s --max-time 5 "http://127.0.0.1:9901$1"; }

# app_connect_timeouts — the destination proxy's collapsed app-cluster counter
# (every app_<namespace>_<pod>_<port> cluster shares alt_stat_name "app").
app_connect_timeouts() {
	dst_admin '/stats?filter=^cluster\.app\.upstream_cx_connect_timeout$' |
		awk -F': ' '{ s += $2 } END { print s + 0 }'
}

dst_pods() {
	kc -n "$TEST_NS" get pod -l "app=$DT_DST" --no-headers \
		-o custom-columns=N:.metadata.name,D:.metadata.deletionTimestamp,P:.status.phase |
		awk '$2 == "<none>" && $3 == "Running" { print $1 }'
}

dt_scale() {
	# No preStop: the app is stopped the moment the kubelet acts.
	kc -n "$TEST_NS" patch deploy "$DT_DST" --type=strategic \
		-p '{"spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","lifecycle":null}]}}}}' >/dev/null
	kc -n "$TEST_NS" rollout status "deploy/$DT_DST" --timeout=180s >/dev/null || die "$DT_DST never reached 2 Ready replicas"
	local deadline=$((SECONDS + 60))
	until [ "$(dst_pods | grep -c .)" -eq 2 ]; do
		[ "$SECONDS" -lt "$deadline" ] || die "want exactly 2 Running $DT_DST pods"
		sleep 2
	done
}

# warm — both replicas carry the source's requests before the leg starts.
warm() {
	local deadline=$((SECONDS + 120)) n
	while true; do
		n="$(req_batch "$DT_SRC" "$DT_DST" /hostname 30 | awk '$1 == 200 { print $2 }' | sort -u | grep -c . || true)"
		[ "$n" -ge 2 ] && break
		[ "$SECONDS" -lt "$deadline" ] || die "the source never reached both $DT_DST replicas"
		sleep 3
	done
}

# teardown_victim POD — the containerd teardown order, compressed: stop the
# app (waited for), set lo DOWN in the pod netns, delete the pod.
teardown_victim() {
	local pod="$1" sb ppid app apid
	# SIGPIPE rule (#1121, e2e/README.md): this script runs under pipefail, so
	# no pipeline may end in a reader that exits before its writer is done
	# (`head`, `grep -q`/`-m`, `awk '...; exit'`): the writer dies of SIGPIPE
	# and the pipeline fails with 141 at a random point. Read to EOF instead
	# (`sed -n '1p'`).
	sb="$(docker exec "$DST_NODE" crictl pods --name "$pod" -q | sed -n '1p')"
	[ -n "$sb" ] || die "no sandbox for $pod on $DST_NODE"
	ppid="$(docker exec "$DST_NODE" crictl inspectp "$sb" | python3 -c 'import json, sys; print(json.load(sys.stdin)["info"]["pid"])')"
	app="$(docker exec "$DST_NODE" crictl ps --pod "$sb" --name app -q | sed -n '1p')"
	apid="$(docker exec "$DST_NODE" crictl inspect "$app" | python3 -c 'import json, sys; print(json.load(sys.stdin)["info"]["pid"])')"
	docker exec "$DST_NODE" sh -c "kill -TERM $apid; while kill -0 $apid 2>/dev/null; do sleep 0.005; done; nsenter -t $ppid -n ip link set lo down" ||
		die "could not stop $pod's app and set its lo down"
	kc -n "$TEST_NS" delete pod "$pod" --wait=false >/dev/null
}

stream_logs() {
	local dir="$1" p
	mkdir -p "$dir"
	: >"$dir/followers"
	for p in $(dst_pods); do
		kc -n "$TEST_NS" logs -f "$p" -c app >"$dir/app-$p.log" 2>/dev/null &
		echo "$!" >>"$dir/followers"
	done
}

collect_logs() {
	local dir="$1" p
	sleep 2
	while read -r p; do kill "$p" 2>/dev/null || true; done <"$dir/followers"
	for p in $(kc -n "$TEST_NS" get pod -l "app=$DT_DST" -o name | sed 's#pod/##'); do
		[ -e "$dir/app-$p.log" ] || kc -n "$TEST_NS" logs "$p" -c app >"$dir/app-$p.log" 2>/dev/null || true
	done
}

# shellcheck disable=SC2016 # evaluated by the pod's shell
LOAD='
url="$1"; tag="$2"; dur="$3"; end=$(( $(date +%s) + dur ))
worker() {
	n=0
	while [ "$(date +%s)" -lt "$end" ]; do
		n=$((n + 1)); id="$tag-g$1-$n"
		out=$(curl -s -o /dev/null --max-time 40 -w "%{http_code} %{time_total}" "$url/echo?msg=$id")
		echo "$id GET ${out:-000 0}"
		sleep 0.1
	done
}
poster() {
	n=0
	while [ "$(date +%s)" -lt "$end" ]; do
		n=$((n + 1)); id="$tag-p-$n"
		( out=$(curl -s -o /dev/null --max-time 40 -d "id=$id" -w "%{http_code} %{time_total}" "$url/echo?msg=$id")
		  echo "$id POST ${out:-000 0}" ) &
		sleep 0.5
	done
	wait
}
for w in 1 2 3 4 5 6; do worker "$w" & done
poster &
wait'

# no_replay TAG DIR — no request id at more than one application.
no_replay() {
	local tag="$1" dir="$2" dups
	cat "$dir"/app-*.log 2>/dev/null | grep -o "msg=$tag-[A-Za-z0-9-]*" | sort | uniq -c >"$dir/ids.txt" || true
	dups="$(awk '$1 > 1' "$dir/ids.txt")"
	[ -z "$dups" ] || die "N: request ids seen more than once at the applications (replayed):
$dups"
	ok "N ($tag): $(grep -c . "$dir/ids.txt") ids at the applications, none more than once"
}

run_once() {
	local i="$1" tag victim dir res pod pid ct0 ct1 bad slow
	tag="dt$(date +%s)r$i"
	dir="$DT_OUT/run$i"
	res="$dir/results"
	dt_scale
	warm
	victim="$(dst_pods | sed -n '1p')"
	stream_logs "$dir"
	ct0="$(app_connect_timeouts)"
	pod="$(pod_of "$DT_SRC")"
	freeze_agent
	kc -n "$TEST_NS" exec "$pod" -c curl -- sh -c "$LOAD" sh \
		"http://$(fqdn "$DT_DST"):$OUTBOUND_PORT" "$tag" "$DT_SECONDS" >"$res" 2>/dev/null &
	pid=$!
	sleep "$DT_LEAD"
	teardown_victim "$victim"
	wait "$pid" || true
	thaw_agent
	sleep 6 # let a red run's 5 s connect timeouts land in the counter
	ct1="$(app_connect_timeouts)"
	collect_logs "$dir"
	[ "$(grep -c . "$res")" -gt 50 ] || die "run $i: only $(grep -c . "$res") results: the load did not run"
	bad="$(awk '$3 != 200' "$res" | wc -l)"
	slow="$(awk -v b="$DT_BOUND" '$4 > b' "$res" | wc -l)"
	printf '  run %d victim %s: %d requests, %d non-200, %d slower than %ss, max %ss, app connect timeouts +%d\n' \
		"$i" "$victim" "$(grep -c . "$res")" "$bad" "$slow" "$DT_BOUND" \
		"$(awk '$4 > m { m = $4 } END { print m + 0 }' "$res")" "$((ct1 - ct0))" | tee -a "$DT_OUT/summary"
	case "$DT_EXPECT" in
	green)
		[ "$bad" -eq 0 ] || die "run $i: $bad requests failed (want every request retried onto the live replica)"
		[ "$slow" -eq 0 ] || die "run $i: $slow requests slower than ${DT_BOUND}s"
		[ "$((ct1 - ct0))" -ge 1 ] || die "run $i: no app connect timed out: the leg never hit the lo-DOWN window (vacuous)"
		ok "run $i (green): every request 200 within ${DT_BOUND}s; $((ct1 - ct0)) dying-pod requests failed fast and were retried"
		;;
	red)
		[ $((bad + slow)) -gt 0 ] || die "run $i (red): nothing failed or hung -- the #1103 signature did not reproduce"
		ok "run $i (red): $bad failed and $slow exceeded ${DT_BOUND}s"
		;;
	*) die "DT_EXPECT must be red or green, got '$DT_EXPECT'" ;;
	esac
	no_replay "$tag" "$dir"
}

dt_verify() {
	log "aether#1103 destination teardown verify (DT_EXPECT=$DT_EXPECT, $DT_RUNS runs, results in $DT_OUT)"
	trap thaw_agent EXIT
	echo "  app connect_timeout on $DST_NODE: $(dst_admin '/config_dump?resource=dynamic_active_clusters' |
		python3 -c 'import json, sys
d = json.load(sys.stdin)
print(sorted({c["cluster"].get("connect_timeout", "unset (5s)") for c in d["configs"] if c["cluster"]["name"].startswith("app_")}))')"
	local i
	for i in $(seq 1 "$DT_RUNS"); do run_once "$i"; done
	ok "aether#1103 ($DT_EXPECT): $DT_RUNS runs as expected"
}

swap_agent() {
	local tag="$1" ref
	ref="${IMAGE_REGISTRY}/agent:$tag"
	kind load docker-image "$ref" --name "$CLUSTER" >/dev/null || die "could not load $ref"
	kc -n "$NS" set image ds/aether-agent "agent=$ref" >/dev/null
	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the agent never rolled onto $ref"
	ok "agent DaemonSet on $ref"
}

case "${1:-}" in
up) up ;;
verify) dt_verify ;;
swap-agent) swap_agent "${2:?usage: $0 swap-agent <tag>}" ;;
down) down ;;
*) die "usage: $0 {up|verify|swap-agent <tag>|down}" ;;
esac
