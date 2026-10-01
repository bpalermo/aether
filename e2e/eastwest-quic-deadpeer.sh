#!/usr/bin/env bash
# aether#1087: an east-west HTTP/3 request whose destination's network vanishes
# AFTER the request was delivered must fail fast, and must never be replayed.
#
# THE SOAK SIGNATURE (2026-09-30, 2026-10-01). Five and six `504 UT` requests,
# each exactly 15 s, over a `quic:` twin to a terminating svc-3 pod during the
# TRIPLE roll. The destination-reporter access log for the SAME x-request-ids
# shows the requests ARRIVED: the destination inbound answered 503 UF after
# 5 s (its app was going away) or lost the stream at 3-5 s -- and the source
# never saw any of it. By then CNI DEL had removed the pod's veth, so nothing
# the destination proxy wrote into that netns could leave it. The source had
# nothing in flight (its request was ACKed), so no PTO and no blackhole
# detector ran; the only timers left were the QUIC idle timeout (min of QUICHE's
# 600 s and the inbound's 300 s) and QUICHE's 15 s keep-alive, and the 15 s
# route timeout won.
#
# THE HARNESS. EWQ_WORKER=1 shape of e2e/eastwest-quic.sh (sources on the
# control plane, destinations on the worker), with quic-a scaled to 2 replicas.
# The source node's agent is FROZEN (SIGSTOP, so it neither restarts nor
# pushes) for the duration of each leg -- the source proxy keeps the endpoint in
# its EDS exactly as a source whose agent is away does. A destination pod's
# network is cut by deleting its host-side veth on the worker (the half of CNI
# DEL that matters here, at a moment the harness chooses) and the pod is then
# deleted with no preStop.
#
#   A  in flight   2 GET + 2 POST pinned to the victim (x-aether-pod) run a
#                  10 s handler; 2 s in, the victim's veth is cut. Nothing else
#                  is sent to the victim, so the source connection stays as
#                  quiet as the soak's.
#                    red (main):   every one hangs to the route timeout, 504
#                    green:        every one fails 503 within DP_BOUND
#   B  continuous  4 sequential GET loops + one slow (3 s) POST per second, all
#                  unpinned, for DP_SECONDS; the victim's veth is cut 3 s in.
#                    green: no 504, nothing slower than DP_BOUND + 3 s (the
#                    slow POST's own 3 s), failures only 503
#                    red:   reported, not gated
#   N  no replay   every request id appears at most once across the app logs
#                  of every quic-a pod (the victim's streamed before it died),
#                  and every id the client saw 200 for appears exactly once.
#                  A retried request would show up on the second replica.
#
# The bound (DP_BOUND, default 11.5 s): keep-alive 1 s + QUICHE's 1 s ping alarm
# granularity + idle_network_timeout 8 s = 10 s after the peer's last packet,
# plus scheduling slack (config.QUICTwinKeepaliveInterval,
# config.QUICTwinNetworkIdleTimeout).
#
# Usage:
#   EWQ_IMAGE_TAG=<tag> EWQ_SKIP_BUILD=1 e2e/eastwest-quic-deadpeer.sh up
#   DP_EXPECT=red   e2e/eastwest-quic-deadpeer.sh verify   # agent from main
#   e2e/eastwest-quic-deadpeer.sh swap-agent <tag>         # roll the agent image
#   DP_EXPECT=green e2e/eastwest-quic-deadpeer.sh verify   # agent with the fix
#   e2e/eastwest-quic-deadpeer.sh down
set -euo pipefail
if [ -n "${DP_TRACE:-}" ]; then set -x; fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export EWQ_CLUSTER="${EWQ_CLUSTER:-aether-1087}"
export EWQ_WORKER=1

# shellcheck source=e2e/eastwest-quic.sh
. "$HERE/eastwest-quic.sh"

