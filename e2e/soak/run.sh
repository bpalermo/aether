#!/usr/bin/env bash
# The soak's ONE kickoff (proposal 042, #1323): pre-flight -> engines -> load ->
# churn driver (soak) -> restart watchdog AT T0 -> saver armed. It replaces the
# workstation scripts that used to do this from outside the repository
# (nightly-soak-*.sh, soak-kickoff-*.sh, save-k6.sh, run-e2e-short.sh).
#
#   e2e/soak/run.sh e2e  --context talos-main            # 15 minutes of load
#   e2e/soak/run.sh soak --context talos-main --label nightly-1007/2.5.0-abc1234
#   e2e/soak/run.sh soak --context talos-main --preflight   # check only, change nothing
#
# Modes:
#   e2e    the plan's e2e profile (15 min), the restart watchdog for exactly that
#          long, no churn driver. --rolls adds one agent roll (T+3m) and one
#          aether-proxy roll (T+8m) under the load.
#   soak   the 8h30m profile, then -- once the load has settled -- the churn
#          driver (its start line is T0), the restart watchdog for 8h from T0,
#          and the T0+30m proxy RSS baseline.
#
# What it does NOT do: deploy or upgrade aether. That stays a deliberate,
# separately authorized `helm upgrade` by whoever starts the run.
#
# What it writes to the cluster, in order:
#   1. PriorityClass aether-soak-loader (cluster-scoped; same object
#      k6-runner.yaml defines) and PVC sortie-soak-reports in the test namespace.
#   2. Helm release <release> (default `soak`) of the sortie chart in the test
#      namespace, engines only: DaemonSet <release>-sortie-engine (mesh-managed
#      pods, one per worker node), headless Service <release>-sortie-engine-nodes,
#      ServiceAccount <release>-sortie.
#   3. The same release upgraded with the plan: a ConfigMap and a Job
#      (<release>-sortie-<hash>), whose one pod drives every engine.
#   4. soak only: what churn.sh does (rollout restarts, the new-SA and uds-csi
#      steps, the SHRINK). e2e with --rolls: two rollout restarts.
# Nothing else. sortie-teardown.sh removes 2 and 3.
#
# Everything a run produces lives in ONE directory (--out, default
# ~/aether-soak-logs/<run tag>): run.env, plan.yaml, shares.tsv, engines.tsv,
# churn.log, restart-watch.log, proxy-rss.tsv, save.log, and what the saver
# copies (report.json, sortie.log, job.json, engine-*.log). Pass that directory
# to sortie-save.sh, sortie-gate.sh and sortie-teardown.sh.
#
# Options:
#   --context NAME        REQUIRED. Never the kubeconfig's current-context: `kind
#                         delete cluster` clears it (#951).
#   --label TEXT          build label for the churn log header and run.env
#   --out DIR             the run directory (created; must not hold a run)
#   --namespace NS        test namespace (aether-test)
#   --system-namespace NS aether's own (aether-system)
#   --release NAME        Helm release (soak)
#   --targets FILE        targets file (sortie-targets.txt)
#   --rate N              requests per second per node (60)
#   --duration Ns         overrides the profile's duration
#   --statsd auto|off|IP:PORT
#                         live metrics. auto (default) resolves the cluster IP of
#                         the Service named by SOAK_STATSD_SERVICE
#                         (o11y/otel-scraper) at launch; if it has no UDP port
#                         the run goes ahead without live metrics and says so.
#   --storage-class NAME  for the report PVC (default: the cluster's default
#                         class, else its only class)
#   --values FILE         extra Helm values, after sortie-values.yaml (repeatable)
#   --settle SECONDS      load start -> churn start (soak: 240; e2e: unused)
#   --rolls               e2e only: roll the agent and the proxy under the load
#   --skip-anyport        soak only: skip anyport-probe.sh (README step 0c)
#   --skip-newsa          soak only: skip churn.sh --new-sa-once (README step 0d)
#   --preflight           run the pre-flight, print the result, exit
#
# Environment: SOAK_ENGINE_SETTLE (30) seconds between the engines turning Ready
# and the first request; SOAK_STATSD_SERVICE (o11y/otel-scraper);
# SOAK_SORTIE_MAX_PENDING / SOAK_SORTIE_IDLE_STRATEGY pass through to
# sortie-plan.sh; SOAK_READER_IMAGE (sortie-save.sh). churn.sh, restart-watch.sh
# and sample-proxy-rss.sh keep their own SOAK_* knobs, except the three log
# paths, which this script points into the run directory.
#
# Start it from the MAIN session: the churn driver, the watchdog and the saver
# are detached with `nohup setsid`, but a parent that is itself reaped (a
# subagent) takes the foreground part of this script with it.
#
# Needs kubectl, helm (3.13+: it pulls the chart by digest), jq, awk.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

