#!/usr/bin/env bash
# Copies a sortie run's report and logs out of the cluster into the run
# directory, BEFORE anything is torn down (proposal 042). The discipline the k6
# saver enforced: the report is the only place the per-target verdict exists,
# and this repository has lost a soak's summary to an early teardown more than
# once. sortie-teardown.sh refuses to run until this has written `SAVED`.
#
#   sortie-save.sh --dir RUN_DIR [--wait]
#   sortie-save.sh --times REPORT_OR_STREAM     # offline: print times.tsv, write nothing
#
# run.sh arms it detached with --wait as soon as the Job exists, before it looks
# at the Job's pod (#1387): it then polls the Job until
# it has finished (or until the plan's duration + 20 minutes has passed, when it
# saves what exists and says so). Without --wait it saves now, whatever state
# the run is in -- run it by hand mid-run for a snapshot of the logs.
#
# A Job still running five minutes past its planned end gets one SORTIE_OVERDUE
# line in the log. Since sortie 76f699b that is no longer the silent node: sortie
# gives up on a backend that has not answered 151 s past the planned end (the
# request timeout, a second of drain and two minutes of grace), waits up to 30 s
# more for the engine to answer the cancellation, and writes the report without
# it -- about three minutes late (181 s on kind, with the node still frozen).
# Five minutes means sortie itself is stuck (or frozen with its own node).
#
# What it writes into RUN_DIR:
#   report.json        the sortie JSON report, read off the report PVC through a
#                      short-lived reader pod (`kubectl exec` is denied on
#                      talos-main, so the pod prints the file and exits)
#   results.jsonl      the results stream, off the same PVC: one line per
#                      execution, written as each finished. Saved as it is on the
#                      PVC. It is this run's own file (run.sh names it by run
#                      tag: sortie appends and never truncates), and a run that
#                      died can leave a last line that does not parse; the saver
#                      counts the lines that do and the ones that do not, and
#                      sortie-gate.sh --stream skips the latter. Saved even when
#                      there is no report: it is then all the run recorded.
#   times.tsv          label, started_at, ended_at, elapsed_ms, backend, the
#                      backend's own started_at: when each execution was
#                      dispatched and had its last answer (sortie's clock) and
#                      when each engine's first worker started (the engine's
#                      clock). From the report, else from the stream. For
#                      lining a run up with the rolls in churn.log / rolls.log.
#   sortie.log         the sortie pod's log: the per-backend --progress lines,
#                      one PASS|FAIL|SKIP line per finished execution, and the
#                      readable summary
#   job.json           the Job (its conditions say Complete or Failed, and when)
#   sortie-pod.json    the sortie pod (exit code, node)
#   engine-<pod>.log   each engine's log
#   engines-end.tsv    podIP, node, pod, restarts at save time; compare with
#                      engines.tsv from the start: the report names backends by IP
#   SAVED              written last, only when report.json parsed
#
# What it writes to the cluster: one pod at a time, <release>-report-reader,
# mounting the report PVC read-only; deleted before it returns.
#
# Exit: 0 saved; 1 the run ended but the report could not be saved (the logs
# still are, and the results stream if it could be read); 2 usage or a run
# directory without run.env.
set -uo pipefail

