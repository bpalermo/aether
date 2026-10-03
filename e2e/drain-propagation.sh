#!/usr/bin/env bash
# aether#1103: how long a SOURCE node proxy keeps selecting a destination after
# the destination's agent marked it DRAINING (pod deletionTimestamp, the
# two-phase drain of #152) -- in steady state, and when the source's own node
# agent is restarting at the same time.
#
# THE SOAK SIGNATURE (2026-10-01 TRIPLE). Five requests went to svc-3 pods 3.4 to
# 3.8 s after their destination agents logged "endpoint marked draining". The
# registrar fanned the mark out within ~0.15 s; the source proxies did not hear
# it because their ADS stream was DOWN: in the TRIPLE the source agents were
# restarting too. main-worker-04's agent shut down at 05:41:24.45, came back
# serving xDS at 05:41:39.93, and the proxy did not reconnect until 05:41:48.34
# -- 8.4 s after the socket was serving, Envoy's fully jittered 500 ms / 30 s
# xDS reconnect backoff. The three requests went out at 05:41:39.8-40.0.
#
# THE HARNESS. EWQ_WORKER=1 shape of e2e/eastwest-quic.sh (the source client-a
# on the control plane, the destination quic-a on the worker), on the etcd
# registry backend as talos-main runs it (REGISTRY_BACKEND=kubernetes, read at
# `up` and `verify`, for the chart default). quic-a runs 2 replicas with a 15 s preStop sleep, so a deleted
# replica's application keeps serving -- and logging every request it gets --
# for 15 s after its drain mark: the last request it logs is the last request
# a source SELECTED it for. Load is 4 unpinned GET loops from client-a.
#
#   S  steady   DRP_RUNS deletions of one quic-a replica, nothing else moving.
#               gap = last request at the victim's app - the destination
#               agent's drain mark. Gated at DRP_STEADY_BOUND (1 s).
#   R  restart  DRP_RUNS deletions of one quic-a replica DRP_RESTART_LEAD s
#               after the SOURCE node's agent pod was deleted. The mark lands
#               while the source proxy has no ADS stream; it can only hear it
#               after the new agent serves xDS and the proxy reconnects.
#               Reported per run: the gap above, the source agent's outage
#               (old pod gone -> new agent serving xDS), and the RECONNECT LAG
#               (new agent serving -> the proxy's first CDS response on the
#               new stream). The fix (#1103) caps the reconnect backoff at 1 s:
#               green gates every lag at DRP_RECONNECT_BOUND (1.5 s), and
#               that no CDS response after the reconnect REMOVES a cluster (a
#               first snapshot built before the pods' certificates withdraws
#               the `quic:` twin the proxy holds); red reports.
#
# Red and green differ in the proxy bootstrap and the agent image. `bootstrap
# main|fix` rewrites the live proxy ConfigMap with or without the ADS
# retry_back_off and waits for the supervisor's hot restart to load it (read
# back from the admin config dump); `swap-agent TAG` rolls the agent onto an
# image with (or without) the first-serve wait for the local client
# certificates. The bootstrap alone, on main's agent, reconnects fast into a
# first snapshot that lacks the pods' `quic:` twins -- the R gate's removal
# check fails on it -- which is why the two ship together.
#
# REGISTRAR REPLICAS (aether#1124). The registrar runs 2 replicas. Every run
# reports which one the source agent and the destination agent hold their
# registrar connection to (read from the node's conntrack table), and the gap
# is summarized split same-replica / cross-replica. On the kubernetes backend
# the agent's DRAINING mark is a no-op write that only the receiving replica's
# snapshot carries, so cross-replica is the case that matters there; verify
# steers leg S onto both (DRP_PLACEMENT, default alternate on that backend).
#
#   REGISTRY_BACKEND=kubernetes EWQ_CLUSTER=aether-1124 e2e/drain-propagation.sh up
#   REGISTRY_BACKEND=kubernetes DRP_LEGS=S DRP_RUNS=8 e2e/drain-propagation.sh verify
#   e2e/drain-propagation.sh swap-registrar <tag-with-the-fix>
#
# Usage:
#   EWQ_IMAGE_TAG=<tag> EWQ_SKIP_BUILD=1 e2e/drain-propagation.sh up
#   e2e/drain-propagation.sh bootstrap main && DRP_EXPECT=red e2e/drain-propagation.sh verify
#   e2e/drain-propagation.sh swap-agent <tag-with-the-fix>
#   e2e/drain-propagation.sh bootstrap fix && DRP_EXPECT=green e2e/drain-propagation.sh verify
#   e2e/drain-propagation.sh down
set -euo pipefail
if [ -n "${DRP_TRACE:-}" ]; then set -x; fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export EWQ_CLUSTER="${EWQ_CLUSTER:-aether-1103}"
export EWQ_WORKER=1

