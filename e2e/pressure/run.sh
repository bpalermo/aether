#!/usr/bin/env bash
# Collector-shedding harness for issue #662 (fix: #668).
#
# Deliberately drives the shared otel-collector into memory_limiter shedding, then
# restarts ONE node's aether-agent under that pressure and asserts the agent still
# starts, resolves its SPIRE identity and serves. Four soaks never reproduced this
# condition — over the last two the collector peaked at ~22% of its shedding threshold
# with zero refusals — so the branch #668 fixed has never been exercised since it landed.
#
# Read README.md first. NEVER run this during a soak.
#
#   bash e2e/pressure/run.sh --node main-worker-03
#   bash e2e/pressure/run.sh --node main-worker-03 --dry-run   # resolve + print, touch nothing
#
# Exit codes: 0 PASS, 1 FAIL, 2 INCONCLUSIVE (pressure not reached, or lapsed
# mid-test, or aborted on a safety ceiling). Every exit path deletes the Job.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

NODE=""
DRY_RUN=0
JOB_MANIFEST="${JOB_MANIFEST:-${SCRIPT_DIR}/collector-pressure-job.yaml}"
JOB_NAME="${JOB_NAME:-aether-collector-pressure}"
JOB_NS="${JOB_NS:-aether-test}"
AGENT_NS="${AGENT_NS:-aether-system}"
AGENT_SELECTOR="${AGENT_SELECTOR:-app.kubernetes.io/name=aether-agent}"
AGENT_CONTAINER="${AGENT_CONTAINER:-agent}"
PROM_NS="${PROM_NS:-prometheus}"
PROM_SVC="${PROM_SVC:-prometheus-server}"
COLLECTOR_NS="${COLLECTOR_NS:-o11y}"
COLLECTOR_DEPLOY="${COLLECTOR_DEPLOY:-otel-collector}"
# Derived from the Deployment's own .spec.selector at run time when left empty.
# Do NOT default this to app.kubernetes.io/name=opentelemetry-collector: the
# single-replica otel-scraper Deployment carries the same name label, and pulling
# its pods into the set would both add a bogus port-forward and fold a second
# process's refused counters into the sums.
COLLECTOR_SELECTOR="${COLLECTOR_SELECTOR:-}"
COLLECTOR_CONTAINER="${COLLECTOR_CONTAINER:-opentelemetry-collector}"
COLLECTOR_METRICS_PORT="${COLLECTOR_METRICS_PORT:-8888}"
EXPECT_CONTEXT="${EXPECT_CONTEXT:-talos-main}"

# Where the collector's own numbers are read from:
#   collector  — port-forward each replica's :8888/metrics (no scrape lag)
#   prometheus — query Prometheus (the collector pushes self-telemetry every 30s,
#                so this lags shedding onset by 30-60s; see README)
#   auto       — collector if :8888 answers on every replica, else prometheus
METRICS_SOURCE="${METRICS_SOURCE:-auto}"

# Series selector for the SHARED collector's self-telemetry in Prometheus.
# Deliberately not a bare job= match: otel-scraper and the eBPF profiler also emit
# otelcol_* series.
COLLECTOR_SEL='instance=~"otel-collector-.*"'

# memory_limiter settings from the deployed o11y/otel-collector ConfigMap. The
# absolute thresholds are derived from the pod's real memory limit at run time
# rather than hard-coded, so a resize of the o11y plane cannot silently invalidate
# them.
LIMIT_PCT="${LIMIT_PCT:-80}" # memory_limiter limit_percentage
SPIKE_PCT="${SPIKE_PCT:-25}" # memory_limiter spike_limit_percentage
# Abort ceilings.
#
# memory_limiter triggers on the Go HEAP (otelcol_process_runtime_heap_alloc_bytes),
# not on RSS: measured on 2026-09-05, refusal onset was at heap 1,077-1,300MiB while
# RSS at that same instant was 1,250-1,650MiB (GC slack lets RSS run 1.15-1.4x ahead
# of heap). A fixed RSS ceiling therefore lands *inside* the band in which shedding
# is already engaged, and aborts runs that were about to succeed (#699). The primary
# ceiling is heap against GOMEMLIMIT — the point past which the Go runtime, not the
# limiter, is the thing at risk — with RSS kept only as a backstop against the
# cgroup OOM killer.
ABORT_HEAP_PCT="${ABORT_HEAP_PCT:-95}" # of GOMEMLIMIT
ABORT_RSS_PCT="${ABORT_RSS_PCT:-90}"   # of the pod memory limit
# Pre-flight ceiling on the *baseline*. Idle RSS is ~250MiB (22% of the soft limit),
# so a "<20%" gate could never pass; 35% still guarantees the collector is not
# already loaded when the run starts.
MAX_BASELINE_PCT=35

PRESSURE_TIMEOUT="${PRESSURE_TIMEOUT:-300}"
POD_APPEAR_TIMEOUT="${POD_APPEAR_TIMEOUT:-90}"
AGENT_READY_TIMEOUT="${AGENT_READY_TIMEOUT:-120}"
RECOVERY_TIMEOUT="${RECOVERY_TIMEOUT:-300}"
SERIES_FRESH_TIMEOUT="${SERIES_FRESH_TIMEOUT:-180}"
SERIES_FRESH_MAX_AGE="${SERIES_FRESH_MAX_AGE:-120}"
# Left empty on purpose: resolved after the metrics source is known (5s off the
# collector's own endpoint, 15s off Prometheus, which cannot answer faster than it
# is pushed to anyway).
POLL_INTERVAL="${POLL_INTERVAL:-}"