DP_EXPECT="${DP_EXPECT:-green}"
DP_BOUND="${DP_BOUND:-11.5}"
DP_SECONDS="${DP_SECONDS:-15}"
# Request ids carry the run so a pod that outlives one run (the survivor) never
# counts a previous run's ids as replays.
DP_RUN="${DP_RUN:-$(date +%s)}"
DP_DST="quic-a"
DP_SRC="client-a"
DP_OUT="${DP_OUT:-$(mktemp -d)}"

# --- the source agent freeze ---------------------------------------------------

# agent_pids — the node agent's PIDs on the SOURCE node (comm is exactly
# "agent"; spire-agent's is "spire-agent", so -x cannot match it, nor itself).
agent_pids() { docker exec "$NODE" pgrep -x agent || true; }

freeze_agent() {
	local pids
	pids="$(agent_pids)"
	[ -n "$pids" ] || die "no agent process on $NODE to freeze"
	# shellcheck disable=SC2086 # one PID per word
	docker exec "$NODE" kill -STOP $pids
	ok "source agent frozen on $NODE (pid $(echo "$pids" | tr '\n' ' '))"
}

thaw_agent() {
	local pids
	pids="$(agent_pids)"
	# shellcheck disable=SC2086
	[ -z "$pids" ] || docker exec "$NODE" kill -CONT $pids || true
}

# --- the destination's network -------------------------------------------------

# cut_pod_network POD — delete POD's host-side veth on the worker: the
# destination proxy's sockets in the pod netns stay open and the pod keeps
# running, but nothing it writes can leave and nothing reaches it.
cut_pod_network() {
	local pod="$1" ip dev
	ip="$(kc -n "$TEST_NS" get pod "$pod" -o jsonpath='{.status.podIP}')"
	dev="$(docker exec "$DST_NODE" ip -o route get "$ip" | sed -n 's/.* dev \([^ ]*\).*/\1/p')"
	case "$dev" in
	veth*) ;;
	*) die "the route to $pod ($ip) on $DST_NODE is via '$dev', not a pod veth" ;;
	esac
	docker exec "$DST_NODE" ip link del "$dev"
	ok "cut $pod ($ip): deleted $dev on $DST_NODE"
}

dst_pods() {
	kc -n "$TEST_NS" get pod -l "app=$DP_DST" --no-headers \
		-o custom-columns=N:.metadata.name,D:.metadata.deletionTimestamp,P:.status.phase |
		awk '$2 == "<none>" && $3 == "Running" { print $1 }'
}

# The twin's QUIC transport options as the SOURCE node's Envoy holds them: the
# reading that says which agent build the run measured.
twin_quic_options() {
	admin '/config_dump?resource=dynamic_active_clusters' 2>/dev/null |
		python3 -c '
import json, sys
d = json.load(sys.stdin)
for c in d.get("configs", []):
    cl = c.get("cluster", {})
    if not cl.get("name", "").startswith("quic:'"$(fqdn "$DP_DST")"'@"):
        continue
    po = cl.get("typed_extension_protocol_options", {}).get("envoy.extensions.upstreams.http.v3.HttpProtocolOptions", {})
    q = po.get("explicit_http_config", {}).get("http3_protocol_options", {}).get("quic_protocol_options")
    print(cl["name"], json.dumps(q, sort_keys=True))
'
}

# --- workload ------------------------------------------------------------------

dp_scale() {
	kc -n "$TEST_NS" scale "deploy/$DP_DST" --replicas=2 >/dev/null
	kc -n "$TEST_NS" rollout status "deploy/$DP_DST" --timeout=180s >/dev/null || die "$DP_DST never reached 2 Ready replicas"
}