# The chart, by digest (the OCI manifest's). Bump with the two image digests in
# sortie-values.yaml; the version is here for the reader, the digest is what is
# pulled.
SORTIE_CHART="${SORTIE_CHART:-oci://quay.io/sortie/chart-sortie}"
SORTIE_CHART_VERSION="0.1.0-b71b37ea7e7e02779b0bcf0592fb46b8386a62fc"
SORTIE_CHART_DIGEST="${SORTIE_CHART_DIGEST:-sha256:c8f0ecf165ad76df01a01fcd6e63cd54e0ba1959b3813412534c3c62609e67f3}"

MODE=""
CTX=""
LABEL="sortie"
OUT=""
NS="aether-test"
SYS_NS="aether-system"
RELEASE="soak"
TARGETS="$HERE/sortie-targets.txt"
RATE=60
DURATION=""
STATSD="auto"
STATSD_SERVICE="${SOAK_STATSD_SERVICE:-o11y/otel-scraper}"
STORAGE_CLASS=""
EXTRA_VALUES=()
SETTLE=""
ROLLS=0
SKIP_ANYPORT=0
SKIP_NEWSA=0
PREFLIGHT_ONLY=0
ENGINE_SETTLE="${SOAK_ENGINE_SETTLE:-30}"
PRIORITY_CLASS="aether-soak-loader"
PVC="sortie-soak-reports"

usage() {
	echo "usage: run.sh {e2e|soak} --context NAME [--label TEXT] [--out DIR] [--preflight] ... (see the header of $0)" >&2
	exit 2
}

log() { echo "$(date -u +%FT%TZ) $*"; }
die() {
	log "ABORT $*"
	exit 1
}

case "${1:-}" in
e2e | soak) MODE="$1" ;;
__rolls) MODE="__rolls" ;;
*) usage ;;
esac
shift
while [ $# -gt 0 ]; do
	case "$1" in
	--context | --label | --out | --namespace | --system-namespace | --release | --targets | --rate | --duration | --statsd | --storage-class | --values | --settle)
		[ $# -ge 2 ] || usage
		case "$1" in
		--context) CTX="$2" ;;
		--label) LABEL="$2" ;;
		--out) OUT="$2" ;;
		--namespace) NS="$2" ;;
		--system-namespace) SYS_NS="$2" ;;
		--release) RELEASE="$2" ;;
		--targets) TARGETS="$2" ;;
		--rate) RATE="$2" ;;
		--duration) DURATION="$2" ;;
		--statsd) STATSD="$2" ;;
		--storage-class) STORAGE_CLASS="$2" ;;
		--values) EXTRA_VALUES+=("$2") ;;
		--settle) SETTLE="$2" ;;
		esac
		shift 2
		;;
	--rolls) ROLLS=1 && shift ;;
	--skip-anyport) SKIP_ANYPORT=1 && shift ;;
	--skip-newsa) SKIP_NEWSA=1 && shift ;;
	--preflight) PREFLIGHT_ONLY=1 && shift ;;
	*)
		echo "run.sh: unknown option '$1'" >&2
		usage
		;;
	esac
done
[ -n "$CTX" ] || {
	echo "run.sh: --context is required (never the current-context, #951)" >&2
	exit 2
}

k() { kubectl --context "$CTX" "$@"; }
h() { helm --kube-context "$CTX" "$@"; }