MIB=$((1024 * 1024))

JOB_APPLIED=0
PF_PID=""
PF_LOG=""
PROM_PORT=""
MAX_RSS=0
MAX_HEAP=0
COLLECTOR_PODS=()
CPF_PIDS=()
CPF_PORTS=()
CPF_LOGS=()
SCRATCH=""

usage() {
	cat <<EOF
usage: $0 --node <node-name> [options]

  --node <name>          node whose aether-agent pod is restarted under pressure (required)
  --dry-run              resolve GOMEMLIMIT, limits, thresholds and the metrics source,
                         print the plan and the current readings, then exit 0.
                         Applies no Job and touches no agent.
  --metrics-source S     auto|collector|prometheus (default ${METRICS_SOURCE})
  --pressure-timeout N   seconds to wait for shedding to engage (default ${PRESSURE_TIMEOUT})
  --ready-timeout N      seconds for the replacement agent pod to become Ready (default ${AGENT_READY_TIMEOUT})
  --job-manifest PATH    pressure Job manifest (default ${JOB_MANIFEST})
  -h, --help             this text
EOF
}

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
step() { printf '\n%s ==== %s\n' "$(date -u +%FT%TZ)" "$*"; }
die() {
	log "ABORT: $*"
	exit 2
}
fail() {
	log "FAIL: $*"
	exit 1
}
mib() { echo $(($1 / MIB)); }
pct_of() { echo $((100 * $1 / $2)); }

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--node)
			NODE="${2:-}"
			shift 2
			;;
		--dry-run)
			DRY_RUN=1
			shift
			;;
		--metrics-source)
			METRICS_SOURCE="${2:-}"
			shift 2
			;;
		--pressure-timeout)
			PRESSURE_TIMEOUT="${2:-}"
			shift 2
			;;
		--ready-timeout)
			AGENT_READY_TIMEOUT="${2:-}"
			shift 2
			;;
		--job-manifest)
			JOB_MANIFEST="${2:-}"
			shift 2
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			usage >&2
			die "unknown argument: $1"
			;;
		esac
	done
	[ -n "$NODE" ] || {
		usage >&2
		die "--node is required"
	}
	case "$METRICS_SOURCE" in
	auto | collector | prometheus) ;;
	*)
		usage >&2
		die "--metrics-source must be auto, collector or prometheus (got '${METRICS_SOURCE}')"
		;;
	esac
}

cleanup() {
	local rc=$? pid
	trap - EXIT INT TERM
	if [ "$JOB_APPLIED" = 1 ]; then
		log "cleanup: deleting job ${JOB_NS}/${JOB_NAME}"
		kubectl -n "$JOB_NS" delete job "$JOB_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	fi
	if [ -n "$PF_PID" ]; then kill "$PF_PID" >/dev/null 2>&1 || true; fi
	if [ -n "$PF_LOG" ]; then rm -f "$PF_LOG"; fi
	for pid in ${CPF_PIDS[@]+"${CPF_PIDS[@]}"}; do kill "$pid" >/dev/null 2>&1 || true; done
	if [ -n "$SCRATCH" ]; then rm -rf "$SCRATCH"; fi
	exit "$rc"
}

# ------------------------------------------------------------------- helpers

# parse_bytes <quantity> -> bytes. Accepts Go's GOMEMLIMIT spelling (B/KiB/MiB/GiB/TiB)
# and Kubernetes resource quantities (Ki/Mi/Gi/Ti and k/M/G/T), plus a bare byte count.
parse_bytes() {
	local q="${1:-}" num unit
	[ -n "$q" ] || return 1
	num="${q%%[!0-9.]*}"
	unit="${q#"$num"}"
	[ -n "$num" ] || return 1
	case "$unit" in
	"" | B | b) echo "${num%.*}" ;;
	KiB | Ki | K | k) awk -v n="$num" 'BEGIN{printf "%.0f", n*1024}' ;;
	MiB | Mi | M) awk -v n="$num" 'BEGIN{printf "%.0f", n*1024*1024}' ;;
	GiB | Gi | G) awk -v n="$num" 'BEGIN{printf "%.0f", n*1024*1024*1024}' ;;
	TiB | Ti | T) awk -v n="$num" 'BEGIN{printf "%.0f", n*1024*1024*1024*1024}' ;;
	KB) awk -v n="$num" 'BEGIN{printf "%.0f", n*1000}' ;;
	MB) awk -v n="$num" 'BEGIN{printf "%.0f", n*1000*1000}' ;;
	GB) awk -v n="$num" 'BEGIN{printf "%.0f", n*1000*1000*1000}' ;;
	*) return 1 ;;
	esac
}