# shellcheck source=e2e/eastwest-quic.sh
. "$HERE/eastwest-quic.sh"

DRP_EXPECT="${DRP_EXPECT:-green}"
# REGISTRY_BACKEND (or DRP_BACKEND) = etcd | kubernetes, read at `up` and by
# `verify` for the placement steering below.
DRP_BACKEND="${REGISTRY_BACKEND:-${DRP_BACKEND:-etcd}}"
case "$DRP_BACKEND" in
etcd | kubernetes) ;;
*)
	printf 'REGISTRY_BACKEND must be etcd or kubernetes, got %s\n' "$DRP_BACKEND" >&2
	exit 1
	;;
esac
# DRP_PLACEMENT: which registrar replica the SOURCE agent is on relative to the
# DESTINATION agent's (aether#1124). The destination agent writes the drain mark
# to the replica it holds its registrar connection to; the source agent hears it
# from the replica IT watches. `alternate` (the default on the kubernetes
# backend) steers odd runs onto the same replica and even runs onto the other
# one, by restarting the source agent until its new connection lands there;
# `same` / `cross` pin every run; `any` (the etcd default) only reports.
DRP_PLACEMENT="${DRP_PLACEMENT:-$([ "$DRP_BACKEND" = kubernetes ] && echo alternate || echo any)}"
DRP_RUNS="${DRP_RUNS:-5}"
DRP_LEGS="${DRP_LEGS:-S R}"
DRP_STEADY_BOUND="${DRP_STEADY_BOUND:-1.0}"
DRP_RECONNECT_BOUND="${DRP_RECONNECT_BOUND:-1.5}"
DRP_RESTART_LEAD="${DRP_RESTART_LEAD:-2}"
DRP_PRESTOP="${DRP_PRESTOP:-15}"
DRP_OUT="${DRP_OUT:-$(mktemp -d)}"
mkdir -p "$DRP_OUT"
DRP_DST="quic-a"
DRP_SRC="client-a"
ETCD_NAME="$CLUSTER-etcd"
ETCD_IMAGE="${ETCD_IMAGE:-quay.io/coreos/etcd:v3.5.16}"

# --- registry backend ----------------------------------------------------------

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

# --- registrar replica placement (aether#1124) ---------------------------------

# registrar_of AGENT_POD — the registrar replica AGENT_POD holds its registrar
# connection to. Registration (the drain mark) and the endpoint watch share that
# one gRPC connection, dialled at the registrar Service VIP; kube-proxy DNATs it
# to one replica, so the node's conntrack table names the replica's pod IP as the
# reply source. Prints the replica's pod name, "?" when there is no established
# flow, or "a|b" when the agent holds flows to both.
registrar_of() {
	local pod="$1" node src vip ips ip names=""
	node="$(kc -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
	src="$(kc -n "$NS" get pod "$pod" -o jsonpath='{.status.podIP}')"
	vip="$(kc -n "$NS" get svc aether-registrar -o jsonpath='{.spec.clusterIP}')"
	ips="$(docker exec "$node" conntrack -L -p tcp --orig-src "$src" --orig-dst "$vip" 2>/dev/null |
		awk '/ESTABLISHED/ { n = 0; for (i = 1; i <= NF; i++) if ($i ~ /^src=/ && ++n == 2) print substr($i, 5) }' | sort -u)"
	for ip in $ips; do
		names="${names:+$names|}$(kc -n "$NS" get pod -l app.kubernetes.io/name=aether-registrar \
			-o jsonpath="{.items[?(@.status.podIP==\"$ip\")].metadata.name}")"
	done
	echo "${names:-?}"
}

# place_source WANT — restart the source agent until its registrar replica is
# the destination agent's (WANT=same) or the other one (WANT=cross). Each new
# agent dials the Service afresh, so each restart is a fresh draw.
place_source() {
	local want="$1" tries=0 src dst got
	while true; do
		src="$(registrar_of "$(agent_pod_on "$NODE")")"
		dst="$(registrar_of "$(agent_pod_on "$DST_NODE")")"
		got="?"
		if [[ "$src" != *"|"* && "$src" != "?" && "$dst" != *"|"* && "$dst" != "?" ]]; then
			if [ "$src" = "$dst" ]; then got=same; else got=cross; fi
		fi
		[ "$got" = "$want" ] && return 0
		tries=$((tries + 1))
		[ "$tries" -le 12 ] || die "could not place the source agent on the $want registrar replica (source on $src, destination on $dst)"
		echo "  placement: source agent on $src, destination agent on $dst ($got); want $want -- restarting the source agent"
		kc -n "$NS" delete pod "$(agent_pod_on "$NODE")" --wait=false >/dev/null
		sleep 3
		kc -n "$NS" rollout status ds/aether-agent --timeout=180s >/dev/null || die "the agent DaemonSet never became Ready"
		sleep 3
	done
}

# --- the destination workload --------------------------------------------------

# drp_workload — quic-a at 2 replicas on the worker, each holding its app for
# DRP_PRESTOP s after deletion (a native sleep preStop, as the soak's services).
drp_workload() {
	local grace=$((DRP_PRESTOP + 15))
	kc -n "$TEST_NS" patch deploy "$DRP_DST" --type=strategic -p "$(
		cat <<JSON
{"spec":{"replicas":2,"template":{"spec":{"terminationGracePeriodSeconds":$grace,
 "containers":[{"name":"app","lifecycle":{"preStop":{"sleep":{"seconds":$DRP_PRESTOP}}}}]}}}}
JSON
	)" >/dev/null
	kc -n "$TEST_NS" rollout status "deploy/$DRP_DST" --timeout=180s >/dev/null || die "$DRP_DST never rolled to 2 Ready replicas"
}

