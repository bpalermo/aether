#!/usr/bin/env bash
# Soak restart watchdog (#1242): samples every container's restartCount in the
# soak's namespaces, from a baseline taken at start, and writes one unambiguous
# status line per sample -- so an OOMKill or crash is visible DURING the run and
# the log alone can prove "0 new restarts" at the end.
#
# Start it right after the churn driver (T0), detached, from the MAIN session:
#   bash "$PWD/e2e/soak/restart-watch.sh" --context talos-main --preflight &&
#     nohup setsid bash "$PWD/e2e/soak/restart-watch.sh" --context talos-main >/dev/null 2>&1 &
#
# It runs for --duration (default 8h = the graded T0..T0+8h window) and ends
# with a SUMMARY line. Do not run it past ~T0+8h26m: the k6 runners exit by
# themselves there and their container restarts once, by design.
#
# ---------------------------------------------------------------- the log
#
# Every line starts with a UTC timestamp. The lines a grader reads:
#
#   BASELINE count=<n> ...           restarts that existed BEFORE the run, one
#   BASELINE <ns>/<pod>/<ctr> ...    line each; reported, never counted as new
#   ok count=0 sample=<k> ...        every namespace listed, no new restart since
#                                    the baseline. The ONLY line that means "none".
#   RESTARTS count=<n> sample=<k>    n containers restarted since the baseline
#   RESTART <ns>/<pod>/<ctr> restarts=<N> new=<M> lastReason=<OOMKilled|Error|...>
#     exitCode=<code> finishedAt=<ts> state=<running|waiting:CrashLoopBackOff|...>
#     seen=<live|gone>               one per container; cumulative over the run,
#                                    so a restarted pod that a later roll deletes
#                                    stays listed (seen=gone) instead of vanishing
#   ERROR kubectl failed: ns=<ns> ...  the API call (or its parse) failed for that
#                                    namespace. That sample proves NOTHING for it;
#                                    no `ok` line is written for that sample.
#   SUMMARY restart-watch verdict=<PASS|FAIL|UNPROVEN> new_restarts=<n> ...
#                                    the end-of-run grade line, then one
#   SUMMARY RESTART <ns>/<pod>/<ctr> ...   line per restarted container.
#
# verdict=PASS     no new restart, no failed sample, ran to the end.
# verdict=FAIL     at least one container restarted after the baseline.
# verdict=UNPROVEN no new restart seen, but some sample failed (error_samples>0),
#                  the run was cut short (ended=TERM|INT), or no sample followed
#                  the baseline. A restart in the gap may have been missed: a
#                  restartCount is cumulative for the life of a pod, so only a
#                  pod that restarted AND was deleted inside the gap is lost.
#
# Pods are keyed by UID, so a pod created mid-run (every roll makes new ones)
# starts from 0 and any restart it takes is new. Init containers are included.
#
# ---------------------------------------------------------------- options
#
#   --context NAME     kubeconfig context for every call (also SOAK_CONTEXT;
#                      default talos-main). Never the current-context (#951).
#   --namespaces "a b" (SOAK_RESTART_NAMESPACES) default "aether-system
#                      aether-ingress aether-test" -- aether-test holds the
#                      prober DaemonSet, the k6 runners and the soak workloads.
#   --interval S       seconds between samples (SOAK_RESTART_INTERVAL, 120). A
#                      pod that restarts AND is rolled away inside one interval is
#                      missed, so keep it well under the 12-minute roll spacing.
#   --duration D       total run time, Ns/Nm/Nh (SOAK_RESTART_DURATION, 8h).
#   --samples N        stop after N samples past the baseline (0 = no limit).
#   --log PATH         (SOAK_RESTART_LOG, /tmp/soak-restart-watch.log); `-` is
#                      stdout. A previous log is archived as <log>.<ts>.prev.
#   --preflight        take the baseline only, print it to stderr, exit 0 (or 2
#                      if it cannot be taken). Writes no log.
#
# Needs kubectl and jq. RESTART_WATCH_KUBECTL replaces kubectl (the test hook).
set -uo pipefail