# start_pf <namespace> <target> <remote-port> <logfile> -> "<pid> <localport>"
# kubectl picks the local port (":<remote>") so concurrent runs cannot collide.
start_pf() {
	local ns=$1 target=$2 port=$3 logf=$4 pid lport waited=0
	# Create the log before forking: the first sed below can otherwise race the
	# background redirection and print a spurious "can't read" to stderr.
	: >"$logf"
	kubectl -n "$ns" port-forward "$target" ":${port}" --address 127.0.0.1 >"$logf" 2>&1 &
	pid=$!
	while [ "$waited" -lt 30 ]; do
		lport=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\).*/\1/p' "$logf" | head -1)
		if [ -n "$lport" ]; then
			echo "$pid $lport"
			return 0
		fi
		kill -0 "$pid" 2>/dev/null || {
			echo "$pid "
			return 1
		}
		sleep 1
		waited=$((waited + 1))
	done
	kill "$pid" >/dev/null 2>&1 || true
	echo "$pid "
	return 1
}

# ---------------------------------------------------------------- Prometheus

start_port_forward() {
	# The Prometheus LB address is not reachable from a workstation and `kubectl exec`
	# into Prometheus is not permitted here, so everything is read over a port-forward
	# on an ephemeral local port.
	local out
	PF_LOG="${SCRATCH}/prom-pf.log"
	out=$(start_pf "$PROM_NS" "svc/${PROM_SVC}" 80 "$PF_LOG") || {
		PF_PID=${out%% *}
		die "could not establish a port-forward to ${PROM_NS}/${PROM_SVC}: $(cat "$PF_LOG")"
	}
	PF_PID=${out%% *}
	PROM_PORT=${out##* }
	log "prometheus reachable on 127.0.0.1:${PROM_PORT} (port-forward pid ${PF_PID})"
}

# promq <promql> -> first sample's value, floored to an integer. Empty on no data.
promq() {
	local out
	out=$(curl -sS --max-time 10 --get --data-urlencode "query=$1" \
		"http://127.0.0.1:${PROM_PORT}/api/v1/query" 2>/dev/null) || return 0
	jq -r 'if .status == "success" and (.data.result | length) > 0
	       then (.data.result[0].value[1] | tonumber | floor)
	       else "" end' <<<"$out" 2>/dev/null || echo ""
}

# promq_req <promql> <what> -> like promq, but a missing sample is fatal.
promq_req() {
	local v
	v=$(promq "$1")
	[ -n "$v" ] || die "no data for $2 — is Prometheus healthy? (query: $1)"
	echo "$v"
}

series_age() { promq "time() - timestamp(aether_agent_storage_pods{node=\"${NODE}\"})"; }

# ------------------------------------------------- collector self-telemetry

discover_collector_pods() {
	local names sel
	if [ -n "$COLLECTOR_SELECTOR" ]; then
		sel="$COLLECTOR_SELECTOR"
	else
		sel=$(kubectl -n "$COLLECTOR_NS" get deploy "$COLLECTOR_DEPLOY" -o json 2>/dev/null |
			jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")')
		[ -n "$sel" ] && [ "$sel" != "null" ] ||
			die "could not read .spec.selector.matchLabels from ${COLLECTOR_NS}/${COLLECTOR_DEPLOY}"
	fi
	names=$(kubectl -n "$COLLECTOR_NS" get pods -l "$sel" \
		--field-selector status.phase=Running \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
	[ -n "$names" ] || die "no Running collector pods matched -l ${sel} in ${COLLECTOR_NS}"
	mapfile -t COLLECTOR_PODS <<<"$names"
	log "collector replicas (-l ${sel}): ${COLLECTOR_PODS[*]}"
}

# GOMEMLIMIT is what memory_limiter's heap headroom is really measured against, and
# it is what the Go runtime will thrash/OOM around. Read it off the running pod. If
# it is derived (valueFrom: resourceFieldRef) jsonpath returns nothing, so fall back
# to the container's memory limit, which is what such a derivation would resolve to.
resolve_collector_limits() {
	local pod=${COLLECTOR_PODS[0]} raw lim csel
	csel='{.spec.containers[?(@.name=="'"$COLLECTOR_CONTAINER"'")]'
	raw=$(kubectl -n "$COLLECTOR_NS" get pod "$pod" -o jsonpath="${csel}.env[?(@.name=='GOMEMLIMIT')].value}" 2>/dev/null)
	lim=$(kubectl -n "$COLLECTOR_NS" get pod "$pod" -o jsonpath="${csel}.resources.limits.memory}" 2>/dev/null)
	[ -n "$lim" ] || die "container ${COLLECTOR_CONTAINER} in ${COLLECTOR_NS}/${pod} has no memory limit — cannot size the safety ceilings"
	POD_MEM_LIMIT_BYTES=$(parse_bytes "$lim") || die "cannot parse the container memory limit '${lim}'"
	if [ -n "$raw" ]; then
		GOMEMLIMIT_BYTES=$(parse_bytes "$raw") || die "cannot parse GOMEMLIMIT '${raw}'"
		GOMEMLIMIT_SRC="pod env GOMEMLIMIT=${raw}"
	else
		GOMEMLIMIT_BYTES=$POD_MEM_LIMIT_BYTES
		GOMEMLIMIT_SRC="derived: no literal GOMEMLIMIT in the pod env, using resources.limits.memory=${lim}"
		log "WARN: ${GOMEMLIMIT_SRC}"
	fi

	HARD_LIMIT_BYTES=$((POD_MEM_LIMIT_BYTES * LIMIT_PCT / 100))
	SOFT_LIMIT_BYTES=$((POD_MEM_LIMIT_BYTES * (LIMIT_PCT - SPIKE_PCT) / 100))
	ABORT_HEAP_BYTES=$((GOMEMLIMIT_BYTES * ABORT_HEAP_PCT / 100))
	ABORT_RSS_BYTES=$((POD_MEM_LIMIT_BYTES * ABORT_RSS_PCT / 100))
}

# Bring up one port-forward per replica to the collector's own Prometheus endpoint.
# Returns non-zero if any replica does not answer, in which case the caller falls
# back to Prometheus.
start_collector_metrics() {
	local i=0 pod out pid port body
	for pod in "${COLLECTOR_PODS[@]}"; do
		CPF_LOGS[i]="${SCRATCH}/collector-pf-${i}.log"
		out=$(start_pf "$COLLECTOR_NS" "pod/${pod}" "$COLLECTOR_METRICS_PORT" "${CPF_LOGS[i]}") || {
			pid=${out%% *}
			CPF_PIDS[i]=$pid
			log "WARN: no port-forward to ${pod}:${COLLECTOR_METRICS_PORT}"
			return 1
		}
		pid=${out%% *}
		port=${out##* }
		CPF_PIDS[i]=$pid
		CPF_PORTS[i]=$port
		body=$(curl -sS --max-time 8 "http://127.0.0.1:${port}/metrics" 2>/dev/null) || body=""
		if ! grep -q '^otelcol_' <<<"$body"; then
			log "WARN: ${pod}:${COLLECTOR_METRICS_PORT}/metrics did not serve otelcol_* samples"
			return 1
		fi
		i=$((i + 1))
	done
	return 0
}

stop_collector_metrics() {
	local pid
	for pid in ${CPF_PIDS[@]+"${CPF_PIDS[@]}"}; do kill "$pid" >/dev/null 2>&1 || true; done
	CPF_PIDS=()
	CPF_PORTS=()
}

# agg_samples <file> <mode:sum|max> <name-alternation> -> integer.
# Matches "<name>" and "<name>_total", with or without a label set, so the same call
# works across the collector versions that renamed these counters.
agg_samples() {
	awk -v re="^($3)(_total)?[{ ]" -v mode="$2" '
		/^#/ { next }
		$0 ~ re {
			v = $NF + 0
			if (mode == "max") { if (v > m) m = v } else { m += v }
		}
		END { printf "%.0f\n", m + 0 }
	' "$1"
}

REFUSED_POINTS_NAMES='otelcol_processor_memory_limiter_refused_metric_points|otelcol_processor_refused_metric_points|otelcol_receiver_refused_metric_points'
REFUSED_LOGS_NAMES='otelcol_processor_memory_limiter_refused_log_records|otelcol_processor_refused_log_records|otelcol_receiver_refused_log_records'

# collector_snapshot -> sets M_HEAP, M_RSS, M_POINTS, M_LOGS.
# heap/RSS are the MAX across replicas (each is a per-process limit); the refused
# counters are the SUM (any replica shedding is shedding).
collector_snapshot() {
	local i=0 f v
	M_HEAP=0
	M_RSS=0
	M_POINTS=0
	M_LOGS=0
	for i in "${!CPF_PORTS[@]}"; do
		f="${SCRATCH}/metrics-${i}.txt"
		curl -sS --max-time 8 "http://127.0.0.1:${CPF_PORTS[i]}/metrics" -o "$f" 2>/dev/null ||
			die "lost the metrics port-forward to ${COLLECTOR_PODS[i]} — re-run, or force --metrics-source prometheus"
		v=$(agg_samples "$f" max 'otelcol_process_runtime_heap_alloc_bytes')
		[ "$v" -le "$M_HEAP" ] || M_HEAP=$v
		v=$(agg_samples "$f" max 'otelcol_process_memory_rss_bytes')
		[ "$v" -le "$M_RSS" ] || M_RSS=$v
		M_POINTS=$((M_POINTS + $(agg_samples "$f" sum "$REFUSED_POINTS_NAMES")))
		M_LOGS=$((M_LOGS + $(agg_samples "$f" sum "$REFUSED_LOGS_NAMES")))
	done
}

prom_snapshot() {
	M_HEAP=$(promq_req "max(otelcol_process_runtime_heap_alloc_bytes{${COLLECTOR_SEL}})" "collector heap")
	M_RSS=$(promq_req "max(otelcol_process_memory_rss_bytes{${COLLECTOR_SEL}})" "collector RSS")
	# `or` unions the two metric names (their __name__ labels differ, so nothing is
	# dropped); `or vector(0)` keeps a never-yet-incremented counter from reading as
	# "no data", which is the normal state before the first refusal.
	M_POINTS=$(promq_req "sum(otelcol_processor_memory_limiter_refused_metric_points_total{${COLLECTOR_SEL}} or otelcol_receiver_refused_metric_points_total{${COLLECTOR_SEL}}) or vector(0)" "refused metric points")
	M_LOGS=$(promq_req "sum(otelcol_processor_memory_limiter_refused_log_records_total{${COLLECTOR_SEL}} or otelcol_receiver_refused_log_records_total{${COLLECTOR_SEL}}) or vector(0)" "refused log records")
}

snapshot() {
	if [ "$METRICS_SOURCE" = collector ]; then collector_snapshot; else prom_snapshot; fi
	track_memory
}

select_metrics_source() {
	case "$METRICS_SOURCE" in
	prometheus)
		log "metrics source: prometheus (forced)"
		;;
	collector)
		start_collector_metrics ||
			die "--metrics-source collector was forced but :${COLLECTOR_METRICS_PORT}/metrics is not reachable on every replica"
		log "metrics source: collector :${COLLECTOR_METRICS_PORT}/metrics (no scrape lag)"
		;;
	auto)
		if start_collector_metrics; then
			METRICS_SOURCE=collector
			log "metrics source: collector :${COLLECTOR_METRICS_PORT}/metrics (no scrape lag)"
		else
			stop_collector_metrics
			METRICS_SOURCE=prometheus
			log "WARN: the collector does not serve :${COLLECTOR_METRICS_PORT}/metrics — falling back to Prometheus."
			log "WARN: self-telemetry is PUSHED to Prometheus every 30s, so shedding onset is seen 30-60s late."
			log "WARN: to remove the lag, add a pull reader to service.telemetry.metrics.readers (see README)."
		fi
		;;
	esac
	if [ -z "$POLL_INTERVAL" ]; then
		if [ "$METRICS_SOURCE" = collector ]; then POLL_INTERVAL=5; else POLL_INTERVAL=15; fi
	fi
}

# --------------------------------------------------------------- pre-flight

agent_pod_on_node() {
	kubectl -n "$AGENT_NS" get pods -l "$AGENT_SELECTOR" \
		--field-selector "spec.nodeName=${NODE}" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

pod_field() { kubectl -n "$AGENT_NS" get pod "$1" -o jsonpath="$2" 2>/dev/null; }

agent_status() { pod_field "$1" '{.status.containerStatuses[?(@.name=="'"$AGENT_CONTAINER"'")]'"$2"'}'; }

preflight_cluster() {
	local ctx ready spec
	ctx=$(kubectl config current-context)
	[ "$ctx" = "$EXPECT_CONTEXT" ] || die "kubectl context is '${ctx}', expected '${EXPECT_CONTEXT}' (set EXPECT_CONTEXT to override)"
	kubectl get node "$NODE" >/dev/null 2>&1 || die "node ${NODE} not found"
	spec=$(kubectl -n "$COLLECTOR_NS" get deploy "$COLLECTOR_DEPLOY" -o jsonpath='{.spec.replicas}')
	ready=$(kubectl -n "$COLLECTOR_NS" get deploy "$COLLECTOR_DEPLOY" -o jsonpath='{.status.readyReplicas}')
	[ "$ready" = "$spec" ] || die "otel-collector is ${ready}/${spec} ready — fix the o11y plane before pressuring it"
	log "context=${ctx} node=${NODE} collector=${ready}/${spec} ready"
}

preflight_no_soak() {
	# A soak grades on cumulative prober counters exported through this very
	# collector. Shedding it mid-run destroys the SLI (soak README gotcha 3/4).
	if kubectl -n aether-test get ds k6-soak-loader >/dev/null 2>&1; then
		die "k6-soak-loader is deployed — a soak looks active. NEVER run this during a soak."
	fi
	if pgrep -f 'soak/churn\.sh' >/dev/null 2>&1; then
		die "a churn.sh driver is running on this workstation — a soak looks active."
	fi
	if kubectl -n "$JOB_NS" get job "$JOB_NAME" >/dev/null 2>&1; then
		die "job ${JOB_NS}/${JOB_NAME} already exists — delete it first"
	fi
}

preflight_agent() {
	local pod ready restarts
	pod=$(agent_pod_on_node)
	[ -n "$pod" ] || die "no aether-agent pod on node ${NODE}"
	ready=$(agent_status "$pod" '.ready')
	restarts=$(agent_status "$pod" '.restartCount')
	[ "$ready" = "true" ] || die "agent pod ${pod} is not Ready before the test even starts"
	[ "$restarts" = "0" ] || die "agent pod ${pod} already has ${restarts} restarts — start from a clean node"
	log "agent pod ${pod} Ready, restarts=0"
}

preflight_signals() {
	local probe age
	probe=$(promq_req 'sum(rate(aether_probe_requests_total{result="success"}[3m]))' "prober rate")
	[ "$probe" -gt 0 ] || die "external prober is not reporting successes — the availability signal is dead"
	age=$(series_age)
	[ -n "$age" ] || die "aether_agent_storage_pods{node=\"${NODE}\"} has no samples — the agent's export is already broken"
	log "prober ~${probe}/s success, agent series age ${age}s"
}

preflight_baseline() {
	snapshot
	BASE_HEAP=$M_HEAP
	BASE_RSS=$M_RSS
	BASE_LOGS=$M_LOGS
	BASE_POINTS=$M_POINTS
	local pct
	pct=$(pct_of "$BASE_RSS" "$SOFT_LIMIT_BYTES")
	[ "$pct" -lt "$MAX_BASELINE_PCT" ] ||
		die "baseline collector RSS $(mib "$BASE_RSS")MiB is ${pct}% of the ${MAX_BASELINE_PCT}%-max shedding threshold — it is already loaded"
	log "baseline: heap $(mib "$BASE_HEAP")MiB, RSS $(mib "$BASE_RSS")MiB (${pct}% of the $(mib "$SOFT_LIMIT_BYTES")MiB soft limit), refused_log_records=${BASE_LOGS}, refused_metric_points=${BASE_POINTS}"
}

print_plan() {
	cat <<EOF

$(date -u +%FT%TZ) ==== resolved plan
  kube context             $(kubectl config current-context)
  node under test          ${NODE}
  collector replicas       ${COLLECTOR_PODS[*]}
  metrics source           ${METRICS_SOURCE} (poll every ${POLL_INTERVAL}s)
  GOMEMLIMIT               $(mib "$GOMEMLIMIT_BYTES")MiB   [${GOMEMLIMIT_SRC}]
  pod memory limit         $(mib "$POD_MEM_LIMIT_BYTES")MiB
  memory_limiter hard      $(mib "$HARD_LIMIT_BYTES")MiB   (${LIMIT_PCT}% of the pod limit; refuse + forced GC)
  memory_limiter soft      $(mib "$SOFT_LIMIT_BYTES")MiB   ($((LIMIT_PCT - SPIKE_PCT))% of the pod limit; shedding begins)
  ABORT heap ceiling       $(mib "$ABORT_HEAP_BYTES")MiB   (${ABORT_HEAP_PCT}% of GOMEMLIMIT)  <- primary
  ABORT RSS backstop       $(mib "$ABORT_RSS_BYTES")MiB   (${ABORT_RSS_PCT}% of the pod memory limit)
  current heap             $(mib "$M_HEAP")MiB ($(pct_of "$M_HEAP" "$GOMEMLIMIT_BYTES")% of GOMEMLIMIT)
  current RSS              $(mib "$M_RSS")MiB ($(pct_of "$M_RSS" "$SOFT_LIMIT_BYTES")% of the soft limit)
  refused counters         log_records=${M_LOGS}, metric_points=${M_POINTS}
  pressure job             ${JOB_MANIFEST} -> ${JOB_NS}/${JOB_NAME}
EOF
}

# ----------------------------------------------------------------- pressure

# Primary ceiling: heap against GOMEMLIMIT. Backstop: RSS against the pod's memory
# limit, i.e. the cgroup OOM killer. Crossing either means the flood is outrunning
# the limiter's 5s check and a collector restart — the one thing this harness
# promised not to cause — becomes plausible. Tear down and report.
track_memory() {
	local reason=""
	if [ "$M_RSS" -gt "$MAX_RSS" ]; then MAX_RSS=$M_RSS; fi
	if [ "$M_HEAP" -gt "$MAX_HEAP" ]; then MAX_HEAP=$M_HEAP; fi
	if [ "$M_HEAP" -gt "$ABORT_HEAP_BYTES" ]; then
		reason="heap $(mib "$M_HEAP")MiB crossed the $(mib "$ABORT_HEAP_BYTES")MiB ceiling (${ABORT_HEAP_PCT}% of the $(mib "$GOMEMLIMIT_BYTES")MiB GOMEMLIMIT)"
	elif [ "$M_RSS" -gt "$ABORT_RSS_BYTES" ]; then
		reason="RSS $(mib "$M_RSS")MiB crossed the $(mib "$ABORT_RSS_BYTES")MiB backstop (${ABORT_RSS_PCT}% of the $(mib "$POD_MEM_LIMIT_BYTES")MiB pod limit)"
	fi
	[ -n "$reason" ] || return 0
	kubectl -n "$JOB_NS" delete job "$JOB_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	JOB_APPLIED=0
	die "collector ${reason} — job deleted, no agent was touched"
}

apply_job() {
	kubectl apply -f "$JOB_MANIFEST" >/dev/null
	JOB_APPLIED=1
	log "applied ${JOB_MANIFEST} (hard stop: activeDeadlineSeconds, plus the cleanup trap)"
}

# Wait until the collector refuses metric points, i.e. the agents' own exports are
# being shed — the precise #662 condition. Refused log records are the fast-moving
# proxy and are reported alongside.
wait_for_shedding() {
	local deadline=$((SECONDS + PRESSURE_TIMEOUT)) summary
	while [ "$SECONDS" -lt "$deadline" ]; do
		sleep "$POLL_INTERVAL"
		snapshot
		log "  heap $(mib "$M_HEAP")MiB ($(pct_of "$M_HEAP" "$GOMEMLIMIT_BYTES")% of GOMEMLIMIT)  RSS $(mib "$M_RSS")MiB  refused_logs +$((M_LOGS - BASE_LOGS))  refused_points +$((M_POINTS - BASE_POINTS))"
		if [ "$M_POINTS" -gt "$BASE_POINTS" ]; then
			log "shedding ENGAGED: metric points refused (+$((M_POINTS - BASE_POINTS)))"
			return 0
		fi
	done
	summary="Peak heap $(mib "$MAX_HEAP")MiB ($(pct_of "$MAX_HEAP" "$GOMEMLIMIT_BYTES")% of GOMEMLIMIT), peak RSS $(mib "$MAX_RSS")MiB ($(pct_of "$MAX_RSS" "$SOFT_LIMIT_BYTES")% of the soft limit)."
	if [ "$M_LOGS" -gt "$BASE_LOGS" ]; then
		die "log records are being shed but metric points are not, after ${PRESSURE_TIMEOUT}s — pressure is intermittent. ${summary} Raise --rate/parallelism (see README)."
	fi
	die "could not reach pressure in ${PRESSURE_TIMEOUT}s. ${summary} No refusals. Raise --rate/parallelism (see README)."
}

# ------------------------------------------------------- agent under pressure

restart_agent() {
	OLD_POD=$(agent_pod_on_node)
	[ -n "$OLD_POD" ] || die "no aether-agent pod on ${NODE} to restart"
	# Pod-scoped on purpose: `kubectl rollout restart ds/aether-agent` is DaemonSet-wide
	# and would restart every node's agent under a shedding collector at once.
	kubectl -n "$AGENT_NS" delete pod "$OLD_POD" --wait=false >/dev/null
	log "deleted agent pod ${OLD_POD} on ${NODE} while the collector is shedding"
}

# The DaemonSet only creates the replacement once the old pod is gone (30s grace),
# so pod-appearance and pod-readiness get separate budgets: the readiness clock starts
# when the new pod exists, not when the old one was told to die.
wait_agent_replaced() {
	local deadline=$((SECONDS + POD_APPEAR_TIMEOUT)) pod
	NEW_POD=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		sleep 5
		pod=$(agent_pod_on_node)
		if [ -n "$pod" ] && [ "$pod" != "$OLD_POD" ]; then
			NEW_POD="$pod"
			log "replacement agent pod ${NEW_POD} created"
			return 0
		fi
	done
	fail "no replacement agent pod appeared on ${NODE} within ${POD_APPEAR_TIMEOUT}s of deleting ${OLD_POD}"
}

wait_agent_ready() {
	local deadline=$((SECONDS + AGENT_READY_TIMEOUT)) ready reason
	while [ "$SECONDS" -lt "$deadline" ]; do
		reason=$(agent_status "$NEW_POD" '.state.waiting.reason')
		if [ "$reason" = "CrashLoopBackOff" ]; then
			fail "replacement agent pod ${NEW_POD} is in CrashLoopBackOff — #662 reproduced, the fix did not hold"
		fi
		ready=$(agent_status "$NEW_POD" '.ready')
		if [ "$ready" = "true" ]; then
			log "replacement agent pod ${NEW_POD} Ready"
			return 0
		fi
		sleep 5
	done
	fail "replacement agent pod ${NEW_POD} did not become Ready within ${AGENT_READY_TIMEOUT}s (waiting reason: ${reason:-none})"
}

# The FAIL criterion is #662's signature and nothing else (#699):
#
#   'failed to create SPIRE Workload API source' in the log, and/or the container
#   exiting — CrashLoopBackOff, restartCount > 0, or a terminated state.
#
# Any other ERROR line is reported as INFORMATIONAL. Run 4 of 2026-09-05 came Ready
# in 16s with 0 restarts and SPIRE resolved — the fix demonstrably held — yet the old
# criterion printed FAIL because the agent logged one self-recovering client retry
# (`failed to start watch stream, retrying` / `context canceled`, tracked in #700).
# A harness that fails on unrelated log noise cannot be used to close #662.
verify_agent() {
	local restarts term_reason term_exit logs errors n
	restarts=$(agent_status "$NEW_POD" '.restartCount')
	[ "$restarts" = "0" ] || fail "agent pod ${NEW_POD} started with restartCount=${restarts} — it died at least once under pressure (#662's signature)"
	term_reason=$(agent_status "$NEW_POD" '.lastState.terminated.reason')
	term_exit=$(agent_status "$NEW_POD" '.lastState.terminated.exitCode')
	[ -z "$term_reason" ] || fail "agent pod ${NEW_POD} has a terminated previous container (${term_reason}, exit ${term_exit:-?}) — it exited under pressure (#662's signature)"

	# From the beginning of the log, not the tail: the evidence is the startup sequence.
	logs=$(kubectl -n "$AGENT_NS" logs "$NEW_POD" -c "$AGENT_CONTAINER" --limit-bytes=8000000 2>/dev/null)
	if grep -q 'failed to create SPIRE Workload API source' <<<"$logs"; then
		fail "agent ${NEW_POD} logged 'failed to create SPIRE Workload API source' — #662's signature"
	fi
	grep -q 'resolved workload trust domain from SPIRE' <<<"$logs" ||
		fail "agent ${NEW_POD} never logged 'resolved workload trust domain from SPIRE' — the Workload API source was not established"

	# Informational only. These do not gate the verdict.
	errors=$(grep -E '"level":"ERROR"|[[:space:]]ERROR[[:space:]]' <<<"$logs" || true)
	n=$(grep -cE '"level":"ERROR"|[[:space:]]ERROR[[:space:]]' <<<"$logs" || true)
	AGENT_ERROR_COUNT=$n
	if [ "$n" -gt 0 ]; then
		log "INFO: agent ${NEW_POD} logged ${n} ERROR line(s) — informational, not a FAIL (#699). Shedding-adjacent retries are expected; see #700:"
		head -20 <<<"$errors" | cut -c1-300 | sed 's/^/  | /'
		[ "$n" -le 20 ] || log "  | ... $((n - 20)) more"
	fi
	log "agent ${NEW_POD}: restarts=0, no terminated container, SPIRE Workload API source established"
}

# The claim is only valid if the collector was still shedding while the agent came
# up. If the flood lapsed in that window the agent had an easy start and proved
# nothing.
verify_pressure_held() {
	snapshot
	log "during the restart window: refused_logs +$((M_LOGS - PRE_RESTART_LOGS)), refused_points +$((M_POINTS - PRE_RESTART_POINTS))"
	if [ "$M_LOGS" -le "$PRE_RESTART_LOGS" ] && [ "$M_POINTS" -le "$PRE_RESTART_POINTS" ]; then
		die "the collector stopped shedding during the restart window — the agent did not start under pressure. Inconclusive; re-run with more pressure."
	fi
	HELD_LOGS=$((M_LOGS - PRE_RESTART_LOGS))
	HELD_POINTS=$((M_POINTS - PRE_RESTART_POINTS))
}

# ----------------------------------------------------------------- recovery

stop_pressure() {
	kubectl -n "$JOB_NS" delete job "$JOB_NAME" --ignore-not-found --wait=false >/dev/null
	JOB_APPLIED=0
	log "pressure job deleted"
}

wait_shedding_stops() {
	local deadline=$((SECONDS + RECOVERY_TIMEOUT)) prev
	snapshot
	prev=$M_LOGS
	while [ "$SECONDS" -lt "$deadline" ]; do
		sleep "$POLL_INTERVAL"
		snapshot
		if [ "$M_LOGS" = "$prev" ]; then
			log "shedding stopped (refused_log_records flat at ${M_LOGS}), collector heap $(mib "$M_HEAP")MiB / RSS $(mib "$M_RSS")MiB"
			return 0
		fi
		log "  draining: refused_logs +$((M_LOGS - prev)) in the last ${POLL_INTERVAL}s, heap $(mib "$M_HEAP")MiB, RSS $(mib "$M_RSS")MiB"
		prev=$M_LOGS
	done
	log "WARN: refusals were still incrementing ${RECOVERY_TIMEOUT}s after the job was deleted"
	return 0
}

wait_series_fresh() {
	local deadline=$((SECONDS + SERIES_FRESH_TIMEOUT)) age
	while [ "$SECONDS" -lt "$deadline" ]; do
		age=$(series_age)
		if [ -n "$age" ] && [ "$age" -lt "$SERIES_FRESH_MAX_AGE" ]; then
			log "aether_agent_storage_pods{node=\"${NODE}\"} is fresh again (${age}s old)"
			return 0
		fi
		sleep "$POLL_INTERVAL"
	done
	fail "aether_agent_storage_pods{node=\"${NODE}\"} did not go fresh within ${SERIES_FRESH_TIMEOUT}s (age: ${age:-no samples}) — the agent recovered but its telemetry did not resume"
}

report_pass() {
	snapshot
	cat <<EOF

$(date -u +%FT%TZ) ==== PASS
  node under test          ${NODE}
  metrics source           ${METRICS_SOURCE}
  agent pod                ${OLD_POD} -> ${NEW_POD} (restartCount 0, Ready)
  collector heap           baseline $(mib "$BASE_HEAP")MiB -> peak $(mib "$MAX_HEAP")MiB ($(pct_of "$MAX_HEAP" "$GOMEMLIMIT_BYTES")% of the $(mib "$GOMEMLIMIT_BYTES")MiB GOMEMLIMIT)
  collector RSS            baseline $(mib "$BASE_RSS")MiB -> peak $(mib "$MAX_RSS")MiB ($(pct_of "$MAX_RSS" "$SOFT_LIMIT_BYTES")% of the $(mib "$SOFT_LIMIT_BYTES")MiB soft limit)
  refused (whole run)      log_records +$((M_LOGS - BASE_LOGS)), metric_points +$((M_POINTS - BASE_POINTS))
  refused (restart window) log_records +${HELD_LOGS}, metric_points +${HELD_POINTS}
  agent evidence           'resolved workload trust domain from SPIRE' present; no
                           'failed to create SPIRE Workload API source'; restartCount 0;
                           no CrashLoopBackOff; no terminated container
                           (${AGENT_ERROR_COUNT} other ERROR line(s), informational — see above)
  telemetry recovery       aether_agent_storage_pods{node="${NODE}"} fresh again

  #662's startup decoupling (#668) holds: the agent started, attested and served
  while the collector was refusing its exports.
EOF
}

main() {
	parse_args "$@"
	for c in kubectl jq curl awk; do command -v "$c" >/dev/null || die "missing required command: $c"; done
	SCRATCH=$(mktemp -d)
	trap cleanup EXIT INT TERM

	step "pre-flight"
	preflight_cluster
	preflight_no_soak
	preflight_agent
	discover_collector_pods
	resolve_collector_limits
	start_port_forward
	select_metrics_source
	preflight_signals
	preflight_baseline

	if [ "$DRY_RUN" = 1 ]; then
		print_plan
		log "dry run: no Job applied, no agent touched."
		exit 0
	fi

	step "applying pressure"
	apply_job
	wait_for_shedding

	step "restarting the agent on ${NODE} under pressure"
	PRE_RESTART_LOGS=$M_LOGS
	PRE_RESTART_POINTS=$M_POINTS
	restart_agent
	wait_agent_replaced
	wait_agent_ready
	verify_agent
	verify_pressure_held

	step "releasing pressure"
	stop_pressure
	wait_shedding_stops
	wait_series_fresh

	report_pass
}

main "$@"