dst_pods() {
	kc -n "$TEST_NS" get pod -l "app=$DRP_DST" --no-headers \
		-o custom-columns=N:.metadata.name,D:.metadata.deletionTimestamp,P:.status.phase |
		awk '$2 == "<none>" && $3 == "Running" { print $1 }'
}

# wait_two_serving — 2 Running quic-a pods, both receiving client-a's requests
# (checked in their app logs), and no terminating quic-a pod left.
wait_two_serving() {
	local deadline=$((SECONDS + 240)) pods p n tag
	while true; do
		pods="$(dst_pods)"
		if [ "$(printf '%s\n' "$pods" | grep -c .)" -eq 2 ] &&
			[ "$(kc -n "$TEST_NS" get pod -l "app=$DRP_DST" --no-headers | grep -c .)" -eq 2 ]; then
			tag="warm$RANDOM"
			req_batch "$DRP_SRC" "$DRP_DST" "/echo?msg=$tag" 30 >/dev/null || true
			n=0
			# SIGPIPE rule (#1121, e2e/README.md): this script runs under
			# pipefail, so no pipeline may end in a reader that exits before its
			# writer is done (`head`, `grep -q`/`-m`, `awk '...; exit'`): the
			# writer dies of SIGPIPE and the pipeline fails with 141 at a random
			# point. Read to EOF, or capture the writer first (as here).
			for p in $pods; do
				grep -q "msg=$tag" <<<"$(kc -n "$TEST_NS" logs "$p" -c app 2>/dev/null)" && n=$((n + 1))
			done
			[ "$n" -eq 2 ] && return 0
		fi
		[ "$SECONDS" -lt "$deadline" ] || die "never had 2 serving $DRP_DST replicas"
		sleep 3
	done
}

# --- readings ------------------------------------------------------------------

agent_pod_on() {
	kc -n "$NS" get pod -l app.kubernetes.io/name=aether-agent --field-selector "spec.nodeName=$1" \
		--no-headers -o custom-columns=N:.metadata.name,D:.metadata.deletionTimestamp | awk '!f && $2 == "<none>" { print $1; f = 1 }'
}

proxy_pod_on() {
	kc -n "$NS" get pod -l app.kubernetes.io/name=aether-proxy --field-selector "spec.nodeName=$1" \
		--no-headers -o custom-columns=N:.metadata.name,D:.metadata.deletionTimestamp | awk '!f && $2 == "<none>" { print $1; f = 1 }'
}

