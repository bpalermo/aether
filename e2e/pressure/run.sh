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
#
# Exit codes: 0 PASS, 1 FAIL, 2 INCONCLUSIVE (pressure not reached, or lapsed
# mid-test, or aborted on the safety ceiling). Every exit path deletes the Job.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

NODE=""
JOB_MANIFEST="${JOB_MANIFEST:-${SCRIPT_DIR}/collector-pressure-job.yaml}"
JOB_NAME="${JOB_NAME:-aether-collector-pressure}"
JOB_NS="${JOB_NS:-aether-test}"
AGENT_NS="${AGENT_NS:-aether-system}"
AGENT_SELECTOR="${AGENT_SELECTOR:-app.kubernetes.io/name=aether-agent}"
AGENT_CONTAINER="${AGENT_CONTAINER:-agent}"
PROM_NS="${PROM_NS:-prometheus}"
PROM_SVC="${PROM_SVC:-prometheus-server}"
EXPECT_CONTEXT="${EXPECT_CONTEXT:-talos-main}"

# Series selector for the SHARED collector's self-telemetry. Deliberately not a
# bare job= match: otel-scraper and the eBPF profiler also emit otelcol_* series.
COLLECTOR_SEL='instance=~"otel-collector-.*"'

# Deployed collector: 2Gi limit, memory_limiter check_interval 5s,
# limit_percentage 80, spike_limit_percentage 25 (o11y/otel-collector ConfigMap).
#   hard limit = 80% of 2Gi          = 1638 MiB  -> receiver refuses, GC forced
#   soft limit = (80-25)% of 2Gi     = 1126 MiB  -> shedding begins here
MIB=$((1024 * 1024))
HARD_LIMIT_BYTES=$((1638 * MIB))
SOFT_LIMIT_BYTES=$((1126 * MIB))
# Abort ceiling. If a replica's RSS gets this close to the hard limit the flood is
# outrunning the limiter's 5s check and an OOMKill (i.e. a collector restart, which
# is what we promised not to cause) becomes plausible. Tear down and report.
ABORT_RSS_BYTES=$((1500 * MIB))
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
POLL_INTERVAL="${POLL_INTERVAL:-15}"

JOB_APPLIED=0
PF_PID=""
PF_LOG=""
PROM_PORT=""
MAX_RSS=0

usage() {
	cat <<EOF
usage: $0 --node <node-name> [options]

  --node <name>        node whose aether-agent pod is restarted under pressure (required)
  --pressure-timeout N seconds to wait for shedding to engage (default ${PRESSURE_TIMEOUT})
  --ready-timeout N    seconds for the replacement agent pod to become Ready (default ${AGENT_READY_TIMEOUT})
  --job-manifest PATH  pressure Job manifest (default ${JOB_MANIFEST})
  -h, --help           this text
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
pct_of_soft() { echo $((100 * $1 / SOFT_LIMIT_BYTES)); }

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--node)
			NODE="${2:-}"
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
}

cleanup() {
	local rc=$?
	trap - EXIT INT TERM
	if [ "$JOB_APPLIED" = 1 ]; then
		log "cleanup: deleting job ${JOB_NS}/${JOB_NAME}"
		kubectl -n "$JOB_NS" delete job "$JOB_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	fi
	if [ -n "$PF_PID" ]; then kill "$PF_PID" >/dev/null 2>&1 || true; fi
	if [ -n "$PF_LOG" ]; then rm -f "$PF_LOG"; fi
	exit "$rc"
}

# ---------------------------------------------------------------- Prometheus

start_port_forward() {
	# The Prometheus LB address is not reachable from a workstation and `kubectl exec`
	# into Prometheus is not permitted here, so everything is read over a port-forward
	# on an ephemeral local port.
	PF_LOG=$(mktemp)
	kubectl -n "$PROM_NS" port-forward "svc/${PROM_SVC}" :80 --address 127.0.0.1 >"$PF_LOG" 2>&1 &
	PF_PID=$!
	local waited=0
	while [ "$waited" -lt 30 ]; do
		PROM_PORT=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\).*/\1/p' "$PF_LOG" | head -1)
		if [ -n "$PROM_PORT" ]; then break; fi
		kill -0 "$PF_PID" 2>/dev/null || die "port-forward to ${PROM_NS}/${PROM_SVC} died: $(cat "$PF_LOG")"
		sleep 1
		waited=$((waited + 1))
	done
	[ -n "$PROM_PORT" ] || die "could not establish a port-forward to ${PROM_NS}/${PROM_SVC}"
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

collector_rss() { promq_req "max(otelcol_process_memory_rss_bytes{${COLLECTOR_SEL}})" "collector RSS"; }
refused_logs() { promq_req "sum(otelcol_receiver_refused_log_records_total{${COLLECTOR_SEL}})" "refused log records"; }
refused_points() { promq_req "sum(otelcol_receiver_refused_metric_points_total{${COLLECTOR_SEL}})" "refused metric points"; }
series_age() { promq "time() - timestamp(aether_agent_storage_pods{node=\"${NODE}\"})"; }