# --- the e2e rolls, detached (internal: run.sh __rolls --context ... --out DIR)
if [ "$MODE" = __rolls ]; then
	t0=$(date +%s)
	for step in "180 daemonset/aether-agent" "480 daemonset/aether-proxy"; do
		at=${step%% *} what=${step#* }
		now=$(date +%s)
		[ $((t0 + at)) -gt "$now" ] && sleep $((t0 + at - now))
		if k -n "$SYS_NS" rollout restart "$what" >/dev/null 2>&1 &&
			k -n "$SYS_NS" rollout status "$what" --timeout=10m >/dev/null 2>&1; then
			log "ROLLED $SYS_NS/$what"
		else
			log "FAILED $SYS_NS/$what"
		fi
	done
	exit 0
fi

ENGINE_DS="$RELEASE-sortie-engine"
ENGINE_SVC="$RELEASE-sortie-engine-nodes"
[ -n "$SETTLE" ] || SETTLE=240
RUN_TAG="$MODE-$(date -u +%Y%m%dT%H%M%SZ)"
[ -n "$OUT" ] || OUT="$HOME/aether-soak-logs/$RUN_TAG"
VALUES=(-f "$HERE/sortie-values.yaml")
for v in ${EXTRA_VALUES[@]+"${EXTRA_VALUES[@]}"}; do
	[ -r "$v" ] || die "cannot read --values $v"
	VALUES+=(-f "$v")
done

# ------------------------------------------------------------------ pre-flight
# Every check runs; the run starts only if none failed. Nothing here writes to
# the cluster except, in soak mode, the two documented one-shot probes.
PF_FAILS=0
pf_ok() { log "preflight ok    $*"; }
pf_fail() {
	log "preflight FAIL  $*"
	PF_FAILS=$((PF_FAILS + 1))
}

preflight() {
	local tool
	for tool in kubectl helm jq awk; do
		command -v "$tool" >/dev/null 2>&1 || pf_fail "$tool is not on PATH"
	done
	if ! k --request-timeout=15s get --raw /readyz >/dev/null 2>&1; then
		pf_fail "context '$CTX' does not answer /readyz (check \`kubectl config get-contexts\`)"
		return
	fi
	pf_ok "context '$CTX' answers"

	# The share table doubles as the targets file's syntax check.
	local shares
	if ! shares="$(bash "$HERE/sortie-plan.sh" shares --targets "$TARGETS" --rate "$RATE" 2>&1)"; then
		pf_fail "$shares"
		return
	fi
	pf_ok "plan: $(printf '%s\n' "$shares" | awk -F'\t' '{printf "%s%s=%d", (NR > 1 ? " " : ""), $1, $2}') rps per node"

	# Every aether pod Running with every container ready, compared as numbers
	# (pods-not-ready.awk; the old awk back-reference flagged every pod, #1323).
	local pods rc
	pods="$(k -n "$SYS_NS" get pods --no-headers 2>&1)"
	printf '%s\n' "$pods" | awk -f "$HERE/pods-not-ready.awk"
	rc=$?
	case "$rc" in
	0) pf_ok "$SYS_NS: $(printf '%s\n' "$pods" | wc -l | tr -d ' ') pods, all ready" ;;
	1) pf_fail "$SYS_NS has pods that are not ready (listed above)" ;;
	*) pf_fail "could not list pods in $SYS_NS: $pods" ;;
	esac

	# Targets present: the mesh Service behind each URL's <svc>.<ns>.<domain> host.
	local url host svc ns missing=""
	while IFS=$'\t' read -r _ _ url; do
		host="${url#http://}"
		host="${host%%[:/]*}"
		svc="${host%%.*}"
		ns="${host#*.}"
		ns="${ns%%.*}"
		k -n "$ns" get svc "$svc" >/dev/null 2>&1 || missing="$missing $ns/$svc"
	done <<<"$shares"
	if [ -n "$missing" ]; then pf_fail "no mesh Service for target(s):$missing"; else pf_ok "every target's mesh Service exists"; fi

	# One run per release: `helm upgrade` with a new plan REPLACES the Job, and a
	# finished Job's pod log is the only copy of its summary until it is saved.
	local jobs
	jobs="$(k -n "$NS" get jobs -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/name=sortie" -o name 2>/dev/null)"
	if [ -n "$jobs" ]; then
		pf_fail "release '$RELEASE' already has a run in $NS ($(echo "$jobs" | tr '\n' ' ')): save it (sortie-save.sh) and tear it down (sortie-teardown.sh) first"
	else
		pf_ok "no sortie run exists for release '$RELEASE'"
	fi
	if k -n "$NS" get ds k6-soak-loader >/dev/null 2>&1; then
		log "preflight note  the k6 runner (ds/k6-soak-loader) is up: this is a side-by-side run, and the node carries both loads"
	fi

	if ! SOAK_CONTEXT="$CTX" bash "$HERE/restart-watch.sh" --context "$CTX" --preflight >/dev/null 2>&1; then
		pf_fail "restart-watch.sh --preflight could not take a baseline"
	else
		pf_ok "restart watchdog can take its baseline"
	fi

	[ "$MODE" = soak ] || return 0
	if pgrep -f "bash $HERE/churn.sh" >/dev/null; then
		pf_fail "a churn driver is already running (pgrep -af 'bash $HERE/churn.sh')"
	fi
	if pgrep -f "bash $HERE/restart-watch.sh" >/dev/null; then
		pf_fail "a restart watchdog is already running (pgrep -af 'bash $HERE/restart-watch.sh')"
	fi
	if bash "$HERE/churn.sh" --context "$CTX" --preflight; then
		pf_ok "churn.sh --preflight"
	else
		pf_fail "churn.sh --preflight (its reason is above)"
	fi
	log "preflight note  echo=$(k -n "$NS" get pods -l app=echo --no-headers 2>/dev/null | wc -l | tr -d ' ') mixed-svc=$(k -n "$NS" get pods -l app=mixed-svc --no-headers 2>/dev/null | wc -l | tr -d ' ') udp-echo=$(k -n "$NS" get pods -l app=udp-echo --no-headers 2>/dev/null | wc -l | tr -d ' ') (expect 3 each, spread)"
	[ "$PREFLIGHT_ONLY" = 1 ] && return 0
	mkdir -p "$OUT"
	if [ "$SKIP_ANYPORT" = 0 ]; then
		if CTX="$CTX" NS="$NS" bash "$HERE/anyport-probe.sh" >"$OUT/anyport-probe.log" 2>&1; then
			pf_ok "anyport-probe.sh ($(tail -1 "$OUT/anyport-probe.log"))"
		else
			pf_fail "anyport-probe.sh (see $OUT/anyport-probe.log)"
		fi
	fi
	if [ "$SKIP_NEWSA" = 0 ]; then
		SOAK_CHURN_LOG="$OUT/newsa-preflight.log" bash "$HERE/churn.sh" --context "$CTX" --new-sa-once >/dev/null 2>&1
		if grep -q ' ROLLED newsa/' "$OUT/newsa-preflight.log" 2>/dev/null && ! grep -q FAILED "$OUT/newsa-preflight.log"; then
			pf_ok "new-SA step: $(grep ' ROLLED newsa/' "$OUT/newsa-preflight.log" | tail -1 | cut -d' ' -f2-)"
		else
			pf_fail "new-SA pre-flight step (see $OUT/newsa-preflight.log)"
		fi
	fi
}

