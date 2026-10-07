#!/usr/bin/env bash
# Copies a sortie run's report and logs out of the cluster into the run
# directory, BEFORE anything is torn down (proposal 042). The discipline the k6
# saver enforced: the report is the only place the per-target verdict exists,
# and this repository has lost a soak's summary to an early teardown more than
# once. sortie-teardown.sh refuses to run until this has written `SAVED`.
#
#   sortie-save.sh --dir RUN_DIR [--wait]
#
# run.sh arms it detached with --wait at load start: it then polls the Job until
# it has finished (or until the plan's duration + 20 minutes has passed, when it
# saves what exists and says so). Without --wait it saves now, whatever state
# the run is in -- run it by hand mid-run for a snapshot of the logs.
#
# What it writes into RUN_DIR:
#   report.json        the sortie JSON report, read off the report PVC through a
#                      short-lived reader pod (`kubectl exec` is denied on
#                      talos-main, so the pod prints the file and exits)
#   sortie.log         the sortie pod's log: the readable summary and the
#                      per-backend --progress lines
#   job.json           the Job (its conditions say Complete or Failed, and when)
#   sortie-pod.json    the sortie pod (exit code, node)
#   engine-<pod>.log   each engine's log
#   engines-end.tsv    podIP, node, pod, restarts at save time; compare with
#                      engines.tsv from the start: the report names backends by IP
#   SAVED              written last, only when report.json parsed
#
# What it writes to the cluster: one pod, <release>-report-reader, mounting the
# report PVC read-only; deleted before it returns.
#
# Exit: 0 saved; 1 the run ended but the report could not be saved (the logs
# still are); 2 usage or a run directory without run.env.
set -uo pipefail

DIR=""
WAIT=0
READER_IMAGE="${SOAK_READER_IMAGE:-curlimages/curl:8.22.0}"
while [ $# -gt 0 ]; do
	case "$1" in
	--dir)
		DIR="${2:-}"
		shift 2 || exit 2
		;;
	--wait) WAIT=1 && shift ;;
	*)
		echo "usage: sortie-save.sh --dir RUN_DIR [--wait]" >&2
		exit 2
		;;
	esac
done
[ -n "$DIR" ] && [ -r "$DIR/run.env" ] || {
	echo "sortie-save.sh: --dir must name a run directory holding run.env (written by run.sh)" >&2
	exit 2
}
CTX="" NS="" RELEASE="" PVC="" JOB="" T_LOAD="" DURATION_S="" REPORT_FILE=""
# shellcheck disable=SC1091 # written by run.sh: KEY=value lines
. "$DIR/run.env"
[ -n "$CTX" ] && [ -n "$NS" ] && [ -n "$JOB" ] && [ -n "$REPORT_FILE" ] || {
	echo "sortie-save.sh: $DIR/run.env is incomplete (the load never started?)" >&2
	exit 2
}

k() { kubectl --context "$CTX" -n "$NS" "$@"; }
log() { echo "$(date -u +%FT%TZ) $*"; }

# job_state: complete | failed | active | gone
job_state() {
	local j
	j="$(k get job "$JOB" -o json 2>/dev/null)" || {
		echo gone
		return
	}
	printf '%s' "$j" | jq -r '
		if any(.status.conditions[]?; .type == "Complete" and .status == "True") then "complete"
		elif any(.status.conditions[]?; .type == "Failed" and .status == "True") then "failed"
		else "active" end'
}

STATE="$(job_state)"
if [ "$WAIT" = 1 ]; then
	DEADLINE=$((T_LOAD + DURATION_S + 1200))
	log "waiting for job/$JOB (plan ${DURATION_S}s; giving up at $(date -u -d "@$DEADLINE" +%FT%TZ))"
	while [ "$STATE" = active ] && [ "$(date +%s)" -lt "$DEADLINE" ]; do
		sleep 30
		STATE="$(job_state)"
	done
fi
log "job/$JOB state=$STATE"

POD="$(k get pods -l "job-name=$JOB" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
k get job "$JOB" -o json >"$DIR/job.json" 2>/dev/null
if [ -n "$POD" ]; then
	k get pod "$POD" -o json >"$DIR/sortie-pod.json" 2>/dev/null
	k logs "$POD" --tail=-1 >"$DIR/sortie.log" 2>"$DIR/sortie.log.err"
fi
k get pods -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=engine" -o json 2>/dev/null |
	jq -r '.items[] | [.status.podIP, .spec.nodeName, .metadata.name, ([.status.containerStatuses[]?.restartCount] | add // 0)] | @tsv' |
	sort >"$DIR/engines-end.tsv"
while IFS=$'\t' read -r _ _ pod _; do
	[ -n "$pod" ] && k logs "$pod" --tail=-1 >"$DIR/engine-$pod.log" 2>/dev/null
done <"$DIR/engines-end.tsv"
log "saved sortie.log ($(wc -l <"$DIR/sortie.log" 2>/dev/null || echo 0) lines), job.json, $(wc -l <"$DIR/engines-end.tsv" | tr -d ' ') engine log(s)"

if [ "$STATE" = active ]; then
	log "SORTIE_SAVE_INCOMPLETE the run has not finished: logs saved, no report yet (run sortie-save.sh --dir $DIR again when it has)"
	exit 1
fi

# The report, off the PVC. The reader lands on the PV's node by the volume's
# own node affinity; it is not mesh-managed.
READER="$RELEASE-report-reader"
k delete pod "$READER" --ignore-not-found --wait=true >/dev/null 2>&1
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $READER
  labels:
    app.kubernetes.io/part-of: aether-soak
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    fsGroup: 65532
    seccompProfile: {type: RuntimeDefault}
  tolerations: [{operator: Exists}]
  containers:
    - name: reader
      image: $READER_IMAGE
      command: ["cat", "/report/$(basename "$REPORT_FILE")"]
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: {drop: ["ALL"]}
      volumeMounts:
        - {name: report, mountPath: /report, readOnly: true}
  volumes:
    - name: report
      persistentVolumeClaim: {claimName: $PVC, readOnly: true}
EOF
RPHASE=""
for _ in $(seq 1 90); do
	RPHASE="$(k get pod "$READER" -o jsonpath='{.status.phase}' 2>/dev/null)"
	case "$RPHASE" in Succeeded | Failed) break ;; esac
	sleep 2
done
k logs "$READER" >"$DIR/report.json.tmp" 2>"$DIR/report.json.err"
k delete pod "$READER" --ignore-not-found --wait=false >/dev/null 2>&1
if [ "$RPHASE" = Succeeded ] && jq -e '.executions | type == "array"' "$DIR/report.json.tmp" >/dev/null 2>&1; then
	mv "$DIR/report.json.tmp" "$DIR/report.json"
	rm -f "$DIR/report.json.err"
	date -u +%FT%TZ >"$DIR/SAVED"
	log "SORTIE_SAVED dir=$DIR job=$STATE executions=$(jq '.executions | length' "$DIR/report.json") pass=$(jq -r '.pass' "$DIR/report.json")"
	exit 0
fi
log "SORTIE_SAVE_FAILED the reader pod ended '$RPHASE' and $DIR/report.json.tmp is not a sortie report ($(head -c 200 "$DIR/report.json.err" 2>/dev/null | tr '\n' ' ')); the logs above ARE saved, the report is still on pvc/$PVC at $REPORT_FILE -- do not tear down with --purge"
exit 1