# warm — enough unpinned requests that the source holds a twin connection to
# every quic-a replica, and both replicas are in the source's EDS.
warm() {
	local pods p ip dump n
	pods="$(dst_pods)"
	[ "$(printf '%s\n' "$pods" | grep -c .)" -eq 2 ] || die "want 2 Running $DP_DST pods, have: $pods"
	local deadline=$((SECONDS + 120)) before
	while true; do
		before="$(twin_rq)"
		req_batch "$DP_SRC" "$DP_DST" "/hostname" 20 >/dev/null
		dump="$(admin /clusters)"
		# Every request of the batch must have ridden the twin: a source
		# whose arm is not live yet takes on_no_match, the h2 cluster, and
		# the legs below would then measure h2, not HTTP/3.
		if [ $(($(host_rq "$dump" "$(twin "$DP_DST" "$DP_SRC")") - before)) -lt 20 ]; then
			[ "$SECONDS" -lt "$deadline" ] || die "the source's requests never all rode the twin"
			sleep 3
			continue
		fi
		n=0
		for p in $pods; do
			ip="$(kc -n "$TEST_NS" get pod "$p" -o jsonpath='{.status.podIP}')"
			if printf '%s\n' "$dump" | awk -F'::' -v c="$(twin "$DP_DST" "$DP_SRC")" -v h="$ip:18008" \
				'$1 == c && $2 == h && $3 == "rq_total" && $4 > 0 { f = 1 } END { exit !f }'; then
				n=$((n + 1))
			fi
		done
		[ "$n" -eq 2 ] && break
		[ "$SECONDS" -lt "$deadline" ] || die "the source's twin never carried requests to both $DP_DST replicas"
		sleep 3
	done
	ok "twin $(twin "$DP_DST" "$DP_SRC") carries every request, to both replicas"
}

# twin_rq — the source twin's rq_total summed over its hosts.
twin_rq() { host_rq "$(admin /clusters)" "$(twin "$DP_DST" "$DP_SRC")"; }

# assert_on_twin BEFORE N LEG — at least N of the leg's requests rode the twin.
assert_on_twin() {
	local d
	d=$(($(twin_rq) - $1))
	[ "$d" -ge "$2" ] || die "$3: only $d of >= $2 requests rode the twin: this would measure the h2 cluster, not HTTP/3"
	ok "$3: $d requests rode the twin"
}

# stream_logs DIR — follow every current quic-a pod's app log into DIR, so a
# pod that dies mid-leg still has its log read (kubectl logs reaches the
# kubelet, not the pod network). The followers' PIDs go to DIR/followers.
stream_logs() {
	local dir="$1" p
	mkdir -p "$dir"
	: >"$dir/followers"
	for p in $(dst_pods); do
		kc -n "$TEST_NS" logs -f "$p" -c app >"$dir/app-$p.log" 2>/dev/null &
		echo "$!" >>"$dir/followers"
	done
}

# collect_logs DIR — stop the followers, then read every quic-a pod that has
# no followed log (the replacement the Deployment started mid-leg).
collect_logs() {
	local dir="$1" p
	sleep 2
	while read -r p; do kill "$p" 2>/dev/null || true; done <"$dir/followers"
	for p in $(kc -n "$TEST_NS" get pod -l "app=$DP_DST" -o name | sed 's#pod/##'); do
		[ -e "$dir/app-$p.log" ] || kc -n "$TEST_NS" logs "$p" -c app >"$dir/app-$p.log" 2>/dev/null || true
	done
}

# in_pod SCRIPT ARGS... — run a shell script in the source pod.
in_pod() {
	local pod
	pod="$(pod_of "$DP_SRC")"
	[ -n "$pod" ] || die "no Running $DP_SRC pod"
	kc -n "$TEST_NS" exec "$pod" -c curl -- sh -c "$@"
}

# shellcheck disable=SC2016 # evaluated by the pod's shell
LEG_A='
url="$1"; victim="$2"; tag="$3"; i=0
for m in GET GET POST POST; do
	i=$((i + 1)); id="$tag-$m-$i"
	if [ "$m" = POST ]; then d="-d id=$id"; else d=""; fi
	( out=$(curl -s -o /dev/null $d -H "x-aether-pod: $victim" --max-time 40 \
		-w "%{http_code} %{time_total}" "$url/shell?cmd=sleep%2010%3Becho%20$id")
	  echo "$id $m ${out:-000 0}" ) &