# The python helpers print epoch seconds (float) or nothing.
PY_AGENT_TS='
import json, sys, datetime
want_msg, want_pod = sys.argv[1], (sys.argv[2] if len(sys.argv) > 2 else "")
for line in sys.stdin:
    try:
        r = json.loads(line)
    except Exception:
        continue
    if want_msg not in r.get("message", ""):
        continue
    if want_pod and r.get("pod") != want_pod:
        continue
    ts = r["timestamp"].replace("Z", "+00:00")
    if "." in ts:
        head, rest = ts.split(".", 1)
        frac, tz = rest[:-6], rest[-6:]
        ts = head + "." + frac[:6] + tz
    print("%.3f" % datetime.datetime.fromisoformat(ts).timestamp())
'

# klog lines: "I1001 14:44:18.805051  1 log.go:245] GET /echo?msg=..."
PY_APP_LAST='
import sys, datetime, calendar
tag = sys.argv[1]
year = datetime.datetime.now(datetime.timezone.utc).year
last = None
for line in sys.stdin:
    if "GET /echo?msg=" + tag not in line:
        continue
    p = line.split()
    mmdd, hms = p[0][1:], p[1]
    t = datetime.datetime.strptime("%d%s %s" % (year, mmdd, hms), "%Y%m%d %H:%M:%S.%f")
    last = calendar.timegm(t.timetuple()) + t.microsecond / 1e6
if last is not None:
    print("%.3f" % last)
'

# Envoy JSON log line time: "time":"2026-10-01T05:41:48.514+00:00"
PY_PROXY_TS='
import json, sys, datetime
want = sys.argv[1]
for line in sys.stdin:
    if want not in line:
        continue
    try:
        r = json.loads(line)
    except Exception:
        continue
    t = r.get("time")
    if t:
        print("%.3f" % datetime.datetime.fromisoformat(t).timestamp())
'

# POLL_HEALTH, run on the SOURCE node: the source proxy's view of one host, from
# its admin /clusters every 100 ms for DUR s. One line per host entry:
# "<epoch> <cluster>::<ip>:<port>::health_flags::<flags>". The first
# /failed_eds_health is the source's pool close (aether#1144): the destination
# agent's phase-2 UNHEALTHY, or the pod's Ready drop, as this source heard it
# (close_connections_on_host_health_failure closes the pools at that update).
# shellcheck disable=SC2016 # evaluated by the node's shell
POLL_HEALTH='
ip="$1"; end=$(( $(date +%s) + $2 ))
while [ "$(date +%s)" -lt "$end" ]; do
	t="$(date +%s.%N)"
	curl -s --max-time 1 http://127.0.0.1:9901/clusters | grep -F "::$ip:" | grep -F "::health_flags::" | sed "s|^|$t |"
	sleep 0.1
done'

# --- load ----------------------------------------------------------------------

# shellcheck disable=SC2016 # evaluated by the pod's shell
LOAD='
url="$1"; tag="$2"; dur="$3"; end=$(( $(date +%s) + dur ))
worker() {
	n=0
	while [ "$(date +%s)" -lt "$end" ]; do
		n=$((n + 1))
		curl -s -o /dev/null --max-time 20 -w "%{http_code}\n" "$url/echo?msg=$tag-w$1-$n"
		sleep 0.05
	done
}
for w in 1 2 3 4; do worker "$w" & done
wait'

start_load() {
	local tag="$1" dur="$2" pod
	pod="$(pod_of "$DRP_SRC")"
	[ -n "$pod" ] || die "no Running $DRP_SRC pod"
	kc -n "$TEST_NS" exec "$pod" -c curl -- sh -c "$LOAD" sh \
		"http://$(fqdn "$DRP_DST"):$OUTBOUND_PORT" "$tag" "$dur" >"$DRP_OUT/$tag.codes" 2>/dev/null &
	LOAD_PID=$!
}

# --- one run ---------------------------------------------------------------------