CTX="${SOAK_CONTEXT:-talos-main}"
NAMESPACES="${SOAK_RESTART_NAMESPACES:-aether-system aether-ingress aether-test}"
INTERVAL="${SOAK_RESTART_INTERVAL:-120}"
DURATION="${SOAK_RESTART_DURATION:-8h}"
MAX_SAMPLES=0
LOG="${SOAK_RESTART_LOG:-/tmp/soak-restart-watch.log}"
KUBECTL="${RESTART_WATCH_KUBECTL:-kubectl}"
BASELINE_TRIES="${SOAK_RESTART_BASELINE_TRIES:-5}"
BASELINE_RETRY_DELAY="${SOAK_RESTART_BASELINE_RETRY_DELAY:-10}"
PREFLIGHT_ONLY=0

usage="usage: restart-watch.sh [--context NAME] [--namespaces \"a b\"] [--interval S] [--duration D] [--samples N] [--log PATH|-] [--preflight]"
while [ $# -gt 0 ]; do
	case "$1" in
	--context | --namespaces | --interval | --duration | --samples | --log)
		if [ $# -lt 2 ]; then
			echo "restart-watch.sh: $1 needs a value ($usage)" >&2
			exit 2
		fi
		case "$1" in
		--context) CTX="$2" ;;
		--namespaces) NAMESPACES="$2" ;;
		--interval) INTERVAL="$2" ;;
		--duration) DURATION="$2" ;;
		--samples) MAX_SAMPLES="$2" ;;
		--log) LOG="$2" ;;
		esac
		shift 2
		;;
	--preflight)
		PREFLIGHT_ONLY=1
		shift
		;;
	*)
		echo "restart-watch.sh: unknown argument '$1' ($usage)" >&2
		exit 2
		;;
	esac
done

if [ -z "$CTX" ]; then
	echo "restart-watch.sh: --context needs a kubeconfig context name" >&2
	exit 2
fi
if ! [[ "$INTERVAL" =~ ^[0-9]+$ ]] || ! [[ "$MAX_SAMPLES" =~ ^[0-9]+$ ]]; then
	echo "restart-watch.sh: --interval and --samples take whole numbers" >&2
	exit 2
