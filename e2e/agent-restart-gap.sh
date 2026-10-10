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
# With the surge strategy (proposal 041, `strategy on`) the new agent is a
# standby long before the gap opens, so replace/startup run negative and the
# terms that matter are:
#
#   lock       gap start (the old agent's exit) -> `node lock acquired`
#   takeover   lock -> `this agent owns the node` (the storage diff)
#   bound      owned -> the proxy's fresh stream (reconnect backoff)
#
# LOAD. While `measure` runs, every source pod sends a request to a destination
# on the OTHER node every ARG_LOAD_INTERVAL s (both node proxies on the path);
# the non-200 count is reported per run (not gated: the proxies route on their
# last config while their agent is away, so it should be 0 either way).
#
# Readings are wall-clock on one host (kind nodes are containers), so the
# proxy-side and log-side times compare directly; the poll period bounds the
# gap's resolution.
#
# Usage:
#   EWQ_IMAGE_TAG=<tag> EWQ_SKIP_BUILD=1 e2e/agent-restart-gap.sh up   # ARG_SURGE=1: surge from the install
#   ARG_ROLLS=10 e2e/agent-restart-gap.sh measure        # report + gate
#   e2e/agent-restart-gap.sh strategy on|off             # surge / delete-then-create (no roll)
#   e2e/agent-restart-gap.sh overlap                     # CNI ADD + DEL inside a surge overlap
#   e2e/agent-restart-gap.sh delete-pod                  # kubectl delete pod: serves at once
#   e2e/agent-restart-gap.sh swap <tag>                  # agent + cni-install images
#   e2e/agent-restart-gap.sh down
#
# ARG_EXPECT=green (default) fails `measure` when any gap exceeds ARG_BOUND.
# Unset, the bound follows the strategy measured: 1 s for surge (proposal 041),
# 5 s (the #1123 target) for delete-then-create. ARG_EXPECT=report only reports.
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
ARG_BOUND="${ARG_BOUND:-}" # resolved per run by gate_bound
ARG_POLL="${ARG_POLL:-0.05}"
ARG_SETTLE="${ARG_SETTLE:-15}"
ARG_SURGE="${ARG_SURGE:-0}"
ARG_LOAD="${ARG_LOAD:-1}"
ARG_LOAD_INTERVAL="${ARG_LOAD_INTERVAL:-0.05}"
ARG_OVERLAP_HOLD="${ARG_OVERLAP_HOLD:-30}"
ARG_OUT="${ARG_OUT:-$(mktemp -d)}"
mkdir -p "$ARG_OUT"
ETCD_NAME="$CLUSTER-etcd"
# shellcheck source=e2e/etcd-image.sh
. "$HERE/etcd-image.sh"
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

# Surge (proposal 041): the standby takes the node over when the old agent exits.
t_lock, _ = first(agent, "node lock acquired")
t_owned, _ = first(agent, "this agent owns the node")

# A surge takeover can be shorter than one poll period (~80 ms with the curl
# cost): the proxy drops and is back between two samples, and no 0 is ever
# read. Then the gap is bounded by the samples bracketing the takeover, which
# must both read 1 (and the next three too, the usual "stays up" rule): report
# that interval as the gap, an upper bound, rather than "unreadable".
if down is None and t_owned is not None and t_from <= t_owned <= t_to + 30:
    for i in range(1, len(samples) - 3):
        if samples[i - 1][0] <= t_owned < samples[i][0]:
            if all(v == 1 for _, v in samples[i - 1:i + 3]):
                down, up = samples[i - 1][0], samples[i][0]
            break

print("\t".join([d(up, down), d(t_start, down), d(t_init1, t_init0), d(t_serving, t_start), dur(held), d(up, t_serving),
                 d(t_lock, down), d(t_owned, t_lock), d(up, t_owned)]))
'

summarize() {
	local col="$1" name="$2"
	# A column of nothing but `-` makes `grep -v` exit 1; under `set -e` +
	# pipefail that aborted the summary before awk's n=0 line (#1143). Filter
	# in awk instead, which exits 0 on no input.
	cut -f"$col" "$ARG_OUT/gap.tsv" | awk '$0 != "-"' | LC_ALL=C sort -g | awk -v n="$name" '
		{ v[NR] = $1 } END {
			if (!NR) { printf "    %-34s n=0\n", n; exit }
			p99 = v[int(0.99 * NR + 0.999)]; if (p99 == "") p99 = v[NR]
			printf "    %-34s n=%-3d min %6.3f  median %6.3f  p99 %6.3f  max %6.3f\n", n, NR, v[1], v[int((NR + 1) / 2)], p99, v[NR] }'
}