# run_once LEG I — delete one quic-a replica under load (after restarting the
# source agent for leg R) and append one result line to $DRP_OUT/LEG.tsv:
#   S: run gap_s reqs_after_mark+1s non200 placement src_replica dst_replica
#   R: run gap_s reqs_after_mark+1s non200 agent_outage_s reconnect_lag_s
#      cds_removals placement src_replica dst_replica
run_once() {
	local leg="$1" i="$2" tag victim dst_agent src_agent_old t0 dur=30 want
	tag="drp$leg$i$RANDOM"
	# Leg R restarts the source agent itself, so only leg S is steered.
	want="$DRP_PLACEMENT"
	if [ "$want" = alternate ]; then want="$([ $((i % 2)) -eq 1 ] && echo same || echo cross)"; fi
	if [ "$leg" = S ] && { [ "$want" = same ] || [ "$want" = cross ]; }; then place_source "$want"; fi
	wait_two_serving
	victim="$(dst_pods | sed -n '1p')"
	dst_agent="$(agent_pod_on "$DST_NODE")"
	src_agent_old="$(agent_pod_on "$NODE")"
	[ -n "$dst_agent" ] && [ -n "$src_agent_old" ] || die "agent pods not found"
	local follow="$DRP_OUT/$tag.app.log"
	kc -n "$TEST_NS" logs -f "$victim" -c app >"$follow" 2>/dev/null &
	local fpid=$!
	t0="$(date +%s)"
	start_load "$tag" "$dur"
	sleep 4
	if [ "$leg" = R ]; then
		kc -n "$NS" delete pod "$src_agent_old" --wait=false >/dev/null
		sleep "$DRP_RESTART_LEAD"
	fi
	# The registrar replica each side is on as the mark is written: the
	# destination agent's (it sends the mark) and the source agent's (the replica
	# the source proxy's EDS comes from; leg R re-reads it once the new agent is up).
	local src_rep dst_rep place
	dst_rep="$(registrar_of "$dst_agent")"
	if [ "$leg" = S ]; then src_rep="$(registrar_of "$src_agent_old")"; fi
	local victim_ip health="$DRP_OUT/$tag.health" hpid
	victim_ip="$(kc -n "$TEST_NS" get pod "$victim" -o jsonpath='{.status.podIP}')"
	docker exec "$NODE" sh -c "$POLL_HEALTH" sh "$victim_ip" $((DRP_PRESTOP + 25)) >"$health" 2>/dev/null &
	hpid=$!
	kc -n "$TEST_NS" delete pod "$victim" --wait=false >/dev/null
	wait "$LOAD_PID" || true
	wait "$hpid" || true
	sleep 2
	kill "$fpid" 2>/dev/null || true

	local t_mark t_last gap after
	t_mark="$(kc -n "$NS" logs "$dst_agent" -c agent --since=5m 2>/dev/null |
		python3 -c "$PY_AGENT_TS" "endpoint marked draining ahead of shutdown" "$victim" | sed -n '1p')"
	[ -n "$t_mark" ] || die "$leg$i: no drain mark for $victim in $dst_agent's log"
	t_last="$(python3 -c "$PY_APP_LAST" "$tag" <"$follow")"
	[ -n "$t_last" ] || die "$leg$i: $victim logged none of the run's requests (the load did not reach it)"
	gap="$(awk -v a="$t_last" -v b="$t_mark" 'BEGIN { printf "%.3f", a - b }')"
	after="$(grep "GET /echo?msg=$tag" "$follow" | python3 -c '
import sys, datetime, calendar
mark = float(sys.argv[1]); year = datetime.datetime.now(datetime.timezone.utc).year; n = 0
for line in sys.stdin:
    p = line.split()
    t = datetime.datetime.strptime("%d%s %s" % (year, p[0][1:], p[1]), "%Y%m%d %H:%M:%S.%f")
    if calendar.timegm(t.timetuple()) + t.microsecond / 1e6 > mark + 1.0:
        n += 1