done
wait'

# shellcheck disable=SC2016
LEG_B='
url="$1"; tag="$2"; dur="$3"; end=$(( $(date +%s) + dur ))
worker() {
	n=0
	while [ "$(date +%s)" -lt "$end" ]; do
		n=$((n + 1)); id="$tag-g$1-$n"
		out=$(curl -s -o /dev/null --max-time 40 -w "%{http_code} %{time_total}" "$url/echo?msg=$id")
		echo "$id GET ${out:-000 0}"
		sleep 0.2
	done
}
poster() {
	n=0
	while [ "$(date +%s)" -lt "$end" ]; do
		n=$((n + 1)); id="$tag-p-$n"
		( out=$(curl -s -o /dev/null --max-time 40 -d "id=$id" -w "%{http_code} %{time_total}" \
			"$url/shell?cmd=sleep%203%3Becho%20$id")
		  echo "$id POST ${out:-000 0}" ) &
		sleep 1
	done
	wait
}
for w in 1 2 3 4; do worker "$w" & done
poster &
wait'

# no_replay TAG DIR — leg N over the app logs collected in DIR for TAG
# (the client's results in DIR/results).
no_replay() {
	local tag="$1" dir="$2" results counts
	results="$dir/results"
	counts="$dir/ids.txt"
	cat "$dir"/app-*.log 2>/dev/null | grep '\] GET /' | grep -o "$tag-[A-Za-z0-9-]*" | sort | uniq -c >"$counts" || true
	local dups
	dups="$(awk '$1 > 1' "$counts")"
	[ -z "$dups" ] || die "N: request ids seen more than once at the application (replayed):
$dups"
	local missing=""
	while read -r id _m code _t; do
		[ "$code" = 200 ] || continue
		awk -v i="$id" '$2 == i { f = 1 } END { exit !f }' "$counts" || missing="$missing $id"
	done <"$results"
	[ -z "$missing" ] || die "N: 200s with no application record (the id never reached an app):$missing"
	ok "N ($tag): $(wc -l <"$counts") ids at the application, none more than once; every 200 accounted for"
}

leg_a() {
	log "A in flight: 2 GET + 2 POST pinned to the victim, its network cut 2 s in, source agent frozen"
	local pods victim url res rq0
	pods="$(dst_pods)"
	victim="$(printf '%s\n' "$pods" | head -n 1)"
	url="http://$(fqdn "$DP_DST"):$OUTBOUND_PORT"
	local dir="$DP_OUT/legA"
	res="$dir/results"
	stream_logs "$dir"
	rq0="$(twin_rq)"
	freeze_agent
	in_pod "$LEG_A" sh "$url" "$victim" "dpA$DP_RUN" >"$res" 2>/dev/null &
	local pid=$!
	sleep 2
	cut_pod_network "$victim"
	kc -n "$TEST_NS" delete pod "$victim" --wait=false >/dev/null
	wait "$pid" || true
	# Read while the agent is still frozen: its first push after the thaw
	# removes the victim host row, and its rq_total with it.
	assert_on_twin "$rq0" 4 A
	thaw_agent
	sed 's/^/    /' "$res"
	[ "$(grep -c . "$res")" -eq 4 ] || die "A: want 4 results, got $(grep -c . "$res")"
	collect_logs "$dir"
	case "$DP_EXPECT" in
	red)
		awk '$3 != 504 || $4 < 14 { bad = 1 } END { exit bad }' "$res" ||
			die "A (red): not every in-flight request hung to the route timeout with 504 -- the soak signature did not reproduce"
		ok "A (red): every in-flight request to the vanished destination hung $(awk '{ print $4 }' "$res" | sort -n | tail -n 1) s and answered 504"
		;;
	green)
		awk -v b="$DP_BOUND" '$3 != 503 || $4 > b { bad = 1 } END { exit bad }' "$res" ||
			die "A: an in-flight request to the vanished destination did not fail 503 within ${DP_BOUND}s"
		ok "A: every in-flight request (GET and POST) failed 503 within ${DP_BOUND}s (max $(awk '{ print $4 }' "$res" | sort -n | tail -n 1) s)"
		;;
	*) die "DP_EXPECT must be red or green, got '$DP_EXPECT'" ;;
	esac
	no_replay "dpA$DP_RUN" "$dir"
}

