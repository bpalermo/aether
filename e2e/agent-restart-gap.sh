#!/usr/bin/env bash
# aether#1123: how long a node's proxy has NO ADS stream across a node-agent roll,
# measured from the proxy's side, and where that time goes.
#
# The proxy routes on whatever it last heard while its agent is away: drain
# marks, endpoint removals, new twins and LDS changes all wait, and a successor
# proxy's per-resource initial fetches (SDS/RDS/EDS, 15 s) can time out inside a
# long gap (#1129). The gate is the gap itself.
#
# THE HARNESS. The EWQ_WORKER=1 shape of e2e/eastwest-quic.sh (control plane +
# one worker, both meshed, SPIRE on), on the etcd registry backend as talos-main
# runs it (ARG_BACKEND=kubernetes for the chart default). A poller inside each
# kind node reads the node proxy's `control_plane.connected_state` from its
# admin (127.0.0.1:9901) every ARG_POLL s and timestamps it; `kubectl rollout
# restart ds/aether-agent` is then run ARG_ROLLS times. For every node and roll:
#
#   gap        connected_state 1 -> 0 (the old agent's stream ended) until it
#              is 1 again and stays 1 for 3 samples (a fresh stream to the new
#              agent; a reconnect attempt to a missing socket can flash 1 for
#              a moment, which the 3-sample rule ignores).
#   replace    gap start -> the new agent process's first log line: pod
#              replacement (delete, create, sandbox, image checks, the
#              cni-install init container, container start).
#     init     of which the cni-install init container (first -> last line).
#   startup    new agent's first line -> its xDS socket opening (the latest of
#              `generating the initial snapshot` / `client certificates`).
#     identity of which the wait for this agent's own SVID (`held`).
#   reconnect  socket open -> the proxy's fresh stream (the gap's end).
#
# Readings are wall-clock on one host (kind nodes are containers), so the
# proxy-side and log-side times compare directly; the poll period bounds the
# gap's resolution.
#
# Usage:
#   EWQ_IMAGE_TAG=<tag> EWQ_SKIP_BUILD=1 e2e/agent-restart-gap.sh up
#   ARG_ROLLS=10 e2e/agent-restart-gap.sh measure        # report + gate
#   e2e/agent-restart-gap.sh swap <tag>                  # agent + cni-install images
#   e2e/agent-restart-gap.sh down
#
# ARG_EXPECT=green (default) fails `measure` when any gap exceeds ARG_BOUND
# (5 s, the #1123 target); ARG_EXPECT=report only reports.
set -euo pipefail
if [ -n "${ARG_TRACE:-}" ]; then set -x; fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export EWQ_CLUSTER="${EWQ_CLUSTER:-aether-1123}"
export EWQ_WORKER=1

# shellcheck source=e2e/eastwest-quic.sh
. "$HERE/eastwest-quic.sh"

ARG_EXPECT="${ARG_EXPECT:-green}"
ARG_BACKEND="${ARG_BACKEND:-etcd}"
ARG_ROLLS="${ARG_ROLLS:-5}"
ARG_BOUND="${ARG_BOUND:-5.0}"
ARG_POLL="${ARG_POLL:-0.05}"
ARG_SETTLE="${ARG_SETTLE:-15}"
ARG_OUT="${ARG_OUT:-$(mktemp -d)}"
mkdir -p "$ARG_OUT"
ETCD_NAME="$CLUSTER-etcd"
ETCD_IMAGE="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.5.16}"
POLL_LOG="/tmp/aether-1123-connected-state.log"
POLL_STOP="/tmp/aether-1123-connected-state.stop"
NODES=("$NODE" "$DST_NODE")

# --- registry backend (as e2e/drain-propagation.sh) ------------------------------

start_etcd() {
	log "starting etcd on the 'kind' docker network"
	docker rm -f "$ETCD_NAME" >/dev/null 2>&1 || true
	docker run -d --name "$ETCD_NAME" --network kind "$ETCD_IMAGE" \
		etcd --name s1 --data-dir /tmp/etcd \
		--listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://0.0.0.0:2379 >/dev/null
	local deadline=$((SECONDS + 30))
	until docker exec "$ETCD_NAME" etcdctl endpoint health >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$deadline" ] || die "etcd never became healthy"
		sleep 1
	done
	local ip
	ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$ETCD_NAME")"
	[ -n "$ip" ] || die "no etcd IP"
	EWQ_EXTRA_HELM_ARGS+=(--set registrar.registryBackend=etcd --set "registrar.etcd.endpoints[0]=http://$ip:2379")
	ok "etcd at http://$ip:2379; registrar on the etcd backend"
}

# --- the proxy-side poller -------------------------------------------------------