print(n)' "$t_mark")"

	local non200
	non200="$(grep -vc '^200$' "$DRP_OUT/$tag.codes" || true)"
	if [ "$leg" = R ]; then src_rep="$(registrar_of "$(agent_pod_on "$NODE")")"; fi
	place="?"
	if [[ "$src_rep" != *"|"* && "$src_rep" != "?" && "$dst_rep" != *"|"* && "$dst_rep" != "?" ]]; then
		if [ "$src_rep" = "$dst_rep" ]; then place=same; else place=cross; fi
	fi
	# The source proxy's pool close (first /failed_eds_health on the victim) and
	# its DRAINING (first draining flag), each relative to the mark; "-" when the
	# poll never saw it.
	# Also the last poll that still listed the host (its EDS removal, when that
	# fell inside the poll window).
	local t_close t_drain t_seen close drain seen
	t_close="$(awk '!f && /failed_eds_health/ { print $1; f = 1 }' "$health")"
	t_drain="$(awk '!f && tolower($0) ~ /draining/ { print $1; f = 1 }' "$health")"
	t_seen="$(awk '{ t = $1 } END { print t }' "$health")"
	close="$([ -n "$t_close" ] && awk -v a="$t_close" -v b="$t_mark" 'BEGIN { printf "%.3f", a - b }' || echo -)"
	drain="$([ -n "$t_drain" ] && awk -v a="$t_drain" -v b="$t_mark" 'BEGIN { printf "%.3f", a - b }' || echo -)"
	seen="$([ -n "$t_seen" ] && awk -v a="$t_seen" -v b="$t_mark" 'BEGIN { printf "%.3f", a - b }' || echo -)"
	if [ "$leg" = S ]; then
		printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$i" "$gap" "$after" "$non200" "$place" "$src_rep" "$dst_rep" "$close" "$drain" "$seen" >>"$DRP_OUT/S.tsv"
		ok "S$i [$place-replica: source agent on $src_rep, destination agent on $dst_rep]: victim $victim  mark->last request ${gap}s  requests >1 s after the mark: $after  non-200: $non200  mark->source DRAINING ${drain}s  mark->source pool close ${close}s  host last listed ${seen}s (SIGTERM at ~${DRP_PRESTOP}s)"
		return
	fi

	# Leg R: the source agent's outage and the proxy's reconnect lag.
	local src_agent_new t_gone t_serving t_reconnect outage lag ppod
	src_agent_new="$(agent_pod_on "$NODE")"
	t_gone="$(kc -n "$NS" logs "$src_agent_new" -c agent 2>/dev/null |
		python3 -c "$PY_AGENT_TS" "starting aether agent" | sed -n '1p')"
	# The socket opens after PreListen's last step: the registry load, then
	# (since #1103) the wait for the local client certificates when there was
	# one to wait for. The latest of those lines is when the agent serves xDS.
	local agent_log
	agent_log="$(kc -n "$NS" logs "$src_agent_new" -c agent 2>/dev/null)"
	t_serving="$({
		printf '%s\n' "$agent_log" | python3 -c "$PY_AGENT_TS" "generating the initial snapshot"
		printf '%s\n' "$agent_log" | python3 -c "$PY_AGENT_TS" "client certificates"
	} | sort -g | tail -n 1)"
	ppod="$(proxy_pod_on "$NODE")"
	t_reconnect="$(kc -n "$NS" logs "$ppod" -c proxy --since=3m 2>/dev/null |
		python3 -c "$PY_PROXY_TS" "cds: response indicates" |
		awk -v s="$t_serving" '!f && $1 >= s - 0.05 { print; f = 1 }')"
	[ -n "$t_serving" ] && [ -n "$t_reconnect" ] || die "R$i: could not read the source agent's serving time ($t_serving) or the proxy's reconnect ($t_reconnect)"
	# CDS responses that REMOVED clusters in the 15 s after the reconnect: the
	# restarted agent withdrawing what the proxy holds (the `quic:` twin, when
	# its first snapshot predates the pod certificates).
	local removals
	removals="$(kc -n "$NS" logs "$ppod" -c proxy --since=3m 2>/dev/null |
		{ grep -E 'cds: response indicates [0-9]+ added/updated cluster\(s\), [1-9][0-9]* removed' || true; } |
		python3 -c "$PY_PROXY_TS" "cds: response indicates" |
		awk -v s="$t_reconnect" '$1 >= s - 0.05 && $1 <= s + 15 { n++ } END { print n + 0 }')"
	outage="$(awk -v a="$t_serving" -v b="$t0" 'BEGIN { printf "%.3f", a - b - 4 }')"
	lag="$(awk -v a="$t_reconnect" -v b="$t_serving" 'BEGIN { printf "%.3f", a - b }')"
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$i" "$gap" "$after" "$non200" "$outage" "$lag" "$removals" "$place" "$src_rep" "$dst_rep" >>"$DRP_OUT/R.tsv"
	ok "R$i [$place-replica: new source agent on $src_rep, destination agent on $dst_rep]: victim $victim  mark->last request ${gap}s  requests >1 s after the mark: $after  non-200: $non200  agent delete->serving ${outage}s (new agent started $(awk -v a="$t_gone" -v b="$t0" 'BEGIN { printf "%.1f", a - b - 4 }')s)  reconnect lag ${lag}s  CDS removals after reconnect: $removals"
}

summarize_leg() {
	local leg="$1" col="$2" name="$3" place="${4:-}" pcol
	pcol="$([ "$leg" = S ] && echo 5 || echo 8)"
	awk -F'\t' -v c="$col" -v p="$place" -v pc="$pcol" 'p == "" || $pc == p { print $c }' "${DRP_OUT_TSV:-$DRP_OUT/$leg.tsv}" |
		LC_ALL=C sort -g | awk -v n="$name${place:+ [$place-replica]}" '
		{ v[NR] = $1 } END { if (NR) printf "    %s: n=%d min %.3f median %.3f max %.3f\n", n, NR, v[1], v[int((NR + 1) / 2)], v[NR] }'
}