if [ -e "$OUT/run.env" ]; then die "$OUT already holds a run (run.env exists); pass a fresh --out"; fi
log "run.sh $MODE context=$CTX namespace=$NS release=$RELEASE label=$LABEL out=$OUT"
preflight
if [ "$PF_FAILS" -gt 0 ]; then die "pre-flight failed ($PF_FAILS); nothing was started"; fi
log "PREFLIGHT PASSED"
if [ "$PREFLIGHT_ONLY" = 1 ]; then exit 0; fi
mkdir -p "$OUT" || die "cannot create $OUT"

# --------------------------------------------------------------------- engines
# The chart only references these two; they are ours to create.
if [ -z "$STORAGE_CLASS" ]; then
	STORAGE_CLASS="$(k get storageclass -o json | jq -r '
		[.items[] | select(.metadata.annotations["storageclass.kubernetes.io/is-default-class"] == "true") | .metadata.name] as $d
		| if ($d | length) == 1 then $d[0] elif (.items | length) == 1 then .items[0].metadata.name else "" end')"
	[ -n "$STORAGE_CLASS" ] || die "no default StorageClass and more than one to choose from: pass --storage-class"
fi
k apply -f - >/dev/null <<EOF || die "could not apply the PriorityClass and the report PVC"
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: $PRIORITY_CLASS
  labels:
    app.kubernetes.io/part-of: aether-soak
