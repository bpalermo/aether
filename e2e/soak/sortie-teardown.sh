#!/usr/bin/env bash
# Removes a sortie run from the cluster (proposal 042): the Helm release -- the
# engine DaemonSet, its headless Service, the Job and its pod, the plan
# ConfigMap, the ServiceAccount.
#
#   sortie-teardown.sh --dir RUN_DIR [--force] [--purge]
#
# It REFUSES unless sortie-save.sh has written RUN_DIR/SAVED: uninstalling the
# release deletes the sortie pod, and with it the only copy of the readable
# summary. --force tears down anyway (a run that never produced a report, a
# kickoff that aborted half-way); say why in the grade.
#
# Kept on purpose:
#   pvc/sortie-soak-reports   every run's JSON report (one small file each), so a
#                             report survives even a lost run directory. --purge
#                             deletes it.
#   priorityclass/aether-soak-loader   cluster-scoped, shared with k6-runner.yaml.
#
# It never stops the churn driver or the restart watchdog: those are
# workstation processes (see "Stopping the churn driver" in README.md).
set -uo pipefail

DIR=""
FORCE=0
PURGE=0
while [ $# -gt 0 ]; do
	case "$1" in
	--dir)
		DIR="${2:-}"
		shift 2 || exit 2
		;;
	--force) FORCE=1 && shift ;;
	--purge) PURGE=1 && shift ;;
	*)
		echo "usage: sortie-teardown.sh --dir RUN_DIR [--force] [--purge]" >&2
		exit 2
		;;
	esac
done
[ -n "$DIR" ] && [ -r "$DIR/run.env" ] || {
	echo "sortie-teardown.sh: --dir must name a run directory holding run.env (written by run.sh)" >&2
	exit 2
}
CTX="" NS="" RELEASE="" PVC="" JOB=""
# shellcheck disable=SC1091 # written by run.sh: KEY=value lines
. "$DIR/run.env"
[ -n "$CTX" ] && [ -n "$NS" ] && [ -n "$RELEASE" ] || {
	echo "sortie-teardown.sh: $DIR/run.env is incomplete" >&2
	exit 2
}
log() { echo "$(date -u +%FT%TZ) $*"; }

if [ ! -e "$DIR/SAVED" ] && [ "$FORCE" = 0 ]; then
	log "REFUSED: $DIR/SAVED does not exist -- the report has not been saved. Run: sortie-save.sh --dir $DIR   (or --force to tear down without it)"
	exit 1
fi
if [ -n "$JOB" ] && [ "$FORCE" = 0 ]; then
	active="$(kubectl --context "$CTX" -n "$NS" get job "$JOB" -o jsonpath='{.status.active}' 2>/dev/null)"
	if [ "${active:-0}" != 0 ]; then
		log "REFUSED: job/$JOB is still running (--force cancels it: sortie stops its engines' executions on SIGTERM and the run is reported cancelled, not evaluated)"
		exit 1
	fi
fi

if helm --kube-context "$CTX" -n "$NS" status "$RELEASE" >/dev/null 2>&1; then
	helm --kube-context "$CTX" -n "$NS" uninstall "$RELEASE" --wait --timeout 3m >/dev/null 2>&1 ||
		{
			log "helm uninstall $RELEASE failed"
			exit 1
		}
	log "uninstalled release $RELEASE from $NS (engines, Job, plan)"
else
	log "release $RELEASE is not installed in $NS; nothing to uninstall"
fi
kubectl --context "$CTX" -n "$NS" delete pod "$RELEASE-report-reader" --ignore-not-found >/dev/null 2>&1
if [ "$PURGE" = 1 ]; then
	kubectl --context "$CTX" -n "$NS" delete pvc "$PVC" --ignore-not-found >/dev/null 2>&1 && log "deleted pvc/$PVC (--purge)"
else
	log "kept pvc/$PVC (the JSON reports; --purge deletes it)"
fi
log "SORTIE_TEARDOWN_DONE"