# summarize_gap LEG — the mark -> last request gap, overall and split by whether
# the source agent watched the replica the mark was written to (aether#1124).
summarize_gap() {
	local leg="$1" place
	summarize_leg "$leg" 2 "mark -> last request at the victim (s)"
	for place in same cross "?"; do
		summarize_leg "$leg" 2 "mark -> last request at the victim (s)" "$place"
	done
}

drp_verify() {
	log "aether#1103 drain propagation (DRP_EXPECT=$DRP_EXPECT, backend $DRP_BACKEND, placement $DRP_PLACEMENT, legs: $DRP_LEGS, $DRP_RUNS runs each; results in $DRP_OUT)"
	echo "  registrar backend running: $(kc -n "$NS" get deploy aether-registrar -o jsonpath='{.spec.template.spec.containers[0].args}' | tr ',' '\n' | grep -o 'registry-backend=[a-z]*' || echo '?')"
	echo "  proxy ADS retry policy: $(ads_retry_policy)"
	drp_workload
	local leg i
	for leg in $DRP_LEGS; do
		: >"$DRP_OUT/$leg.tsv"
		for i in $(seq 1 "$DRP_RUNS"); do run_once "$leg" "$i"; done
	done

	if [[ " $DRP_LEGS " == *" S "* ]]; then
		log "S steady state"
		summarize_gap S
		# Pool close (aether#1144): the destination agent's phase-2 UNHEALTHY
		# lands ~1 s before SIGTERM on every replica, or only on the one it was
		# written to.
		local place
		awk -F'\t' '$8 != "-" { print }' "$DRP_OUT/S.tsv" >"$DRP_OUT/S.close.tsv"
		for place in "" same cross; do
			DRP_OUT_TSV="$DRP_OUT/S.close.tsv" summarize_leg S 8 "mark -> source pool close (s)" "$place"
		done
		echo "    runs whose source never saw the pool close: $(awk -F'\t' '$8 == "-"' "$DRP_OUT/S.tsv" | grep -c . || true)"
		awk -F'\t' -v b="$DRP_STEADY_BOUND" '$2 > b { bad = 1 } END { exit bad }' "$DRP_OUT/S.tsv" ||
			die "S: a source selected the drained endpoint more than ${DRP_STEADY_BOUND}s after its drain mark"
		ok "S: every gap <= ${DRP_STEADY_BOUND}s"
	fi
	if [[ " $DRP_LEGS " == *" R "* ]]; then
		log "R source agent restarting"
		summarize_gap R
		summarize_leg R 5 "source agent delete -> serving xDS (s)"
		summarize_leg R 6 "source agent serving -> proxy reconnected (s)"
		summarize_leg R 7 "CDS responses removing a held cluster after the reconnect"
		case "$DRP_EXPECT" in
		green)
			awk -F'\t' -v b="$DRP_RECONNECT_BOUND" '$6 > b { bad = 1 } END { exit bad }' "$DRP_OUT/R.tsv" ||
				die "R: the source proxy reconnected more than ${DRP_RECONNECT_BOUND}s after its agent was serving xDS"
			ok "R: every reconnect within ${DRP_RECONNECT_BOUND}s of the agent serving"
			awk -F'\t' '$7 > 0 { bad = 1 } END { exit bad }' "$DRP_OUT/R.tsv" ||
				die "R: a restarted agent withdrew a cluster the proxy held on reconnect (its first snapshot predates the local client certificates)"
			ok "R: no restarted agent withdrew a cluster the proxy held"
			;;
		red) ok "R (red): reported, not gated" ;;
		*) die "DRP_EXPECT must be red or green, got '$DRP_EXPECT'" ;;
		esac
	fi
}

# --- the proxy bootstrap arm ---------------------------------------------------

proxy_cm() { kc -n "$NS" get cm -o name | grep -- '-proxy-config$' | sed -n '1p'; }