value: 1000
preemptionPolicy: Never
globalDefault: false
description: "aether soak load generator: outranks the priority-0 test workloads as a preemption victim, preempts nothing."
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $PVC
  namespace: $NS
  labels:
    app.kubernetes.io/part-of: aether-soak
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $STORAGE_CLASS
  resources:
    requests:
      storage: 64Mi
EOF
log "applied priorityclass/$PRIORITY_CLASS and pvc/$PVC (storage class $STORAGE_CLASS)"

CHART="$OUT/chart"
mkdir -p "$CHART"
h pull "$SORTIE_CHART@$SORTIE_CHART_DIGEST" -d "$CHART" >"$CHART/pull.log" 2>&1 || die "could not pull $SORTIE_CHART@$SORTIE_CHART_DIGEST (see $CHART/pull.log)"
CHART_TGZ="$(find "$CHART" -name '*.tgz' | head -n1)"
[ -n "$CHART_TGZ" ] || die "the chart pull left no .tgz in $CHART"
log "chart $SORTIE_CHART $SORTIE_CHART_VERSION @$SORTIE_CHART_DIGEST"

# Engines FIRST and alone: the plan's dns pool is resolved once, when the run
# starts, and takes the first non-empty answer -- a Job started beside a
# DaemonSet still rolling out would drive only the pods that were ready first.
h upgrade --install "$RELEASE" "$CHART_TGZ" -n "$NS" "${VALUES[@]}" --set job.enabled=false >"$OUT/helm-engines.log" 2>&1 ||
	die "helm install of the engines failed (see $OUT/helm-engines.log)"
k -n "$NS" rollout status "daemonset/$ENGINE_DS" --timeout=5m >/dev/null 2>&1 || die "daemonset/$ENGINE_DS did not roll out in 5m"
read -r DESIRED READY < <(k -n "$NS" get ds "$ENGINE_DS" -o jsonpath='{.status.desiredNumberScheduled} {.status.numberReady}{"\n"}')
[ "${DESIRED:-0}" -gt 0 ] && [ "$DESIRED" = "$READY" ] || die "daemonset/$ENGINE_DS: desired=$DESIRED ready=$READY"
BACKENDS="$DESIRED"
k -n "$NS" get pods -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=engine" \
	-o jsonpath='{range .items[*]}{.status.podIP}{"\t"}{.spec.nodeName}{"\t"}{.metadata.name}{"\n"}{end}' | sort >"$OUT/engines.tsv"
# The headless Service must already list every engine: that list IS the pool.
EP=0
for _ in $(seq 1 30); do
	EP="$(k -n "$NS" get endpointslices -l "kubernetes.io/service-name=$ENGINE_SVC" -o json | jq '[.items[].endpoints[]? | select(.conditions.ready == true) | .addresses[]] | unique | length')"
	[ "$EP" = "$BACKENDS" ] && break
	sleep 2
done
[ "$EP" = "$BACKENDS" ] || die "service/$ENGINE_SVC lists $EP ready engine(s), want $BACKENDS"
log "ENGINES READY backends=$BACKENDS ($(cut -f2 "$OUT/engines.tsv" | tr '\n' ' '))"
# Ready means the engine's gRPC port answers, not that the node's proxy has the
# new pods' identity and clusters: give the mesh a moment before the first
# request, or the run opens with first-use failures that are the harness's
# (the #1086 shape: 141 k6 failures at loader start).
log "waiting ${ENGINE_SETTLE}s for the mesh to take the engine pods in"
sleep "$ENGINE_SETTLE"