# --- load -------------------------------------------------------------------------

LOAD_STOP="/tmp/aether-1123-load.stop"

# One loop per source pod (on $NODE), each to a destination on $DST_NODE, so
# every request crosses both node proxies. One line per request:
# "<epoch> <http_code>".
# shellcheck disable=SC2016 # evaluated by the pod's shell
LOADER='
rm -f "$3"
while [ ! -e "$3" ]; do
	c=$(curl -s -o /dev/null --max-time 2 -w "%{http_code}" "$1")
	echo "$(date +%s) ${c:-000}"
	sleep "$2"
done'

start_load() {
	[ "$ARG_LOAD" = 1 ] || return 0
	local src dst i=0 pod
	local dsts=("${QUIC_DSTS[@]}")
	LOAD_PIDS=()
	for src in "${SOURCES[@]}"; do
		dst="${dsts[$((i % ${#dsts[@]}))]}"
		i=$((i + 1))
		pod="$(pod_of "$src")"
		[ -n "$pod" ] || die "load: no pod for source $src"
		kc -n "$TEST_NS" exec "$pod" -c curl -- sh -c "$LOADER" sh \
			"http://$(fqdn "$dst"):$OUTBOUND_PORT/hostname" "$ARG_LOAD_INTERVAL" "$LOAD_STOP" \
			>"$ARG_OUT/load-$src.log" 2>/dev/null &
		LOAD_PIDS+=("$!")
	done
	ok "load running: ${SOURCES[*]} -> ${dsts[*]} every ${ARG_LOAD_INTERVAL}s"
}

stop_load() {
	[ "$ARG_LOAD" = 1 ] || return 0
	local src pod pid
	for src in "${SOURCES[@]}"; do
		pod="$(pod_of "$src")"
		[ -z "$pod" ] || kc -n "$TEST_NS" exec "$pod" -c curl -- touch "$LOAD_STOP" >/dev/null 2>&1 || true
	done
	for pid in "${LOAD_PIDS[@]+"${LOAD_PIDS[@]}"}"; do wait "$pid" 2>/dev/null || true; done
	LOAD_PIDS=()
}

report_load() {
	[ "$ARG_LOAD" = 1 ] || return 0
	cat "$ARG_OUT"/load-*.log 2>/dev/null | awk '
		{ n++; c[$2]++ } END {
			bad = n - c["200"]
			printf "    load: %d requests, %d non-200", n, bad
			for (k in c) if (k != "200") printf "  [%s x%d]", k, c[k]
			printf "\n" }'
}

stop_all() {
	stop_pollers
	stop_load
}

# gate_bound — ARG_BOUND if set; otherwise 1.0 for a surge DaemonSet (proposal
# 041) and 5.0 (#1123) for delete-then-create. With no cluster to ask (a
# `report` of a finished run), the run itself says which: surge rows carry a
# takeover reading.
gate_bound() {
	if [ -n "$ARG_BOUND" ]; then
		printf '%s' "$ARG_BOUND"
		return
	fi
	local surge
	surge="$(kc -n "$NS" get ds aether-agent -o jsonpath='{.spec.updateStrategy.rollingUpdate.maxSurge}' 2>/dev/null || true)"
	if [ -z "$surge" ] && [ -s "$ARG_OUT/gap.tsv" ]; then
		surge="$(awk -F'\t' '$10 != "-" { s = 1 } END { print s + 0 }' "$ARG_OUT/gap.tsv")"
	fi
	if [ "$surge" = 1 ]; then printf '1.0'; else printf '5.0'; fi
}