# ads_retry_policy — the ADS retry policy the SOURCE node's running proxy
# loaded, from its admin bootstrap dump ("none" when absent).
ads_retry_policy() {
	admin '/config_dump' 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
for c in d.get("configs", []):
    b = c.get("bootstrap")
    if not b:
        continue
    gs = b.get("dynamic_resources", {}).get("ads_config", {}).get("grpc_services", [{}])
    rp = gs[0].get("envoy_grpc", {}).get("retry_policy")
    print(json.dumps(rp, sort_keys=True) if rp else "none")
' 2>/dev/null || echo "unreadable"
}

# bootstrap main|fix — rewrite the live proxy ConfigMap without (main) or with
# (fix) the ADS retry_back_off, then wait for the source proxy to load it.
drp_bootstrap() {
	local arm="$1" cm tmp want
	cm="$(proxy_cm)"
	[ -n "$cm" ] || die "no proxy ConfigMap"
	tmp="$(mktemp)"
	kc -n "$NS" get "$cm" -o yaml >"$tmp"
	python3 - "$tmp" "$arm" <<'PY'
import re, sys
path, arm = sys.argv[1], sys.argv[2]
s = open(path).read()
block = ("              retry_policy:\n"
         "                retry_back_off:\n"
         "                  base_interval: 0.1s\n"
         "                  max_interval: 1s\n")
anchor = "              cluster_name: agent_xds\n"
s = s.replace(anchor + block, anchor)
if arm == "fix":
    i = s.find("      ads_config:")
    j = s.find(anchor, i)
    if i < 0 or j < 0:
        sys.exit("ads_config / agent_xds anchor not found")
    s = s[:j] + anchor + block + s[j + len(anchor):]
open(path, "w").write(s)
PY
	kc apply -f "$tmp" >/dev/null 2>&1 || kc replace -f "$tmp" >/dev/null
	rm -f "$tmp"
	case "$arm" in
	main) want="none" ;;
	fix) want='{"retry_back_off": {"base_interval": "0.100s", "max_interval": "1s"}}' ;;
	*) die "bootstrap arm must be main or fix" ;;
	esac
	local deadline=$((SECONDS + 300)) got
	while true; do
		got="$(ads_retry_policy)"
		[ "$got" = "$want" ] && break
		[ "$SECONDS" -lt "$deadline" ] || die "the source proxy never loaded the $arm bootstrap (ADS retry policy: $got)"
		sleep 5
	done
	kc -n "$NS" rollout status ds/aether-proxy --timeout=180s >/dev/null || true
	ok "source proxy runs the $arm bootstrap (ADS retry policy: $got)"
}

drp_up() {
	raise_inotify
	build_images
	create_cluster
	if [ "$DRP_BACKEND" = etcd ]; then start_etcd; fi
	load_images
	install_gwapi_crds
	install_spire
	install_aether
	deploy_workloads
	drp_workload
}

drp_down() {
	docker rm -f "$ETCD_NAME" >/dev/null 2>&1 || true
	down
}

# swap_agent TAG — roll the agent DaemonSet onto <registry>/agent:TAG (loaded
# into the cluster first): the agent half of a red -> green step.
swap_agent() {
	local ref="${IMAGE_REGISTRY}/agent:$1"
	kind load docker-image "$ref" --name "$CLUSTER" >/dev/null || die "could not load $ref"
	kc -n "$NS" set image ds/aether-agent "agent=$ref" >/dev/null
	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the agent never rolled onto $ref"
	ok "agent DaemonSet on $ref"
}

# swap_registrar TAG — roll the registrar Deployment onto <registry>/registrar:TAG
# (loaded into the cluster first): the red -> green step of aether#1124.
swap_registrar() {
	local ref="${IMAGE_REGISTRY}/registrar:$1"
	kind load docker-image "$ref" --name "$CLUSTER" >/dev/null || die "could not load $ref"
	kc -n "$NS" set image deploy/aether-registrar "registrar=$ref" >/dev/null
	kc -n "$NS" rollout status deploy/aether-registrar --timeout=300s >/dev/null || die "the registrar never rolled onto $ref"
	ok "registrar Deployment on $ref"
}

case "${1:-}" in
up) drp_up ;;
verify) drp_verify ;;
bootstrap) drp_bootstrap "${2:?usage: $0 bootstrap main|fix}" ;;
swap-agent) swap_agent "${2:?usage: $0 swap-agent <tag>}" ;;
swap-registrar) swap_registrar "${2:?usage: $0 swap-registrar <tag>}" ;;
down) drp_down ;;
*) die "usage: $0 {up|verify|bootstrap main|fix|swap-agent <tag>|swap-registrar <tag>|down}" ;;
esac