# --------------------------------------------------------------- pre-flight

agent_pod_on_node() {
	kubectl -n "$AGENT_NS" get pods -l "$AGENT_SELECTOR" \
		--field-selector "spec.nodeName=${NODE}" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

pod_field() { kubectl -n "$AGENT_NS" get pod "$1" -o jsonpath="$2" 2>/dev/null; }

preflight_cluster() {
	local ctx ready spec
	ctx=$(kubectl config current-context)
	[ "$ctx" = "$EXPECT_CONTEXT" ] || die "kubectl context is '${ctx}', expected '${EXPECT_CONTEXT}' (set EXPECT_CONTEXT to override)"
	kubectl get node "$NODE" >/dev/null 2>&1 || die "node ${NODE} not found"
	spec=$(kubectl -n o11y get deploy otel-collector -o jsonpath='{.spec.replicas}')
	ready=$(kubectl -n o11y get deploy otel-collector -o jsonpath='{.status.readyReplicas}')
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
	ready=$(pod_field "$pod" '{.status.containerStatuses[?(@.name=="'"$AGENT_CONTAINER"'")].ready}')
	restarts=$(pod_field "$pod" '{.status.containerStatuses[?(@.name=="'"$AGENT_CONTAINER"'")].restartCount}')
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
	BASE_RSS=$(collector_rss)
	BASE_LOGS=$(refused_logs)
	BASE_POINTS=$(refused_points)
	MAX_RSS=$BASE_RSS
	local pct
	pct=$(pct_of_soft "$BASE_RSS")
	[ "$pct" -lt "$MAX_BASELINE_PCT" ] ||
		die "baseline collector RSS $(mib "$BASE_RSS")MiB is ${pct}% of the ${MAX_BASELINE_PCT}%-max shedding threshold — it is already loaded"
	log "baseline: RSS $(mib "$BASE_RSS")MiB (${pct}% of the $(mib "$SOFT_LIMIT_BYTES")MiB soft limit), refused_log_records=${BASE_LOGS}, refused_metric_points=${BASE_POINTS}"
}

# ----------------------------------------------------------------- pressure

track_rss() {
	local rss=$1
	if [ "$rss" -gt "$MAX_RSS" ]; then MAX_RSS=$rss; fi
	if [ "$rss" -gt "$ABORT_RSS_BYTES" ]; then
		kubectl -n "$JOB_NS" delete job "$JOB_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
		JOB_APPLIED=0
		die "collector RSS $(mib "$rss")MiB crossed the $(mib "$ABORT_RSS_BYTES")MiB safety ceiling (hard limit $(mib "$HARD_LIMIT_BYTES")MiB) — job deleted, no agent was touched"
	fi
	return 0
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
	local deadline=$((SECONDS + PRESSURE_TIMEOUT)) rss rl rp
	while [ "$SECONDS" -lt "$deadline" ]; do
		sleep "$POLL_INTERVAL"
		rss=$(collector_rss)
		rl=$(refused_logs)
		rp=$(refused_points)
		track_rss "$rss"
		log "  RSS $(mib "$rss")MiB ($(pct_of_soft "$rss")% of soft limit)  refused_logs +$((rl - BASE_LOGS))  refused_points +$((rp - BASE_POINTS))"
		if [ "$rp" -gt "$BASE_POINTS" ]; then
			log "shedding ENGAGED: metric points refused (+$((rp - BASE_POINTS)))"
			return 0
		fi
	done
	if [ "$(refused_logs)" -gt "$BASE_LOGS" ]; then
		die "log records are being shed but metric points are not, after ${PRESSURE_TIMEOUT}s — pressure is intermittent. Max RSS $(mib "$MAX_RSS")MiB ($(pct_of_soft "$MAX_RSS")% of the soft limit). Raise --rate/parallelism (see README)."
	fi
	die "could not reach pressure in ${PRESSURE_TIMEOUT}s. Max RSS $(mib "$MAX_RSS")MiB ($(pct_of_soft "$MAX_RSS")% of the $(mib "$SOFT_LIMIT_BYTES")MiB soft limit), no refusals. Raise --rate/parallelism (see README)."
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
		reason=$(pod_field "$NEW_POD" '{.status.containerStatuses[?(@.name=="'"$AGENT_CONTAINER"'")].state.waiting.reason}')
		if [ "$reason" = "CrashLoopBackOff" ]; then
			fail "replacement agent pod ${NEW_POD} is in CrashLoopBackOff — #662 reproduced, the fix did not hold"
		fi
		ready=$(pod_field "$NEW_POD" '{.status.containerStatuses[?(@.name=="'"$AGENT_CONTAINER"'")].ready}')
		if [ "$ready" = "true" ]; then
			log "replacement agent pod ${NEW_POD} Ready"
			return 0
		fi
		sleep 5
	done
	fail "replacement agent pod ${NEW_POD} did not become Ready within ${AGENT_READY_TIMEOUT}s (waiting reason: ${reason:-none})"
}

verify_agent() {
	local restarts logs
	restarts=$(pod_field "$NEW_POD" '{.status.containerStatuses[?(@.name=="'"$AGENT_CONTAINER"'")].restartCount}')
	[ "$restarts" = "0" ] || fail "agent pod ${NEW_POD} started with restartCount=${restarts} — it died at least once under pressure"

	# From the beginning of the log, not the tail: the evidence is the startup sequence.
	logs=$(kubectl -n "$AGENT_NS" logs "$NEW_POD" -c "$AGENT_CONTAINER" --limit-bytes=8000000 2>/dev/null)
	grep -q 'resolved workload trust domain from SPIRE' <<<"$logs" ||
		fail "agent ${NEW_POD} never logged 'resolved workload trust domain from SPIRE' — the Workload API source was not established"
	if grep -q 'failed to create SPIRE Workload API source' <<<"$logs"; then
		fail "agent ${NEW_POD} logged 'failed to create SPIRE Workload API source' — #662's signature"
	fi
	if grep '"level":"ERROR"' <<<"$logs" | grep -q 'context canceled'; then
		fail "agent ${NEW_POD} logged an ERROR containing 'context canceled' — startup is still coupled to the cancelled context"
	fi
	log "agent ${NEW_POD}: restarts=0, SPIRE source established, no 'context canceled' errors"
}

# The claim is only valid if the collector was still shedding while the agent came
# up. If the flood lapsed in that window the agent had an easy start and proved
# nothing.
verify_pressure_held() {
	local rl rp
	rl=$(refused_logs)
	rp=$(refused_points)
	track_rss "$(collector_rss)"
	log "during the restart window: refused_logs +$((rl - PRE_RESTART_LOGS)), refused_points +$((rp - PRE_RESTART_POINTS))"
	if [ "$rl" -le "$PRE_RESTART_LOGS" ] && [ "$rp" -le "$PRE_RESTART_POINTS" ]; then
		die "the collector stopped shedding during the restart window — the agent did not start under pressure. Inconclusive; re-run with more pressure."
	fi
	HELD_LOGS=$((rl - PRE_RESTART_LOGS))
	HELD_POINTS=$((rp - PRE_RESTART_POINTS))
}

# ----------------------------------------------------------------- recovery

stop_pressure() {
	kubectl -n "$JOB_NS" delete job "$JOB_NAME" --ignore-not-found --wait=false >/dev/null
	JOB_APPLIED=0
	log "pressure job deleted"
}

wait_shedding_stops() {
	local deadline=$((SECONDS + RECOVERY_TIMEOUT)) prev cur rss
	prev=$(refused_logs)
	while [ "$SECONDS" -lt "$deadline" ]; do
		sleep "$POLL_INTERVAL"
		cur=$(refused_logs)
		rss=$(collector_rss)
		if [ "$cur" = "$prev" ]; then
			log "shedding stopped (refused_log_records flat at ${cur}), collector RSS $(mib "$rss")MiB"
			return 0
		fi
		log "  draining: refused_logs +$((cur - prev)) in the last ${POLL_INTERVAL}s, RSS $(mib "$rss")MiB"
		prev=$cur
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
	cat <<EOF

$(date -u +%FT%TZ) ==== PASS
  node under test          ${NODE}
  agent pod                ${OLD_POD} -> ${NEW_POD} (restartCount 0, Ready)
  collector RSS            baseline $(mib "$BASE_RSS")MiB -> peak $(mib "$MAX_RSS")MiB ($(pct_of_soft "$MAX_RSS")% of the $(mib "$SOFT_LIMIT_BYTES")MiB soft limit)
  refused (whole run)      log_records +$(($(refused_logs) - BASE_LOGS)), metric_points +$(($(refused_points) - BASE_POINTS))
  refused (restart window) log_records +${HELD_LOGS}, metric_points +${HELD_POINTS}
  agent evidence           'resolved workload trust domain from SPIRE' present; no 'context canceled' ERROR; no CrashLoopBackOff
  telemetry recovery       aether_agent_storage_pods{node="${NODE}"} fresh again

  #662's startup decoupling (#668) holds: the agent started, attested and served
  while the collector was refusing its exports.
EOF
}

main() {
	parse_args "$@"
	for c in kubectl jq curl; do command -v "$c" >/dev/null || die "missing required command: $c"; done
	trap cleanup EXIT INT TERM

	step "pre-flight"
	preflight_cluster
	preflight_no_soak
	preflight_agent
	start_port_forward
	preflight_signals
	preflight_baseline

	step "applying pressure"
	apply_job
	wait_for_shedding

	step "restarting the agent on ${NODE} under pressure"
	PRE_RESTART_LOGS=$(refused_logs)
	PRE_RESTART_POINTS=$(refused_points)
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