measure() {
	ARG_BOUND="$(gate_bound)"
	log "aether#1123 agent restart gap: $ARG_ROLLS rolls, poll ${ARG_POLL}s, gate ${ARG_BOUND}s (ARG_EXPECT=$ARG_EXPECT); results in $ARG_OUT"
	kc -n "$NS" get ds aether-agent -o jsonpath='{range .spec.template.spec.initContainers[*]}{.name}={.image} {end}{range .spec.template.spec.containers[*]}{.name}={.image}{end}{"\n"}'
	kc -n "$NS" get ds aether-agent -o jsonpath='  strategy: maxSurge={.spec.updateStrategy.rollingUpdate.maxSurge} maxUnavailable={.spec.updateStrategy.rollingUpdate.maxUnavailable}{"\n"}'
	: >"$ARG_OUT/gap.tsv"
	start_pollers
	start_load
	trap stop_all EXIT
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
			printf '  roll %-2s %-26s gap %6ss  (replace %6ss [init %ss]  startup %6ss [identity %ss]  reconnect %ss | lock %ss takeover %ss bound %ss)\n' "$r" "$n" $row
		done
	done
	stop_all
	trap - EXIT
	report_and_gate
}

# report_and_gate — summarize and gate the run in ARG_OUT (also `report`, to
# re-read a finished run).
report_and_gate() {
	ARG_BOUND="$(gate_bound)"
	log "summary (seconds, per node per roll)"
	summarize 3 "gap (proxy without an ADS stream)"
	summarize 4 "replace (gap start -> agent start)"
	summarize 5 "  cni-install init container"
	summarize 6 "startup (agent start -> serving)"
	summarize 7 "  own SVID wait"
	summarize 8 "reconnect (serving -> stream)"
	summarize 9 "surge: old exit -> lock acquired"
	summarize 10 "surge: lock -> owned (takeover)"
	summarize 11 "surge: owned -> stream"
	report_load

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

# --- proposal 041: the surge strategy ------------------------------------------------

# strategy on|off — switch the agent DaemonSet between surge (maxSurge 1,
# maxUnavailable 0: the chart's agent.updateStrategy.surge=true) and
# delete-then-create. Spec only, not the pod template: nothing rolls.
strategy() {
	local p
	case "$1" in
	on) p='{"spec":{"updateStrategy":{"type":"RollingUpdate","rollingUpdate":{"maxSurge":1,"maxUnavailable":0}}}}' ;;
	off) p='{"spec":{"updateStrategy":{"type":"RollingUpdate","rollingUpdate":{"maxSurge":0,"maxUnavailable":1}}}}' ;;
	*) die "usage: $0 strategy on|off" ;;
	esac
	kc -n "$NS" patch ds aether-agent --type merge -p "$p" >/dev/null
	ok "agent DaemonSet strategy: $(kc -n "$NS" get ds aether-agent -o jsonpath='maxSurge={.spec.updateStrategy.rollingUpdate.maxSurge} maxUnavailable={.spec.updateStrategy.rollingUpdate.maxUnavailable}')"
}

# agent_log_has POD NEEDLE — 0 iff POD's agent log has a line containing NEEDLE.
agent_log_has() {
	kc -n "$NS" logs "$1" -c agent 2>/dev/null | grep -cF -- "$2" >/dev/null
}

# wait_until DESC TIMEOUT CMD... — poll CMD every 0.2 s.
wait_until() {
	local desc="$1" timeout="$2"
	shift 2
	local deadline=$((SECONDS + timeout))
	until "$@"; do
		[ "$SECONDS" -lt "$deadline" ] || die "timed out after ${timeout}s waiting for: $desc"
		sleep 0.2
	done
}

# mesh_pod NAME — a meshed agnhost pod pinned to $DST_NODE under its own
# ServiceAccount (= its mesh service), with no grace period, so its CNI DEL
# follows the delete within a second or two.
mesh_pod() {
	kc apply -f - >/dev/null <<YAML || die "pod $1 apply failed"
apiVersion: v1
kind: ServiceAccount
metadata: {name: $1, namespace: $TEST_NS}
---
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: $TEST_NS
  labels: {app: $1, aether.io/managed: "true"}
  annotations: {endpoint.aether.io/port: "$APP_PORT"}
spec:
  serviceAccountName: $1
  nodeName: $DST_NODE
  terminationGracePeriodSeconds: 0
  containers:
    - name: app
      image: $AGNHOST_IMAGE
      args: ["netexec", "--http-port=$APP_PORT"]
      ports: [{containerPort: $APP_PORT}]
      readinessProbe: {httpGet: {path: /, port: $APP_PORT}, periodSeconds: 1}
YAML
}