# One line per sample: "<epoch.ns> <connected_state>" (empty value = admin did
# not answer, e.g. the proxy itself restarting; such samples are skipped).
# shellcheck disable=SC2016 # evaluated by the node's shell
POLLER='
rm -f "$2"
while [ ! -e "$2" ]; do
	v=$(curl -s --max-time 0.5 "http://127.0.0.1:9901/stats?filter=^control_plane.connected_state$" | sed -n "s/^control_plane.connected_state: //p")
	echo "$(date +%s.%N) $v"
	sleep "$3"
done >>"$1"'

start_pollers() {
	local n
	for n in "${NODES[@]}"; do
		docker exec "$n" rm -f "$POLL_LOG" "$POLL_STOP"
		docker exec -d "$n" sh -c "$POLLER" sh "$POLL_LOG" "$POLL_STOP" "$ARG_POLL"
	done
	sleep 2
	# SIGPIPE rule (#1121, e2e/README.md): this script runs under pipefail, so
	# no pipeline may end in a reader that exits before its writer is done
	# (`head`, `grep -q`/`-m`, `awk '...; exit'`): the writer dies of SIGPIPE
	# and the pipeline fails with 141 at a random point. Read to EOF instead
	# (`grep -c ... >/dev/null`, awk without `exit`).
	for n in "${NODES[@]}"; do
		docker exec "$n" tail -n 1 "$POLL_LOG" | grep -c ' 1$' >/dev/null ||
			die "the proxy on $n is not connected to its agent before the first roll ($(docker exec "$n" tail -n 1 "$POLL_LOG"))"
	done
	ok "proxy-side pollers running every ${ARG_POLL}s on ${NODES[*]}"
}

stop_pollers() {
	local n
	for n in "${NODES[@]}"; do
		docker exec "$n" touch "$POLL_STOP" || true
		docker exec "$n" cat "$POLL_LOG" >"$ARG_OUT/cs-$n.log" || true
	done
}

agent_pod_on() {
	kc -n "$NS" get pod -l app.kubernetes.io/name=aether-agent --field-selector "spec.nodeName=$1" \
		--no-headers -o custom-columns=N:.metadata.name,D:.metadata.deletionTimestamp | awk '!f && $2 == "<none>" { print $1; f = 1 }'
}

# --- analysis --------------------------------------------------------------------

# analyze.py CS_LOG ROLL_START ROLL_END AGENT_LOG INIT_LOG -> one TSV row:
# gap replace init startup identity reconnect (seconds; "-" when unreadable)
PY_ROW='
import json, sys, datetime

cs, t_from, t_to, agent_log, init_log = sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), sys.argv[4], sys.argv[5]

samples = []
for line in open(cs):
    p = line.split()
    if len(p) == 2 and p[1] in ("0", "1"):
        samples.append((float(p[0]), int(p[1])))

# The first 1 -> 0 transition inside the roll, and the first run of three 1s after it.
down = up = None
for i in range(1, len(samples)):
    t, v = samples[i]
    if t < t_from or t > t_to + 30:
        continue
    if down is None:
        if v == 0 and samples[i - 1][1] == 1:
            down = t
        continue
    if v == 1 and i + 2 < len(samples) and samples[i + 1][1] == 1 and samples[i + 2][1] == 1:
        up = t
        break

def ts(r):
    t = r.get("timestamp") or r.get("time") or ""
    t = t.replace("Z", "+00:00")
    if "." in t:
        head, rest = t.split(".", 1)
        tz = rest[-6:]
        t = head + "." + rest[:-6][:6] + tz
    return datetime.datetime.fromisoformat(t).timestamp()

def records(path):
    out = []
    for line in open(path):
        try:
            r = json.loads(line)
        except Exception:
            continue
        if isinstance(r, dict) and ("message" in r or "msg" in r):
            r.setdefault("message", r.get("msg", ""))
            try:
                out.append((ts(r), r))
            except Exception:
                pass
    return out

agent = records(agent_log)
init = records(init_log)

def first(recs, needle):
    for t, r in recs:
        if needle in r["message"]:
            return t, r
    return None, None

t_start, _ = first(agent, "starting aether agent")
_, held = first(agent, "identity acquired; generating the initial snapshot")
t_reg, _ = first(agent, "registry connected; generating the initial snapshot")
t_certs, _ = first(agent, "client certificates")
cands = [x for x in (t_reg, t_certs) if x is not None]
t_serving = max(cands) if cands else None
t_init0 = init[0][0] if init else None
t_init1 = init[-1][0] if init else None

def d(a, b):
    return "-" if a is None or b is None else "%.3f" % (a - b)

def dur(s):
    if not s:
        return "-"
    s = s.get("held", "")
    try:
        if s.endswith("ms"):
            return "%.3f" % (float(s[:-2]) / 1000)
        if s.endswith("s"):
            return "%.3f" % float(s[:-1])
    except ValueError:
        pass
    return "-"

print("\t".join([d(up, down), d(t_start, down), d(t_init1, t_init0), d(t_serving, t_start), dur(held), d(up, t_serving)]))
'