# ------------------------------------------------------------------------ plan
case "$STATSD" in
off) STATSD="" ;;
auto)
	sns="${STATSD_SERVICE%%/*}" ssvc="${STATSD_SERVICE#*/}"
	STATSD="$(k -n "$sns" get svc "$ssvc" -o json 2>/dev/null | jq -r '
		(.spec.clusterIP // "") as $ip | ([.spec.ports[]? | select(.protocol == "UDP") | .port][0] // "") as $p
		| if $ip != "" and $ip != "None" and ($p | tostring) != "" then "\($ip):\($p)" else "" end')"
	if [ -n "$STATSD" ]; then
		log "statsd: $STATSD_SERVICE resolved to $STATSD"
	else
		log "statsd: service $STATSD_SERVICE has no cluster IP with a UDP port -- running WITHOUT live metrics (the report is unaffected)"
	fi
	;;
esac
PLAN_ARGS=(render --profile "$MODE" --targets "$TARGETS" --rate "$RATE" --backends "$BACKENDS"
	--dns "$ENGINE_SVC.$NS.svc.cluster.local:8443")
[ -n "$DURATION" ] && PLAN_ARGS+=(--duration "$DURATION")
[ -n "$STATSD" ] && PLAN_ARGS+=(--statsd "$STATSD")
[ -n "${SOAK_SORTIE_MAX_PENDING:-}" ] && PLAN_ARGS+=(--max-pending "$SOAK_SORTIE_MAX_PENDING")
[ -n "${SOAK_SORTIE_IDLE_STRATEGY:-}" ] && PLAN_ARGS+=(--idle-strategy "$SOAK_SORTIE_IDLE_STRATEGY")
bash "$HERE/sortie-plan.sh" "${PLAN_ARGS[@]}" >"$OUT/plan.yaml" || die "could not render the plan"
bash "$HERE/sortie-plan.sh" shares --targets "$TARGETS" --rate "$RATE" >"$OUT/shares.tsv" || die "could not render the share table"
DURATION_S="$(awk '$1 == "duration:" {sub(/s$/, "", $2); print $2}' "$OUT/plan.yaml")"
REPORT_FILE="/var/run/sortie/$RUN_TAG.json"

cat >"$OUT/run.env" <<EOF
RUN_TAG=$RUN_TAG
MODE=$MODE
LABEL=$LABEL
CTX=$CTX
NS=$NS
SYS_NS=$SYS_NS
RELEASE=$RELEASE
PVC=$PVC
BACKENDS=$BACKENDS
DURATION_S=$DURATION_S
REPORT_FILE=$REPORT_FILE
STATSD=$STATSD
EOF

# ------------------------------------------------------------------------ load
h upgrade "$RELEASE" "$CHART_TGZ" -n "$NS" "${VALUES[@]}" --set job.enabled=true \
	--set-file "plan=$OUT/plan.yaml" --set "report.path=$REPORT_FILE" >"$OUT/helm-run.log" 2>&1 ||
	die "helm upgrade with the plan failed (see $OUT/helm-run.log)"
JOB=""
for _ in $(seq 1 30); do
	JOB="$(k -n "$NS" get jobs -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/name=sortie" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
	[ -n "$JOB" ] && break
	sleep 2
done
[ -n "$JOB" ] || die "the release has no Job after the upgrade"
PHASE=""
for _ in $(seq 1 90); do
	PHASE="$(k -n "$NS" get pods -l "job-name=$JOB" -o jsonpath='{.items[0].status.phase}' 2>/dev/null)"
	case "$PHASE" in Running | Succeeded | Failed) break ;; esac
	sleep 2