pod_gone() { ! kc -n "$TEST_NS" get pod "$1" >/dev/null 2>&1; }
replaced() {
	local p
	p="$(agent_pod_on "$DST_NODE")" && [ -n "$p" ] && [ "$p" != "$1" ]
}
pod_ready() {
	[ "$(kc -n "$TEST_NS" get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = True ]
}
standby_started() { agent_log_has "$1" "starting as a standby"; }
not_owned_yet() { ! agent_log_has "$1" "this agent owns the node"; }
proxy_listeners() { docker exec "$DST_NODE" curl -s --max-time 5 "http://127.0.0.1:9901/listeners"; }

# overlap — the "CNI ADD during the overlap" row of proposal 041, and its DEL
# twin (open question 1), on a real roll. minReadySeconds holds the old agent
# alive for ARG_OVERLAP_HOLD s after the standby is Ready, a deterministic
# window: inside it a pod is CREATED and another DELETED on $DST_NODE. The old
# agent must serve both (it owns cni.sock); after the takeover the new agent
# must hold the added pod's listeners and not the deleted one's, and the added
# pod must be reachable through the mesh.
overlap() {
	log "proposal 041 overlap: CNI ADD + DEL on $DST_NODE while a standby waits"
	[ "$(kc -n "$NS" get ds aether-agent -o jsonpath='{.spec.updateStrategy.rollingUpdate.maxSurge}')" = 1 ] ||
		die "overlap needs the surge strategy (e2e/agent-restart-gap.sh strategy on)"
	kc create ns "$TEST_NS" >/dev/null 2>&1 || true
	kc -n "$TEST_NS" delete pod overlap-add overlap-del --ignore-not-found --wait >/dev/null
	mesh_pod overlap-del
	wait_until "overlap-del Ready" 120 pod_ready overlap-del

	local old new
	old="$(agent_pod_on "$DST_NODE")"
	kc -n "$NS" patch ds aether-agent --type merge -p "{\"spec\":{\"minReadySeconds\":$ARG_OVERLAP_HOLD}}" >/dev/null
	trap 'kc -n "$NS" patch ds aether-agent --type merge -p "{\"spec\":{\"minReadySeconds\":0}}" >/dev/null 2>&1 || true' EXIT
	kc -n "$NS" rollout restart ds/aether-agent >/dev/null

	# The new pod on $DST_NODE, while the old one still runs.
	new=""
	local deadline=$((SECONDS + 180)) p
	while [ -z "$new" ]; do
		[ "$SECONDS" -lt "$deadline" ] || die "no standby agent appeared on $DST_NODE"
		for p in $(kc -n "$NS" get pod -l app.kubernetes.io/name=aether-agent --field-selector "spec.nodeName=$DST_NODE" -o name); do
			p="${p#pod/}"
			[ "$p" != "$old" ] && new="$p"
		done
		sleep 0.5
	done
	wait_until "$new logs it is a standby" 120 standby_started "$new"
	ok "standby $new beside $old"

	mesh_pod overlap-add
	kc -n "$TEST_NS" delete pod overlap-del --wait=false >/dev/null
	wait_until "overlap-add Ready" 120 pod_ready overlap-add
	wait_until "overlap-del gone" 120 pod_gone overlap-del
	not_owned_yet "$new" ||
		die "the standby took the node over before the ADD and DEL completed; raise ARG_OVERLAP_HOLD (${ARG_OVERLAP_HOLD}s)"
	agent_log_has "$old" '"pod":"overlap-add"' || die "the OLD agent did not serve overlap-add's CNI ADD"
	kc -n "$NS" logs "$old" -c agent | grep -F '"pod":"overlap-del"' | grep -cF "CNI DEL served" >/dev/null ||
		die "the OLD agent did not serve overlap-del's CNI DEL"
	ok "inside the overlap: the old agent served the ADD (overlap-add) and the DEL (overlap-del); the standby had not taken over"

	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the roll never completed"
	kc -n "$NS" patch ds aether-agent --type merge -p '{"spec":{"minReadySeconds":0}}' >/dev/null
	trap - EXIT
	wait_until "$new owns the node" 60 agent_log_has "$new" "this agent owns the node"

	local line
	line="$(kc -n "$NS" logs "$new" -c agent | grep -F "takeover: applied the previous agent's CNI ADD/DEL" || true)"
	[ -n "$line" ] || die "the new agent logged no takeover reconcile"
	printf '    %s\n' "$line"
	printf '%s\n' "$line" | grep -cE '"added":[1-9]' >/dev/null || die "the takeover did not pick up the overlap's ADD"
	printf '%s\n' "$line" | grep -cE '"removed":[1-9]' >/dev/null || die "the takeover did not pick up the overlap's DEL"

	local ls
	ls="$(proxy_listeners)"
	printf '%s\n' "$ls" | grep -c "^outbound_http_${TEST_NS}_overlap-add::" >/dev/null ||
		die "after the takeover the node proxy has no listener for overlap-add"
	if printf '%s\n' "$ls" | grep -c 'overlap-del' >/dev/null; then
		die "after the takeover the node proxy still has a listener for the DELETED overlap-del"
	fi
	ok "after the takeover: overlap-add's listeners are served, overlap-del's are gone"

	local src code="" i
	src="$(pod_of "${SOURCES[0]}")"
	for i in $(seq 1 30); do
		code="$(kc -n "$TEST_NS" exec "$src" -c curl -- curl -s -o /dev/null --max-time 3 -w '%{http_code}' \
			"http://$(fqdn overlap-add):$OUTBOUND_PORT/hostname" 2>/dev/null || true)"
		[ "$code" = 200 ] && break
		sleep 1
	done
	[ "$code" = 200 ] || die "overlap-add is not reachable through the mesh after the takeover (last code $code)"
	ok "overlap-add answers 200 through the mesh after the takeover"
	kc -n "$TEST_NS" delete pod overlap-add --wait=false >/dev/null
}

# delete-pod — open question 4: `kubectl delete pod` of an agent, whatever the
# strategy, is delete-then-create. The new agent must find the lock free and
# own the node at once (no standby, no takeover step), and the proxy must be
# back on a stream within the pre-041 bound.
delete_pod() {
	log "proposal 041 open question 4: kubectl delete pod of the agent on $DST_NODE"
	local old new t0
	old="$(agent_pod_on "$DST_NODE")"
	start_pollers
	trap stop_pollers EXIT
	t0="$(date +%s.%N)"
	kc -n "$NS" delete pod "$old" --wait=false >/dev/null
	wait_until "a replacement agent on $DST_NODE" 180 replaced "$old"
	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the replacement never became Ready"
	sleep "$ARG_SETTLE"
	new="$(agent_pod_on "$DST_NODE")"
	[ "$new" != "$old" ] || die "no replacement pod"
	if agent_log_has "$new" "starting as a standby"; then
		die "$new started as a standby after a plain pod delete: the old agent was still holding the lock"
	fi
	if agent_log_has "$new" "node lock acquired"; then
		die "$new waited for the node lock after a plain pod delete"
	fi
	kc -n "$NS" logs "$new" -c agent >"$ARG_OUT/delete-pod-agent.log"
	docker exec "$DST_NODE" cat "$POLL_LOG" >"$ARG_OUT/cs-delete-pod.log"
	stop_pollers
	trap - EXIT
	local row gap
	row="$(python3 -c "$PY_ROW" "$ARG_OUT/cs-delete-pod.log" "$t0" "$(date +%s.%N)" "$ARG_OUT/delete-pod-agent.log" /dev/null)"
	gap="$(printf '%s\n' "$row" | cut -f1)"
	[ "$gap" != "-" ] || die "the proxy on $DST_NODE never reconnected after the agent pod was deleted"
	awk -v g="$gap" 'BEGIN { exit !(g <= 20) }' || die "the proxy on $DST_NODE had no ADS stream for ${gap}s after a pod delete"
	ok "$new owned the node at once (no standby, no lock wait); proxy gap ${gap}s (delete-then-create, as before 041)"
}

arg_up() {
	if [ "$ARG_SURGE" = 1 ]; then
		EWQ_EXTRA_HELM_ARGS+=(--set agent.updateStrategy.surge=true)
	fi
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
strategy) strategy "${2:?usage: $0 strategy on|off}" ;;
report) report_and_gate ;;
overlap) overlap ;;
delete-pod) delete_pod ;;
down) arg_down ;;
*) die "usage: $0 {up|measure|report|strategy on|off|overlap|delete-pod|swap <tag>|down}" ;;
esac
