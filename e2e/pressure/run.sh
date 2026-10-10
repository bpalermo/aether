#!/usr/bin/env bash
# Collector-shedding harness for issue #662 (fix: #668).
#
# Deliberately drives the cluster's shared OpenTelemetry collector into
# memory_limiter shedding, then restarts ONE node's aether-agent under that
# pressure and asserts the agent still starts, resolves its SPIRE identity and
# serves. Four soaks never reproduced this condition — over the last two the
# collector peaked at ~22% of its shedding threshold with zero refusals — so the
# branch #668 fixed has never been exercised since it landed.
#
# Read README.md first. NEVER run this during a soak.
#
# Three inputs have no default, because any default would be one cluster's
# layout (#1579): the kube context you mean to pressure, and the namespaces of
# the collector and of Prometheus.
#
#   export EXPECT_CONTEXT=<kube-context> COLLECTOR_NS=<collector-namespace> PROM_NS=<prometheus-namespace>
#   bash e2e/pressure/run.sh --node <node> --dry-run           # resolve + print, change nothing
#   bash e2e/pressure/run.sh --node <node> --no-soak-running
#
# WHAT A FAILED QUESTION MEANS (#1578). Every kubectl and curl call of this
# script either gets an answer or aborts the run with exit 2 and the tool's own
# error on the terminal: "could not ask" is never read as "absent", and never
# as a verdict. Where absence is an answer (no such node, no agent pod yet, no
# GOMEMLIMIT, no sample in Prometheus) it is read from a call that succeeded.
# The wait loops ask again after a failed call, and abort (exit 2), not FAIL
# (exit 1), when the last call before their deadline failed. README.md has the
# table.
#
# WHAT THE SOAK GUARD CAN AND CANNOT KNOW (#1555). A soak is run by an external
# soak harness, maintained outside this repository. What such a harness may read
# from a mesh is written down in test/harnesscontract/external-harness.yaml, and
# that contract names no identity for a load driver: the namespace, names and
# labels of a harness's workloads are its own (the file's `not_contract`
# section). So this script cannot detect a running soak, and does not claim to.
# The guard is:
#
#   1. an acknowledgement the operator must give: --no-soak-running (or
#      NO_SOAK_RUNNING=1). Without it a real run aborts, as the first thing
#      after its arguments are read and before any command is run against the
#      cluster. It is the operator's statement, not something the script
#      verified.
#   2. two best-effort refusals, which can only ever ADD a refusal:
#        - SOAK_POD_SELECTOR, when set to the label selector the operator's
#          harness puts on its pods, refuses while any pod in any namespace
#          carries it. Unset, the check is skipped, and the script says so.
#        - a DaemonSet of aether-system (AGENT_NS) that is mid-roll refuses the
#          run: a soak rolls them, and so does an upgrade, and a pressure run
#          on top of either cannot be attributed. A soak between two rolls
#          passes this.
#
# Neither looks at the processes of the machine the script runs on: a harness
# need not run there.
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
# No default (#1579): the namespace Prometheus runs in, the namespace the
# collector runs in, and the kube context the run is meant for. `preflight_inputs`
# aborts a run that lacks one, before it asks the cluster anything.
PROM_NS="${PROM_NS:-}"
PROM_SVC="${PROM_SVC:-prometheus-server}"
COLLECTOR_NS="${COLLECTOR_NS:-}"
COLLECTOR_DEPLOY="${COLLECTOR_DEPLOY:-otel-collector}"
# Derived from the Deployment's own .spec.selector at run time when left empty.
# Do NOT default this to app.kubernetes.io/name=opentelemetry-collector: every
# Deployment of the upstream collector chart carries that name label (a second,
# scraping collector for one), and pulling another Deployment's pods into the
# set would both add a bogus port-forward and fold a second process's refused
# counters into the sums.
COLLECTOR_SELECTOR="${COLLECTOR_SELECTOR:-}"
COLLECTOR_CONTAINER="${COLLECTOR_CONTAINER:-opentelemetry-collector}"
COLLECTOR_METRICS_PORT="${COLLECTOR_METRICS_PORT:-8888}"
# The OTLP gRPC endpoint the pressure Job floods. Left empty, it is the
# collector Deployment's same-named Service: <deploy>.<namespace>.svc.cluster.local:4317.
COLLECTOR_OTLP_ENDPOINT="${COLLECTOR_OTLP_ENDPOINT:-}"
EXPECT_CONTEXT="${EXPECT_CONTEXT:-}"

# The soak guard (see the header). NO_SOAK_RUNNING=1 is the operator's
# acknowledgement, the same as --no-soak-running. SOAK_POD_SELECTOR is empty on
# purpose: the external-harness contract names no label of a load driver, so
# any default here would be a guess that reads as a check.
NO_SOAK_RUNNING="${NO_SOAK_RUNNING:-0}"
SOAK_POD_SELECTOR="${SOAK_POD_SELECTOR:-}"

# Where the collector's own numbers are read from:
#   collector  — port-forward each replica's :8888/metrics (no scrape lag)
#   prometheus — query Prometheus (the collector pushes self-telemetry every 30s,
#                so this lags shedding onset by 30-60s; see README)
#   auto       — collector if :8888 answers on every replica, else prometheus
METRICS_SOURCE="${METRICS_SOURCE:-auto}"