done
T_LOAD="$(date +%s)"
{
	echo "JOB=$JOB"
	echo "T_LOAD=$T_LOAD"
} >>"$OUT/run.env"
[ "$PHASE" = Running ] || die "the sortie pod of job/$JOB is '$PHASE', not Running (kubectl -n $NS describe job/$JOB)"
log "LOAD STARTED job=$JOB rate=$RATE rps per node x $BACKENDS nodes for ${DURATION_S}s (ends ~$(date -u -d "@$((T_LOAD + DURATION_S))" +%FT%TZ))"

# The saver first: whatever happens next, the report is copied out when the run
# ends, before anyone tears it down.
nohup setsid bash "$HERE/sortie-save.sh" --dir "$OUT" --wait >"$OUT/save.log" 2>&1 </dev/null &
log "saver armed (log $OUT/save.log)"

start_watchdog() {
	# $1 = duration, $2 = interval. AT T0, not after the kickoff returns (#1323).
	export SOAK_RESTART_LOG="$OUT/restart-watch.log"
	if bash "$HERE/restart-watch.sh" --context "$CTX" --preflight >/dev/null 2>&1; then
		nohup setsid bash "$HERE/restart-watch.sh" --context "$CTX" --duration "$1" --interval "$2" >/dev/null 2>&1 </dev/null &
		log "restart watchdog armed for $1 (log $SOAK_RESTART_LOG)"
	else
		log "WARNING restart watchdog pre-flight FAILED -- the run goes on WITHOUT it; grade restarts another way"
	fi
}

if [ "$MODE" = e2e ]; then
	start_watchdog "${DURATION_S}s" 30
	if [ "$ROLLS" = 1 ]; then
		nohup setsid bash "$HERE/run.sh" __rolls --context "$CTX" --system-namespace "$SYS_NS" >"$OUT/rolls.log" 2>&1 </dev/null &
		log "rolls armed: aether-agent at T+3m, aether-proxy at T+8m (log $OUT/rolls.log)"
	fi
	log "E2E_KICKOFF_DONE out=$OUT -- when $OUT/save.log says SORTIE_SAVED: sortie-gate.sh --dir $OUT, then sortie-teardown.sh --dir $OUT"
	exit 0
fi

# ---------------------------------------------------------------- soak: churn
log "settling ${SETTLE}s before the churn driver"
sleep "$SETTLE"
k -n "$NS" logs "job/$JOB" --tail=-1 2>/dev/null >"$OUT/sortie-settle.log"
PHASE="$(k -n "$NS" get pods -l "job-name=$JOB" -o jsonpath='{.items[0].status.phase}' 2>/dev/null)"
[ "$PHASE" = Running ] || die "the sortie pod is '$PHASE' after ${SETTLE}s -- the load is not running; churn NOT started (see $OUT/sortie-settle.log)"
if pgrep -f "bash $HERE/churn.sh" >/dev/null; then die "a churn driver is already running; churn NOT started"; fi
export SOAK_CHURN_LOG="$OUT/churn.log"
export SOAK_PROXY_RSS_TSV="$OUT/proxy-rss.tsv"
bash "$HERE/churn.sh" --context "$CTX" --preflight || die "churn.sh --preflight failed; churn NOT started (the load keeps running)"
nohup setsid bash "$HERE/churn.sh" --context "$CTX" "$LABEL" >/dev/null 2>&1 </dev/null &
sleep 10
log "CHURN STARTED: $(head -n 2 "$SOAK_CHURN_LOG" 2>/dev/null | tr '\n' '|')"
grep -q "$(date -u +%F)" "$SOAK_CHURN_LOG" 2>/dev/null || die "$SOAK_CHURN_LOG has no start line from today: the detached driver refused to start"
start_watchdog 8h 120
# The T0+30m age-matched proxy RSS baseline (churn.sh takes the later ones).
nohup setsid bash -c "sleep 1800; SOAK_PROXY_RSS_TSV='$OUT/proxy-rss.tsv' bash '$HERE/sample-proxy-rss.sh' --context '$CTX' --at-age 1800" >"$OUT/rss-baseline.log" 2>&1 </dev/null &
log "SOAK_KICKOFF_DONE out=$OUT -- T0 is the first line of $SOAK_CHURN_LOG"