leg_b() {
	log "B continuous: 4 GET loops + 1 slow POST/s for ${DP_SECONDS}s, unpinned; the victim's network cut 3 s in, source agent frozen"
	local pods victim url res rq0
	pods="$(dst_pods)"
	victim="$(printf '%s\n' "$pods" | head -n 1)"
	url="http://$(fqdn "$DP_DST"):$OUTBOUND_PORT"
	local dir="$DP_OUT/legB"
	res="$dir/results"
	stream_logs "$dir"
	rq0="$(twin_rq)"
	freeze_agent
	in_pod "$LEG_B" sh "$url" "dpB$DP_RUN" "$DP_SECONDS" >"$res" 2>/dev/null &
	local pid=$!
	sleep 3
	cut_pod_network "$victim"
	kc -n "$TEST_NS" delete pod "$victim" --wait=false >/dev/null
	wait "$pid" || true
	# Read while the agent is still frozen: its first push after the thaw
	# removes the victim host row, and its rq_total with it.
	assert_on_twin "$rq0" 20 B
	thaw_agent
	collect_logs "$dir"
	awk '{ c[$2 " " $3]++; if ($4 > max) max = $4; if ($4 >= 14) slow++ }
		END { for (k in c) printf "    %s x%d\n", k, c[k]; printf "    total %d, max %.3f s, >=14 s: %d\n", NR, max, slow + 0 }' "$res"
	[ "$(grep -c . "$res")" -gt 20 ] || die "B: only $(grep -c . "$res") results: the load did not run"
	if [ "$DP_EXPECT" = green ]; then
		local b3
		b3="$(awk -v b="$DP_BOUND" 'BEGIN { print b + 3 }')"
		awk -v b="$b3" '($3 != 200 && $3 != 503) || $4 > b { bad = 1 } END { exit bad }' "$res" ||
			die "B: a request answered something other than 200/503, or took longer than ${b3}s"
		ok "B: no 504, nothing slower than ${b3}s, failures only 503 ($(awk '$3 == 503' "$res" | wc -l) of $(grep -c . "$res"))"
	fi
	no_replay "dpB$DP_RUN" "$dir"
}

dp_verify() {
	log "aether#1087 dead-peer verify (DP_EXPECT=$DP_EXPECT, results in $DP_OUT)"
	trap thaw_agent EXIT
	dp_scale
	warm
	echo "  twin QUIC transport options on $NODE:"
	twin_quic_options | sed 's/^/    /'
	leg_a
	dp_scale
	warm
	leg_b
	ok "aether#1087 ($DP_EXPECT): legs A, B and N as expected"
}

# swap_agent TAG — roll the agent DaemonSet onto <registry>/agent:TAG (loaded
# into the cluster first), the red -> green step on one cluster.
swap_agent() {
	local tag="$1" ref
	ref="${IMAGE_REGISTRY}/agent:$tag"
	kind load docker-image "$ref" --name "$CLUSTER" >/dev/null || die "could not load $ref"
	kc -n "$NS" set image ds/aether-agent "agent=$ref" >/dev/null
	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the agent never rolled onto $ref"
	ok "agent DaemonSet on $ref"
}

dp_up() {
	up
	dp_scale
}

case "${1:-}" in
up) dp_up ;;
verify) dp_verify ;;
swap-agent) swap_agent "${2:?usage: $0 swap-agent <tag>}" ;;
down) down ;;
*) die "usage: $0 {up|verify|swap-agent <tag>|down}" ;;
esac