# Series selector for the SHARED collector's self-telemetry in Prometheus: its
# pods, by the Deployment's name. Deliberately not a bare job= match: any other
# collector-based process in the cluster (a scraper, a profiler) also emits
# otelcol_* series. The name goes into a regex, and the one regex character a
# Deployment's name may hold is the dot, so it is escaped (twice: once for the
# PromQL string, once for the regex).
COLLECTOR_SEL="${COLLECTOR_SEL:-instance=~\"${COLLECTOR_DEPLOY//./\\\\.}-.*\"}"

# memory_limiter settings, as in the collector's deployed config. The absolute
# thresholds are derived from the pod's real memory limit at run time rather
# than hard-coded, so a resize of the collector cannot silently invalidate
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

# The share of a wait loop's polls that must have got an answer before its
# deadline may be read as a FAIL (#1599). A FAIL is a verdict on the agent, and
# one answered poll in a window of failed ones is one look, not a watch.
MIN_POLL_OK_PCT="${MIN_POLL_OK_PCT:-50}"

MIB=$((1024 * 1024))

JOB_APPLIED=0
# 1 from the moment this run asks for the agent pod's deletion: the request may
# have been acted on even if its answer was lost.
AGENT_RESTARTED=0
# 1 from the moment this run asks for the Job to be applied, for good: JOB_APPLIED
# goes back to 0 when the Job is deleted, and "none applied now" is not "none
# was ever applied".
JOB_EVER_APPLIED=0
OLD_POD=""
KUBE_CONTEXT=""
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
  --no-soak-running      the operator's acknowledgement that no soak, release
                         validation or other graded run is using this cluster
                         (or NO_SOAK_RUNNING=1). Required for a real run: the
                         script cannot detect a soak (see the header).
  --metrics-source S     auto|collector|prometheus (default ${METRICS_SOURCE})
  --pressure-timeout N   seconds to wait for shedding to engage (default ${PRESSURE_TIMEOUT})
  --ready-timeout N      seconds for the replacement agent pod to become Ready (default ${AGENT_READY_TIMEOUT})
  --job-manifest PATH    pressure Job manifest (default ${JOB_MANIFEST})
  -h, --help             this text

required environment (no default: each is your cluster's own):
  EXPECT_CONTEXT         the kube context this run is meant for; the run aborts
                         when the current context is another one
  COLLECTOR_NS           namespace of the collector Deployment (COLLECTOR_DEPLOY,
                         default ${COLLECTOR_DEPLOY})
  PROM_NS                namespace of the Prometheus Service (PROM_SVC, default
                         ${PROM_SVC})
EOF
}

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
step() { printf '\n%s ==== %s\n' "$(date -u +%FT%TZ)" "$*"; }
# die and fail end the run. Called inside a $(...), their message would go to
# the caller's variable and the run would end without a word (#1578), so there
# it goes to stderr; the exit status reaches the caller either way.
die() {
	if [ "${BASH_SUBSHELL:-0}" -gt 0 ]; then log "ABORT: $*" >&2; else log "ABORT: $*"; fi
	exit 2
}
fail() {
	if [ "${BASH_SUBSHELL:-0}" -gt 0 ]; then log "FAIL: $*" >&2; else log "FAIL: $*"; fi
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
		--no-soak-running)
			NO_SOAK_RUNNING=1
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
		# The exit status of the run stands. A delete that failed is said: the
		# flood may still be running.
		delete_job ||
			log "WARN: could not confirm the deletion of job ${JOB_NS}/${JOB_NAME} (kubectl's error is above). It may still be running: check, and delete it by hand (kubectl --context ${EXPECT_CONTEXT} -n ${JOB_NS} delete job ${JOB_NAME}). Its activeDeadlineSeconds ends it regardless."
	fi
	if [ -n "$PF_PID" ]; then kill "$PF_PID" >/dev/null 2>&1 || true; fi
	if [ -n "$PF_LOG" ]; then rm -f "$PF_LOG"; fi
	for pid in ${CPF_PIDS[@]+"${CPF_PIDS[@]}"}; do kill "$pid" >/dev/null 2>&1 || true; done
	if [ -n "$SCRATCH" ]; then rm -rf "$SCRATCH"; fi
	exit "$rc"
}

# ------------------------------------------------------------------- helpers

# kc: kubectl against the context the run is meant for. The current context is
# checked once (preflight_cluster), and it can change under a run that lasts
# minutes: making a kind cluster takes it. So no call after the check relies
# on it, and no apply or delete, the cleanup trap's included, can land on
# another cluster.
kc() { kubectl --context "$EXPECT_CONTEXT" "$@"; }

# delete_job: deletes the pressure Job. A Job that is not there is fine
# (--ignore-not-found). An API error is not: kubectl's status is returned and
# its error stays on stderr. A failed delete is one whose outcome is not known
# (the API server may have acted before the answer was lost), so a caller may
# say neither "deleted" nor "not deleted" of it.
delete_job() {
	kc -n "$JOB_NS" delete job "$JOB_NAME" --ignore-not-found --wait=false >/dev/null
}

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
	kc -n "$ns" port-forward "$target" ":${port}" --address 127.0.0.1 >"$logf" 2>&1 &
	pid=$!
	while [ "$waited" -lt 30 ]; do
		# SIGPIPE rule (#1121, e2e/README.md): this script runs under pipefail,
		# so no pipeline may end in a reader that exits before its writer is
		# done (`head`, `grep -q`/`-m`, `awk '...; exit'`): the writer dies of
		# SIGPIPE and the pipeline fails with 141. Read to EOF (`sed -n '1p'`).
		lport=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\).*/\1/p' "$logf" | sed -n '1p')
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
	# Read over a port-forward on an ephemeral local port: it needs neither a
	# route from the operator's machine to Prometheus nor an exec into its pod.
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

