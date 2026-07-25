#!/usr/bin/env bash
# 8-hour soak churn driver: 30 rolling restarts on a ~15-minute cadence, including
# three aether-mesh-dns DaemonSet rolls (surge + SO_REUSEPORT handoff) and one
# CONCURRENT triple (agent + proxy + svc at the same instant) as the stress peak.
#
# Run detached — it sleeps between rolls for ~7h15m:
#   bash e2e/soak/churn.sh "rev183/0.86.0" &
#
# Progress is appended to $LOG with UTC timestamps; grep ROLLED to count.
set -uo pipefail

BUILD_LABEL="${1:-unspecified-build}"
LOG="${SOAK_CHURN_LOG:-/tmp/soak-churn.log}"
T0=$(date +%s)

log() { echo "$(date -u +%FT%TZ) $*" >>"$LOG"; }

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

log "churn driver start T0=$(date -u +%FT%TZ) build=$BUILD_LABEL"

# "<offset-minutes> <kind> [name]"
SCHED=(
	"15 svc svc-1" "30 svc svc-2" "45 proxy" "60 svc svc-3" "75 meshdns"
	"90 agent" "105 svc svc-5" "120 proxy" "135 svc svc-1" "150 edge"
	"165 svc svc-2" "180 meshdns" "195 svc svc-3" "210 svc svc-4" "225 svc svc-5"
	"240 svc svc-1" "255 edge" "270 proxy" "285 svc svc-2" "300 TRIPLE"
	"315 svc svc-4" "330 proxy" "345 meshdns" "360 svc svc-1" "375 svc svc-2"
	"390 svc svc-3" "405 svc svc-4" "420 proxy" "435 svc svc-1"
)

for entry in "${SCHED[@]}"; do
	read -r off kind name <<<"$entry"
	waituntil "$off"
	case "$kind" in
	svc) roll aether-test "deployment/$name" ;;
	proxy) roll aether-system daemonset/aether-proxy ;;
	agent) roll aether-system daemonset/aether-agent ;;
	meshdns) roll aether-system daemonset/aether-mesh-dns ;;
	edge) roll aether-ingress deployment/aether-edge ;;
	TRIPLE)
		log "CONCURRENT triple begin"
		roll aether-system daemonset/aether-agent &
		roll aether-system daemonset/aether-proxy &
		roll aether-test deployment/svc-3 &
		wait
		log "CONCURRENT triple done"
		;;
	*) log "UNKNOWN kind $kind" ;;
	esac
done

log "churn driver complete (30 rolls: 5 proxy, 2 agent incl triple, 2 edge, 3 meshdns, 18 svc)"
