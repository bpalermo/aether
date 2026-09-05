#!/usr/bin/env bash
# 8-hour soak churn driver: 31 rolling restarts (29 schedule entries; the TRIPLE
# fires three at once), including three aether-mesh-dns DaemonSet rolls (surge +
# SO_REUSEPORT handoff) and one CONCURRENT triple (agent + proxy + svc at the same
# instant) as the stress peak -- followed by a deliberate 90-minute NO-ROLL WINDOW
# and a demand-set SHRINK.
#
# Run detached -- it sleeps between rolls for ~7h32m:
#   nohup setsid bash e2e/soak/churn.sh "rev192/0.92.0" >/dev/null 2>&1 &
#
# Progress is appended to $LOG with UTC timestamps; grep ROLLED to count.
#
# ---------------------------------------------------------------- the schedule
#
#   T0+   what                       why
#   ----  -------------------------  --------------------------------------------
#   12    svc-1        24  svc-2
#   36    proxy  *     48  svc-3
#   60    mesh-dns     72  agent      the ONLY scheduled agent roll besides TRIPLE
#   84    svc-5        96  proxy  *
#   108   svc-1        120 edge
#   132   svc-2        144 mesh-dns
#   156   svc-3        168 svc-4
#   180   svc-5        192 svc-1
#   204   edge         216 proxy  *
#   228   svc-2        240 svc-4
#   252   proxy  *     264 svc-3
#   276   svc-1
#   300   TRIPLE *     agent + proxy + svc-3 concurrently -- the stress peak, and
#                      the LAST agent roll of the run (see the no-roll window)
#   312   mesh-dns     324 svc-4
#   336   proxy  *     348 svc-2
#   360   svc-1        the last roll of any kind
#   ----  -------------------------  --------------------------------------------
#   360   NO-ROLL WINDOW BEGIN       nothing is rolled for 90 minutes
#   450   NO-ROLL WINDOW END
#   450   SHRINK                     svc-5 -> 0 replicas for 90s, then restored
#   ~452  churn driver complete      (k6 runs 7h40m, so both land under load)
#
#   * = 30 minutes later an age-matched proxy RSS sample is taken in the
#       background (sample-proxy-rss.sh --at-age 1800), for #628.
#
# ------------------------------------------------ why the window and the shrink
#
# #682: the node agent's demand-scoped dependency set holds each observed upstream
# for 1h, and rolling aether-agent rebuilds that set with a fresh TTL. The old
# schedule rolled the agent at T0+90m and again at T0+300m, so the TTL could never
# expire inside a soak -- the harness was STRUCTURALLY BLIND to the whole
# TTL-expiry -> ODCDS-stall class for its entire history, and the defect only ever
# surfaced in the quiet hours between runs. The no-roll window sits after the last
# agent roll and lasts longer than the TTL, so the expiry now happens under load,
# inside the graded window, on the nodes with no local replica of the upstream.
#
# The SHRINK is the cheap second trigger the 2026-09-05 grading found: ANY demand-set
# shrink -- not the 1h TTL specifically -- drops the cluster on a node with no local
# replica and exposes the ODCDS stall, in seconds instead of an hour. svc-5 is the
# target on purpose: it is the one service k6 declares as an upstream but never
# actually drives, so bouncing it cannot pollute the k6 error rate or the prober SLI.
#
# Both steps are opt-out (default ON):  SOAK_NO_ROLL_WINDOW=0   SOAK_SHRINK=0
set -uo pipefail

BUILD_LABEL="${1:-unspecified-build}"
LOG="${SOAK_CHURN_LOG:-/tmp/soak-churn.log}"
T0=$(date +%s)

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SAMPLER="${SOAK_PROXY_RSS_SAMPLER:-$HERE/sample-proxy-rss.sh}"

# Age-matched proxy RSS sampling (#628): sleep 28 minutes after a proxy roll, then
# let the sampler poll the last couple of minutes until the youngest proxy pod is
# exactly 30 minutes old. Without the age match, a "higher" node is usually just an
# older incarnation.
PROXY_RSS_AGE="${SOAK_PROXY_RSS_AGE:-1800}"
PROXY_RSS_DELAY="${SOAK_PROXY_RSS_DELAY:-1680}"

# No-roll window (#682). Begins when the last scheduled roll returns.
NO_ROLL_END_MIN="${SOAK_NO_ROLL_END_MIN:-450}"

# Demand-set shrink (#682).
SHRINK_NS="${SOAK_SHRINK_NS:-aether-test}"
SHRINK_TARGET="${SOAK_SHRINK_TARGET:-deployment/svc-5}"
SHRINK_SECONDS="${SOAK_SHRINK_SECONDS:-90}"
SHRINK_PREV=""

# Start a FRESH log, archiving any previous run alongside it. Without this the driver
# appends to the last soak's file, and `grep -c ROLLED` -- which teardown uses to confirm
# 31 rolls -- silently double-counts, so a run looks complete when it is not.
if [ -s "$LOG" ]; then
	mv -f "$LOG" "$LOG.$(date -u +%Y%m%dT%H%M%SZ).prev"
fi
: >"$LOG"

log() { echo "$(date -u +%FT%TZ) $*" >>"$LOG"; }

# Never leave the shrink target scaled to zero, even if the driver is killed mid-shrink.
restore_shrink() {
	if [ -n "$SHRINK_PREV" ]; then
		local prev="$SHRINK_PREV"
		SHRINK_PREV=""
		if kubectl -n "$SHRINK_NS" scale "$SHRINK_TARGET" --replicas="$prev" >>"$LOG" 2>&1; then
			log "SHRINK done $SHRINK_NS/$SHRINK_TARGET restored to replicas=$prev"
		else
			log "SHRINK FAILED to restore $SHRINK_NS/$SHRINK_TARGET to replicas=$prev -- RESTORE BY HAND"
		fi
	fi
}
trap restore_shrink EXIT INT TERM