# promq <promql> -> first sample's value, floored to an integer.
# Status 0 with empty output: Prometheus answered, and the answer has no sample.
# Non-zero, with the reason on stderr: the query could not be sent, or
# Prometheus did not answer it (an error status, or something that is not its
# JSON). The two are different answers and no caller may fold them (#1578).
promq() {
	local out v
	out=$(curl -sS --max-time 10 --get --data-urlencode "query=$1" \
		"http://127.0.0.1:${PROM_PORT}/api/v1/query") || return 1
	v=$(jq -r 'if .status == "success"
	           then (if (.data.result | length) > 0
	                 then (.data.result[0].value[1] | tonumber | floor)
	                 else "" end)
	           else error("prometheus answered status=\(.status): \(.error // "no error text")") end' <<<"$out") || {
		printf 'prometheus did not answer the query: %s\n' "${out:0:300}" >&2
		return 1
	}
	echo "$v"
}

# promq_req <promql> <what> -> like promq, but a missing sample is fatal, and
# so is a query that could not be asked. The message says which.
promq_req() {
	local v
	v=$(promq "$1") ||
		die "could not query Prometheus for $2 (the error is above) — not reading that as no data (query: $1)"
	[ -n "$v" ] || die "no data for $2 — Prometheus answered with no sample (query: $1)"
	echo "$v"
}

# The age in seconds of the agent's exported series; promq's contract.
series_age() { promq "time() - timestamp(aether_agent_storage_pods{node=\"${NODE}\"})"; }

# ------------------------------------------------- collector self-telemetry