# times_tsv FILE: the timestamps of a report (one JSON document) or of a results
# stream (JSON Lines), as TSV. A line of a stream that is not an execution is
# skipped; an execution with no backend result still gets a row.
times_tsv() {
	jq -Rrn '
		[inputs] as $raw
		| (try ($raw | join("\n") | fromjson) catch null) as $doc
		| (if ($doc | type) == "object" and ($doc.executions | type) == "array" then $doc.executions
		   else [$raw[] | select(test("[^[:space:]]")) | (try fromjson catch null) | select(type == "object" and (.label | type) == "string")] end)[]
		| . as $e
		| (if (($e.results // []) | length) > 0 then $e.results[] else {backend: "-"} end)
		| [$e.label, ($e.started_at // "-"), ($e.ended_at // "-"), ($e.elapsed_ms // 0),
		   .backend, (.started_at // "-"), (if $e.not_run // false then "not_run" elif $e.pass then "pass" else "fail" end)]
		| @tsv' "$1"
}

DIR=""
WAIT=0
READER_IMAGE="${SOAK_READER_IMAGE:-curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777}"
while [ $# -gt 0 ]; do
	case "$1" in
	--dir)
		DIR="${2:-}"
		shift 2 || exit 2
		;;
	--wait) WAIT=1 && shift ;;
	--times)
		[ -r "${2:-}" ] || {
			echo "sortie-save.sh: --times needs a readable report or results stream" >&2
			exit 2
		}
		times_tsv "$2"
		exit $?
		;;
	*)
		echo "usage: sortie-save.sh --dir RUN_DIR [--wait] | --times REPORT_OR_STREAM" >&2
		exit 2
		;;
	esac
done
[ -n "$DIR" ] && [ -r "$DIR/run.env" ] || {
	echo "sortie-save.sh: --dir must name a run directory holding run.env (written by run.sh)" >&2
	exit 2
}
CTX="" NS="" RELEASE="" PVC="" JOB="" T_JOB="" T_LOAD="" DURATION_S="" REPORT_FILE="" STREAM_FILE=""
# shellcheck disable=SC1091 # written by run.sh: KEY=value lines
. "$DIR/run.env"
# run.sh arms this saver when the Job exists, before the pod is running and so
# before T_LOAD is written: T_JOB, the moment the Job was found, stands in.
T_LOAD="${T_LOAD:-${T_JOB:-$(date +%s)}}"
[ -n "$CTX" ] && [ -n "$NS" ] && [ -n "$JOB" ] && [ -n "$REPORT_FILE" ] || {
	echo "sortie-save.sh: $DIR/run.env is incomplete (the load never started?)" >&2
	exit 2
}

# SORTIE_SAVE_KUBECTL: another kubectl (harness_test.sh passes a fake one).
k() { "${SORTIE_SAVE_KUBECTL:-kubectl}" --context "$CTX" -n "$NS" "$@"; }
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
	OVERDUE_SAID=0
	while [ "$STATE" = active ] && [ "$(date +%s)" -lt "$DEADLINE" ]; do
		sleep 30
		STATE="$(job_state)"
		# A run ends within seconds of its duration, or -- with requests still in
		# flight, or a backend that has gone silent -- within the 30 s request
		# timeout or the 181 s sortie takes over a silent backend (151 s of
		# deadline past the planned end, 30 s for the cancellation; sortie
		# 76f699b; before it, sortie waited for a silent node without limit and
		# this line was the only notice). A Job still running five minutes late
		# is past all of that. Say so once, while someone can look.
		if [ "$STATE" = active ] && [ "$OVERDUE_SAID" = 0 ] && [ "$(date +%s)" -gt $((T_LOAD + DURATION_S + 300)) ]; then
			OVERDUE_SAID=1
			log "SORTIE_OVERDUE job/$JOB is still running $(($(date +%s) - T_LOAD - DURATION_S))s past its planned end, which is past what sortie takes over a silent backend (about 181 s): sortie itself is stuck, or frozen with its node. Check: kubectl --context $CTX -n $NS logs job/$JOB --tail=20; kubectl --context $CTX get nodes; kubectl --context $CTX -n $NS get pods -l app.kubernetes.io/component=engine -o wide. What exists so far: the progress lines of the sortie pod's log, and the results stream on the PVC"
		fi
	done
fi
log "job/$JOB state=$STATE"

POD="$(k get pods -l "job-name=$JOB" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
k get job "$JOB" -o json >"$DIR/job.json" 2>/dev/null
if [ -n "$POD" ]; then
	k get pod "$POD" -o json >"$DIR/sortie-pod.json" 2>/dev/null
	k logs "$POD" --tail=-1 >"$DIR/sortie.log" 2>"$DIR/sortie.log.err"
else
	# Evicted with its node, or deleted: the log went with it. What the run
	# recorded is on the PVC (the results stream, and the report if it got
	# that far), which is read below.
	log "the sortie pod of job/$JOB is gone: no sortie.log (the progress lines and the summary are lost; the PVC is still read)"
fi
k get pods -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=engine" -o json 2>/dev/null |
	jq -r '.items[] | [.status.podIP, .spec.nodeName, .metadata.name, ([.status.containerStatuses[]?.restartCount] | add // 0)] | @tsv' |
	sort >"$DIR/engines-end.tsv"
while IFS=$'\t' read -r _ _ pod _; do
	[ -n "$pod" ] && k logs "$pod" --tail=-1 >"$DIR/engine-$pod.log" 2>/dev/null
done <"$DIR/engines-end.tsv"
log "saved sortie.log ($(if [ -r "$DIR/sortie.log" ]; then wc -l <"$DIR/sortie.log" | tr -d ' '; else echo 0; fi) lines), job.json, $(wc -l <"$DIR/engines-end.tsv" | tr -d ' ') engine log(s)"

if [ "$STATE" = active ]; then
	log "SORTIE_SAVE_INCOMPLETE the run has not finished: logs saved, no report yet (run sortie-save.sh --dir $DIR again when it has). sortie.log holds each target's cumulative counters per node up to its last progress line, and a PASS|FAIL|SKIP line for every execution that has finished"
	exit 1
fi

# reader_gone: delete the reader pod and make sure its name is free again. Every
# wait is bounded: the reader lands on the PV's node, and a pod on a node that
# does not answer is never confirmed deleted by its kubelet -- an unbounded
# `--wait` there would hold the saver for ever, before it has said anything.
# Returns 0 when no pod of that name is left.
READER="$RELEASE-report-reader"
reader_gone() {
	k delete pod "$READER" --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 && return 0
	k delete pod "$READER" --ignore-not-found --force --grace-period=0 --wait=true --timeout=30s >/dev/null 2>&1 && return 0
	! k get pod "$READER" >/dev/null 2>&1
}

# read_pvc_file BASENAME DEST: print one file of the report PVC through a reader
# pod and keep it as DEST.tmp, with what went wrong in DEST.err. The reader
# lands on the PV's node by the volume's own node affinity; it is not
# mesh-managed. Returns 0 only when the pod succeeded AND its whole output was
# downloaded: a `kubectl logs` that broke off half-way leaves what looks like a
# shorter file, and that must not replace a good copy.
read_pvc_file() {
	local file="$1" dest="$2" phase="" got=0
	RPHASE=""
	rm -f "$dest.tmp" "$dest.err"
	if ! reader_gone; then
		RPHASE="stuck"
		echo "an earlier pod/$READER could not be deleted within 90 s (is the node that holds pvc/$PVC answering?)" >"$dest.err"
		return 1
	fi
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
      command: ["cat", "/report/$file"]
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
	for _ in $(seq 1 90); do
		phase="$(k get pod "$READER" -o jsonpath='{.status.phase}' 2>/dev/null)"
		case "$phase" in Succeeded | Failed) break ;; esac
		sleep 2
	done
	RPHASE="${phase:-absent}"
	if k logs "$READER" >"$dest.tmp" 2>"$dest.err"; then
		got=1
	else
		echo "kubectl logs pod/$READER did not complete: the download is not whole" >>"$dest.err"
	fi
	# A reader that failed printed why (`cat: can't open ...`) where the file
	# would have been: that is the reason, not a download.
	if [ "$phase" != Succeeded ] && [ -s "$dest.tmp" ]; then
		echo "the reader said: $(head -c 200 "$dest.tmp" | tr '\n' ' ')" >>"$dest.err"
	fi
	reader_gone || log "pod/$READER could not be deleted within 90 s; sortie-teardown.sh removes it"
	[ "$phase" = Succeeded ] && [ "$got" = 1 ]
}

# The results stream first: when the report is missing it is the only record.
# Kept as it is on the PVC, whatever its last line looks like.
STREAM_OK=0 STREAM_N=0 STREAM_BAD=0 RPHASE=""
if [ -n "$STREAM_FILE" ]; then
	if read_pvc_file "$(basename "$STREAM_FILE")" "$DIR/results.jsonl"; then
		mv "$DIR/results.jsonl.tmp" "$DIR/results.jsonl"
		rm -f "$DIR/results.jsonl.err"
		read -r STREAM_N STREAM_BAD < <(jq -Rrn '
			[inputs | select(test("[^[:space:]]")) | (try fromjson catch null)] as $l
			| ($l | map(select(type == "object" and (.label | type) == "string")) | length) as $ok
			| "\($ok) \(($l | length) - $ok)"' "$DIR/results.jsonl" 2>/dev/null)
		STREAM_N="${STREAM_N:-0}" STREAM_BAD="${STREAM_BAD:-0}"
		[ "$STREAM_N" -gt 0 ] && STREAM_OK=1
		log "saved results.jsonl (the results stream): $STREAM_N execution(s)$([ "$STREAM_BAD" -gt 0 ] && echo ", $STREAM_BAD line(s) that do not parse -- an unfinished last line; sortie-gate.sh skips them")"
	else
		# Whatever came down is not the file. A copy saved earlier stays.
		rm -f "$DIR/results.jsonl.tmp"
		log "the results stream could not be read off pvc/$PVC (reader pod '$RPHASE': $(head -c 200 "$DIR/results.jsonl.err" 2>/dev/null | tr '\n' ' '))$([ -s "$DIR/results.jsonl" ] && echo "; the results.jsonl saved earlier is kept")"
	fi
fi

if read_pvc_file "$(basename "$REPORT_FILE")" "$DIR/report.json" && jq -e '.executions | type == "array"' "$DIR/report.json.tmp" >/dev/null 2>&1; then
	mv "$DIR/report.json.tmp" "$DIR/report.json"
	rm -f "$DIR/report.json.err"
	times_tsv "$DIR/report.json" >"$DIR/times.tsv"
	date -u +%FT%TZ >"$DIR/SAVED"
	log "SORTIE_SAVED dir=$DIR job=$STATE executions=$(jq '.executions | length' "$DIR/report.json") pass=$(jq -r '.pass' "$DIR/report.json") not_run=$(jq '[.executions[] | select(.not_run // false)] | length' "$DIR/report.json") window=$(jq -r '[.executions[] | .started_at // empty] | min // "?"' "$DIR/report.json")..$(jq -r '[.executions[] | .ended_at // empty] | max // "?"' "$DIR/report.json") stream_executions=$STREAM_N"
	exit 0
fi
if [ "$STREAM_OK" = 1 ]; then
	times_tsv "$DIR/results.jsonl" >"$DIR/times.tsv"
	log "SORTIE_SAVE_FAILED no report (reader pod '$RPHASE': $(head -c 200 "$DIR/report.json.err" 2>/dev/null | tr '\n' ' ')), but the results stream IS saved: $STREAM_N execution(s) the run recorded before it ended. Grade them with: sortie-gate.sh --dir $DIR --stream $DIR/results.jsonl   -- and do not tear down with --purge: both files are still on pvc/$PVC"
	exit 1
fi
log "SORTIE_SAVE_FAILED no report: the reader pod ended '$RPHASE' and what it printed is not a sortie report ($(head -c 300 "$DIR/report.json.err" 2>/dev/null | tr '\n' ' ')); the logs above ARE saved, and whatever the run wrote is still on pvc/$PVC ($REPORT_FILE) -- do not tear down with --purge"
exit 1