# Sleep until T0 + $1 minutes.
waituntil() {
	local target now delta
	target=$((T0 + $1 * 60))
	now=$(date +%s)
	delta=$((target - now))
	if [ "$delta" -gt 0 ]; then sleep "$delta"; fi
}

roll() {
	if kubectl rollout restart "$2" -n "$1" >>"$LOG" 2>&1; then
		log "ROLLED $1/$2"
	else
		log "FAILED $1/$2"
	fi
}

# Queue an age-matched proxy RSS sample 30 minutes from now, detached, so it can
# never delay the schedule.
schedule_rss_sample() {
	if [ "${SOAK_PROXY_RSS:-1}" = "0" ]; then return; fi
	if [ ! -r "$SAMPLER" ]; then
		log "RSS SAMPLE skipped: no sampler at $SAMPLER"
		return
	fi
	(
		sleep "$PROXY_RSS_DELAY"
		bash "$SAMPLER" --at-age "$PROXY_RSS_AGE"
	) >>"$LOG" 2>&1 &
	log "RSS SAMPLE queued for T+${PROXY_RSS_AGE}s after this aether-proxy roll (#628)"
}

roll_proxy() {
	roll aether-system daemonset/aether-proxy
	schedule_rss_sample
}

shrink() {
	local prev
	prev=$(kubectl -n "$SHRINK_NS" get "$SHRINK_TARGET" -o jsonpath='{.spec.replicas}' 2>>"$LOG")
	if ! [[ "$prev" =~ ^[0-9]+$ ]] || [ "$prev" -eq 0 ]; then
		log "SHRINK skipped: cannot read a non-zero .spec.replicas from $SHRINK_NS/$SHRINK_TARGET (got '${prev}')"
		return
	fi
	log "SHRINK begin $SHRINK_NS/$SHRINK_TARGET replicas=$prev -> 0 for ${SHRINK_SECONDS}s (demand-set shrink, #682)"
	if ! kubectl -n "$SHRINK_NS" scale "$SHRINK_TARGET" --replicas=0 >>"$LOG" 2>&1; then
		log "SHRINK FAILED to scale $SHRINK_NS/$SHRINK_TARGET down; leaving it at $prev"
		return
	fi
	SHRINK_PREV="$prev"
	sleep "$SHRINK_SECONDS"
	restore_shrink
}

log "churn driver start T0=$(date -u +%FT%TZ) build=$BUILD_LABEL"

# "<offset-minutes> <kind> [name]"
SCHED=(
	"12 svc svc-1" "24 svc svc-2" "36 proxy" "48 svc svc-3" "60 meshdns"
	"72 agent" "84 svc svc-5" "96 proxy" "108 svc svc-1" "120 edge"
	"132 svc svc-2" "144 meshdns" "156 svc svc-3" "168 svc svc-4" "180 svc svc-5"
	"192 svc svc-1" "204 edge" "216 proxy" "228 svc svc-2" "240 svc svc-4"
	"252 proxy" "264 svc svc-3" "276 svc svc-1" "300 TRIPLE" "312 meshdns"
	"324 svc svc-4" "336 proxy" "348 svc svc-2" "360 svc svc-1"
)

for entry in "${SCHED[@]}"; do
	read -r off kind name <<<"$entry"
	waituntil "$off"
	case "$kind" in
	svc) roll aether-test "deployment/$name" ;;
	proxy) roll_proxy ;;
	agent) roll aether-system daemonset/aether-agent ;;
	meshdns) roll aether-system daemonset/aether-mesh-dns ;;
	edge) roll aether-ingress deployment/aether-edge ;;
	TRIPLE)
		log "CONCURRENT triple begin"
		# Wait on these three PIDs specifically: a bare `wait` would also block on
		# any RSS sampler still parked in its --at-age poll, delaying the log.
		roll aether-system daemonset/aether-agent &
		t_agent=$!
		roll aether-system daemonset/aether-proxy &
		t_proxy=$!
		roll aether-test deployment/svc-3 &
		t_svc=$!
		wait "$t_agent" "$t_proxy" "$t_svc"
		log "CONCURRENT triple done"
		# The TRIPLE-fresh proxy incarnation is #628's worst observed data point
		# (316Mi in 9 minutes on 08-03), so age-match it too.
		schedule_rss_sample
		;;
	*) log "UNKNOWN kind $kind" ;;
	esac
done

# The no-roll window: the 1h demand-scoped TTL expires under load, with no agent or
# proxy roll to reset it. See the header. Nothing is rolled until NO_ROLL_END_MIN.
if [ "${SOAK_NO_ROLL_WINDOW:-1}" = "0" ]; then
	log "no-roll window SKIPPED (SOAK_NO_ROLL_WINDOW=0)"
else
	log "no-roll window begin T0+360m; nothing is rolled until T0+${NO_ROLL_END_MIN}m (#682 TTL expiry under load)"
	waituntil "$NO_ROLL_END_MIN"
	log "no-roll window end T0+${NO_ROLL_END_MIN}m"
fi

if [ "${SOAK_SHRINK:-1}" = "0" ]; then
	log "SHRINK skipped (SOAK_SHRINK=0)"
else
	shrink
fi

# 29 schedule entries, but the TRIPLE logs its three concurrent rolls separately, so
# `grep -c ROLLED` is 31. (The old footer said 30: it forgot the TRIPLE's proxy. The
# set of rolls is unchanged -- only the tally is now honest.)
log "churn driver complete (31 rolls: 6 proxy incl triple, 2 agent incl triple, 2 edge, 3 meshdns, 18 svc incl triple)"