discover_collector_pods() {
	local names sel dep
	if [ -n "$COLLECTOR_SELECTOR" ]; then
		sel="$COLLECTOR_SELECTOR"
	else
		dep=$(kc -n "$COLLECTOR_NS" get deploy "$COLLECTOR_DEPLOY" -o json) ||
			die "could not read Deployment ${COLLECTOR_NS}/${COLLECTOR_DEPLOY} (kubectl's error is above)"
		sel=$(jq -r '.spec.selector.matchLabels // {} | to_entries | map("\(.key)=\(.value)") | join(",")' <<<"$dep") ||
			die "could not read .spec.selector.matchLabels from ${COLLECTOR_NS}/${COLLECTOR_DEPLOY}: kubectl's answer is not the Deployment's JSON"
		[ -n "$sel" ] ||
			die "could not read .spec.selector.matchLabels from ${COLLECTOR_NS}/${COLLECTOR_DEPLOY}: the Deployment has none"
	fi
	# An empty list is "none Running"; a list that could not be read is not.
	names=$(kc -n "$COLLECTOR_NS" get pods -l "$sel" \
		--field-selector status.phase=Running \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}') ||
		die "could not list the collector pods (-l ${sel}) of ${COLLECTOR_NS} (kubectl's error is above) — not reading that as none Running"
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
	# A jsonpath that matches nothing prints nothing and succeeds: that is the
	# "not set" answer. A kubectl that failed has not said "not set".
	raw=$(kc -n "$COLLECTOR_NS" get pod "$pod" -o jsonpath="${csel}.env[?(@.name=='GOMEMLIMIT')].value}") ||
		die "could not read GOMEMLIMIT of ${COLLECTOR_NS}/${pod} (kubectl's error is above) — not reading that as not set"
	lim=$(kc -n "$COLLECTOR_NS" get pod "$pod" -o jsonpath="${csel}.resources.limits.memory}") ||
		die "could not read the memory limit of ${COLLECTOR_NS}/${pod} (kubectl's error is above) — not reading that as no limit"
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
	local i=0 pod out pid port body missing
	for pod in "${COLLECTOR_PODS[@]}"; do
		CPF_LOGS[i]="${SCRATCH}/collector-pf-${i}.log"
		out=$(start_pf "$COLLECTOR_NS" "pod/${pod}" "$COLLECTOR_METRICS_PORT" "${CPF_LOGS[i]}") || {
			pid=${out%% *}
			CPF_PIDS[i]=$pid
			log "WARN: no port-forward to ${pod}:${COLLECTOR_METRICS_PORT}: $(tr '\n' ' ' <"${CPF_LOGS[i]}")"
			return 1
		}
		pid=${out%% *}
		port=${out##* }
		CPF_PIDS[i]=$pid
		CPF_PORTS[i]=$port
		# Three different things, said apart: no port-forward (above), a fetch
		# that failed (a collector with no pull reader listens on no such port),
		# and an answer without the two gauges the safety ceilings read.
		body="${SCRATCH}/metrics-first-${i}.txt"
		if ! curl -fsS --max-time 8 "http://127.0.0.1:${port}/metrics" -o "$body"; then
			log "WARN: could not fetch ${pod}:${COLLECTOR_METRICS_PORT}/metrics (curl's error is above)"
			return 1
		fi
		missing=$(missing_safety_samples "$body")
		if [ -n "$missing" ]; then
			log "WARN: ${pod}:${COLLECTOR_METRICS_PORT}/metrics did not serve ${missing}: without it a safety ceiling could never fire, so this endpoint is not used"
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

# The two gauges the safety ceilings are read from (track_memory).
SAFETY_SAMPLES='otelcol_process_runtime_heap_alloc_bytes otelcol_process_memory_rss_bytes'

# missing_safety_samples <file> -> the names in SAFETY_SAMPLES that the
# /metrics body in <file> has no sample of; nothing when both are there.
# agg_samples reads a gauge that is not there as 0, and a ceiling compared
# with 0 never fires. So "any otelcol_* sample" is not enough: both gauges
# must be there, on the first fetch and on every poll.
missing_safety_samples() {
	local name out=""
	for name in $SAFETY_SAMPLES; do
		grep -qE "^${name}[{ ]" "$1" || out="${out}${out:+ }${name}"
	done
	printf '%s' "$out"
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
	local i=0 f v missing
	M_HEAP=0
	M_RSS=0
	M_POINTS=0
	M_LOGS=0
	for i in "${!CPF_PORTS[@]}"; do
		f="${SCRATCH}/metrics-${i}.txt"
		curl -fsS --max-time 8 "http://127.0.0.1:${CPF_PORTS[i]}/metrics" -o "$f" ||
			die "could not read ${COLLECTOR_PODS[i]}'s /metrics over its port-forward (curl's error is above) — re-run, or force --metrics-source prometheus"
		# A gauge that is not in the answer (another server's page, or a gauge
		# that was there a poll ago) would aggregate to 0, which reads as a
		# healthy, idle collector and disarms that gauge's ceiling.
		missing=$(missing_safety_samples "$f")
		[ -z "$missing" ] ||
			die "${COLLECTOR_PODS[i]}'s /metrics answered without ${missing} — a gauge that is not there is not a zero, and its safety ceiling could not fire"
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

# The names of the agent pods on ${NODE}, one per line. No output with status 0
# is "none": between the old pod going and its replacement being created there
# is none, and that is an answer. A non-zero status is kubectl's, with its
# error on stderr: the list could not be read. (`{.items[0]...}` cannot tell
# the two apart: kubectl fails on an empty list with it.)
agent_pods_on_node() {
	kc -n "$AGENT_NS" get pods -l "$AGENT_SELECTOR" \
		--field-selector "spec.nodeName=${NODE}" \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

# sole_agent_pod <names>: the one agent pod of the node, or an abort (#1601).
# Two pods are a surge-rolled standby next to the owner (proposal 041), or a
# replacement next to a pod that is still terminating. Which of them this run
# would restart, and which one's status it would read, is not the script's to
# guess.
sole_agent_pod() {
	local n
	n=$(printf '%s\n' "$1" | wc -l | tr -d ' ')
	[ "$n" = 1 ] ||
		die "${n} aether-agent pods on node ${NODE} ($(printf '%s' "$1" | tr '\n' ' ')) — a roll's standby, or a pod still terminating, is next to the owner. Wait until the node has one agent pod, then run again."
	printf '%s\n' "$1"
}

# polls_support_fail <last-answered> <answered> <polls> <what could not be asked>:
# returns when a wait loop that ran out of time may FAIL; aborts (exit 2) when
# its polls do not support a verdict: the last one failed, or fewer than
# MIN_POLL_OK_PCT percent of them got an answer.
polls_support_fail() {
	[ "$1" = 1 ] ||
		die "$4 when the time ran out — this is not a verdict on the agent"
	[ $(($2 * 100)) -ge $(($3 * MIN_POLL_OK_PCT)) ] ||
		die "$4 on most polls: only $2 of $3 polls got an answer, and a FAIL needs ${MIN_POLL_OK_PCT}% (MIN_POLL_OK_PCT) — this is not a verdict on the agent"
}

# No output with status 0: the field is not set. Non-zero: it could not be read.
pod_field() { kc -n "$AGENT_NS" get pod "$1" -o jsonpath="$2"; }

agent_status() { pod_field "$1" '{.status.containerStatuses[?(@.name=="'"$AGENT_CONTAINER"'")]'"$2"'}'; }

# The inputs that have no default: each is the layout of one cluster, so a
# default would be a guess (#1579). main calls this before any command is run.
preflight_inputs() {
	local missing=""
	# A percentage, checked here: a wait loop does arithmetic with it, and a
	# value that is not a number would end the run there with the shell's own
	# status, while one above 100 would make a FAIL impossible.
	case "$MIN_POLL_OK_PCT" in
	"" | *[!0-9]*) die "MIN_POLL_OK_PCT must be a whole number from 0 to 100 (got '${MIN_POLL_OK_PCT}')" ;;
	esac
	[ "$MIN_POLL_OK_PCT" -le 100 ] ||
		die "MIN_POLL_OK_PCT must be a whole number from 0 to 100 (got '${MIN_POLL_OK_PCT}')"
	[ -n "$EXPECT_CONTEXT" ] || missing="${missing} EXPECT_CONTEXT (the kube context this run is meant for)"
	[ -n "$COLLECTOR_NS" ] || missing="${missing} COLLECTOR_NS (the namespace of the ${COLLECTOR_DEPLOY} Deployment)"
	[ -n "$PROM_NS" ] || missing="${missing} PROM_NS (the namespace of the ${PROM_SVC} Service)"
	[ -z "$missing" ] || die "set:${missing}. They have no default: see --help."
	[ -n "$COLLECTOR_OTLP_ENDPOINT" ] ||
		COLLECTOR_OTLP_ENDPOINT="${COLLECTOR_DEPLOY}.${COLLECTOR_NS}.svc.cluster.local:4317"
}

preflight_cluster() {
	local ctx ready spec out
	ctx=$(kubectl config current-context) ||
		die "could not read the current kube context (kubectl's error is above)"
	[ "$ctx" = "$EXPECT_CONTEXT" ] || die "kubectl context is '${ctx}', expected '${EXPECT_CONTEXT}' (EXPECT_CONTEXT names the context this run is meant for)"
	# From here on every call names this context (kc).
	KUBE_CONTEXT=$ctx
	# --ignore-not-found: an absent node is an empty answer with status 0, so a
	# non-zero status is "could not ask" and nothing else.
	out=$(kc get node "$NODE" --ignore-not-found -o name) ||
		die "could not ask for node ${NODE} (kubectl's error is above) — not reading that as an absent node"
	[ -n "$out" ] || die "node ${NODE} not found"
	# No --ignore-not-found: the collector is required, and kubectl's own
	# NotFound is the message for a Deployment that is not there.
	out=$(kc -n "$COLLECTOR_NS" get deploy "$COLLECTOR_DEPLOY" -o jsonpath='{.spec.replicas} {.status.readyReplicas}') ||
		die "could not read Deployment ${COLLECTOR_NS}/${COLLECTOR_DEPLOY} (kubectl's error is above)"
	spec=${out%% *}
	ready=${out#* }
	# .status.readyReplicas is absent from a Deployment with no ready replica.
	ready=${ready:-0}
	[ "$ready" = "$spec" ] || die "collector ${COLLECTOR_NS}/${COLLECTOR_DEPLOY} is ${ready}/${spec} ready — fix the telemetry plane before pressuring it"
	log "context=${ctx} node=${NODE} collector=${ready}/${spec} ready"
}

# Names the DaemonSets of ${AGENT_NS} that are mid-roll, one per line, from
# `kubectl get ds -o json` on stdin. Mid-roll: the controller has not observed
# the current spec, or not every scheduled pod is updated, or a pod is
# unavailable.
daemonsets_mid_roll() {
	jq -r '.items[]
		| select((.metadata.generation // 0) != (.status.observedGeneration // 0)
			or (.status.updatedNumberScheduled // 0) < (.status.desiredNumberScheduled // 0)
			or (.status.numberUnavailable // 0) > 0)
		| .metadata.name'
}

# The operator's half of the soak guard. A soak grades on cumulative prober
# counters exported through the collector this script sheds, and the script
# cannot detect a soak (header, "WHAT THE SOAK GUARD CAN AND CANNOT KNOW"), so
# the operator says there is none. main calls this first, before it runs any
# command: a run that was not acknowledged asks the cluster nothing.
preflight_ack() {
	[ "$NO_SOAK_RUNNING" = 1 ] && return 0
	if [ "$DRY_RUN" = 1 ]; then
		log "WARN: --no-soak-running not given. A real run needs it: this script cannot detect a soak."
		return 0
	fi
	die "refusing to run without --no-soak-running (or NO_SOAK_RUNNING=1). This script cannot detect a soak: confirm that no soak, release validation or other graded run is using this cluster, then say so. NEVER run this during a soak."
}

preflight_no_soak() {
	# The script's half of the soak guard: two best-effort checks that can only
	# add a refusal to the operator's acknowledgement (preflight_ack). Each
	# fails closed: a list that could not be read is not an empty list.
	local pods ds rolling job
	if [ -n "$SOAK_POD_SELECTOR" ]; then
		pods=$(kc get pods --all-namespaces -l "$SOAK_POD_SELECTOR" -o name) ||
			die "could not list pods by SOAK_POD_SELECTOR='${SOAK_POD_SELECTOR}' — not assuming there are none"
		if [ -n "$pods" ]; then
			die "$(printf '%s\n' "$pods" | wc -l | tr -d ' ') pod(s) carry SOAK_POD_SELECTOR='${SOAK_POD_SELECTOR}' — a soak looks active. NEVER run this during a soak."
		fi
		log "no pod carries SOAK_POD_SELECTOR='${SOAK_POD_SELECTOR}'"
	else
		log "SOAK_POD_SELECTOR is not set: no pod was looked for. The soak guard is the operator's acknowledgement and the roll check only."
	fi
	ds=$(kc -n "$AGENT_NS" get ds -o json) ||
		die "could not list the DaemonSets of ${AGENT_NS} — not assuming none is mid-roll"
	rolling=$(printf '%s' "$ds" | daemonsets_mid_roll) ||
		die "could not read the DaemonSets of ${AGENT_NS} — not assuming none is mid-roll"
	if [ -n "$rolling" ]; then
		die "DaemonSet(s) mid-roll in ${AGENT_NS}: $(printf '%s' "$rolling" | tr '\n' ' ') — a soak's churn or an upgrade looks active. A pressure run on top of a roll cannot be attributed."
	fi
	log "no DaemonSet of ${AGENT_NS} is mid-roll"
	# A Job left by an earlier run is still flooding. "Could not ask" is not
	# "there is none" (#1578): with --ignore-not-found an absent Job is an empty
	# answer with status 0.
	job=$(kc -n "$JOB_NS" get job "$JOB_NAME" --ignore-not-found -o name) ||
		die "could not ask for job ${JOB_NS}/${JOB_NAME} (kubectl's error is above) — not reading that as an absent job"
	[ -z "$job" ] || die "job ${JOB_NS}/${JOB_NAME} already exists — delete it first"
}

preflight_agent() {
	local pods pod ready restarts
	pods=$(agent_pods_on_node) ||
		die "could not list the aether-agent pods on node ${NODE} (kubectl's error is above) — not reading that as none"
	[ -n "$pods" ] || die "no aether-agent pod on node ${NODE}"
	pod=$(sole_agent_pod "$pods") || exit $?
	ready=$(agent_status "$pod" '.ready') ||
		die "could not read the status of agent pod ${pod} (kubectl's error is above) — not reading that as not Ready"
	restarts=$(agent_status "$pod" '.restartCount') ||
		die "could not read the status of agent pod ${pod} (kubectl's error is above) — not reading that as a restart count"
	[ "$ready" = "true" ] || die "agent pod ${pod} is not Ready before the test even starts"
	[ "$restarts" = "0" ] || die "agent pod ${pod} already has ${restarts} restarts — start from a clean node"
	log "agent pod ${pod} Ready, restarts=0"
}

preflight_signals() {
	local probe age
	probe=$(promq_req 'sum(rate(aether_probe_requests_total{result="success"}[3m]))' "prober rate")
	[ "$probe" -gt 0 ] || die "external prober is not reporting successes — the availability signal is dead"
	age=$(series_age) ||
		die "could not query Prometheus for the age of aether_agent_storage_pods{node=\"${NODE}\"} (the error is above) — not reading that as a series with no samples"
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
  kube context             ${KUBE_CONTEXT}
  node under test          ${NODE}
  collector                ${COLLECTOR_NS}/${COLLECTOR_DEPLOY}, flooded at ${COLLECTOR_OTLP_ENDPOINT}
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
	# This is read after the restart step too (#1598): say what is true of the agent.
	local agent="no agent was touched"
	[ "$AGENT_RESTARTED" != 1 ] || agent="agent pod ${OLD_POD} on ${NODE} had already been deleted by this run"
	# No Job to delete now. Either none was ever applied (the baseline is read
	# before the apply, and a dry run never applies: nothing was changed), or
	# this run has already deleted it (the ceiling is read while the collector
	# drains, too), and then things were changed.
	if [ "$JOB_APPLIED" != 1 ]; then
		[ "$JOB_EVER_APPLIED" = 1 ] ||
			die "collector ${reason} — no job had been applied, nothing was changed"
		die "collector ${reason} — the pressure job had already been deleted by this run, and ${agent}"
	fi
	if delete_job; then
		JOB_APPLIED=0
		die "collector ${reason} — job deleted, ${agent}"
	fi
	# Not known to be deleted, not known to be still there. JOB_APPLIED stays
	# 1: the cleanup trap tries the delete again.
	die "collector ${reason} — and the deletion of the job could NOT be confirmed (kubectl's error is above): the flood may still be running. The cleanup trap tries again; activeDeadlineSeconds ends it regardless. Also: ${agent}."
}

# manifest_defines_job <rendered manifest>: status 0 when the manifest is the
# Job this run watches and deletes, and nothing else (#1600).
#
# An ALLOW-LIST of one shape, not a list of forms to refuse. YAML has many ways
# to show kubectl an object that a line check does not see (a second document
# behind `---` or `...`, flow-style `metadata: {...}`, JSON, a List, tags,
# directives, anchors and merge keys, a repeated key), and kubectl applies a
# stream in order: a Job by another name ahead of anything that fails is
# created, and the cleanup trap deletes only the expected name. So every line
# that is not blank or a comment must be one of:
#
#   - a document marker, `---` or `...`, alone on its line (a comment may
#     follow). After the first line of content a marker ends the manifest: no
#     content may follow it;
#   - one of four top-level lines, each exactly once, in any order:
#     `apiVersion: batch/v1`, `kind: Job`, `metadata:`, `spec:`. Nothing else
#     may start in the first column;
#   - under `metadata:`, at two spaces, plain `key:` lines, among them exactly
#     one `  name: ${JOB_NAME}` and one `  namespace: ${JOB_NS}`, no other
#     `name` or `namespace`, and no `generateName`. Deeper lines (labels,
#     annotations) are not looked at;
#   - under `spec:`, anything indented, with exactly one
#     `  activeDeadlineSeconds: <positive integer>` at two spaces. That
#     deadline is the stop that needs neither this script nor the API server
#     to be reachable from it, and the messages of a delete that could not be
#     confirmed rely on it.
#
# A tab, a carriage return or a byte-order mark anywhere refuses the manifest.
#
# Why lines and not a parser: the structural way is `kubectl create
# --dry-run=client -o json`, and it is not client-only. kubectl 1.35 asks the
# API server for its OpenAPI document, and for its API group list even with
# --validate=false.
manifest_defines_job() {
	case "$1" in
	*$'\t'* | *$'\r'* | $'\xef\xbb\xbf'*) return 1 ;;
	esac
	WANT_NAME="  name: ${JOB_NAME}" WANT_NS="  namespace: ${JOB_NS}" awk '
		function refuse() { ok = 0; exit }
		BEGIN { ok = 1 }
		/^[[:space:]]*(#.*)?$/ { next }
		/^(---|\.\.\.)[[:space:]]*(#.*)?$/ { if (content) ended = 1; next }
		ended { refuse() }
		{ content = 1 }
		/^[^ ]/ {
			sub(/[[:space:]]+$/, "")
			if ($0 == "apiVersion: batch/v1") section = "apiVersion"
			else if ($0 == "kind: Job") section = "kind"
			else if ($0 == "metadata:") section = "metadata"
			else if ($0 == "spec:") section = "spec"
			else refuse()
			if (seen[section]++) refuse()
			next
		}
		section == "metadata" {
			if ($0 ~ /^ [^ ]/) refuse()
			if ($0 ~ /^  [^ ]/) {
				if ($0 !~ /^  [A-Za-z][A-Za-z0-9]*:( |$)/) refuse()
				if ($0 == ENVIRON["WANT_NAME"]) name++
				else if ($0 == ENVIRON["WANT_NS"]) namespace++
				else if ($0 ~ /^  (name|namespace|generateName):/) refuse()
			}
			next
		}
		section == "spec" {
			if ($0 ~ /^  activeDeadlineSeconds:/) {
				if ($0 !~ /^  activeDeadlineSeconds: [1-9][0-9]*[[:space:]]*$/) refuse()
				deadline++
			}
			next
		}
		{ refuse() }
		END {
			exit !(ok && seen["apiVersion"] == 1 && seen["kind"] == 1 && seen["metadata"] == 1 &&
				seen["spec"] == 1 && name == 1 && namespace == 1 && deadline == 1)
		}' <<<"$1"
}

# The manifest on stdout, with the tokens of the shipped manifest filled in:
# the collector the caller named, and the Job's own namespace and name (#1600).
# A manifest of the caller's own (--job-manifest) that has no token passes
# through as it is.
render_job() {
	sed -e "s|__COLLECTOR_OTLP_ENDPOINT__|${COLLECTOR_OTLP_ENDPOINT:-${COLLECTOR_DEPLOY}.${COLLECTOR_NS}.svc.cluster.local:4317}|g" \
		-e "s|__COLLECTOR_NS__|${COLLECTOR_NS}|g" \
		-e "s|__JOB_NS__|${JOB_NS}|g" \
		-e "s|__JOB_NAME__|${JOB_NAME}|g" "$JOB_MANIFEST"
}

apply_job() {
	local rendered
	rendered=$(render_job) || die "could not read the job manifest ${JOB_MANIFEST}"
	# The Job that is applied must be the Job that is watched and deleted
	# (#1600): anything else is refused before anything is applied.
	manifest_defines_job "$rendered" ||
		die "${JOB_MANIFEST} is not the one shape this run applies: one block-style YAML document whose top-level keys are exactly 'apiVersion: batch/v1', 'kind: Job', 'metadata:' and 'spec:', with '  name: ${JOB_NAME}' and '  namespace: ${JOB_NS}' under metadata (or the __JOB_NAME__ and __JOB_NS__ tokens) and one positive '  activeDeadlineSeconds:' under spec. Nothing was applied. See manifest_defines_job in this script."
	# Set before the apply: one that failed may still have created the Job, and
	# the cleanup trap deletes it only when this says so.
	JOB_APPLIED=1
	JOB_EVER_APPLIED=1
	printf '%s\n' "$rendered" | kc apply -f - >/dev/null ||
		die "could not confirm that ${JOB_MANIFEST} was applied (the error is above) — job ${JOB_NS}/${JOB_NAME} may have been created, and the cleanup trap tries to delete it. No agent was touched."
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
	local pods
	pods=$(agent_pods_on_node) ||
		die "could not list the aether-agent pods on node ${NODE} (kubectl's error is above) — no agent was restarted"
	[ -n "$pods" ] || die "no aether-agent pod on ${NODE} to restart"
	OLD_POD=$(sole_agent_pod "$pods") || exit $?
	# Pod-scoped on purpose: `kubectl rollout restart ds/aether-agent` is DaemonSet-wide
	# and would restart every node's agent under a shedding collector at once.
	AGENT_RESTARTED=1
	kc -n "$AGENT_NS" delete pod "$OLD_POD" --wait=false >/dev/null ||
		die "could not confirm the deletion of agent pod ${OLD_POD} (kubectl's error is above) — whether the agent was restarted is not known, so nothing was proven"
	log "deleted agent pod ${OLD_POD} on ${NODE} while the collector is shedding"
}

# The DaemonSet only creates the replacement once the old pod is gone (30s grace),
# so pod-appearance and pod-readiness get separate budgets: the readiness clock starts
# when the new pod exists, not when the old one was told to die.
wait_agent_replaced() {
	local deadline=$((SECONDS + POD_APPEAR_TIMEOUT)) pods pod asked=1 answered=0 polls=0
	NEW_POD=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		sleep 5
		# A list that could not be read is asked for again: one failed call is
		# not the agent's fault. An empty list is the gap before the DaemonSet
		# creates the replacement.
		polls=$((polls + 1))
		if pods=$(agent_pods_on_node); then
			asked=1
			answered=$((answered + 1))
			while IFS= read -r pod; do
				if [ -n "$pod" ] && [ "$pod" != "$OLD_POD" ]; then
					NEW_POD="$pod"
					log "replacement agent pod ${NEW_POD} created"
					return 0
				fi
			done <<<"$pods"
		else
			asked=0
			log "WARN: could not list the aether-agent pods on node ${NODE} (kubectl's error is above); asking again"
		fi
	done
	# FAIL is a verdict on the agent. A list that could not be read at the
	# deadline is not one, and neither is one answered poll among failed ones.
	polls_support_fail "$asked" "$answered" "$polls" "could not list the aether-agent pods on node ${NODE}"
	fail "no replacement agent pod appeared on ${NODE} within ${POD_APPEAR_TIMEOUT}s of deleting ${OLD_POD}"
}

wait_agent_ready() {
	local deadline=$((SECONDS + AGENT_READY_TIMEOUT)) ready reason="" asked=1 answered=0 polls=0
	while [ "$SECONDS" -lt "$deadline" ]; do
		# A status that could not be read is asked for again, and is neither
		# "not Ready" nor "not crash-looping".
		polls=$((polls + 1))
		# The waiting reason is acted on as soon as it is read: a crash loop
		# that was seen is the signature, whatever the next read does.
		if reason=$(agent_status "$NEW_POD" '.state.waiting.reason'); then
			if [ "$reason" = "CrashLoopBackOff" ]; then
				fail "replacement agent pod ${NEW_POD} is in CrashLoopBackOff — #662 reproduced, the fix did not hold"
			fi
			ready=$(agent_status "$NEW_POD" '.ready') || reason=unread
		else
			reason=unread
		fi
		if [ "$reason" != unread ]; then
			asked=1
			answered=$((answered + 1))
			if [ "$ready" = "true" ]; then
				log "replacement agent pod ${NEW_POD} Ready"
				return 0
			fi
		else
			asked=0
			log "WARN: could not read the status of agent pod ${NEW_POD} (kubectl's error is above); asking again"
		fi
		sleep 5
	done
	polls_support_fail "$asked" "$answered" "$polls" "could not read the status of agent pod ${NEW_POD}"
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
	# Every read below is evidence for the verdict. One that failed is not
	# evidence either way: the run is INCONCLUSIVE (exit 2), never a FAIL that
	# says #662 is back, and never a PASS on an unread field.
	local unread="(kubectl's error is above) — an unread status is not evidence, so this is not a verdict on the agent"
	restarts=$(agent_status "$NEW_POD" '.restartCount') ||
		die "could not read the status of agent pod ${NEW_POD} ${unread}"
	[ "$restarts" = "0" ] || fail "agent pod ${NEW_POD} started with restartCount=${restarts} — it died at least once under pressure (#662's signature)"
	term_reason=$(agent_status "$NEW_POD" '.lastState.terminated.reason') ||
		die "could not read the status of agent pod ${NEW_POD} ${unread}"
	# The exit code is a detail of the message. A terminated container that
	# was read is the signature whether or not its exit code can be read too.
	term_exit=""
	[ -z "$term_reason" ] || term_exit=$(agent_status "$NEW_POD" '.lastState.terminated.exitCode') || term_exit=""
	[ -z "$term_reason" ] || fail "agent pod ${NEW_POD} has a terminated previous container (${term_reason}, exit ${term_exit:-?}) — it exited under pressure (#662's signature)"

	# From the beginning of the log, not the tail: the evidence is the startup sequence.
	# A log that could not be read has not "never logged" anything.
	logs=$(kc -n "$AGENT_NS" logs "$NEW_POD" -c "$AGENT_CONTAINER" --limit-bytes=8000000) ||
		die "could not read the log of agent pod ${NEW_POD} (kubectl's error is above) — an unread log is not evidence, so this is not a verdict on the agent"
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
	# JOB_APPLIED stays 1 on a failed delete, so the cleanup trap tries again.
	# The agent's checks have passed by now, but the recovery was not measured:
	# INCONCLUSIVE, not FAIL.
	delete_job ||
		die "could not confirm the deletion of job ${JOB_NS}/${JOB_NAME} (kubectl's error is above) — the flood may still be running and the recovery was not measured. The cleanup trap tries again."
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
	local deadline=$((SECONDS + SERIES_FRESH_TIMEOUT)) age="" asked=1 answered=0 polls=0
	while [ "$SECONDS" -lt "$deadline" ]; do
		# A query that could not be asked is asked again, and is not "stale".
		polls=$((polls + 1))
		if age=$(series_age); then
			asked=1
			answered=$((answered + 1))
			if [ -n "$age" ] && [ "$age" -lt "$SERIES_FRESH_MAX_AGE" ]; then
				log "aether_agent_storage_pods{node=\"${NODE}\"} is fresh again (${age}s old)"
				return 0
			fi
		else
			asked=0
			age=""
			log "WARN: could not query Prometheus for the age of aether_agent_storage_pods{node=\"${NODE}\"} (the error is above); asking again"
		fi
		sleep "$POLL_INTERVAL"
	done
	polls_support_fail "$asked" "$answered" "$polls" "could not query Prometheus for the age of aether_agent_storage_pods{node=\"${NODE}\"}"
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
	preflight_ack
	preflight_inputs
	for c in kubectl jq curl awk sed; do command -v "$c" >/dev/null || die "missing required command: $c"; done
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

# Sourced (preflight_test.sh calls the pre-flight functions one at a time):
# define everything, run nothing.
if [ "${BASH_SOURCE[0]}" != "$0" ]; then
	return 0
fi

main "$@"