fi
if [[ "$DURATION" =~ ^([0-9]+)([hms]?)$ ]]; then
	case "${BASH_REMATCH[2]}" in
	h) DURATION_S=$((10#${BASH_REMATCH[1]} * 3600)) ;;
	m) DURATION_S=$((10#${BASH_REMATCH[1]} * 60)) ;;
	*) DURATION_S=$((10#${BASH_REMATCH[1]})) ;;
	esac
else
	echo "restart-watch.sh: --duration takes Ns, Nm or Nh (got '$DURATION')" >&2
	exit 2
fi
read -r -a NS_LIST <<<"$NAMESPACES"
if [ "${#NS_LIST[@]}" -eq 0 ]; then
	echo "restart-watch.sh: --namespaces is empty" >&2
	exit 2
fi
if ! command -v jq >/dev/null 2>&1; then
	echo "restart-watch.sh: jq not found on PATH" >&2
	exit 2
fi

k() { "$KUBECTL" --context "$CTX" "$@"; }

# Lines go to stderr during --preflight, else to $LOG (or stdout for `-`).
out() {
	if [ "$PREFLIGHT_ONLY" = "1" ]; then
		echo "$(date -u +%FT%TZ) $*" >&2
	elif [ "$LOG" = "-" ]; then
		echo "$(date -u +%FT%TZ) $*"
	else
		echo "$(date -u +%FT%TZ) $*" >>"$LOG"
	fi
}

# One TSV row per container (init containers too) of a `get pods -o json` List:
# ns, pod, uid, container, restartCount, lastReason, exitCode, finishedAt, state.
# Every field is non-empty ("-" when absent): `read` with a tab IFS collapses
# empty fields. A document that is not a List of pods is a parse error.
# shellcheck disable=SC2016 # $p is a jq variable, not a shell one
JQ_ROWS='
if (.items | type) != "array" then error("not a list of pods (no .items array)") else .items[] end
| . as $p
| ((.status.initContainerStatuses // []) + (.status.containerStatuses // []))[]
| [ ($p.metadata.namespace // "-"), ($p.metadata.name // "-"), ($p.metadata.uid // $p.metadata.name // "-"),
    (.name // "-"), ((.restartCount // 0) | tostring),
    (.lastState.terminated.reason // "-"),
    ((.lastState.terminated.exitCode // "-") | tostring),
    (.lastState.terminated.finishedAt // "-"),
    (if .state.waiting then "waiting:" + (.state.waiting.reason // "-")
     elif .state.terminated then "terminated:" + (.state.terminated.reason // "-")
     elif .state.running then "running" else "unknown" end) ]
| map(if . == "" then "-" else . end)
| @tsv'

declare -A BASE=()  # uid/container -> restartCount at baseline
declare -A NEW=()   # uid/container -> restarts since baseline (latest seen)
declare -A DESC=()  # uid/container -> "ns/pod/ctr restarts=.. new=.. lastReason=.. ..."
declare -A LIVE=()  # uid/container -> 1 if present in the latest sample of its namespace
declare -A NS_OF=() # uid/container -> namespace
BASELINE_COUNT=0
SAMPLES=0
ERROR_SAMPLES=0
STARTED=""
FINISHED=0

# Collapse a kubectl error to one bounded line.
one_line() { tr '\n\t' '  ' | tr -s ' ' | sed 's/ *$//' | cut -c1-300; }

# Fetch one namespace into $ROWS. Returns 1 and sets $ERR on any failure.
fetch_ns() {
	local ns="$1" json errf rc
	errf=$(mktemp)
	json=$(k --request-timeout=20s -n "$ns" get pods -o json 2>"$errf")
	rc=$?
	if [ "$rc" -ne 0 ]; then
		ERR="exit=$rc $(one_line <"$errf")"
		rm -f "$errf"
		return 1
	fi
	if ! ROWS=$(jq -r "$JQ_ROWS" <<<"$json" 2>"$errf"); then
		ERR="unparseable output: $(one_line <"$errf")"
		rm -f "$errf"
		return 1
	fi
	rm -f "$errf"
}

# The baseline: every namespace must answer, else nothing is recorded.
take_baseline() {
	local ns all="" ns_rows lines=() n pod uid ctr rc reason code fin state
	for ns in "${NS_LIST[@]}"; do
		if ! fetch_ns "$ns"; then
			out "ERROR kubectl failed: ns=$ns baseline $ERR"
			return 1
		fi
		ns_rows="$ROWS"
		if [ -n "$ns_rows" ]; then all+="$ns_rows"$'\n'; fi
	done
	while IFS=$'\t' read -r n pod uid ctr rc reason code fin state; do
		[ -n "${n:-}" ] || continue
		BASE["$uid/$ctr"]=$rc
		if [ "$rc" -gt 0 ]; then
			lines+=("BASELINE $n/$pod/$ctr restarts=$rc lastReason=$reason exitCode=$code finishedAt=$fin state=$state")
		fi
	done <<<"$all"
	BASELINE_COUNT=${#lines[@]}
	out "BASELINE count=$BASELINE_COUNT context=$CTX namespaces=${NAMESPACES// /,} (pre-existing restarts: reported, never counted as new)"
	local l
	for l in "${lines[@]}"; do out "$l"; done
}

# One sample past the baseline.
take_sample() {
	local ns key failed=0 n pod uid ctr rc reason code fin state new
	SAMPLES=$((SAMPLES + 1))
	for ns in "${NS_LIST[@]}"; do
		if ! fetch_ns "$ns"; then
			out "ERROR kubectl failed: ns=$ns sample=$SAMPLES $ERR"
			failed=1
			continue
		fi
		# This namespace answered: whatever it no longer lists is gone.
		for key in "${!NS_OF[@]}"; do
			if [ "${NS_OF[$key]}" = "$ns" ]; then LIVE["$key"]=0; fi
		done
		while IFS=$'\t' read -r n pod uid ctr rc reason code fin state; do
			[ -n "${n:-}" ] || continue
			key="$uid/$ctr"
			new=$((rc - ${BASE[$key]:-0}))
			if [ "$new" -gt 0 ]; then
				NEW["$key"]=$new
				NS_OF["$key"]=$ns
				LIVE["$key"]=1
				DESC["$key"]="$n/$pod/$ctr restarts=$rc new=$new lastReason=$reason exitCode=$code finishedAt=$fin state=$state"
			fi
		done <<<"$ROWS"
	done
	if [ "$failed" = "1" ]; then ERROR_SAMPLES=$((ERROR_SAMPLES + 1)); fi
	if [ "${#NEW[@]}" -eq 0 ]; then
		if [ "$failed" = "0" ]; then
			out "ok count=0 sample=$SAMPLES baseline=$BASELINE_COUNT"
		fi
		return
	fi
	out "RESTARTS count=${#NEW[@]} sample=$SAMPLES baseline=$BASELINE_COUNT"
	for key in $(sorted_keys); do
		out "RESTART ${DESC[$key]} seen=$(seen "$key")"
	done
}

# The restarted containers' keys, in ns/pod/container order.
sorted_keys() {
	local key
	for key in "${!NEW[@]}"; do printf '%s\t%s\n' "${DESC[$key]}" "$key"; done | sort | cut -f2
}

seen() { if [ "${LIVE[$1]:-1}" = "1" ]; then echo live; else echo gone; fi; }

finish() {
	local ended="$1" verdict total=0 key
	if [ "$FINISHED" = "1" ]; then return; fi
	FINISHED=1
	for key in "${!NEW[@]}"; do total=$((total + ${NEW[$key]})); done
	if [ "${#NEW[@]}" -gt 0 ]; then
		verdict=FAIL
	elif [ "$ERROR_SAMPLES" -gt 0 ] || [ "$ended" != "complete" ] || [ "$SAMPLES" -eq 0 ]; then
		verdict=UNPROVEN
	else
		verdict=PASS
	fi
	out "SUMMARY restart-watch verdict=$verdict new_restarts=$total containers=${#NEW[@]} samples=$SAMPLES error_samples=$ERROR_SAMPLES baseline=$BASELINE_COUNT ended=$ended from=$STARTED to=$(date -u +%FT%TZ) context=$CTX namespaces=${NAMESPACES// /,}"
	for key in $(sorted_keys); do
		out "SUMMARY RESTART ${DESC[$key]} seen=$(seen "$key")"
	done
}

# Baseline, with a few retries: a watchdog that cannot see the cluster at T0
# has nothing to compare against, so it refuses to run rather than log "ok".
baseline_or_die() {
	local try=1
	while ! take_baseline; do
		if [ "$try" -ge "$BASELINE_TRIES" ]; then
			out "SUMMARY restart-watch verdict=UNPROVEN reason=no-baseline tries=$try context=$CTX namespaces=${NAMESPACES// /,}"
			exit 2
		fi
		try=$((try + 1))
		sleep "$BASELINE_RETRY_DELAY"
	done
}

if [ "$PREFLIGHT_ONLY" = "1" ]; then
	baseline_or_die
	echo "restart-watch.sh: pre-flight OK on context '$CTX' (${#NS_LIST[@]} namespaces listed; $BASELINE_COUNT pre-existing restarted containers)" >&2
	exit 0
fi

if [ "$LOG" != "-" ]; then
	if [ -s "$LOG" ]; then mv -f "$LOG" "$LOG.$(date -u +%Y%m%dT%H%M%SZ).prev"; fi
	: >"$LOG"
fi

STARTED=$(date -u +%FT%TZ)
START_S=$(date +%s)
END_S=$((START_S + DURATION_S))
out "restart-watch start context=$CTX namespaces=${NAMESPACES// /,} interval=${INTERVAL}s duration=${DURATION_S}s"
baseline_or_die

# `sleep & wait` so TERM/INT are handled at once, not after the sleep.
SLEEP_PID=""
stop() {
	if [ -n "$SLEEP_PID" ]; then kill "$SLEEP_PID" 2>/dev/null; fi
	finish "$1"
}
trap 'finish complete' EXIT
trap 'stop TERM; exit 143' TERM
trap 'stop INT; exit 130' INT

next=$START_S
while :; do
	next=$((next + INTERVAL))
	now=$(date +%s)
	target=$next
	if [ "$target" -gt "$END_S" ]; then target=$END_S; fi
	if [ "$target" -gt "$now" ]; then
		sleep $((target - now)) &
		SLEEP_PID=$!
		wait "$SLEEP_PID"
		SLEEP_PID=""
	fi
	take_sample
	if [ "$(date +%s)" -ge "$END_S" ]; then break; fi
	if [ "$MAX_SAMPLES" -gt 0 ] && [ "$SAMPLES" -ge "$MAX_SAMPLES" ]; then break; fi
done