summarize() {
	local col="$1" name="$2"
	cut -f"$col" "$ARG_OUT/gap.tsv" | grep -v '^-$' | LC_ALL=C sort -g | awk -v n="$name" '
		{ v[NR] = $1 } END {
			if (!NR) { printf "    %-34s n=0\n", n; exit }
			p99 = v[int(0.99 * NR + 0.999)]; if (p99 == "") p99 = v[NR]
			printf "    %-34s n=%-3d min %6.3f  median %6.3f  p99 %6.3f  max %6.3f\n", n, NR, v[1], v[int((NR + 1) / 2)], p99, v[NR] }'
}

measure() {
	log "aether#1123 agent restart gap: $ARG_ROLLS rolls, poll ${ARG_POLL}s, gate ${ARG_BOUND}s (ARG_EXPECT=$ARG_EXPECT); results in $ARG_OUT"
	kc -n "$NS" get ds aether-agent -o jsonpath='{range .spec.template.spec.initContainers[*]}{.name}={.image} {end}{range .spec.template.spec.containers[*]}{.name}={.image}{end}{"\n"}'
	: >"$ARG_OUT/gap.tsv"
	start_pollers
	trap stop_pollers EXIT
	local r n t0 t1
	for r in $(seq 1 "$ARG_ROLLS"); do
		t0="$(date +%s.%N)"
		kc -n "$NS" rollout restart ds/aether-agent >/dev/null
		kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "roll $r never completed"
		sleep "$ARG_SETTLE"
		t1="$(date +%s.%N)"
		for n in "${NODES[@]}"; do
			local pod
			pod="$(agent_pod_on "$n")"
			kc -n "$NS" logs "$pod" -c agent >"$ARG_OUT/agent-$r-$n.log" 2>/dev/null || true
			kc -n "$NS" logs "$pod" -c cni-install >"$ARG_OUT/init-$r-$n.log" 2>/dev/null || true
			docker exec "$n" cat "$POLL_LOG" >"$ARG_OUT/cs-$n.log"
			local row
			row="$(python3 -c "$PY_ROW" "$ARG_OUT/cs-$n.log" "$t0" "$t1" "$ARG_OUT/agent-$r-$n.log" "$ARG_OUT/init-$r-$n.log")"
			printf '%s\t%s\t%s\n' "$r" "$n" "$row" >>"$ARG_OUT/gap.tsv"
			# shellcheck disable=SC2086 # split the row into the printf arguments
			printf '  roll %-2s %-26s gap %6ss  (replace %6ss [init %ss]  startup %6ss [identity %ss]  reconnect %ss)\n' "$r" "$n" $row
		done
	done
	stop_pollers
	trap - EXIT

	log "summary (seconds, per node per roll)"
	summarize 3 "gap (proxy without an ADS stream)"
	summarize 4 "replace (gap start -> agent start)"
	summarize 5 "  cni-install init container"
	summarize 6 "startup (agent start -> serving)"
	summarize 7 "  own SVID wait"
	summarize 8 "reconnect (serving -> stream)"

	case "$ARG_EXPECT" in
	green)
		awk -F'\t' -v b="$ARG_BOUND" '$3 == "-" || $3 > b { bad = 1 } END { exit bad }' "$ARG_OUT/gap.tsv" ||
			die "a node's proxy had no ADS stream for more than ${ARG_BOUND}s across an agent roll (or the gap was unreadable)"
		ok "every gap <= ${ARG_BOUND}s"
		;;
	report) ok "reported, not gated" ;;
	*) die "ARG_EXPECT must be green or report, got '$ARG_EXPECT'" ;;
	esac
}

# swap TAG — roll the agent DaemonSet onto <registry>/agent:TAG and
# <registry>/cni-install:TAG (both loaded into the cluster first).
swap() {
	local tag="$1" img
	for img in agent cni-install; do
		kind load docker-image "${IMAGE_REGISTRY}/$img:$tag" --name "$CLUSTER" >/dev/null || die "could not load $img:$tag"
	done
	kc -n "$NS" set image ds/aether-agent "agent=${IMAGE_REGISTRY}/agent:$tag" "cni-install=${IMAGE_REGISTRY}/cni-install:$tag" >/dev/null
	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the agent never rolled onto $tag"
	ok "agent DaemonSet on agent:$tag + cni-install:$tag"
}

arg_up() {
	raise_inotify
	build_images
	create_cluster
	if [ "$ARG_BACKEND" = etcd ]; then start_etcd; fi
	load_images
	install_gwapi_crds
	install_spire
	install_aether
	deploy_workloads
}

arg_down() {
	docker rm -f "$ETCD_NAME" >/dev/null 2>&1 || true
	down
}

case "${1:-}" in
up) arg_up ;;
measure) measure ;;
swap) swap "${2:?usage: $0 swap <tag>}" ;;
down) arg_down ;;
*) die "usage: $0 {up|measure|swap <tag>|down}" ;;
esac
