#!/usr/bin/env bash
# Dry tests for the soak harness's parsing logic -- no cluster, no network:
#   - restart-watch.sh (#1242) against canned `kubectl get pods -o json`
#     fixtures in testdata/restart-watch/, through a fake kubectl; and its stop
#     (#1386): a TERM before every command it runs up to its first wait, and
#     during every kubectl call, must end it with exit 143 and leave no process
#     in its session;
#   - udscsi-window.awk (#1243), the uds-csi step's plugin-down detector,
#     against canned `kubectl get pods -w -o jsonpath` lines;
#   - pods-not-ready.awk (#1323), the kickoff's readiness check, against canned
#     `kubectl get pods --no-headers` listings in testdata/preflight/ -- and the
#     expression it replaces, shown flagging every healthy pod;
#   - sortie-plan.sh (proposal 042): the share arithmetic (whole rps, sum,
#     divisibility) and the rendered plan of both profiles, without the
#     workarounds sortie 94cf103 and 9fcbb81 retired;
#   - sortie-gate.sh (proposal 042) against sortie JSON reports in
#     testdata/sortie/, in the format of sortie 9fcbb81 (results with
#     statistics / totals / backend_errors / started_at / not_run): clean, one
#     target with stream resets, a missing target, pool_overflow, a lost backend
#     (and one that reported partial counters), every backend lost, a node that
#     froze and answered late, failures and a slow node on one backend only, a
#     cancelled run, a short pool, requests still in flight at the end
#     (http_inflight_lost), the count judged as a range, a stage refused at the
#     execution cap with the stages after it not run (known by sortie f0750ec's
#     `refused` field, and for a 9fcbb81 report by its text), a results stream
#     with a broken last line and one holding two runs, and two old-format
#     reports, which are refused. The kind-* files are REAL: trimmed from kind
#     runs of this harness against sortie 9fcbb81 (kind-clean, kind-results-
#     stream, kind-not-run: a clean run, its results stream, a capped staircase
#     and the sortie pod's log of it) and against sortie f0750ec (kind-refused:
#     the same staircase, report, stream and log; kind-capped: eight targets
#     against engines capped at 4, the whole report and stream). The others
#     are canned: the shape is sortie's, the numbers are made up;
#   - sortie-save.sh: its offline --times, and the save itself against a fake
#     kubectl (both files, a stream with a broken last line and no report, a
#     download that breaks off, a reader pod that cannot be deleted);
#   - run.sh, run (#1387): the kickoff against a fake kubectl and a fake helm,
#     up to a Job whose pod has already ended; the saver must have been armed
#     first and the run directory must hold that run's report and stream;
#   - sortie-values.yaml and run.sh, read: the three digest pins, no CPU limit
#     on the engine, the PriorityClass on the engines AND the sortie pod, and
#     the engine's name taken from its node;
#   - churn.sh, run (#1419): the whole schedule against a fake kubectl on a
#     clock of its own. Every kubectl call and the second after T0 it is made
#     at must be testdata/churn/schedule.tsv, which the driver wrote BEFORE its
#     waits were changed. And its stop: a TERM before each command it runs (a
#     full run, a new-SA step, a uds-csi step, with and without a queued RSS
#     sampler) and during each kubectl call must end it with exit 143, leave no
#     process in its session and leave the schedule where it was; and
#     sample-proxy-rss.sh, which it queues, the same during its three calls;
#   - prober-grade.sh (#1390, #1423) against canned Prometheus query responses
#     in testdata/prober-grade/: failure series born inside the window (the
#     2026-10-08 shape: 40 raw, 37 as increase() counts), a counter reset, a
#     replaced prober pod, the unpinned-cluster counter moving on a series born
#     in the window, a Prometheus with neither metric, one that does not
#     answer, and the cross-check against AETHER_PROBE_FAIL lines. The
#     unpinned-cluster gate's verdict per reason (#1491): the validation gap,
#     an agent start, a no-TLS state that stood, a reason the script does not
#     know, a series with no reason label, an absent gauge; a state standing
#     at the window's start (a range query answered as a server answers it,
#     left-open), one that outlives its pod, a replaced agent with no gauge
#     of its own, and --expect-tls-not-published, on and off. And a series
#     the query at T0 returns that has no sample in the window (#1469): an
#     agent silent for the whole window, a node whose agent never reports, pods
#     replaced shortly before T0, a prober pod deleted right after it.
#
#   bazel test //e2e/soak:harness_test  # jq is the Bazel-pinned one
#   bash e2e/soak/harness_test.sh       # by hand: needs bash, jq, awk on PATH
#
# Exits non-zero on failure. Offline by construction, so it runs in CI as part
# of `bazel test //...` even though the soak itself only runs from a
# workstation against a real cluster.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
WATCH="$HERE/restart-watch.sh"
AWK_WINDOW="$HERE/udscsi-window.awk"
FIX="$HERE/testdata/restart-watch"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Under Bazel, JQ_RLOCATIONPATH names the pinned jq in the runfiles; put it on
# PATH as `jq`, which is what restart-watch.sh calls.
if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	mkdir -p "$TMP/bin"
	ln -s "${TEST_SRCDIR:?}/${JQ_RLOCATIONPATH}" "$TMP/bin/jq"
	PATH="$TMP/bin:$PATH"
fi
echo "jq: $(command -v jq) ($(jq --version))"

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}

# The fake kubectl: serves the Nth `-n <ns> get pods` call (0 = the baseline)
# from $FAKE_DIR/<ns>.<N>.err (stderr, exit 1), .raw (stdout as-is), .json, or
# the static <ns>.json.
cat >"$TMP/kubectl" <<'EOF'
#!/usr/bin/env bash
ns=""
while [ $# -gt 0 ]; do
	case "$1" in
	-n) ns="$2"; shift 2 ;;
	*) shift ;;
	esac
done
# FAKE_TERM_AT_CALL=n: on the n-th call of the run, send TERM to the caller's
# session leader (the watchdog, started under setsid), stay in the foreground
# for another 50 ms, and then note whether the watchdog is still there: one
# that waited for this call is, one that died under it is not.
if [ -n "${FAKE_TERM_AT_CALL:-}" ]; then
	calls=$(($(cat "$FAKE_STATE/calls" 2>/dev/null || echo 0) + 1))
	echo "$calls" >"$FAKE_STATE/calls"
	if [ "$calls" -eq "$FAKE_TERM_AT_CALL" ]; then
		leader="$(ps -o sid= -p "$$" | tr -d ' ')"
		kill -TERM "$leader"
		sleep 0.05
		if kill -0 "$leader" 2>/dev/null; then : >"$FAKE_STATE/waited-for"; fi
	fi
fi
cf="$FAKE_STATE/$ns.count"
n=$(cat "$cf" 2>/dev/null || echo 0)
echo $((n + 1)) >"$cf"
if [ -f "$FAKE_DIR/$ns.$n.err" ]; then cat "$FAKE_DIR/$ns.$n.err" >&2; exit 1; fi
for f in "$FAKE_DIR/$ns.$n.raw" "$FAKE_DIR/$ns.$n.json" "$FAKE_DIR/$ns.json"; do
	if [ -f "$f" ]; then cat "$f"; exit 0; fi
done
echo "error: the server doesn't have a resource type \"pods\" for ns=$ns (no fixture)" >&2
exit 1
EOF
chmod +x "$TMP/kubectl"

# run_watch <scenario> <out> [restart-watch args...]: a fresh fake-kubectl state per run.
run_watch() {
	local scen="$1" outf="$2"
	shift 2
	rm -rf "$TMP/state" && mkdir -p "$TMP/state"
	FAKE_DIR="$FIX/$scen" FAKE_STATE="$TMP/state" RESTART_WATCH_KUBECTL="$TMP/kubectl" \
		SOAK_RESTART_BASELINE_RETRY_DELAY=0 \
		bash "$WATCH" --context fake --interval 0 --log "$outf" "$@"
}

# expect <file> <name> <ERE> [count]: the ERE matches exactly [count] lines (default: >=1).
expect() {
	local f="$1" name="$2" re="$3" want="${4:-}" got
	got=$(grep -cE -- "$re" "$f")
	if [ -z "$want" ]; then
		if [ "$got" -ge 1 ]; then pass "$name"; else fail "$name: no line matches /$re/"; fi
	elif [ "$got" -eq "$want" ]; then
		pass "$name"
	else
		fail "$name: /$re/ matched $got lines, want $want"
	fi
}

show() {
	echo "----- $1"
	sed 's/^/  | /' "$2"
}

# --- restart-watch: clean run with a pre-existing restart ----------------------
L="$TMP/clean.log"
run_watch clean "$L" --samples 2
show "clean (baseline OOM, nothing new)" "$L"
expect "$L" "clean: baseline counts the pre-existing OOM" ' BASELINE count=1 ' 1
expect "$L" "clean: baseline line names it" ' BASELINE aether-system/aether-agent-7ddgc/agent restarts=2 lastReason=OOMKilled exitCode=137 finishedAt=2026-10-04T21:13:07Z state=running$' 1
expect "$L" "clean: one ok line per sample" ' ok count=0 sample=[12] baseline=1$' 2
expect "$L" "clean: no RESTART lines" ' RESTARTS? ' 0
expect "$L" "clean: verdict PASS" ' SUMMARY restart-watch verdict=PASS new_restarts=0 containers=0 samples=2 error_samples=0 baseline=1 ended=complete ' 1

# --- restart-watch: new restarts, a rolled-away pod, an init container ---------
L="$TMP/restarts.log"
run_watch restarts "$L" --samples 2
show "restarts (OOM again, pod rolled away, crash loop, init failure)" "$L"
expect "$L" "restarts: the baseline OOM is not new" ' ok count=0 ' 0
expect "$L" "restarts: sample 1 reports the agent's new OOM" 'Z RESTART aether-system/aether-agent-7ddgc/agent restarts=3 new=1 lastReason=OOMKilled exitCode=137 finishedAt=2026-10-05T04:41:12Z state=running seen=live$' 1
expect "$L" "restarts: sample 1 count" ' RESTARTS count=1 sample=1 ' 1
expect "$L" "restarts: sample 2 keeps the rolled-away pod as gone" 'Z RESTART aether-system/aether-agent-7ddgc/agent .* seen=gone$' 1
expect "$L" "restarts: crash-looping proxy" 'Z RESTART aether-system/aether-proxy-k2x9q/proxy restarts=4 new=4 lastReason=Error exitCode=1 .* state=waiting:CrashLoopBackOff seen=live$' 1
expect "$L" "restarts: new pod's init container counts" 'Z RESTART aether-test/svc-1-5d8c-x7k2p/identity-ready restarts=1 new=1 lastReason=Error exitCode=1 .* state=terminated:Completed seen=live$' 1
expect "$L" "restarts: sample 2 count is cumulative" ' RESTARTS count=3 sample=2 ' 1
expect "$L" "restarts: verdict FAIL" ' SUMMARY restart-watch verdict=FAIL new_restarts=6 containers=3 samples=2 error_samples=0 ' 1
expect "$L" "restarts: one SUMMARY RESTART per container" ' SUMMARY RESTART ' 3

# --- restart-watch: a failed call never reads as "none" -------------------------
L="$TMP/errors.log"
run_watch errors "$L" --samples 3
show "errors (refused, then a non-List, then clean)" "$L"
expect "$L" "errors: refused call is an ERROR line" ' ERROR kubectl failed: ns=aether-ingress sample=1 exit=1 .*connection refused' 1
expect "$L" "errors: non-List answer is an ERROR line" ' ERROR kubectl failed: ns=aether-test sample=2 unparseable output: ' 1
expect "$L" "errors: no ok line for a failed sample" ' ok count=0 sample=[12] ' 0
expect "$L" "errors: ok once every namespace answers" ' ok count=0 sample=3 ' 1
expect "$L" "errors: verdict UNPROVEN, not PASS" ' SUMMARY restart-watch verdict=UNPROVEN new_restarts=0 containers=0 samples=3 error_samples=2 ' 1

# --- restart-watch: no baseline, no run -----------------------------------------
L="$TMP/nobase.log"
SOAK_RESTART_BASELINE_TRIES=2 run_watch nonexistent "$L" --samples 1
rc=$?
show "no baseline (every call fails)" "$L"
if [ "$rc" -eq 2 ]; then pass "nobase: exits 2"; else fail "nobase: exit $rc, want 2"; fi
expect "$L" "nobase: ERROR per try" ' ERROR kubectl failed: ns=aether-system baseline exit=1 ' 2
expect "$L" "nobase: no ok line" ' ok ' 0
expect "$L" "nobase: summary says why" ' SUMMARY restart-watch verdict=UNPROVEN reason=no-baseline tries=2 ' 1

# --- restart-watch: stopped early, and nothing left behind (#1386) --------------
# The watchdog is started as run.sh starts it, under setsid, so everything it
# forks is in a session of its own and "did it leave a process behind?" is a
# question `ps` can answer: after it has exited, that session must be empty.
#
# start_watch <log> [restart-watch args...]: starts it in the background on the
# `clean` fixtures, an hour between samples, and leaves $wpid (to signal and
# wait for; a `timeout` that ends a watchdog which never exits, so a broken
# script fails this test instead of hanging it) and $wsid (the session).
start_watch() {
	local logf="$1"
	shift
	# A fresh log every time: a log that exists is archived first, which is one
	# more command, and the sweep below counts commands.
	rm -rf "$TMP/state" "$TMP/watch.pid" "$logf" "$logf".*.prev && mkdir -p "$TMP/state"
	# shellcheck disable=SC2016 # $$ and $@ are the inner shell's
	FAKE_DIR="$FIX/clean" FAKE_STATE="$TMP/state" RESTART_WATCH_KUBECTL="$TMP/kubectl" \
		timeout -s KILL 10 setsid bash -c 'echo "$$" >"$1"; shift; exec bash "$@"' _ "$TMP/watch.pid" \
		"$WATCH" --context fake --interval 3600 --duration 2h --log "$logf" "$@" &
	wpid=$!
	for _ in $(seq 1 100); do
		[ -s "$TMP/watch.pid" ] && break
		sleep 0.02
	done
	wsid=$(cat "$TMP/watch.pid" 2>/dev/null)
}
# session_left <sid>: what is still running in that session, "pid args" per line.
session_left() { ps -e -o sid=,pid=,args= | awk -v s="$1" '$1 == s { $1 = ""; sub(/^ +/, ""); print }'; }
# survivors <sid>: prints what the watchdog left behind, and kills it (an orphan
# `sleep 3599` holds this test's stdout open for an hour: that was the symptom).
# A child that was just ending may take a moment to go; one that is still there
# after half a second was left behind. (Half a second, not more: the watchdog
# waits in two-second slices, and a two-second sleep left behind must not get
# the time to end by itself and pass for nothing.)
survivors() {
	local left=""
	for _ in $(seq 1 10); do
		left=$(session_left "$1")
		[ -z "$left" ] && return 0
		sleep 0.05
	done
	echo "$left"
	echo "$left" | awk '{print $1}' | xargs -r kill -KILL 2>/dev/null
}
if [ -z "$(ps -e -o sid=,pid=,args= 2>/dev/null)" ] || ! command -v setsid >/dev/null 2>&1 || ! command -v timeout >/dev/null 2>&1; then
	fail "term: this test needs ps (procps), setsid and timeout to see what the watchdog leaves behind"
fi

L="$TMP/term.log"
start_watch "$L"
for _ in $(seq 1 50); do
	if grep -q ' BASELINE count=' "$L" 2>/dev/null; then break; fi
	sleep 0.1
done
t0=$(date +%s)
kill -TERM "$wsid"
wait "$wpid"
rc=$?
show "TERM mid-interval" "$L"
if [ "$rc" -eq 143 ] && [ $(($(date +%s) - t0)) -lt 5 ]; then pass "term: exits 143 at once"; else fail "term: exit $rc after $(($(date +%s) - t0))s"; fi
expect "$L" "term: verdict UNPROVEN ended=TERM" ' SUMMARY restart-watch verdict=UNPROVEN new_restarts=0 containers=0 samples=0 error_samples=0 baseline=1 ended=TERM ' 1
left=$(survivors "$wsid")
if [ -z "$left" ]; then pass "term: no process of the watchdog is left"; else fail "term: left behind: $left"; fi

# TERM at EVERY point. A trap runs between two commands, so the points that
# matter are the command boundaries, and they can be enumerated instead of
# guessed at with a timer: a DEBUG trap (loaded through BASH_ENV, into
# restart-watch.sh only) counts the commands the main shell runs and sends it
# TERM before the k-th. One traced run finds N, the command that is the first
# wait between samples (`read -r -t`; in the old script, `wait`); then k = 1..N,
# each a run of its own. Every one must exit 143 and leave nothing. The old
# script fails at k = N-1, before `SLEEP_PID=$!`, every time (the sleep is
# running and its PID is not yet where the trap looks), and in some runs at
# k = N as well, before `wait "$SLEEP_PID"` (the trap's kill is sent and the
# sleep is still there afterwards).
cat >"$TMP/term-hook.sh" <<'EOF'
case "${0##*/}" in restart-watch.sh) ;; *) return 0 ;; esac
__rw_n=0
__rw_hook() {
	[ "$BASHPID" = "$$" ] || return 0
	__rw_n=$((__rw_n + 1))
	if [ -n "${RW_TRACE:-}" ]; then printf '%s\t%s\n' "$__rw_n" "$1" >>"$RW_TRACE"; fi
	if [ "$__rw_n" = "${RW_TERM_AT:-0}" ]; then kill -TERM "$$"; fi
	return 0
}
set -T
trap '__rw_hook "$BASH_COMMAND"' DEBUG
EOF
: >"$TMP/term-trace.tsv"
BASH_ENV="$TMP/term-hook.sh" RW_TRACE="$TMP/term-trace.tsv" start_watch "$TMP/term-trace.log" --namespaces aether-ingress
for _ in $(seq 1 100); do
	if cut -f2 "$TMP/term-trace.tsv" 2>/dev/null | grep -qE '^(read -r -t|wait) '; then break; fi
	sleep 0.1
done
kill -TERM "$wsid"
wait "$wpid"
survivors "$wsid" >/dev/null
N=$(awk -F'\t' '$2 ~ /^(read -r -t|wait) / {print $1; exit}' "$TMP/term-trace.tsv")
if [ "${N:-0}" -ge 30 ]; then
	pass "term sweep: the watchdog runs $N commands up to its first wait between samples"
else
	fail "term sweep: could not trace the watchdog up to its first wait between samples (N='${N:-}'): the sweep below proves nothing"
	N=0
fi
bad="" nbad=0
for k in $(seq 1 "$N"); do
	BASH_ENV="$TMP/term-hook.sh" RW_TERM_AT="$k" start_watch "$TMP/term-sweep.log" --namespaces aether-ingress
	wait "$wpid"
	rc=$?
	left=$(survivors "$wsid")
	if [ "$rc" -ne 143 ] || [ -n "$left" ]; then
		nbad=$((nbad + 1))
		bad="$bad
  TERM before command $k, \`$(awk -F'\t' -v k="$k" '$1 == k {print $2; exit}' "$TMP/term-trace.tsv")\`: exit $rc, left behind: ${left:-nothing}"
		# A watchdog that no longer stops costs 10 s a try: three are enough to say so.
		if [ "$nbad" -ge 3 ]; then
			bad="$bad
  (stopped after three)"
			break
		fi
	fi
done
if [ "$N" -eq 0 ]; then
	:
elif [ -z "$bad" ]; then
	pass "term sweep: TERM before each of the $N commands: exit 143 and no process left, every time"
else
	fail "term sweep: a TERM at these points was not handled cleanly:$bad"
fi
# ... and INSIDE a command: TERM while a kubectl call is in the foreground. The
# fake sends it itself, to the watchdog (its session's leader), on its n-th call
# and then takes another 50 ms to answer: the three calls of the baseline, and
# the three of the first sample. The trap must wait for the call (a kubectl
# killed with the shell would be the orphan) and then end the run.
#
# Sent by the fake and not by a timer, on purpose. A timer was tried, and twice
# in 1,680 runs it showed something else: bash 5.2.21 can lose a trapped signal
# that lands while it is expanding a `$(...)` -- `trap: line 2: unexpected EOF
# while looking for matching ')'`, the handler is not run, and the script goes
# on as if nothing had been sent. That is bash's, it orphans nothing, and a
# test that met it at random would fail for no fault of the watchdog's.
waited_word() { if [ -e "$TMP/state/waited-for" ]; then echo "waited for"; else echo "NOT waited for"; fi; }
bad=""
for n in 1 2 3 4 5 6; do
	FAKE_TERM_AT_CALL="$n" start_watch "$TMP/term-call.log" --interval 0 --samples 3
	wait "$wpid"
	rc=$?
	left=$(survivors "$wsid")
	if [ "$rc" -ne 143 ] || [ -n "$left" ] || ! grep -q ' SUMMARY restart-watch verdict=UNPROVEN .* ended=TERM ' "$TMP/term-call.log" ||
		[ "$(cat "$TMP/state/calls" 2>/dev/null)" != "$n" ] || [ ! -e "$TMP/state/waited-for" ]; then
		bad="$bad
  TERM during kubectl call $n: exit $rc, calls made: $(cat "$TMP/state/calls" 2>/dev/null), the call was $(waited_word), left behind: ${left:-nothing}, last line: $(tail -n 1 "$TMP/term-call.log")"
	fi
done
if [ -z "$bad" ]; then
	pass "term in a call: TERM during each of 6 kubectl calls: that call is waited for, no further one is made, SUMMARY ended=TERM, exit 143, no process left"
else
	fail "term in a call:$bad"
fi
# The same during --preflight, which has no log and so only the trap of the
# script's first lines: without it TERM kills the shell where it stands, and the
# kubectl it was waiting for (up to its 20 s request timeout) lives on.
start_watch "$TMP/term-preflight.log" --preflight
if wait "$wpid"; then pass "term in preflight: (control) --preflight on these fixtures exits 0"; else fail "term in preflight: the control run did not exit 0"; fi
FAKE_TERM_AT_CALL=2 start_watch "$TMP/term-preflight.log" --preflight
wait "$wpid"
rc=$?
left=$(survivors "$wsid")
if [ "$rc" -eq 143 ] && [ -z "$left" ] && [ -e "$TMP/state/waited-for" ] && [ "$(cat "$TMP/state/calls")" = 2 ]; then
	pass "term in preflight: TERM during a kubectl call of --preflight: the call is waited for, exit 143, no process left"
else
	fail "term in preflight: exit $rc, calls made: $(cat "$TMP/state/calls" 2>/dev/null), the call was $(waited_word), left behind: ${left:-nothing}"
fi

# --- restart-watch: the log is the source of truth; a prior run is archived ---
L="$TMP/archive.log"
echo "old run" >"$L"
run_watch clean "$L" --samples 1
if ls "$L".*.prev >/dev/null 2>&1 && ! grep -q 'old run' "$L"; then pass "archive: previous log moved aside"; else fail "archive: previous log not archived"; fi

# --- udscsi-window.awk ------------------------------------------------------------
# window <name> <want> <orig> <lines...>
window() {
	local name="$1" want="$2" orig="$3" got
	shift 3
	got=$(printf '%s\n' "$@" | awk -v orig="$orig" -f "$AWK_WINDOW")
	if [ "$got" = "$want" ]; then pass "window: $name -> $want"; else fail "window: $name -> $got, want $want"; fi
}
O=aether-uds-csi-abcde
N=aether-uds-csi-fghij
window "nothing listed yet" unseen "$O"
window "watch listed only other nodes' noise" unseen "$O" "pod=$N del= ready=True"
window "listed, roll not here yet" up "$O" "pod=$O del= ready=True"
window "orig terminating" down "$O" "pod=$O del= ready=True" "pod=$O del=2026-10-05T06:07:08Z ready=True"
window "orig gone, replacement Pending" down "$O" "pod=$O del= ready=True" "pod=$O del=2026-10-05T06:07:08Z ready=False" \
	"pod=$N del= ready=" "pod=$N del= ready=False"
window "replacement Ready = window passed" back "$O" "pod=$O del= ready=True" "pod=$O del=2026-10-05T06:07:08Z ready=False" \
	"pod=$N del= ready=False" "pod=$N del= ready=True"
window "a poll after the watch (latest line wins)" down "$O" "pod=$O del= ready=True" "pod=$O del=2026-10-05T06:07:08Z ready=True" \
	"pod=$N del= ready=False"
window "a terminating replacement is not Ready" down "$O" "pod=$O del=2026-10-05T06:07:08Z ready=False" \
	"pod=$N del=2026-10-05T06:07:20Z ready=True"

# --- pods-not-ready.awk: the kickoff's readiness check (#1323) ------------------
READY_AWK="$HERE/pods-not-ready.awk"
PF="$HERE/testdata/preflight"
out=$(awk -f "$READY_AWK" "$PF/pods-ready.txt")
rc=$?
if [ "$rc" -eq 0 ] && [ -z "$out" ]; then pass "ready: every pod ready -> no line, exit 0"; else fail "ready: all-ready listing gave exit $rc and: $out"; fi
out=$(awk -f "$READY_AWK" "$PF/pods-not-ready.txt")
rc=$?
echo "$out" >"$TMP/not-ready.out"
show "pods-not-ready.awk on a listing with five not-ready pods" "$TMP/not-ready.out"
if [ "$rc" -eq 1 ]; then pass "ready: a not-ready pod -> exit 1"; else fail "ready: exit $rc, want 1"; fi
want="aether-agent-zvtb6 aether-proxy-sqmpd aether-registrar-8f6c47bcd-9xrhw aether-uds-csi-2gzv8 aether-uds-csi-4sz2r"
got=$(echo "$out" | awk '$1 == "NOT" && $2 == "READY:" {printf "%s%s", (n++ ? " " : ""), $3}')
if [ "$got" = "$want" ]; then pass "ready: flags exactly the 5 not-ready pods (0/1, 1/2, CrashLoopBackOff, 2/10, Init)"; else fail "ready: flagged [$got], want [$want]"; fi
out=$(awk -f "$READY_AWK" /dev/null)
rc=$?
if [ "$rc" -eq 2 ]; then pass "ready: an empty listing is exit 2, never 'all ready'"; else fail "ready: empty listing gave exit $rc, want 2"; fi
# The RED reading: the expression the workstation kickoff used. awk has no
# back-references, so `\1` never matches a digit and every healthy pod is
# flagged -- which is why the check could not flag a real one.
old_n=$(awk '$3!="Running" || $2 !~ /^([0-9]+)\/\1$/ {print "NOT READY:", $0}' "$PF/pods-ready.txt" 2>/dev/null | grep -c 'NOT READY')
all_n=$(grep -c . "$PF/pods-ready.txt")
if [ "$old_n" -eq "$all_n" ]; then
	pass "ready: the OLD back-reference expression flags all $all_n healthy pods (the #1323 bug, seen red)"
else
	fail "ready: the old expression flagged $old_n of $all_n healthy pods -- this awk has back-references? the red reading is gone"
fi

# --- sortie-plan.sh: shares and plan templating (proposal 042) -----------------
PLAN="$HERE/sortie-plan.sh"
plan_err() { # plan_err <name> <ERE> <sortie-plan args...>: must exit 2 with a message matching ERE
	local name="$1" re="$2" msg rc
	shift 2
	msg=$(bash "$PLAN" "$@" 2>&1 >/dev/null)
	rc=$?
	if [ "$rc" -eq 2 ] && echo "$msg" | grep -qE -- "$re"; then pass "plan: $name"; else fail "plan: $name: exit $rc, message: $msg"; fi
}
S="$TMP/shares.tsv"
bash "$PLAN" shares >"$S"
show "shares (sortie-targets.txt at 60 rps per node)" "$S"
if [ "$(awk -F'\t' '{s += $2} END {print s}' "$S")" = 60 ]; then pass "plan: the shares add up to 60 rps"; else fail "plan: shares sum is not 60"; fi
if [ "$(awk -F'\t' '$2 !~ /^[1-9][0-9]*$/' "$S" | wc -l)" -eq 0 ]; then pass "plan: every share is a whole rps"; else fail "plan: a share is not a whole number"; fi
if [ "$(cut -f1,2 "$S" | tr '\t\n' '= ')" = "svc-1=9 svc-2=9 svc-3=9 svc-4=9 echo=9 mixed-svc=9 uds-echo=3 uds-cr-echo=3 " ]; then
	pass "plan: 6 mesh targets x 9 rps + 2 UDS targets x 3 rps"
else
	fail "plan: shares are $(cut -f1,2 "$S" | tr '\t\n' '= ')"
fi
P="$TMP/plan-soak.yaml"
bash "$PLAN" render --profile soak --backends 5 --dns soak-sortie-engine-nodes.aether-test.svc.cluster.local:8443 >"$P"
expect "$P" "plan soak: 8h30m" '^      duration: 30600s$' 1
expect "$P" "plan soak: 60 rps" '^      rate: 60$' 1
expect "$P" "plan soak: rate is per node" '^      per_backend: true$' 1
expect "$P" "plan soak: open loop" '^      open_loop: true$' 1
expect "$P" "plan soak: HTTP/1.1, as k6 drove it" '^    protocol: http1$' 1
expect "$P" "plan soak: the dns pool" '^    dns: soak-sortie-engine-nodes.aether-test.svc.cluster.local:8443$' 1
expect "$P" "plan soak: 8 targets" '^      - \{name: ' 8
# Two workarounds sortie 94cf103 retired. Each was load-bearing against b71b37e,
# and each would now hide something: lifted predicates are sortie's own default,
# and the 2xx total is in the report's `totals`.
expect "$P" "plan soak: no failure_predicates (sortie turns the defaults off itself)" 'failure_predicates|1000000000000' 0
# Two more that sortie 9fcbb81 retired. The idle strategy: WAIT is sortie's own
# default since 7f338df, so the plan templates nothing. The latency carriers
# (`latency_2xx.p50|p99 < 60s`): thresholds that could not fail, there only to
# get the numbers into the report, which now has them as statistics (1b3d404).
expect "$P" "plan soak: nothing is templated (WAIT is sortie's default; no nighthawk_template)" '^ +(nighthawk_template|sequencer_idle_strategy|value):' 0
expect "$P" "plan soak: no latency threshold (the two carriers could not fail)" '^ +- "latency' 0
# The client queue is NOT one of them. It left with the CPU limit and came back
# with the first talos run: with none, a request that falls due while another
# still waits for a connection is refused by the engine (pool_overflow). The
# default is sized, not picked: the largest share x a 2 s stall budget.
expect "$P" "plan soak: a client queue by default, 9 rps x 2 s" '^    max_pending_requests: 18$' 1
expect "$P" "plan soak: the queue says where its size comes from" '^    # The client queue: the largest share, 9 rps per worker, x a stall budget of 2 s\.$' 1
expect "$P" "plan soak: connections is left at the engine's default (100 per worker; not the limit)" '^    connections:' 0
expect "$P" "plan soak: no http_2xx carrier threshold (the report has totals)" 'counter:benchmark.http_2xx' 0
expect "$P" "plan soak: no stats block unless asked" '^(stats:|  backend:)' 0
expect "$P" "plan soak: floor = 99% x 3 rps x 5 nodes" '"rate:benchmark.http_2xx >= 14.85"' 1
# http_inflight_lost is new in sortie 1178261: a request sent and never answered
# is now a number. Every threshold left in the plan can fail.
for c in http_4xx http_5xx stream_resets pool_connection_failure pool_failure_timeout pool_overflow http_inflight_lost; do
	expect "$P" "plan soak: zero-failure threshold $c" "\"counter:benchmark.$c == 0\"" 1
done
expect "$P" "plan soak: eight thresholds, and no other" '^      - "[^"]* (==|>=|<=|<|>) [0-9a-z.]+"$' 8
if [ "$(sed -n 's/.*weight: \([0-9]*\)}.*/\1/p' "$P" | awk '{s += $1} END {print s}')" = 60 ]; then pass "plan soak: the weights are the shares (sum 60)"; else fail "plan soak: weights do not sum to the rate"; fi
P="$TMP/plan-e2e.yaml"
bash "$PLAN" render --profile e2e --backends 2 --dns x.y.svc.cluster.local:8443 --statsd 10.106.234.157:8125 >"$P"
expect "$P" "plan e2e: 15 minutes" '^      duration: 900s$' 1
expect "$P" "plan e2e: floor follows the backend count" '"rate:benchmark.http_2xx >= 5.94"' 1
expect "$P" "plan e2e: statsd address templated in" '^    address: "10.106.234.157:8125"$' 1
# The live series are named by the engine's own name (its node), not its pod IP
# (sortie 0fc5746). The plan asks; sortie-values.yaml gives each engine a name.
# One without the other is a run every engine refuses, so both are pinned.
expect "$P" "plan e2e: with statsd, the engine names itself in the series (stats.backend: name)" '^  backend: name$' 1
expect "$HERE/sortie-values.yaml" "values: ... and each engine is given its node's name (engine.backendNameFrom: node)" '^  backendNameFrom: node$' 1
P="$TMP/plan-knobs.yaml"
bash "$PLAN" render --profile e2e --backends 2 --dns x.y.svc.cluster.local:8443 --max-pending 16 --idle-strategy SLEEP >"$P"
expect "$P" "plan knobs: --max-pending overrides the sized queue" '^    max_pending_requests: 16$' 1
expect "$P" "plan knobs: --idle-strategy SLEEP is templated (an experiment)" '^        value: SLEEP$' 1
expect "$P" "plan knobs: ... under nighthawk_template, and nothing else is" '^      [a-z_]+:$' 1
plan_err "the retired --no-latency is refused, not ignored" "unknown option '--no-latency'" render --profile e2e --backends 2 --dns x:8443 --no-latency
P="$TMP/plan-noqueue.yaml"
bash "$PLAN" render --profile e2e --backends 2 --dns x.y.svc.cluster.local:8443 --max-pending 0 >"$P"
expect "$P" "plan queue: --max-pending 0 writes no queue (the engine's default, for experiments)" '^    max_pending_requests' 0
expect "$P" "plan queue: ... and the plan says it is off" '^    # The client queue: off \(--max-pending 0\)' 1
expect "$P" "plan queue: off or on, pool_overflow stays in the zero-failure set" '"counter:benchmark.pool_overflow == 0"' 1
P="$TMP/plan-budget.yaml"
bash "$PLAN" render --profile e2e --backends 2 --dns x.y.svc.cluster.local:8443 --rate 120 --stall-budget 3 >"$P"
expect "$P" "plan queue: it follows the rate and the budget (18 rps x 3 s)" '^    max_pending_requests: 54$' 1
printf 'a http://a.ns.aether.internal:18081/ 2\nb http://b.ns.aether.internal:18081/ 1\n' >"$TMP/t21.txt"
P="$TMP/plan-conc.yaml"
bash "$PLAN" render --profile e2e --backends 2 --dns x.y.svc.cluster.local:8443 --targets "$TMP/t21.txt" --rate 60 --concurrency 2 >"$P"
expect "$P" "plan queue: it is per worker thread (40 rps over 2 workers x 2 s)" '^    max_pending_requests: 40$' 1
plan_err "a stall budget of 0 is refused (--max-pending 0 is how to turn the queue off)" 'stall-budget must be whole seconds' render --profile e2e --backends 2 --dns x:8443 --stall-budget 0
plan_err "a queue that is not a number is refused" 'max-pending must be a positive integer, or 0' render --profile e2e --backends 2 --dns x:8443 --max-pending many
plan_err "an unknown idle strategy is refused" 'WAIT, SLEEP, POLL or SPIN' render --profile e2e --backends 2 --dns x:8443 --idle-strategy BLOCK
plan_err "a statsd NAME is refused (the sink does not resolve)" 'IPv4 literal' render --profile e2e --backends 2 --dns x:8443 --statsd otel-scraper.o11y.svc:8125
plan_err "an unknown profile is refused" 'profile must be e2e or soak' render --profile nightly --backends 2 --dns x:8443
plan_err "a Go-style duration is refused" 'whole seconds' render --profile e2e --backends 2 --dns x:8443 --duration 15m
plan_err "render needs the backend count" 'backends must be a positive integer' render --profile e2e --dns x:8443
printf 'a http://a.ns.aether.internal:18081/ 1\nb http://b.ns.aether.internal:18081/ 1\nc http://c.ns.aether.internal:18081/ 1\n' >"$TMP/t3.txt"
plan_err "a share that is not a whole rps is refused (100 split 1:1:1)" 'not a whole number' shares --targets "$TMP/t3.txt" --rate 100
plan_err "a share must divide by the concurrency" 'not a multiple of concurrency 2' shares --targets "$HERE/sortie-targets.txt" --concurrency 2
printf 'a http://a.ns.aether.internal:18081/ 1\na http://b.ns.aether.internal:18081/ 1\n' >"$TMP/tdup.txt"
plan_err "a duplicate target name is refused" 'listed twice' shares --targets "$TMP/tdup.txt" --rate 60
printf 'a https://a.ns.aether.internal:18081/ 1\n' >"$TMP/ttls.txt"
plan_err "a non-http target is refused" 'url must be http://' shares --targets "$TMP/ttls.txt" --rate 60
if [ "$(bash "$PLAN" shares --targets "$TMP/t3.txt" --rate 60 | cut -f2 | tr '\n' ' ')" = "20 20 20 " ]; then pass "plan: 60 split 1:1:1 is 20 each"; else fail "plan: 60 split 1:1:1"; fi

# --- sortie-gate.sh: the report parser and the zero-failure gate ---------------
# The fixtures are in the report format of sortie 9fcbb81: per execution,
# `started_at` / `ended_at`, `results` (each backend's counters, elapsed time,
# own `started_at` and latency `statistics`), `totals` and `backend_errors`.
# Their shape is sortie's (internal/report/report.go, and the kind-* files
# further down, which are real); the numbers of these are made up. They were
# first written for 94cf103 and moved to the new shape field by field: the
# latency that used to ride in two carrier thresholds is now the statistics.
GATE="$HERE/sortie-gate.sh"
SF="$HERE/testdata/sortie"
run_gate() { # run_gate <fixture> <out> [gate args]: canned report, 3 targets, 2 backends
	local fx="$1" outf="$2"
	shift 2
	bash "$GATE" --report "$SF/$fx.json" --shares "$SF/shares.tsv" --backends 2 --engines "$SF/engines.tsv" "$@" >"$outf" 2>&1
}
G="$TMP/gate-clean.log"
run_gate clean "$G"
rc=$?
show "gate: clean report" "$G"
if [ "$rc" -eq 0 ]; then pass "gate clean: exit 0"; else fail "gate clean: exit $rc, want 0"; fi
expect "$G" "gate clean: one PASS per target" '^PASS  (svc-1|echo|uds-echo)  rate=.* failures=none ' 3
expect "$G" "gate clean: rate and count come from results/totals, against the planned count" '^PASS  echo  rate=17.99/18rps http_2xx=2699/2700 backends=2/2 ' 1
expect "$G" "gate clean: latency is printed (worst node), not judged" '^PASS  svc-1 .*  lat p50<=2.15ms p99<=2.77ms max<=6.37ms$' 1
expect "$G" "gate clean: no NODE lines unless asked" '^NODE ' 0
expect "$G" "gate clean: verdict" '^VERDICT PASS targets=3 passed=3 failed=0 not_run=0 backends=2 lost_backends=0 report_pass=true$' 1
run_gate clean "$G" --per-node
expect "$G" "gate clean --per-node: one NODE line per target and backend" '^NODE  (svc-1|echo|uds-echo)  main-worker-0[12]  http_2xx=' 6
expect "$G" "gate clean --per-node: count, planned count, rate, latency, per node" '^NODE  echo  main-worker-02  http_2xx=1349/1350 rate=8.99/9rps started=2026-10-07T11:42:08.936Z p50=2.15ms p99=2.77ms max=6.37ms failures=none$' 1

G="$TMP/gate-resets.log"
run_gate stream-resets "$G"
rc=$?
show "gate: one target with stream_resets" "$G"
if [ "$rc" -eq 1 ]; then pass "gate resets: exit 1"; else fail "gate resets: exit $rc, want 1"; fi
expect "$G" "gate resets: exactly one target fails" '^FAIL ' 1
expect "$G" "gate resets: it is uds-echo, by class, by node" '^FAIL  uds-echo .* -- failures stream_resets=12 \[main-worker-02=12\] stream_resets_before_headers=12 ' 1
expect "$G" "gate resets: the other targets still pass" '^PASS  (svc-1|echo) ' 2
expect "$G" "gate resets: verdict" '^VERDICT FAIL targets=3 passed=2 failed=1 ' 1

G="$TMP/gate-missing.log"
run_gate missing-target "$G"
rc=$?
show "gate: a target missing from the report" "$G"
if [ "$rc" -eq 1 ]; then pass "gate missing: exit 1"; else fail "gate missing: exit $rc, want 1"; fi
expect "$G" "gate missing: the absent target is a FAIL, not a silent pass" '^FAIL  uds-echo  -- missing from the report' 1
expect "$G" "gate missing: verdict FAIL although the report itself says pass" '^VERDICT FAIL targets=3 passed=2 failed=1 not_run=0 backends=2 lost_backends=0 report_pass=true$' 1

G="$TMP/gate-overflow.log"
run_gate pool-overflow "$G"
rc=$?
show "gate: pool_overflow (the client refused its own requests)" "$G"
if [ "$rc" -eq 1 ]; then pass "gate overflow: exit 1"; else fail "gate overflow: exit $rc, want 1"; fi
expect "$G" "gate overflow: per-node counts, as the driver's own (not under 'failures')" '^FAIL  echo .* -- driver saturated: 271 request\(s\) not sent \[main-worker-01=130 main-worker-02=141\] \(pool_overflow: ' 1
expect "$G" "gate overflow: the target's OWN floor, per node, as a count against the plan (the pool's 16.19 rps passes the plan-wide 5.94)" 'http_2xx below 99% of the planned 1350 on \[main-worker-01=1220 main-worker-02=1209\]' 1
expect "$G" "gate overflow: only that target" '^FAIL ' 1

# The first talos-main run (2026-10-07, no client queue): 6 requests in 270,000
# refused by the engine's own pool, on two nodes, every other class zero and
# every rate floor held. The shape is that report's; the second target adds a
# real mesh failure on the same node to show the two are worded apart.
G="$TMP/gate-saturated.log"
run_gate driver-saturated "$G" --per-node
rc=$?
show "gate: the driver was saturated (pool_overflow only), and beside a mesh failure" "$G"
if [ "$rc" -eq 1 ]; then pass "gate saturated: exit 1 (requests not sent: the run is not valid for that target)"; else fail "gate saturated: exit $rc, want 1"; fi
expect "$G" "gate saturated: said in words, with the node, and not as a mesh failure" '^FAIL  svc-1  rate=17.98/18rps http_2xx=2697/2700 backends=2/2  -- driver saturated: 3 request\(s\) not sent \[main-worker-02=3\] \(pool_overflow: the engine refused them itself; not a mesh error\)  lat ' 1
expect "$G" "gate saturated: a target whose only class is pool_overflow has no 'failures' list" '^FAIL  svc-1 .* failures ' 0
expect "$G" "gate saturated: the zero-failure threshold it tripped is not repeated" '^FAIL  svc-1 .*thresholds failed' 0
expect "$G" "gate saturated: beside a mesh failure, each is named on its own" '^FAIL  echo .* -- failures http_5xx=2 \[main-worker-01=2\] \| driver saturated: 1 request\(s\) not sent \[main-worker-01=1\] ' 1
expect "$G" "gate saturated: the NODE line keeps the raw counter" '^NODE  svc-1  main-worker-02  http_2xx=1347/1350 rate=8.98/9rps .* failures pool_overflow=3$' 1
expect "$G" "gate saturated: the third target passes" '^PASS  uds-echo ' 1
expect "$G" "gate saturated: verdict" '^VERDICT FAIL targets=3 passed=1 failed=2 not_run=0 backends=2 lost_backends=0 report_pass=false$' 1

# A lost backend (one engine went away mid-run; the kind run deleted its pod).
# sortie 94cf103 names it in backend_errors and reports the other node whole:
# the target FAILS, the lost node is named, and the survivor is still judged.
G="$TMP/gate-lost.log"
run_gate lost-backend "$G" --per-node
rc=$?
show "gate: an engine went away mid-run" "$G"
if [ "$rc" -eq 1 ]; then pass "gate lost: exit 1"; else fail "gate lost: exit $rc, want 1"; fi
expect "$G" "gate lost: every target of the lost engine fails, naming the node and the address" '^FAIL  (svc-1|echo|uds-echo)  .* -- LOST BACKEND main-worker-01 \(10\.10\.1\.14:8443\) \[returned nothing\] ' 3
expect "$G" "gate lost: the survivor is judged and clean" '^FAIL  svc-1  rate=9/18rps http_2xx=1350/2700 backends=1/2  -- LOST BACKEND [^|]* \| survivors main-worker-02=ok  ' 1
expect "$G" "gate lost: a failure ON the survivor is still found, by class and node" '^FAIL  echo .* \| survivors main-worker-02=FAIL \| failures http_5xx=3 \[main-worker-02=3\]' 1
expect "$G" "gate lost: the survivor's own line" '^NODE  svc-1  main-worker-02  http_2xx=1350/1350 rate=9/9rps .* failures=none$' 1
expect "$G" "gate lost: the lost node's own line" '^NODE  (svc-1|echo|uds-echo)  main-worker-01  LOST, returned nothing$' 3
expect "$G" "gate lost: what sortie said about it, once" '^LOST  main-worker-01 \(10\.10\.1\.14:8443\)  targets=3  error: awaiting execution response: .*Server shutdown' 1
expect "$G" "gate lost: verdict counts the lost backend" '^VERDICT FAIL targets=3 passed=0 failed=3 not_run=0 backends=2 lost_backends=1 report_pass=false$' 1
# The plan's own floor does NOT see it on a 9 rps target: one surviving node at
# 9 rps clears "5.94 for the pool". backend_errors is what fails the target.
if grep -q '^FAIL  svc-1 .*thresholds failed' "$G"; then
	fail "gate lost: svc-1 should fail on the lost backend alone (its pool rate clears the plan-wide floor)"
else
	pass "gate lost: svc-1 fails on backend_errors alone -- no plan threshold failed for it"
fi

G="$TMP/gate-partial.log"
run_gate lost-partial "$G" --per-node
rc=$?
show "gate: a lost backend that still reported what it had counted" "$G"
if [ "$rc" -eq 1 ]; then pass "gate partial: exit 1"; else fail "gate partial: exit $rc, want 1"; fi
expect "$G" "gate partial: lost, with its counters" '^FAIL  svc-1 .* -- LOST BACKEND main-worker-01 \(10\.10\.1\.14:8443\) \[counters reported\] \| survivors main-worker-02=ok' 1
expect "$G" "gate partial: its NODE line keeps the count and says LOST" '^NODE  svc-1  main-worker-01  http_2xx=549/1350 .* LOST$' 1
expect "$G" "gate partial: the other targets pass" '^PASS  (echo|uds-echo) ' 2

# Attribution per backend: what the pool's sum hides.
G="$TMP/gate-node.log"
run_gate one-node "$G" --per-node
rc=$?
show "gate: failures on one node only; one slow node" "$G"
if [ "$rc" -eq 1 ]; then pass "gate node: exit 1"; else fail "gate node: exit $rc, want 1"; fi
expect "$G" "gate node: the 503s are placed on their node" '^FAIL  svc-1 .* -- failures http_5xx=40 \[main-worker-02=40\] \| http_2xx below 99% of the planned 1350 on \[main-worker-02=1310\]' 1
expect "$G" "gate node: a slow node with no failure counter is named (the pool's 17.67 rps clears the plan-wide floor)" '^FAIL  echo  rate=17.67/18rps .* -- http_2xx below 99% of the planned 1350 on \[main-worker-01=1300\]  lat ' 1
expect "$G" "gate node: its latency shows on the line, as the worst node's" '^FAIL  echo .*  lat p50<=2.9ms p99<=1250ms max<=2875ms$' 1
expect "$G" "gate node: the clean node of a failing target reads clean" '^NODE  svc-1  main-worker-01  http_2xx=1350/1350 rate=9/9rps .* failures=none$' 1
expect "$G" "gate node: the third target passes" '^PASS  uds-echo ' 1

G="$TMP/gate-cancelled.log"
run_gate cancelled "$G"
rc=$?
show "gate: the run was cancelled (no backend returned anything)" "$G"
if [ "$rc" -eq 1 ]; then pass "gate cancelled: exit 1"; else fail "gate cancelled: exit $rc, want 1"; fi
expect "$G" "gate cancelled: the execution error is the reason" '^FAIL  (svc-1|echo|uds-echo)  .* -- error: execution cancelled$' 3

# Every engine lost (the kind run deleted both pods): an execution error AND
# both backends in backend_errors, no results. Named, not just "error".
G="$TMP/gate-nobackend.log"
run_gate no-backend "$G"
rc=$?
show "gate: every engine went away" "$G"
if [ "$rc" -eq 1 ]; then pass "gate no-backend: exit 1"; else fail "gate no-backend: exit $rc, want 1"; fi
expect "$G" "gate no-backend: both lost backends named on every target" '^FAIL  (svc-1|echo|uds-echo)  rate=\?/[0-9]+rps backends=0/2  -- LOST BACKEND main-worker-01 .* \| LOST BACKEND main-worker-02 .* \| no backend survived$' 3
expect "$G" "gate no-backend: one LOST line per backend" '^LOST  main-worker-0[12] \(10\.10\.[13]\.[0-9]+:8443\)  targets=3  error: ' 2
expect "$G" "gate no-backend: verdict" '^VERDICT FAIL targets=3 passed=0 failed=3 not_run=0 backends=2 lost_backends=2 report_pass=false$' 1

# A node that froze and thawed (the kind run: docker pause for four minutes).
# sortie waited for it and took its late answer: it is NOT in backend_errors,
# and the pool's rate clears the plan-wide floor, so the report says pass.
G="$TMP/gate-frozen.log"
run_gate frozen "$G"
rc=$?
show "gate: a backend that answered 289 s into a 150 s plan" "$G"
if [ "$rc" -eq 1 ]; then pass "gate frozen: exit 1 although report_pass=true"; else fail "gate frozen: exit $rc, want 1"; fi
expect "$G" "gate frozen: the node that ran off plan is named, with its elapsed time and its count" '^FAIL  svc-1 .* -- ran off plan \(150s\) on \[main-worker-01=289.51s\] \| http_2xx below 99% of the planned 1350 on \[main-worker-01=416\]' 1
expect "$G" "gate frozen: the report itself passed" '^VERDICT FAIL targets=3 passed=2 failed=1 not_run=0 backends=2 lost_backends=0 report_pass=true$' 1

G="$TMP/gate-backends.log"
bash "$GATE" --report "$SF/clean.json" --shares "$SF/shares.tsv" --backends 5 >"$G" 2>&1
rc=$?
show "gate: the pool resolved 2 engines, 5 expected" "$G"
if [ "$rc" -eq 1 ]; then pass "gate backends: exit 1"; else fail "gate backends: exit $rc, want 1"; fi
expect "$G" "gate backends: a short pool fails every target" ' -- backends 2/5  lat ' 3

# A report of sortie b71b37e (what this harness was first built against): it has
# thresholds, a `counter:benchmark.http_2xx > 0` carrier and no per-backend
# results. Graded as if it were new, every counter would read zero.
G="$TMP/gate-old.log"
run_gate old-format "$G"
rc=$?
show "gate: an old-format report" "$G"
if [ "$rc" -eq 2 ]; then pass "gate old-format: exit 2, not a verdict"; else fail "gate old-format: exit $rc, want 2"; fi
expect "$G" "gate old-format: says what is wrong and which sortie wrote it" 'OLD-FORMAT sortie report: .*no per-backend .results. \(sortie older than 94cf103' 1
expect "$G" "gate old-format: prints no PASS, FAIL or VERDICT" '^(PASS|FAIL|VERDICT) ' 0

# A report of sortie 94cf103 (the previous pin): it has results, and no
# started_at. That sortie ended a run with requests still in flight and counted
# them nowhere, so http_inflight_lost is absent from it, which reads as zero.
G="$TMP/gate-old2.log"
run_gate old-94cf103 "$G"
rc=$?
show "gate: a report of the previous pin (no in-flight accounting)" "$G"
if [ "$rc" -eq 2 ]; then pass "gate old-94cf103: exit 2, not a verdict"; else fail "gate old-94cf103: exit $rc, want 2"; fi
expect "$G" "gate old-94cf103: says which sortie wrote it and what would read as zero" 'OLD-FORMAT sortie report: .*no .started_at. \(sortie older than 9fcbb81.*http_inflight_lost would read as zero' 1
expect "$G" "gate old-94cf103: prints no PASS, FAIL or VERDICT" '^(PASS|FAIL|VERDICT) ' 0

# --- what sortie 9fcbb81 added, each seen RED ----------------------------------
# http_inflight_lost (sortie 1178261): two requests on one node were sent and had
# neither a response nor a reset when the run and its 30 s request timeout were
# over. Every other class is zero and the count is inside the 1 % range, so this
# counter alone fails the target -- and it is worded apart from resets and from
# the mesh's `failures`, like pool_overflow.
G="$TMP/gate-inflight.log"
run_gate inflight-lost "$G" --per-node
rc=$?
show "gate: requests still in flight at the end (http_inflight_lost)" "$G"
if [ "$rc" -eq 1 ]; then pass "gate inflight: exit 1 (RED: http_inflight_lost > 0 fails)"; else fail "gate inflight: exit $rc, want 1"; fi
expect "$G" "gate inflight: said in words, by node, on its own" '^FAIL  svc-1  rate=17.99/18rps http_2xx=2698/2700 backends=2/2  -- in flight at the end: 2 request\(s\) with no outcome \[main-worker-02=2\] \(http_inflight_lost: sent or queued, neither answered nor reset .*\)  lat ' 1
expect "$G" "gate inflight: not listed under the mesh's failures, and not as a reset" '^FAIL  svc-1 .*(failures |stream_resets)' 0
expect "$G" "gate inflight: the threshold it tripped is not repeated" '^FAIL  svc-1 .*thresholds failed' 0
expect "$G" "gate inflight: the NODE line keeps the raw counter" '^NODE  svc-1  main-worker-02  http_2xx=1348/1350 .* failures http_inflight_lost=2$' 1
expect "$G" "gate inflight: the other targets pass" '^PASS  (echo|uds-echo) ' 2
expect "$G" "gate inflight: verdict" '^VERDICT FAIL targets=3 passed=2 failed=1 not_run=0 backends=2 lost_backends=0 report_pass=false$' 1

# not_run (sortie 1374999): a stage refused at an engine's execution cap stops
# its scenario, and the stages after it are listed with "not_run": true, no
# counters and no times. The fixture is REAL: the kind report of a three-stage
# staircase of five targets against engines capped at 4, cut down to one
# target's three stages (so the "targets" here are tcp-a/stage-1..3), beside
# the sortie pod's own log of that run. A stage that was not run has every
# failure counter at zero, and nothing else to say for itself.
G="$TMP/gate-notrun.log"
bash "$GATE" --report "$SF/kind-not-run.json" --shares "$SF/kind-not-run.shares.tsv" --backends 2 \
	--engines "$SF/kind-not-run.engines.tsv" --log "$SF/kind-not-run.log" >"$G" 2>&1
rc=$?
show "gate: a stage refused at the execution cap, and two stages NOT RUN (a real report)" "$G"
if [ "$rc" -eq 1 ]; then pass "gate not-run: exit 1 (RED: a not_run stage never reads as a pass)"; else fail "gate not-run: exit $rc, want 1"; fi
expect "$G" "gate not-run: the refused stage says so, and is not called a lost backend" '^FAIL  tcp-a/stage-1  .* -- REFUSED AT THE EXECUTION CAP, nothing of this stage ran: backend 10\.10\.1\.10:8443 refused a start because the engine is at its cap of 4 concurrent executions' 1
expect "$G" "gate not-run: ... no LOST BACKEND for an engine that only refused" '^FAIL  tcp-a/stage-1 .*LOST BACKEND' 0
expect "$G" "gate not-run: each stage that was never attempted is a FAIL that says NOT RUN" '^FAIL  tcp-a/stage-[23]  -- NOT RUN: sortie never attempted it, so it has no counters and its zeros mean nothing \(not run: mesh/stage-1 was refused ' 2
expect "$G" "gate not-run: sortie's own log is the second witness (SKIP lines, 'K not run')" '^FAIL  sortie\.log  -- sortie itself reports executions NOT RUN: 4 SKIP line\(s\), summary "FAIL  0/5 executions passed, 10 not run"' 1
expect "$G" "gate not-run: no target passes" '^PASS ' 0
expect "$G" "gate not-run: the verdict counts them (10: the whole run's, from the log; this cut of the report holds 2)" '^VERDICT FAIL targets=3 passed=0 failed=3 not_run=10 backends=2 lost_backends=0 report_pass=false$' 1
expect "$G" "gate not-run: an engine that refused is not listed as LOST" '^LOST ' 0
# Without the log the report alone fails it, and counts its own two.
bash "$GATE" --report "$SF/kind-not-run.json" --shares "$SF/kind-not-run.shares.tsv" --backends 2 >"$G" 2>&1
rc=$?
if [ "$rc" -eq 1 ] && grep -q '^VERDICT FAIL targets=3 passed=0 failed=3 not_run=2 ' "$G"; then pass "gate not-run: the report alone is enough (exit 1, not_run=2)"; else fail "gate not-run: report alone gave exit $rc: $(tail -n 1 "$G")"; fi
# A not_run execution that is NOT in the share table (a plan with more stages
# than the table knows) still fails the run: nothing that was skipped is quiet.
head -n 1 "$SF/kind-not-run.shares.tsv" >"$TMP/one-stage.shares.tsv"
bash "$GATE" --report "$SF/kind-not-run.json" --shares "$TMP/one-stage.shares.tsv" --backends 2 >"$G" 2>&1
expect "$G" "gate not-run: a skipped stage outside the share table is named too" '^FAIL  mesh/tcp-a/stage-[23]  -- in the report but not in the share table \(a different plan\?\) and NOT RUN$' 2
# The same log beside a report that shows nothing of it (the wrong report was
# saved, or sortie's JSON and its summary disagree): the gate still fails.
G="$TMP/gate-notrun-log.log"
run_gate clean "$G" --log "$SF/kind-not-run.log"
rc=$?
show "gate: a clean report, but sortie's log says stages were not run" "$G"
if [ "$rc" -eq 1 ]; then pass "gate not-run log: exit 1 although every target of the report passes"; else fail "gate not-run log: exit $rc, want 1"; fi
expect "$G" "gate not-run log: every target line is still PASS" '^PASS  (svc-1|echo|uds-echo) ' 3
expect "$G" "gate not-run log: the log fails it, loudly" '^FAIL  sortie\.log  -- sortie itself reports executions NOT RUN: ' 1
expect "$G" "gate not-run log: verdict" '^VERDICT FAIL targets=3 passed=3 failed=0 not_run=10 ' 1
# ... and the log of a run in which everything ran changes nothing.
printf '  PASS mesh/svc-1 (scenario mesh, 2m30.764s)\n\nPASS  3/3 executions passed\n' >"$TMP/clean.log"
G="$TMP/gate-clean-log.log"
run_gate clean "$G" --log "$TMP/clean.log"
rc=$?
if [ "$rc" -eq 0 ] && grep -q '^VERDICT PASS .* not_run=0 ' "$G"; then pass "gate: a log with 'N/N executions passed' and no SKIP leaves a clean report PASS"; else fail "gate: clean report + clean log gave exit $rc: $(tail -n 1 "$G")"; fi

# --- what sortie f0750ec added: "refused": "execution_cap", each seen RED -------
# The refused stage is MARKED now, where 9fcbb81 only worded it: the field is on
# every execution of the stage and on each backend_errors entry that is an
# engine which refused. The gate reads the field. The kind-not-run.* fixture
# above is the 9fcbb81 report and has no such field anywhere: it is what keeps
# the one fallback honest (the error text, read only when the field is absent),
# because that pin's reports are still graded with this gate.
if jq -e '[.. | objects | select(has("refused"))] | length == 0' "$SF/kind-not-run.json" >/dev/null; then
	pass "gate refused: the 9fcbb81 fixture has no 'refused' field (so the tests above are the text fallback's)"
else
	fail "gate refused: kind-not-run.json carries a 'refused' field: it no longer exercises the fallback"
fi
# REAL: the same staircase on kind against sortie f0750ec, cut to four of its
# fifteen executions and the pod log's lines about them. tcp-c/stage-1 lists
# both engines in backend_errors, each marked; tcp-a/stage-1 was started and
# then stopped with the stage, and lists none.
G="$TMP/gate-refused.log"
bash "$GATE" --report "$SF/kind-refused.json" --shares "$SF/kind-refused.shares.tsv" --backends 2 \
	--engines "$SF/kind-refused.engines.tsv" --log "$SF/kind-refused.log" --per-node >"$G" 2>&1
rc=$?
show "gate: a stage refused at the execution cap, marked by sortie f0750ec (a real report)" "$G"
if [ "$rc" -eq 1 ]; then pass "gate refused: exit 1"; else fail "gate refused: exit $rc, want 1"; fi
expect "$G" "gate refused: the stage says so, and names the engines that refused, by node" '^FAIL  tcp-c/stage-1  .* -- REFUSED AT THE EXECUTION CAP, nothing of this stage ran: backend 10\.10\.2\.12:8443 refused a start .* \[refused by: sortie-worker sortie-worker2\]$' 1
expect "$G" "gate refused: an execution of that stage with no backend_errors is refused too (it was stopped with the stage)" '^FAIL  tcp-a/stage-1  .* -- REFUSED AT THE EXECUTION CAP, nothing of this stage ran: [^[]*$' 1
expect "$G" "gate refused: the stages after it are NOT RUN" '^FAIL  tcp-c/stage-[23]  -- NOT RUN: ' 2
expect "$G" "gate refused: per node, the engine refused; it was not lost" '^NODE  tcp-c/stage-1  sortie-worker2?  REFUSED the start: the engine was at its execution cap$' 2
expect "$G" "gate refused: no engine is called lost" '(LOST BACKEND|^LOST |LOST, returned nothing)' 0
expect "$G" "gate refused: verdict (10 not run: the whole run's, from the log)" '^VERDICT FAIL targets=4 passed=0 failed=4 not_run=10 backends=2 lost_backends=0 report_pass=false$' 1
# RED for "the field, not the text": the same report with every error reworded,
# so that nothing in it matches what the gate used to look for. The gate of
# 9fcbb81 read this as two LOST backends and `error: ...`.
jq '(.executions[] | select(.refused != null) | .error) = "no slot was free on a backend"
	| (.executions[].backend_errors[]? | .error) = "start turned down"' "$SF/kind-refused.json" >"$TMP/refused-reworded.json"
G="$TMP/gate-refused-reworded.log"
bash "$GATE" --report "$TMP/refused-reworded.json" --shares "$SF/kind-refused.shares.tsv" --backends 2 --engines "$SF/kind-refused.engines.tsv" >"$G" 2>&1
show "gate: the same refusal with its error text reworded (only the field says it)" "$G"
expect "$G" "gate refused: read from the FIELD -- reworded errors change nothing (RED: by text these were lost backends)" '^FAIL  tcp-[ac]/stage-1  .* -- REFUSED AT THE EXECUTION CAP, nothing of this stage ran: no slot was free on a backend' 2
expect "$G" "gate refused: ... and still no engine is called lost" '^VERDICT FAIL targets=4 passed=0 failed=4 not_run=2 backends=2 lost_backends=0 ' 1
# A backend that really was lost, beside one that refused, in the same refused
# execution: the entry without the mark is a lost backend and is named as one.
# (Canned: the real report with one entry's mark taken off and its error
# replaced. RED: a gate that treats every entry of a refused stage as a refusal,
# as the text match had to, hides the lost engine.)
jq '(.executions[] | select(.label == "mesh/tcp-c/stage-1") | .backend_errors[] | select(.backend == "10.10.1.10:8443"))
	|= (del(.refused) | .error = "awaiting execution response: rpc error: code = Unavailable desc = error reading from server: EOF")' \
	"$SF/kind-refused.json" >"$TMP/refused-and-lost.json"
G="$TMP/gate-refused-lost.log"
bash "$GATE" --report "$TMP/refused-and-lost.json" --shares "$SF/kind-refused.shares.tsv" --backends 2 --engines "$SF/kind-refused.engines.tsv" --per-node >"$G" 2>&1
show "gate: one engine refused and another was lost, in the same stage" "$G"
expect "$G" "gate refused: the unmarked entry is a LOST backend, the marked one a refusal" '^FAIL  tcp-c/stage-1  .* -- REFUSED AT THE EXECUTION CAP, .* \[refused by: sortie-worker2\] \| LOST BACKEND sortie-worker \(10\.10\.1\.10:8443\) \[returned nothing\]$' 1
expect "$G" "gate refused: ... listed once before the verdict, with what sortie said" '^LOST  sortie-worker \(10\.10\.1\.10:8443\)  targets=1  error: awaiting execution response: ' 1
expect "$G" "gate refused: ... and counted" '^VERDICT FAIL targets=4 passed=0 failed=4 not_run=2 backends=2 lost_backends=1 ' 1
expect "$G" "gate refused: per node, one of each" '^NODE  tcp-c/stage-1  sortie-worker(  LOST, returned nothing|2  REFUSED the start: .*)$' 2
# The results stream carries the same field (each line is the execution's object
# in the report): REAL, the same four executions as sortie appended them.
G="$TMP/gate-refused-stream.log"
bash "$GATE" --stream "$SF/kind-refused-stream.jsonl" --shares "$SF/kind-refused.shares.tsv" --backends 2 --engines "$SF/kind-refused.engines.tsv" >"$G" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then pass "gate refused: the results stream is graded the same way (exit 1)"; else fail "gate refused: stream gave exit $rc, want 1"; fi
expect "$G" "gate refused: from the stream, the stage is refused by its field" '^FAIL  tcp-[ac]/stage-1  .* -- REFUSED AT THE EXECUTION CAP, ' 2
expect "$G" "gate refused: from the stream, verdict" '^VERDICT FAIL targets=4 passed=0 failed=4 not_run=2 backends=2 lost_backends=0 report_pass=absent source=stream skipped_lines=0$' 1
# The 9fcbb81 report, by its text: unchanged, and its engine is named the same way.
G="$TMP/gate-notrun-fallback.log"
bash "$GATE" --report "$SF/kind-not-run.json" --shares "$SF/kind-not-run.shares.tsv" --backends 2 --engines "$SF/kind-not-run.engines.tsv" >"$G" 2>&1
expect "$G" "gate refused: a 9fcbb81 report (no field) is still read by its error text, and names the engine" '^FAIL  tcp-a/stage-1  .* -- REFUSED AT THE EXECUTION CAP, .* \[refused by: sortie-worker\]$' 1
# ... but ONLY when the field is absent: an execution that carries a `refused`
# the gate does not know is not second-guessed from its text.
jq '(.executions[0].refused) = "something_new"' "$SF/kind-not-run.json" >"$TMP/refused-unknown.json"
G="$TMP/gate-refused-unknown.log"
bash "$GATE" --report "$TMP/refused-unknown.json" --shares "$SF/kind-not-run.shares.tsv" --backends 2 --engines "$SF/kind-not-run.engines.tsv" >"$G" 2>&1
expect "$G" "gate refused: the text fallback is for a report WITHOUT the field only" '^FAIL  tcp-a/stage-1  .*REFUSED AT THE EXECUTION CAP' 0
expect "$G" "gate refused: ... such an execution still fails, on what its backends report" '^FAIL  tcp-a/stage-1  .* -- LOST BACKEND sortie-worker ' 1

# The count is judged as a RANGE against the plan (share x the configured
# duration), 99 %..101 %, not as a rate over the backend's own elapsed time and
# not as an equality: the engine fixes its elapsed time when the run stops and
# it can read a hair under the duration, and a late-woken worker ends a run one
# request short. Both at once still pass ...
jq '.executions[0].results[0] |= (.elapsed_ms = 149999 | .counters["benchmark.http_2xx"] = 1349)
	| .executions[0].totals["benchmark.http_2xx"] = 2699' "$SF/clean.json" >"$TMP/hair-under.json"
G="$TMP/gate-hair.log"
bash "$GATE" --report "$TMP/hair-under.json" --shares "$SF/shares.tsv" --backends 2 --engines "$SF/engines.tsv" --per-node >"$G" 2>&1
rc=$?
show "gate: a backend that reads 149,999 ms of 150,000 and is one request short" "$G"
if [ "$rc" -eq 0 ]; then pass "gate range: elapsed a hair under the duration and 1349 of 1350 is a PASS"; else fail "gate range: exit $rc, want 0"; fi
expect "$G" "gate range: the node's line shows the count against the plan" '^NODE  svc-1  main-worker-01  http_2xx=1349/1350 rate=8.99/9rps ' 1
# ... a backend that stopped at 96 % of the run fails on its count although its
# rate over its own elapsed time is exactly the plan's (RED: the old check,
# rate >= 99 % of the share, passed this) ...
jq '.executions[0].results[0] |= (.elapsed_ms = 144000 | .counters["benchmark.http_2xx"] = 1296)
	| .executions[0].totals["benchmark.http_2xx"] = 2646' "$SF/clean.json" >"$TMP/stopped-early.json"
G="$TMP/gate-early.log"
bash "$GATE" --report "$TMP/stopped-early.json" --shares "$SF/shares.tsv" --backends 2 --engines "$SF/engines.tsv" --per-node >"$G" 2>&1
rc=$?
show "gate: a backend that ran 144 s of 150 s at exactly 9 rps" "$G"
if [ "$rc" -eq 1 ]; then pass "gate range: a run that stopped early fails (exit 1)"; else fail "gate range: exit $rc, want 1"; fi
expect "$G" "gate range: on the count, with the rate reading whole" '^FAIL  svc-1 .* -- http_2xx below 99% of the planned 1350 on \[main-worker-01=1296\]  lat ' 1
expect "$G" "gate range: ... its rate over its own elapsed time is 9 of 9" '^NODE  svc-1  main-worker-01  http_2xx=1296/1350 rate=9/9rps ' 1
# ... and more than the plan asked for is not this plan's load either.
jq '.executions[0].results[0].counters["benchmark.http_2xx"] = 1400 | .executions[0].totals["benchmark.http_2xx"] = 2750' "$SF/clean.json" >"$TMP/over.json"
G="$TMP/gate-over.log"
bash "$GATE" --report "$TMP/over.json" --shares "$SF/shares.tsv" --backends 2 --engines "$SF/engines.tsv" >"$G" 2>&1
rc=$?
if [ "$rc" -eq 1 ]; then pass "gate range: more than 101 % of the plan fails (exit 1)"; else fail "gate range: exit $rc, want 1"; fi
expect "$G" "gate range: above the plan is named" '^FAIL  svc-1 .* -- http_2xx above 101% of the planned 1350 on \[main-worker-01=1400\] ' 1

# Latency comes from each backend's `statistics` now, and the timestamps from
# started_at / ended_at: one WINDOW line to put the run beside the roll times.
G="$TMP/gate-window.log"
run_gate clean "$G"
expect "$G" "gate window: when the run was dispatched and ended, and when the engines' workers started" '^WINDOW  dispatched=2026-10-07T11:42:08\.412Z ended=2026-10-07T11:44:39\.182Z engines_started=2026-10-07T11:42:08\.924Z\.\.2026-10-07T11:42:08\.939Z$' 1

# --- the results stream (sortie 9fcbb81: --results-stream, chart report.stream) --
# What is left of a run whose pod died before it wrote the report: one line per
# finished execution. The fixture is the kind run's own stream, byte for byte as
# sortie wrote it: two of its lines whole, and a third cut off after 700 bytes
# with no newline, as a run that died while writing it leaves it. The gate grades the two whole lines,
# skips the one that does not parse and says so, and the target that line was
# about is MISSING -- a FAIL, not a pass on two of three.
G="$TMP/gate-stream.log"
bash "$GATE" --stream "$SF/kind-results-stream.jsonl" --shares "$SF/kind-clean.shares.tsv" --backends 2 --engines "$SF/kind-clean.engines.tsv" >"$G" 2>&1
rc=$?
show "gate: a results stream whose last line is cut short" "$G"
if [ "$rc" -eq 1 ]; then pass "gate stream: exit 1 (RED: the execution on the broken line is missing)"; else fail "gate stream: exit $rc, want 1"; fi
expect "$G" "gate stream: the whole lines are graded like a report's executions" '^PASS  (tcp-a|tcp-b)  rate=18/18rps http_2xx=5400/5400 backends=2/2  failures=none ' 2
expect "$G" "gate stream: the target on the broken line is missing, not passed" '^FAIL  uds-echo  -- missing from the report' 1
expect "$G" "gate stream: the skipped line is said" '^NOTE  1 line\(s\) of the results stream are not an execution ' 1
expect "$G" "gate stream: the verdict names its source and has no overall pass" '^VERDICT FAIL targets=3 passed=2 failed=1 not_run=0 backends=2 lost_backends=0 report_pass=absent source=stream skipped_lines=1$' 1
# A broken line AFTER every target is whole (and a blank line) costs nothing.
{
	jq -c '.executions[]' "$SF/clean.json"
	printf '\n{"label":"mesh/svc-1","scenario":"me'
} >"$TMP/stream-whole.jsonl"
G="$TMP/gate-stream-whole.log"
bash "$GATE" --stream "$TMP/stream-whole.jsonl" --shares "$SF/shares.tsv" --backends 2 --engines "$SF/engines.tsv" >"$G" 2>&1
rc=$?
if [ "$rc" -eq 0 ]; then pass "gate stream: every target present and clean, then a fragment -> exit 0"; else fail "gate stream: whole stream + fragment gave exit $rc, want 0"; fi
expect "$G" "gate stream: ... and the fragment is still reported" '^VERDICT PASS targets=3 passed=3 failed=0 not_run=0 .* report_pass=absent source=stream skipped_lines=1$' 1
# sortie APPENDS to the stream and never truncates it: a file a second run was
# pointed at holds both runs. The gate cannot tell which one it was asked about.
{
	jq -c '.executions[]' "$SF/clean.json"
	jq -c '.executions[]' "$SF/stream-resets.json"
} >"$TMP/stream-two-runs.jsonl"
G="$TMP/gate-stream-two.log"
bash "$GATE" --stream "$TMP/stream-two-runs.jsonl" --shares "$SF/shares.tsv" --backends 2 >"$G" 2>&1
rc=$?
show "gate: two runs appended to one stream file" "$G"
if [ "$rc" -eq 1 ]; then pass "gate stream: two runs in one file -> exit 1"; else fail "gate stream: two runs in one file gave exit $rc, want 1"; fi
expect "$G" "gate stream: every target listed twice is refused, not graded on either copy" '^FAIL  (svc-1|echo|uds-echo)  -- listed 2 times \(started_at ' 3
echo 'not json at all' >"$TMP/stream-garbage.jsonl"
bash "$GATE" --stream "$TMP/stream-garbage.jsonl" --shares "$SF/shares.tsv" --backends 2 >"$TMP/gate-stream-garbage.log" 2>&1
rc=$?
if [ "$rc" -eq 2 ] && grep -q 'holds no execution' "$TMP/gate-stream-garbage.log"; then pass "gate stream: no parseable line -> exit 2, nothing to grade"; else fail "gate stream: garbage stream gave exit $rc"; fi
# In a run directory with a stream and no report, the gate does not quietly
# grade the stream: it says the report is missing and how to ask for the stream.
mkdir -p "$TMP/rundir"
printf 'BACKENDS=2\n' >"$TMP/rundir/run.env"
cp "$SF/kind-clean.shares.tsv" "$TMP/rundir/shares.tsv"
cp "$SF/kind-results-stream.jsonl" "$TMP/rundir/results.jsonl"
bash "$GATE" --dir "$TMP/rundir" >"$TMP/gate-rundir.log" 2>&1
rc=$?
if [ "$rc" -eq 2 ] && grep -q 'no report.json but it has the results stream.*--stream' "$TMP/gate-rundir.log"; then pass "gate --dir: a stream without a report -> exit 2, with the command to grade the stream"; else fail "gate --dir: stream without report gave exit $rc: $(cat "$TMP/gate-rundir.log")"; fi
bash "$GATE" --dir "$TMP/rundir" --stream "$TMP/rundir/results.jsonl" >"$TMP/gate-rundir2.log" 2>&1
rc=$?
if [ "$rc" -eq 1 ] && grep -q '^VERDICT FAIL .* source=stream skipped_lines=1$' "$TMP/gate-rundir2.log"; then pass "gate --dir --stream: grades the stream"; else fail "gate --dir --stream gave exit $rc"; fi

# sortie-save.sh --times: the saver's times.tsv, offline. One row per execution
# and backend: label, started_at, ended_at, elapsed_ms, backend, the backend's
# own started_at, verdict. From a report, and from a stream with a broken line.
SAVE="$HERE/sortie-save.sh"
T="$TMP/times.tsv"
bash "$SAVE" --times "$SF/clean.json" >"$T"
show "sortie-save.sh --times on a report" "$T"
expect "$T" "times: one row per execution and backend" '^mesh/' 6
expect "$T" "times: dispatch, last answer, elapsed, backend, the engine's own start, verdict" "^mesh/echo	2026-10-07T11:42:08\.415Z	2026-10-07T11:44:39\.179Z	150764	10\.10\.3\.22:8443	2026-10-07T11:42:08\.936Z	pass\$" 1
bash "$SAVE" --times "$SF/kind-results-stream.jsonl" >"$T"
expect "$T" "times: from a stream, the broken line is skipped (2 executions x 2 backends)" '^mesh/tcp-[ab]	2026-10-07T23:26:33\.393Z	' 4
if [ "$(grep -c . "$T")" -eq 4 ]; then pass "times: ... and nothing else is printed for it"; else fail "times: the stream gave $(grep -c . "$T") rows, want 4"; fi
bash "$SAVE" --times "$SF/kind-not-run.json" >"$T"
expect "$T" "times: a stage that was not run has no times and says so" '^mesh/tcp-a/stage-[23]	-	-	0	-	-	not_run$' 2

# --- sortie-save.sh against a fake kubectl ---------------------------------------
# What the saver does with the two files of the report PVC. The fake serves a
# finished Job, a sortie pod, and a reader pod that prints whichever file of
# $FAKE_SAVE/pvc/ it was created to `cat`: phase Failed when the file is not
# there, a `logs` that breaks off after 300 bytes when $FAKE_SAVE/break-<file>
# exists, and a pod that cannot be deleted while $FAKE_SAVE/stuck exists.
cat >"$TMP/kubectl-save" <<'EOF'
#!/usr/bin/env bash
args="$*"
reading="$(cat "$FAKE_SAVE/reading" 2>/dev/null)"
case "$args" in
*" get job "*) echo '{"status":{"conditions":[{"type":"Complete","status":"True"}]}}' ;;
*" get pods -l job-name="*) [ -e "$FAKE_SAVE/no-sortie-pod" ] || printf 'soak-sortie-abc-xyz' ;;
*" get pod soak-sortie-abc-xyz "*) echo '{}' ;;
*" logs soak-sortie-abc-xyz "*) printf '  PASS mesh/tcp-a (scenario mesh, 5m0.785s)\n\nPASS  3/3 executions passed\n' ;;
*" get pods -l app.kubernetes.io/instance="*) echo '{"items":[]}' ;;
*" delete pod soak-report-reader "*)
	[ -e "$FAKE_SAVE/stuck" ] && exit 1
	rm -f "$FAKE_SAVE/reading"
	;;
*" apply -f -"*)
	# Which file the reader pod was made to print: any *.jsonl is the PVC's
	# r.jsonl and any *.json its r.json (run.sh names both by run tag).
	sed -n 's/.*command: \["cat", "\/report\/\(.*\)"\].*/\1/p' |
		sed -e 's/^.*\.jsonl$/r.jsonl/' -e 's/^.*\.json$/r.json/' >"$FAKE_SAVE/reading"
	;;
*" get pod soak-report-reader -o jsonpath="*) if [ -e "$FAKE_SAVE/pvc/$reading" ]; then printf Succeeded; else printf Failed; fi ;;
*" get pod soak-report-reader"*) [ -e "$FAKE_SAVE/stuck" ] ;;
*" logs soak-report-reader"*)
	if [ -e "$FAKE_SAVE/break-$reading" ]; then
		head -c 300 "$FAKE_SAVE/pvc/$reading"
		echo "error: unexpected EOF" >&2
		exit 1
	fi
	cat "$FAKE_SAVE/pvc/$reading" 2>/dev/null || echo "cat: can't open '/report/$reading': No such file or directory"
	;;
*)
	echo "fake kubectl: unexpected call: $args" >&2
	exit 1
	;;
esac
EOF
chmod +x "$TMP/kubectl-save"
# run_save <name> [files to put on the fake PVC as r.json / r.jsonl ...]: a fresh
# run directory and PVC per case; prints nothing, leaves $SD (the run directory),
# $SL (the saver's output) and $src (its exit status).
run_save() {
	local name="$1"
	SD="$TMP/save-$name"
	SL="$TMP/save-$name.log"
	export FAKE_SAVE="$TMP/save-$name.fake"
	mkdir -p "$SD" "$FAKE_SAVE/pvc"
	printf 'CTX=fake\nNS=aether-test\nRELEASE=soak\nPVC=sortie-soak-reports\nJOB=soak-sortie-abc\nT_LOAD=1\nDURATION_S=1\nREPORT_FILE=/var/run/sortie/r.json\nSTREAM_FILE=/var/run/sortie/r.jsonl\n' >"$SD/run.env"
	SORTIE_SAVE_KUBECTL="$TMP/kubectl-save" bash "$SAVE" --dir "$SD" >"$SL" 2>&1
	src=$?
}
SAVE="$HERE/sortie-save.sh"

# The report and the stream are both there: both saved, SAVED written.
mkdir -p "$TMP/save-both.fake/pvc"
cp "$SF/kind-clean.json" "$TMP/save-both.fake/pvc/r.json"
jq -c '.executions[]' "$SF/kind-clean.json" >"$TMP/save-both.fake/pvc/r.jsonl"
run_save both
show "saver: a report and a results stream on the PVC" "$SL"
if [ "$src" -eq 0 ] && [ -e "$SD/SAVED" ]; then pass "save both: exit 0 and SAVED"; else fail "save both: exit $src, SAVED $([ -e "$SD/SAVED" ] && echo written || echo missing)"; fi
if cmp -s "$SD/results.jsonl" "$TMP/save-both.fake/pvc/r.jsonl" && cmp -s "$SD/report.json" "$SF/kind-clean.json"; then pass "save both: results.jsonl and report.json are the PVC's files, byte for byte"; else fail "save both: a saved file differs from the PVC's"; fi
expect "$SL" "save both: the stream is counted" ' saved results\.jsonl \(the results stream\): 3 execution\(s\)$' 1
expect "$SL" "save both: SORTIE_SAVED says what was not run, when the run was, and what the stream holds" ' SORTIE_SAVED dir=.* job=complete executions=3 pass=true not_run=0 window=2026-10-07T23:26:33\.393Z\.\.2026-10-07T23:31:34\.178Z stream_executions=3$' 1
expect "$SD/times.tsv" "save both: times.tsv, one row per execution and backend" '^mesh/(tcp-a|tcp-b|uds-echo)	2026-10-07T23:26:33\.393Z	' 6
expect "$SD/sortie.log" "save both: the sortie pod's log is saved" '^PASS  3/3 executions passed$' 1

# The pod died before the report: only the stream is on the PVC, and its last
# line is cut short. It is saved as it is, counted, and the saver says how to
# grade it -- and does NOT write SAVED, so a teardown still asks.
mkdir -p "$TMP/save-stream.fake/pvc"
cp "$SF/kind-results-stream.jsonl" "$TMP/save-stream.fake/pvc/r.jsonl"
run_save stream
show "saver: a results stream with a broken last line, and no report" "$SL"
if [ "$src" -eq 1 ] && [ ! -e "$SD/SAVED" ]; then pass "save stream-only: exit 1 and no SAVED"; else fail "save stream-only: exit $src, SAVED $([ -e "$SD/SAVED" ] && echo written || echo missing)"; fi
if cmp -s "$SD/results.jsonl" "$SF/kind-results-stream.jsonl"; then pass "save stream-only: the stream is kept as it is on the PVC, broken line and all"; else fail "save stream-only: results.jsonl differs from the PVC's"; fi
expect "$SL" "save stream-only: whole and broken lines are counted apart" ' saved results\.jsonl \(the results stream\): 2 execution\(s\), 1 line\(s\) that do not parse ' 1
expect "$SL" "save stream-only: says the stream is what there is, and how to grade it" " SORTIE_SAVE_FAILED no report \(reader pod 'Failed': the reader said: cat: can't open '/report/r\.json': No such file or directory +\), but the results stream IS saved: 2 execution\(s\) .* sortie-gate\.sh --dir .* --stream " 1
expect "$SD/times.tsv" "save stream-only: times.tsv comes from the stream" '^mesh/tcp-[ab]	' 4
if [ ! -e "$SD/report.json" ]; then pass "save stream-only: no report.json is invented"; else fail "save stream-only: a report.json exists"; fi

# The download of the stream breaks off half-way (kubectl logs fails). What came
# down looks like a shorter stream; it must not replace the copy saved before.
mkdir -p "$TMP/save-break.fake/pvc" "$TMP/save-break"
cp "$SF/kind-clean.json" "$TMP/save-break.fake/pvc/r.json"
jq -c '.executions[]' "$SF/kind-clean.json" >"$TMP/save-break.fake/pvc/r.jsonl"
cp "$TMP/save-break.fake/pvc/r.jsonl" "$TMP/save-break/results.jsonl"
: >"$TMP/save-break.fake/break-r.jsonl"
run_save break
show "saver: the stream's download breaks off" "$SL"
if cmp -s "$SD/results.jsonl" "$TMP/save-break.fake/pvc/r.jsonl" && [ ! -e "$SD/results.jsonl.tmp" ]; then pass "save broken download: the earlier results.jsonl is kept, the partial one is not promoted"; else fail "save broken download: results.jsonl was replaced by a partial download"; fi
expect "$SL" "save broken download: said, with the reason" ' the results stream could not be read off pvc/.*did not complete: the download is not whole.*; the results\.jsonl saved earlier is kept$' 1
expect "$SL" "save broken download: the report is still saved" ' SORTIE_SAVED .* stream_executions=0$' 1

# A reader pod that cannot be deleted (its node does not answer): every wait is
# bounded, so the saver says so and ends instead of hanging on `--wait`.
mkdir -p "$TMP/save-stuck.fake/pvc"
cp "$SF/kind-clean.json" "$TMP/save-stuck.fake/pvc/r.json"
: >"$TMP/save-stuck.fake/stuck"
t0=$(date +%s)
run_save stuck
show "saver: a reader pod that cannot be deleted" "$SL"
if [ "$src" -eq 1 ] && [ ! -e "$SD/SAVED" ] && [ $(($(date +%s) - t0)) -lt 30 ]; then pass "save stuck reader: exit 1, no SAVED, no hang"; else fail "save stuck reader: exit $src after $(($(date +%s) - t0))s"; fi
expect "$SL" "save stuck reader: says the reader could not be deleted, and where to look, for the stream and for the report" 'an earlier pod/soak-report-reader could not be deleted within 90 s \(is the node that holds pvc/sortie-soak-reports answering\?\)' 2
expect "$SL" "save stuck reader: SORTIE_SAVE_FAILED, and the PVC is not to be purged" " SORTIE_SAVE_FAILED no report: the reader pod ended 'stuck' .* do not tear down with --purge\$" 1
grep -c -- '--timeout=' "$SAVE" >"$TMP/timeouts.n"
if [ "$(cat "$TMP/timeouts.n")" -ge 2 ] && ! grep -E 'delete pod .*--wait=true' "$SAVE" | grep -vq -- '--timeout='; then pass "save: every waiting delete of the reader pod has a timeout"; else fail "save: a 'delete pod --wait=true' without --timeout"; fi

# The sortie pod is gone (evicted with its node, as on kind when the driver's
# node was frozen): no log, and the PVC is still read.
mkdir -p "$TMP/save-nopod.fake/pvc"
cp "$SF/kind-clean.json" "$TMP/save-nopod.fake/pvc/r.json"
jq -c '.executions[]' "$SF/kind-clean.json" >"$TMP/save-nopod.fake/pvc/r.jsonl"
: >"$TMP/save-nopod.fake/no-sortie-pod"
run_save nopod
if [ "$src" -eq 0 ] && [ -e "$SD/SAVED" ] && [ ! -e "$SD/sortie.log" ]; then pass "save no sortie pod: the report is saved, and no sortie.log is invented"; else fail "save no sortie pod: exit $src"; fi
expect "$SL" "save no sortie pod: says the log is lost" ' the sortie pod of job/soak-sortie-abc is gone: no sortie\.log ' 1
expect "$SL" "save no sortie pod: no shell error about the missing log" 'No such file or directory' 0

# --- run.sh: a Job that has ended before the kickoff looks at it (#1387) --------
# The kickoff itself, run: `run.sh e2e` against a fake kubectl and a fake helm,
# up to its first look at the sortie pod, which is already `Failed` -- what a
# stage refused at an engine's execution cap does within a second of starting.
# run.sh used to abort there with the saver not yet armed, and that short run's
# report and results stream stayed on the PVC. Now the saver is armed first, the
# abort waits for it, and the run directory holds both files. On the fake PVC:
# the REAL report and stream of exactly that run on kind (sortie f0750ec, eight
# targets against engines capped at 4), so the last step is the gate grading
# what the kickoff saved.
mkdir -p "$TMP/runbin" "$TMP/run.fake"
cat >"$TMP/runbin/kubectl" <<'EOF'
#!/usr/bin/env bash
# What run.sh asks of the cluster; everything else is the saver's, and goes to
# the saver's fake.
args="$*"
case "$args" in
*" get --raw /readyz") ;;
*" get priorityclass "*) echo '{"value":1000,"preemptionPolicy":"Never","globalDefault":false}' ;;
*" get pods --no-headers") cat "$FAKE_RUN/pods.txt" ;;
*" get svc "*) ;;
*" get jobs -l "*" -o name") ;;
*" get jobs -l "*" -o jsonpath="*) printf 'soak-sortie-abc' ;;
*" get ds k6-soak-loader") exit 1 ;;
*" get pods -o json") echo '{"items":[]}' ;;
"--context fake apply -f -") cat >/dev/null ;;
*" get storageclass -o json") echo '{"items":[{"metadata":{"name":"standard"}}]}' ;;
*" rollout status "*) ;;
*" get ds soak-sortie-engine -o jsonpath="*) printf '2 2\n' ;;
*" get pods -l app.kubernetes.io/instance=soak,app.kubernetes.io/component=engine -o jsonpath="*)
	printf '10.10.1.10\tsortie-worker\tsoak-sortie-engine-7f9g2\n10.10.2.12\tsortie-worker2\tsoak-sortie-engine-gb7x4\n'
	;;
*" get endpointslices "*) echo '{"items":[{"endpoints":[{"conditions":{"ready":true},"addresses":["10.10.1.10"]},{"conditions":{"ready":true},"addresses":["10.10.2.12"]}]}]}' ;;
*" get pods -l job-name=soak-sortie-abc -o jsonpath={.items[0].status.phase}")
	# The kickoff's look at the pod. Was the saver armed by now?
	if [ -e "$FAKE_RUN_OUT/save.log" ]; then : >"$FAKE_RUN/armed-before-first-look"; fi
	cat "$FAKE_RUN/phase"
	;;
*) exec "$FAKE_SAVE_KUBECTL" "$@" ;;
esac
EOF
cat >"$TMP/runbin/helm" <<'EOF'
#!/usr/bin/env bash
# pull: leave a chart archive in the -d directory (the last argument).
# template: two images, both by digest. upgrade: nothing.
case "$*" in
*" pull "*) : >"${*: -1}/chart-sortie.tgz" ;;
"template "*) printf 'image: "quay.io/sortie/sortie@sha256:%064d"\nimage: "quay.io/sortie/engine@sha256:%064d"\n' 1 2 ;;
*" upgrade "*) ;;
*)
	echo "fake helm: unexpected call: $*" >&2
	exit 1
	;;
esac
EOF
chmod +x "$TMP/runbin/kubectl" "$TMP/runbin/helm"
cp "$PF/pods-ready.txt" "$TMP/run.fake/pods.txt"
printf Failed >"$TMP/run.fake/phase"
# run_abort <name>: the kickoff against the fake PVC $TMP/save-run-<name>.fake/pvc;
# leaves $RO (the run directory), $RL (run.sh's output) and $rc.
run_abort() {
	RO="$TMP/run-out-$1"
	RL="$TMP/run-$1.log"
	rm -f "$TMP/run.fake/armed-before-first-look"
	PATH="$TMP/runbin:$PATH" FAKE_RUN="$TMP/run.fake" FAKE_RUN_OUT="$RO" FAKE_SAVE="$TMP/save-run-$1.fake" FAKE_SAVE_KUBECTL="$TMP/kubectl-save" \
		SOAK_ENGINE_SETTLE=0 bash "$HERE/run.sh" e2e --context fake --out "$RO" --no-verify --statsd off --duration 120s \
		--targets "$SF/kind-capped.targets.txt" >"$RL" 2>&1
	rc=$?
}
mkdir -p "$TMP/save-run-both.fake/pvc"
cp "$SF/kind-capped.json" "$TMP/save-run-both.fake/pvc/r.json"
cp "$SF/kind-capped-stream.jsonl" "$TMP/save-run-both.fake/pvc/r.jsonl"
run_abort both
show "run.sh e2e: the sortie pod has already ended when the kickoff looks" "$RL"
if [ -r "$RO/save.log" ]; then show "... and the saver it armed" "$RO/save.log"; fi
if [ "$rc" -eq 1 ]; then pass "run abort: the kickoff still aborts (exit 1)"; else fail "run abort: exit $rc, want 1"; fi
expect "$RL" "run abort: it got as far as the Job (the fakes answered every call before it)" ' ENGINES READY backends=2 ' 1
if [ -e "$TMP/run.fake/armed-before-first-look" ]; then
	pass "run abort: the saver was armed before the kickoff's FIRST look at the pod (RED before #1387: it was armed after the check, so never)"
else
	fail "run abort: the saver's log did not exist yet when the kickoff first asked for the pod's phase"
fi
armed=$(grep -n ' saver armed ' "$RL" | head -n 1 | cut -d: -f1)
abort=$(grep -n ' ABORT the sortie pod of job/soak-sortie-abc is .Failed., not Running' "$RL" | head -n 1 | cut -d: -f1)
if [ -n "$armed" ] && [ -n "$abort" ] && [ "$armed" -lt "$abort" ]; then
	pass "run abort: ... and says so before it aborts"
else
	fail "run abort: 'saver armed' at line '${armed:-none}', the abort at line '${abort:-none}': the saver must be armed first"
fi
expect "$RL" "run abort: the abort says the run is saved and how to read it, with the saver's own last line" ' ABORT .* what it wrote is saved -- its report says why: sortie-gate\.sh --dir .*\[saver: SORTIE_SAVED dir=.* job=complete executions=8 pass=false not_run=0 .* stream_executions=8\]$' 1
# Armed that early, the saver has no T_LOAD yet: it must count from T_JOB, not from 1970.
expect "$RO/save.log" "run abort: the saver, armed before the load started, still has a clock (T_JOB)" " waiting for job/soak-sortie-abc \(plan 120s; giving up at $(date -u +%Y)-" 1
expect "$RO/run.env" "run abort: run.env has the Job, when it was found, and the load's start, once each" '^(JOB|T_JOB|T_LOAD)=' 3
if [ -e "$RO/SAVED" ] && cmp -s "$RO/report.json" "$SF/kind-capped.json" && cmp -s "$RO/results.jsonl" "$SF/kind-capped-stream.jsonl"; then
	pass "run abort: the short run's report and results stream are in the run directory, byte for byte"
else
	fail "run abort: the run directory does not hold the report and the stream (SAVED, report.json or results.jsonl is missing, or differs from the PVC)"
fi
G="$TMP/gate-run-abort.log"
bash "$GATE" --dir "$RO" >"$G" 2>&1
rc=$?
show "... and the gate on what the aborted kickoff saved" "$G"
if [ "$rc" -eq 1 ]; then pass "run abort: the gate grades it (exit 1)"; else fail "run abort: the gate gave exit $rc, want 1"; fi
expect "$G" "run abort: every target says why the Job died at once" '^FAIL  (tcp-[a-f]|uds-echo|uds-cr-echo)  .* -- REFUSED AT THE EXECUTION CAP, nothing of this stage ran: ' 8
expect "$G" "run abort: verdict" '^VERDICT FAIL targets=8 passed=0 failed=8 not_run=0 backends=2 lost_backends=0 report_pass=false$' 1
# The same abort when the report could NOT be saved (the pod died before it
# wrote one: only the results stream is on the PVC). The abort must not say the
# report is saved, nor send the reader to `sortie-gate.sh --dir`, which refuses
# a directory with a stream and no report: it passes on what the saver said.
mkdir -p "$TMP/save-run-stream.fake/pvc"
cp "$SF/kind-capped-stream.jsonl" "$TMP/save-run-stream.fake/pvc/r.jsonl"
run_abort stream
show "run.sh e2e: the pod has ended and left a results stream but no report" "$RL"
if [ "$rc" -eq 1 ] && [ ! -e "$RO/SAVED" ]; then pass "run abort, no report: exit 1 and no SAVED"; else fail "run abort, no report: exit $rc, and SAVED exists or the exit is wrong"; fi
expect "$RL" "run abort, no report: the abort does not claim a saved report" ' ABORT .*what it wrote is saved' 0
expect "$RL" "run abort, no report: it says the report could not be saved and passes on the saver's line, with the command that grades the stream" ' ABORT .* is .Failed., not Running .* its REPORT COULD NOT BE SAVED; .*: SORTIE_SAVE_FAILED no report .* the results stream IS saved: 8 execution\(s\) .* sortie-gate\.sh --dir .* --stream ' 1
if cmp -s "$RO/results.jsonl" "$SF/kind-capped-stream.jsonl"; then pass "run abort, no report: the results stream is in the run directory"; else fail "run abort, no report: results.jsonl is missing or differs"; fi

# A real report: the kind run of 2026-10-07 with sortie 9fcbb81 through run.sh
# e2e (two engines, 5 minutes, 43,200 of 43,200 requests), three of its eight
# targets. Nothing in it is made up; it is what the gate has to read.
G="$TMP/gate-kind.log"
bash "$GATE" --report "$SF/kind-clean.json" --shares "$SF/kind-clean.shares.tsv" --backends 2 --engines "$SF/kind-clean.engines.tsv" --per-node >"$G" 2>&1
rc=$?
show "gate: a real sortie 9fcbb81 report (kind, trimmed to three targets)" "$G"
if [ "$rc" -eq 0 ]; then pass "gate kind: exit 0"; else fail "gate kind: exit $rc, want 0"; fi
expect "$G" "gate kind: every target passes, with latency read from the report's statistics" '^PASS  (tcp-a|tcp-b|uds-echo)  rate=.* failures=none  lat p50<=[0-9.]+ms p99<=[0-9.]+ms max<=[0-9.]+ms$' 3
expect "$G" "gate kind: per node, with the engine's own start time" '^NODE  (tcp-a|tcp-b|uds-echo)  sortie-worker2?  http_2xx=[0-9]+/[0-9]+ rate=[0-9.]+/[39]rps started=2026-10-07T[0-9:.]+Z p50=' 6
expect "$G" "gate kind: the run's window" '^WINDOW  dispatched=2026-10-07T[0-9:.]+Z ended=2026-10-07T[0-9:.]+Z engines_started=' 1
expect "$G" "gate kind: verdict" '^VERDICT PASS targets=3 passed=3 failed=0 not_run=0 backends=2 lost_backends=0 report_pass=true$' 1

echo '{"not":"a report"}' >"$TMP/garbage.json"
bash "$GATE" --report "$TMP/garbage.json" --shares "$SF/shares.tsv" --backends 2 >"$TMP/gate-garbage.log" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then pass "gate: not a sortie report -> exit 2, not a verdict"; else fail "gate: garbage report gave exit $rc, want 2"; fi
bash "$GATE" --report "$TMP/nope.json" --shares "$SF/shares.tsv" --backends 2 >"$TMP/gate-nofile.log" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then pass "gate: no report file -> exit 2"; else fail "gate: missing report gave exit $rc, want 2"; fi

# --- the pins and the PriorityClass (sortie-values.yaml, run.sh) ----------------
# No cluster here, so these read the files: the three digests are digests, the
# engine has no CPU limit, and the sortie pod -- the run's single point of
# failure -- names the same PriorityClass as the engines, the one run.sh
# creates. The rendered Job is checked on kind (README, "Load driver: sortie").
VALUES="$HERE/sortie-values.yaml"
RUN="$HERE/run.sh"
expect "$VALUES" "values: both images are pinned by digest" '^ +ref: quay\.io/sortie/(sortie|engine)@sha256:[0-9a-f]{64}$' 2
expect "$VALUES" "values: no image by tag" '^ +ref: [^@]*$' 0
expect "$RUN" "run.sh: the chart is pinned by digest" '^SORTIE_CHART_DIGEST="\$\{SORTIE_CHART_DIGEST:-sha256:[0-9a-f]{64}\}"$' 1
commit=$(sed -n 's/^SORTIE_CHART_VERSION="0\.1\.0-\([0-9a-f]\{40\}\)"$/\1/p' "$RUN")
if [ -n "$commit" ] && grep -q "sortie $commit" "$VALUES"; then
	pass "pins: run.sh's chart version and sortie-values.yaml name the same sortie commit ($commit)"
else
	fail "pins: run.sh's chart version commit '$commit' is not the one sortie-values.yaml names"
fi
# The README states the pin too, and shows the command that verifies the chart:
# both have to be the pin, or a reader verifies the previous chart and is told
# it is this one. A bump that leaves the README behind fails here.
README="$HERE/README.md"
# shellcheck disable=SC2016 # the ${...} is run.sh's text, matched literally
digest=$(sed -n 's/^SORTIE_CHART_DIGEST="\${SORTIE_CHART_DIGEST:-\(sha256:[0-9a-f]\{64\}\)}"$/\1/p' "$RUN")
# lines <grep options> <text or pattern>: how many README lines match; 0 when
# the README cannot be read, so a missing file is a failure and not an error.
lines() { grep -c "$@" -- "$README" 2>/dev/null || true; }
# shellcheck disable=SC2016 # the backticks are the README's, not a substitution
if [ -n "$commit" ] && [ "$(lines -F -e 'The pin is sortie `'"$commit"'`, chart')" = 1 ] &&
	[ "$(lines -F -e '`0.1.0-'"$commit"'`')" = 1 ]; then
	pass "pins: the README names run.sh's sortie commit and chart version"
else
	fail "pins: the README does not say 'The pin is sortie \`$commit\`, chart \`0.1.0-$commit\`' (run.sh's pin)"
fi
if [ -n "$digest" ] && [ "$(lines -F -e "  quay.io/sortie/chart-sortie@$digest")" = 1 ] &&
	[ "$(lines -E -e 'quay\.io/sortie/chart-sortie@sha256:[0-9a-f]{64}')" = 1 ]; then
	pass "pins: the README's cosign command verifies run.sh's chart digest, and names no other"
else
	fail "pins: the README's cosign command does not verify run.sh's chart digest ($digest), or names another"
fi
expect "$RUN" "run.sh: the signer is sortie's publish workflow on main" '^SORTIE_SIGNER_IDENTITY=.*bpalermo/sortie/.*workflows/publish.*refs/heads/main' 1
pc=$(sed -n 's/^PRIORITY_CLASS="\(.*\)"$/\1/p' "$RUN")
expect "$VALUES" "values: the engines name run.sh's PriorityClass ($pc)" "^  priorityClassName: $pc\$" 1
expect "$VALUES" "values: the sortie pod names it too, through the chart's top-level knob" "^priorityClassName: $pc\$" 1
if awk '/^    limits:/ {l = 1; next} l && /^      cpu:/ {bad = 1} l && !/^      / {l = 0} END {exit bad}' "$VALUES"; then
	pass "values: no cpu under engine.resources.limits (no CPU limit on the engine)"
else
	fail "values: engine.resources.limits has a cpu entry (a throttled engine reports its own delay as latency)"
fi
expect "$VALUES" "values: the engine has a CPU request" '^      cpu: [0-9]+m$' 1
expect "$VALUES" "values: --progress is passed (a snapshot no longer copies histograms)" '^  - --progress$' 1

# ======================================================== churn.sh (begin)
# --- churn.sh: the schedule, and its stop (#1419) -------------------------------
# The driver itself, run: the whole 7h32m schedule against a fake kubectl, on a
# clock of its own, so that it takes a second. Three things are held:
#
#   the schedule   every kubectl call and the second after T0 it is made at,
#                  against testdata/churn/schedule.tsv -- which was written by
#                  the driver as it was BEFORE its waits were changed (#1419),
#                  run under this same fake. What the driver does to a cluster,
#                  and when, is in that file; a change to it has to be meant.
#   the stop       a TERM before each command the driver runs, and during each
#                  kubectl call, must end it with exit 143, leave no process in
#                  its session, and leave the clock where it was (a clock that
#                  moves on is a wait that ended: the schedule went on).
#   the leftovers  a run that finishes leaves nothing either. The old driver
#                  left each uds-csi step's `kubectl get pods -w` behind.
#
# The clock. $FAKE_CLOCK holds the time. For the `date` and `sleep` the driver
# still calls, fakes on PATH read and move it. For the two builtins of
# lib-wait.sh, which PATH cannot replace, a file loaded through BASH_ENV (into
# churn.sh only) defines `printf` and `read` as functions: see its comment.
CHURN="$HERE/churn.sh"
CF="$HERE/testdata/churn"
CB="$TMP/churn-bin"
CHURN_EPOCH=1800000000
mkdir -p "$CB" "$TMP/churn-bin-real"
FAKE_REAL_DATE="$(command -v date)"
FAKE_REAL_SLEEP="$(command -v sleep)"
export FAKE_REAL_DATE FAKE_REAL_SLEEP
cat >"$CB/kubectl" <<'EOF'
#!/usr/bin/env bash
# Answers what churn.sh and sample-proxy-rss.sh ask of a cluster, and logs every
# call with the driver's clock, seconds after T0.
args="$*"
read -r now <"$FAKE_CLOCK"
# One line a call: the path of the new-SA client script is the checkout's, not
# part of the schedule.
logged="${args/=\/*\/newsa-client.sh/=<soak>/newsa-client.sh}"
printf '%s\t%s\n' "$((now - FAKE_EPOCH))" "$logged" >>"$FAKE_STATE/calls.tsv"
# This call's number: the lowest not taken (noclobber makes the claim atomic;
# the TRIPLE's three calls, and the two ends of a pipeline, run at once).
n=1
set -C
until : 2>/dev/null >"$FAKE_STATE/call.$n"; do n=$((n + 1)); done
set +C
# FAKE_TERM_AT_CALL=n: on the n-th call, send TERM to the caller's session
# leader (the script under test, started under setsid), stay in the foreground
# for another 50 ms, and note whether the leader is still there: one that
# waited for this call is, one that died under it is not.
if [ "${FAKE_TERM_AT_CALL:-}" = "$n" ]; then
	leader="$(ps -o sid= -p "$$" | tr -d ' ')"
	echo "$n $now" >"$FAKE_STATE/term-at"
	kill -TERM "$leader"
	"$FAKE_REAL_SLEEP" 0.05
	if kill -0 "$leader" 2>/dev/null; then : >"$FAKE_STATE/waited-for"; fi
fi
down='pod=aether-uds-csi-old del=2026-10-05T06:07:08Z ready=True'
case "$args" in
*" get --raw /readyz") echo ok ;;
*" auth can-i "*) echo yes ;;
*" get namespace "*) echo namespace/aether-test ;;
*" get nodes "*) printf 'node-1 True \nnode-2 True \n' ;;
*" rollout restart "*)
	case "$args" in *aether-uds-csi*) : >"$FAKE_STATE/csi-restarted" ;; esac
	# FAKE_HANG_AT=<second after T0>: a restart made at that second does not
	# return until the driver's log says it is waiting for its children (or
	# 20 s have gone by: then the test fails on what the log does not say).
	if [ "${FAKE_HANG_AT:-}" = "$((now - FAKE_EPOCH))" ]; then
		for _ in $(seq 1 1000); do
			if grep -q 'STOP: still waiting' "$SOAK_CHURN_LOG" 2>/dev/null; then break; fi
			"$FAKE_REAL_SLEEP" 0.02
		done
		"$FAKE_REAL_SLEEP" 0.2
		# As for FAKE_TERM_AT_CALL: is the driver (the session leader) still
		# there, 200 ms after it said it was waiting for this call?
		if kill -0 "$(ps -o sid= -p "$$" | tr -d ' ')" 2>/dev/null; then : >"$FAKE_STATE/hang-waited-for"; fi
	fi
	echo restarted
	;;
*" scale "*"--replicas=0")
	# FAKE_FAIL_SCALE_DOWN=1: the scale to zero is refused (exit 1).
	if [ -n "${FAKE_FAIL_SCALE_DOWN:-}" ]; then
		echo "error: the server was unable to return a response in the time allotted" >&2
		exit 1
	fi
	echo done
	;;
*" rollout status "* | *" scale "* | *" delete "* | *" wait --for=delete "*) echo done ;;
*"-o jsonpath={.spec.replicas}"*) printf 3 ;;
*" create configmap "*) printf 'apiVersion: v1\nkind: ConfigMap\n' ;;
*" apply -f -"*) cat >/dev/null ;;
*" logs deployment/sa-new-"*) echo "AETHER_NEWSA_FINAL step=x node=node-1 svc-1:ok=2400,non2xx=0,connerr=0,codes=- svc-3:ok=2400,non2xx=0,connerr=0,codes=-" ;;
*" -w -o jsonpath="*)
	# The plugin watch: the pod as it is, then (once the DaemonSet has been
	# restarted) terminating, then nothing more until it is killed. exec, so
	# that this process is what a kill of the watch has to reach.
	echo "pod=aether-uds-csi-old del= ready=True"
	until [ -e "$FAKE_STATE/csi-restarted" ]; do read -r -t 0.02 _ <><(:); done
	echo "$down"
	exec "$FAKE_REAL_SLEEP" 600
	;;
*"spec.nodeName="*"{.items[0].metadata.name}"*) printf aether-uds-csi-old ;;
*"spec.nodeName="*"{range .items[*]}pod="*) echo "$down" ;;
*" get pods -l app="*"deletionTimestamp"*) printf 'uds-echo-old node-1 \n' ;;
*" get pods -l app="*"{.items[*].metadata.name}"*) printf uds-echo-old ;;
*" get pods -l app="*"status.conditions"*) printf 'uds-echo-old False\nuds-echo-new True\n' ;;
*" get pods -l app="*" -o name") echo pod/uds-echo-old ;;
*"{.spec.nodeName}"*) echo node-1 ;;
*" get csinode "*) printf csi.aether.io ;;
*" get events "*) ;;
# The sampler's two: a proxy pod born this second (so --at-age keeps polling),
# and its working set.
*" get pods -n aether-system -l app.kubernetes.io/name=aether-proxy "*) echo "aether-proxy-1 node-1 $("$FAKE_REAL_DATE" -u -d "@$now" +%FT%TZ)" ;;
*" top pods "*) echo "aether-proxy-1 proxy 10m 100Mi" ;;
*" -o name") echo "${args##* get }" | cut -d' ' -f1 ;;
*)
	echo "fake kubectl: unexpected call: $args" >&2
	exit 1
	;;
esac
EOF
cat >"$CB/date" <<'EOF'
#!/usr/bin/env bash
# The time is $FAKE_CLOCK, in whatever format is asked for.
read -r now <"$FAKE_CLOCK"
case "$*" in
*" -d "*) exec "$FAKE_REAL_DATE" "$@" ;;
*) exec "$FAKE_REAL_DATE" -d "@$now" "$@" ;;
esac
EOF
cat >"$CB/sleep" <<'EOF'
#!/usr/bin/env bash
# A whole number of seconds moves the clock and returns at once; a fraction is
# a real sleep and the clock stands (those are polls that wait for another
# process, not for the schedule).
case "$1" in
0) ;;
*[!0-9]*) exec "$FAKE_REAL_SLEEP" "$1" ;;
*)
	read -r now <"$FAKE_CLOCK"
	echo $((now + $1)) >"$FAKE_CLOCK"
	;;
esac
EOF
chmod +x "$CB/kubectl" "$CB/date" "$CB/sleep"
cp "$CB/kubectl" "$TMP/churn-bin-real/"
cat >"$TMP/churn-hook.sh" <<'EOF'
case "${0##*/}" in churn.sh) ;; *) return 0 ;; esac
# The driver's clock for the two builtins lib-wait.sh uses. `printf -v VAR
# '%(...)T' -1` reads $FAKE_CLOCK. A whole-second `read -t N -u 9` in the main
# shell moves the clock by N and returns as a timeout would: the schedule runs
# without waiting. A fraction (a poll for another process), and any wait in a
# subshell (a queued sampler, which then looks at the clock again), is a real
# wait of 20 ms.
printf() {
	if [ "$#" -eq 4 ] && [ "$1" = -v ] && [ "$4" = -1 ]; then
		local __c
		builtin read -r __c <"$FAKE_CLOCK"
		builtin printf -v "$2" "$3" "$__c"
		return
	fi
	builtin printf "$@"
}
read() {
	if [ "$#" -eq 6 ] && [ "$2" = -t ] && [ "$5" = 9 ]; then
		case "$BASHPID:$3" in
		"$$":*[!0-9]*) builtin read -r -t 0.02 -u 9 _ ;;
		"$$":*)
			local __c
			builtin read -r __c <"$FAKE_CLOCK"
			echo $((__c + $3)) >"$FAKE_CLOCK"
			;;
		*) builtin read -r -t 0.02 -u 9 _ ;;
		esac
		return 142
	fi
	builtin read "$@"
}
# A trace of the main shell's commands (CH_TRACE: number, line, command), and a
# TERM before one of them (CH_TERM_AT), noted in term-at with the clock.
#
# CH_TERM_AT names a code point, not a count: "<queued><TAB><line><TAB><command>",
# the first time the main shell is about to run that command on that line
# (<queued>: 1 once an RSS sampler has been queued, else 0). A count does not
# name the same command in two runs: the uds-csi step polls for another process
# (the plugin watch) and goes round a loop a number of times that depends on
# the machine, so "the 277th command" of one run was past the end of another
# (seen in CI: `exit 0; no TERM was sent`).
__ch_n=0
__ch_q=0
__ch_sent=""
__ch_hook() {
	[ "$BASHPID" = "$$" ] || return 0
	case "${FUNCNAME[1]:-}" in printf | read) return 0 ;; esac
	__ch_n=$((__ch_n + 1))
	local __cmd="${1//[$'\n\t']/ }"
	case "$__cmd" in 'log "RSS SAMPLE queued'*) __ch_q=1 ;; esac
	if [ -n "${CH_TRACE:-}" ]; then builtin printf '%s\t%s\t%s\n' "$__ch_n" "$2" "$__cmd" >>"$CH_TRACE"; fi
	if [ -z "$__ch_sent" ] && [ -n "${CH_TERM_AT:-}" ] && [ "$__ch_q"$'\t'"$2"$'\t'"$__cmd" = "$CH_TERM_AT" ]; then
		__ch_sent=1
		local __c
		builtin read -r __c <"$FAKE_CLOCK"
		echo "$__ch_n $__c" >"$FAKE_STATE/term-at"
		kill -TERM "$$"
	fi
	return 0
}
if [ -n "${CH_TRACE:-}${CH_TERM_AT:-}" ]; then
	set -T
	trap '__ch_hook "$BASH_COMMAND" "$LINENO"' DEBUG
fi
EOF

# churn_start <dir> <mode> [VAR=value ...]: the driver in the background under
# setsid, as run.sh starts it, on a clock of its own. <mode> is `full` (the
# whole schedule) or one of churn.sh's own --new-sa-once / --uds-csi-once.
# Leaves $cpid (to wait for; a `timeout` ends a driver that never exits) and
# $csid (its session).
churn_start() {
	local dir="$1" mode="$2"
	shift 2
	local args=(--context fake dry-run)
	[ "$mode" = full ] || args=(--context fake "$mode")
	rm -rf "$dir" && mkdir -p "$dir"
	echo "$CHURN_EPOCH" >"$dir/clock"
	# shellcheck disable=SC2016 # $$ and $@ are the inner shell's
	env PATH="$CB:$PATH" FAKE_STATE="$dir" FAKE_CLOCK="$dir/clock" FAKE_EPOCH="$CHURN_EPOCH" \
		BASH_ENV="$TMP/churn-hook.sh" SOAK_NAP_SLICE=100000 \
		SOAK_CHURN_LOG="$dir/churn.log" SOAK_PROXY_RSS_TSV="$dir/rss.tsv" "$@" \
		timeout -s KILL "${CHURN_TIMEOUT:-60}" setsid bash -c 'echo "$$" >"$1"; shift; exec bash "$@"' _ "$dir/pid" \
		"$CHURN" "${args[@]}" >"$dir/stdout" 2>"$dir/stderr" &
	cpid=$!
	for _ in $(seq 1 200); do
		[ -s "$dir/pid" ] && break
		sleep 0.01
	done
	csid=$(cat "$dir/pid" 2>/dev/null)
}

# The schedule is the one it was. (The sampler is a stub here, queued with no
# delay: the real one polls a live proxy pod's age.)
cat >"$TMP/sampler-stub.sh" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_STATE/sampler.calls"
EOF
R="$TMP/churn-golden"
churn_start "$R" full SOAK_PROXY_RSS_SAMPLER="$TMP/sampler-stub.sh" SOAK_PROXY_RSS_DELAY=0
wait "$cpid"
rc=$?
left=$(survivors "$csid")
# Sorted by second, then by call (in the C locale, so that it is the same file
# everywhere): the TRIPLE's three rolls are made at once and land in any order.
LC_ALL=C sort -t$'\t' -k1,1n -k2 "$R/calls.tsv" >"$R/calls.sorted"
# HARNESS_WRITE_SCHEDULE=<file> keeps this run's schedule: how the golden file
# is written again when a change to the schedule is meant.
if [ -n "${HARNESS_WRITE_SCHEDULE:-}" ]; then cp "$R/calls.sorted" "$HARNESS_WRITE_SCHEDULE"; fi
if [ "$rc" -eq 0 ]; then pass "churn dry run: the whole schedule runs against the fake cluster (exit 0)"; else fail "churn dry run: exit $rc: $(tail -n 3 "$R/stderr")"; fi
if cmp -s "$R/calls.sorted" "$CF/schedule.tsv"; then
	pass "churn dry run: every kubectl call, and the second after T0 it is made at, is the schedule the driver had before #1419 ($(grep -c '' "$R/calls.sorted") calls)"
else
	fail "churn dry run: the schedule CHANGED (what the driver does to the cluster, or when). If that is meant, say so and write testdata/churn/schedule.tsv again (HARNESS_WRITE_SCHEDULE): $(diff "$CF/schedule.tsv" "$R/calls.sorted" | head -n 6)"
fi
expect "$R/churn.log" "churn dry run: 35 ROLLED lines (33 rolls and two new-SA steps)" ' ROLLED ' 35
expect "$R/churn.log" "churn dry run: it says it is complete" ' churn driver complete \(33 rolls: ' 1
if [ "$(grep -c '' "$R/sampler.calls" 2>/dev/null)" = 6 ]; then pass "churn dry run: six RSS samplers queued, and each ran"; else fail "churn dry run: $(grep -c '' "$R/sampler.calls" 2>/dev/null) sampler runs, want 6"; fi
if [ -z "$left" ]; then
	pass "churn dry run: a driver that has finished leaves no process behind (RED before #1419: each uds-csi step's \`kubectl get pods -w\`)"
else
	fail "churn dry run: left behind: $left"
fi
# The two waits that were a `sleep` took `90s`; the clock arithmetic does not.
SOAK_SHRINK_SECONDS=90s PATH="$CB:$PATH" FAKE_STATE="$TMP" FAKE_CLOCK="$R/clock" FAKE_EPOCH="$CHURN_EPOCH" SOAK_CHURN_LOG="$TMP/churn-refused.log" \
	bash "$CHURN" --context fake --preflight >"$TMP/churn-refused.out" 2>&1
rc=$?
if [ "$rc" -eq 2 ] && grep -q 'SOAK_SHRINK_SECONDS take whole seconds' "$TMP/churn-refused.out" && [ ! -e "$TMP/churn-refused.log" ]; then
	pass "churn: SOAK_SHRINK_SECONDS=90s is refused before T0 (exit 2, no log), not found out at T0+450m"
else
	fail "churn: SOAK_SHRINK_SECONDS=90s gave exit $rc: $(cat "$TMP/churn-refused.out")"
fi

# TERM during a real wait: the real clock, no hook. The driver is waiting for
# its first roll, twelve minutes away. RED before #1419: its `sleep 720` was in
# the foreground, bash ran the trap only when that ended, and the driver was
# still there twelve minutes later (here: until the test's timeout killed it,
# exit 137, with the sleep left behind).
R="$TMP/churn-term"
CHURN_TIMEOUT=10 churn_start "$R" full BASH_ENV= SOAK_NAP_SLICE= PATH="$TMP/churn-bin-real:$PATH"
for _ in $(seq 1 100); do
	if grep -q 'churn driver context=' "$R/churn.log" 2>/dev/null; then break; fi
	sleep 0.05
done
sleep 0.3
t0=$(date +%s)
kill -TERM "$csid"
wait "$cpid"
rc=$?
left=$(survivors "$csid")
if [ "$rc" -eq 143 ] && [ $(($(date +%s) - t0)) -lt 5 ]; then
	pass "churn term: TERM while it waits for the first roll ends it at once (exit 143)"
else
	fail "churn term: exit $rc after $(($(date +%s) - t0))s (137: it did not stop, and the test's timeout killed it)"
fi
if [ -z "$left" ]; then pass "churn term: no process of the driver is left"; else fail "churn term: left behind: $left"; fi
if [ "$(grep -c '' "$R/calls.tsv")" -eq "$(awk -F'\t' '$1 == 0' "$CF/schedule.tsv" | grep -c '')" ]; then
	pass "churn term: and nothing was rolled (only the pre-flight's calls were made)"
else
	fail "churn term: $(grep -c '' "$R/calls.tsv") calls were made; the pre-flight alone is $(awk -F'\t' '$1 == 0' "$CF/schedule.tsv" | grep -c '')"
fi

# TERM at EVERY point, as for the watchdog above: a DEBUG trap counts the
# commands the main shell runs and sends TERM before the k-th. One traced run
# lists them; then one run per code point (line and command), several at a
# time. A full run comes round the same lines 33 times, so it is swept once per
# point, and once more for the points it passes again after an RSS sampler has
# been queued: from then on there is a child to stop. The two steps are swept
# through churn.sh's own --new-sa-once and --uds-csi-once, which run the same
# code right after a short pre-flight.
CJ="$(nproc 2>/dev/null || echo 2)"
[ "$CJ" -gt 8 ] && CJ=8
pool() { while [ "$(jobs -pr | grep -c '')" -ge "$CJ" ]; do wait -n; done; }
# churn_one <dir> <what> <mode> [VAR=value ...]: one run that is sent a TERM.
# Writes <dir>/result: empty when it stopped cleanly, else what was wrong.
#
# With CHURN_MAY_MISS set (the sweep), a run that ends by itself (exit 0,
# nothing left) without having come to its TERM point is not a TERM that was
# mishandled: the point is on a branch this run did not take (a poll that had
# its answer the first time round). It is run again, three times at most, and
# then written to <dir>/unreached: the sweep counts those apart and bounds them.
churn_one() {
	local dir="$1" what="$2" rc left bad clock try
	shift 2
	for try in 1 2 3; do
		bad=""
		churn_start "$dir" "$@"
		wait "$cpid"
		rc=$?
		left=$(survivors "$csid")
		if [ -n "${CHURN_MAY_MISS:-}" ] && [ "$rc" -eq 0 ] && [ -z "$left" ] && [ ! -e "$dir/term-at" ]; then
			if [ "$try" -lt 3 ]; then continue; fi
			echo "  $what" >"$dir/unreached"
			break
		fi
		[ "$rc" -eq 143 ] || bad="$bad exit $rc;"
		[ -z "$left" ] || bad="$bad left behind: $left;"
		if read -r _ clock <"$dir/term-at" 2>/dev/null; then
			[ "$(cat "$dir/clock")" = "$clock" ] || bad="$bad the schedule went on after the TERM (clock $clock -> $(cat "$dir/clock"));"
			if [ -n "${EXPECT_WAITED:-}" ] && [ ! -e "$dir/waited-for" ]; then bad="$bad the call in the foreground was NOT waited for;"; fi
		else
			bad="$bad no TERM was sent;"
		fi
		break
	done
	if [ -n "$bad" ]; then echo "  $what:$bad" >"$dir/result"; else : >"$dir/result"; fi
}
: >"$TMP/churn-seen"
SWEPT=0
SWEEP_UNREACHED_MAX=12
sweep() { # sweep <name> <mode> [VAR=value ...]
	local name="$1" mode="$2" k n point key
	shift 2
	churn_start "$TMP/tr-$name" "$mode" CH_TRACE="$TMP/tr-$name.tsv" "$@"
	wait "$cpid"
	survivors "$csid" >/dev/null
	awk -F'\t' -v seenf="$TMP/churn-seen" '
		BEGIN { while ((getline l < seenf) > 0) seen[l] = 1 }
		$3 ~ /^log "RSS SAMPLE queued/ { queued = 1 }
		{ key = (queued + 0) "\t" $2 "\t" $3 }
		!seen[key]++ { print $1 "\t" key; print key >> seenf }' "$TMP/tr-$name.tsv" >"$TMP/points-$name"
	n=$(grep -c '' "$TMP/points-$name")
	echo "      churn term sweep, $name: $(grep -c '' "$TMP/tr-$name.tsv") commands traced, $n points not swept yet"
	# k is the command's number in the traced run and only names the run's
	# directory; the TERM point is the key (see the hook).
	while IFS= read -r point; do
		k="${point%%$'\t'*}"
		key="${point#*$'\t'}"
		pool
		CHURN_MAY_MISS=1 churn_one "$TMP/sw-$name-$k" "$name, TERM before \`${key#*$'\t'*$'\t'}\` (line $(cut -f2 <<<"$key"), command $k of the traced run)" "$mode" CH_TERM_AT="$key" "$@" &
	done <"$TMP/points-$name"
	wait
	SWEPT=$((SWEPT + n))
}
sweep full full SOAK_NEWSA=0 SOAK_UDSCSI=0
sweep newsa --new-sa-once
sweep udscsi --uds-csi-once
bad=$(cat "$TMP"/sw-*/result 2>/dev/null)
nres=$(find "$TMP" -path '*/sw-*/result' | grep -c '')
unreached=$(cat "$TMP"/sw-*/unreached 2>/dev/null)
nun=$(find "$TMP" -path '*/sw-*/unreached' | grep -c '')
# Not reached again in three runs: a point on a branch only some runs take.
# Those are the polls of the uds-csi step, a handful of commands; more than
# that, and the sweep is no longer sweeping what it traced.
if [ "$nun" -gt 0 ]; then
	echo "      churn term sweep: $nun traced point(s) not reached again in three runs (a branch only some runs take):"
	echo "$unreached"
fi
if [ "$SWEPT" -ge 300 ] && [ "$nres" -eq "$SWEPT" ] && [ -z "$bad" ] && [ "$nun" -le "$SWEEP_UNREACHED_MAX" ]; then
	pass "churn term sweep: TERM before each of $((SWEPT - nun)) commands (the schedule, a TRIPLE, the SHRINK, a new-SA step, a uds-csi step, with and without a queued RSS sampler): exit 143, no process left, the schedule stops, every time"
else
	fail "churn term sweep: $nres of $SWEPT runs reported (300 or more expected), $nun point(s) not reached (at most $SWEEP_UNREACHED_MAX); not handled cleanly:
$(echo "$bad" | head -n 12)"
fi
# Two of those runs, looked at: a stop undoes what the driver had in hand.
k=$(awk -F'\t' '$3 == "nap \"$SHRINK_SECONDS\"" {print $1; exit}' "$TMP/tr-full.tsv")
if [ -n "$k" ] && [ "$(grep -c ' scale deployment/svc-5 --replicas=3$' "$TMP/sw-full-$k/calls.tsv")" -eq 1 ] && grep -q ' SHRINK done aether-test/deployment/svc-5 restored to replicas=3$' "$TMP/sw-full-$k/churn.log"; then
	pass "churn term: TERM inside the SHRINK window restores the replicas before the driver exits"
else
	fail "churn term: TERM inside the SHRINK window (command '${k:-not traced}'): its scale calls were '$(grep ' scale ' "$TMP/sw-full-$k/calls.tsv" 2>/dev/null | tr '\n' '|')'"
fi
k=$(awk -F'\t' '$3 == "nap 10" {print $1; exit}' "$TMP/tr-newsa.tsv")
if [ -n "$k" ] && [ -e "$TMP/sw-newsa-$k/term-at" ] && [ "$(grep -c ' delete deployment/sa-new-[0-9]* configmap/sa-new-[0-9]* serviceaccount/sa-new-[0-9]* ' "$TMP/sw-newsa-$k/calls.tsv")" -eq 1 ]; then
	pass "churn term: TERM inside a new-SA step deletes the step's three objects before the driver exits"
else
	fail "churn term: TERM inside a new-SA step (command '${k:-not traced}'): last call '$(tail -n 1 "$TMP/sw-newsa-$k/calls.tsv" 2>/dev/null)'"
fi

# A stop waits for a child for as long as the child takes. The TRIPLE's three
# `rollout restart` calls are made to hang here (FAKE_HANG_AT: until the
# driver's log says it is waiting for them, so no time is guessed), and the
# TERM comes once all three are under way. RED with a bounded wait: after its
# 100 tries the driver said `left behind` and exited ahead of three rolls that
# were still in flight, whose ROLLED lines then landed in the log of a driver
# that was gone.
key=$(awk -F'\t' '$3 ~ /^log "RSS SAMPLE queued/ { q = 1 } !f && $3 == "wait \"$t_agent\"" { print q + 0 "\t" $2 "\t" $3; f = 1 }' "$TMP/tr-full.tsv")
R="$TMP/churn-stopwait"
churn_start "$R" full SOAK_NEWSA=0 SOAK_UDSCSI=0 FAKE_HANG_AT=18000 CH_TERM_AT="$key"
wait "$cpid"
rc=$?
left=$(survivors "$csid")
after=$(awk '/ STOP: still waiting after 20s for: / { s = 1 } s && / ROLLED / { n++ } END { print n + 0 }' "$R/churn.log" 2>/dev/null)
if [ -n "$key" ] && [ -e "$R/term-at" ] && [ "$rc" -eq 143 ] && [ -z "$left" ] && [ -e "$R/hang-waited-for" ] &&
	[ "$(grep -c ' STOP: still waiting after 20s for: ' "$R/churn.log")" -eq 1 ] && [ "$after" -eq 3 ]; then
	pass "churn term: a stop during the TRIPLE waits for rolls that take longer than 100 tries: it says so once, the driver is still there when they return, all three are logged, and only then does it exit (143, no process left)"
else
	fail "churn term: a stop with three rolls that do not return (point '${key:-not traced}'): exit $rc, left behind: ${left:-nothing}, the driver was $([ -e "$R/hang-waited-for" ] && echo "still there" || echo "GONE") when the rolls returned, ROLLED after the STOP line: $after, log tail: $(tail -n 4 "$R/churn.log" 2>/dev/null | tr '\n' '|')"
fi

# ... and INSIDE a command: TERM while a kubectl call is in the foreground, sent
# by the fake itself (see the watchdog's test above for why not by a timer).
# The call must be waited for, not left: every call of the two steps, and of a
# full run one of each kind and each of the TRIPLE's three, which run in the
# background and are waited for all the same. The plugin watch is the one call
# that is killed instead, and the sweep above covers it.
INCALL=0
incall() { # incall <name> <mode> [VAR=value ...]
	local name="$1" mode="$2" k n
	shift 2
	churn_start "$TMP/ic-$name" "$mode" "$@"
	wait "$cpid"
	survivors "$csid" >/dev/null
	awk -F'\t' -v mode="$mode" '
		/ -w -o jsonpath=/ { next }
		{ kind = $2; gsub(/svc-[0-9]|sa-new-[0-9]+/, "N", kind) }
		mode != "full" || $1 == 18000 || !seen[kind]++ { print NR }' "$TMP/ic-$name/calls.tsv" >"$TMP/calls-$name"
	n=$(grep -c '' "$TMP/calls-$name")
	while read -r k; do
		pool
		EXPECT_WAITED=1 churn_one "$TMP/sw-ic-$name-$k" "$name, TERM during kubectl call $k, \`$(sed -n "${k}p" "$TMP/ic-$name/calls.tsv" | cut -f2 | cut -c1-100)\`" "$mode" FAKE_TERM_AT_CALL="$k" "$@" &
	done <"$TMP/calls-$name"
	wait
	INCALL=$((INCALL + n))
}
incall full full SOAK_NEWSA=0 SOAK_UDSCSI=0 SOAK_PROXY_RSS=0
incall newsa --new-sa-once
incall udscsi --uds-csi-once
bad=$(cat "$TMP"/sw-ic-*/result 2>/dev/null)
nres=$(find "$TMP" -path '*/sw-ic-*/result' | grep -c '')
if [ "$INCALL" -ge 40 ] && [ "$nres" -eq "$INCALL" ] && [ -z "$bad" ]; then
	pass "churn term in a call: TERM during each of $INCALL kubectl calls: the call is waited for, exit 143, no process left, the schedule stops"
else
	fail "churn term in a call: $nres of $INCALL runs reported (40 or more expected); not handled cleanly:
$(echo "$bad" | head -n 12)"
fi

# One of those runs, looked at (review of #1458): TERM during the SHRINK's
# scale-to-zero call itself. The trap runs when the call has returned, and the
# cluster has the scale by then. RED before: SHRINK_PREV was set only after the
# call, so the stop had nothing to restore and exited 143 with the target at 0
# (no `--replicas=3` call, no `SHRINK done` line).
k=$(awk -F'\t' '!f && $2 ~ / scale deployment\/svc-5 --replicas=0$/ { print NR; f = 1 }' "$TMP/ic-full/calls.tsv")
R="$TMP/sw-ic-full-$k"
if [ -n "$k" ] && [ -e "$R/term-at" ] && [ "$(grep -c ' scale deployment/svc-5 --replicas=3$' "$R/calls.tsv")" -eq 1 ] && grep -q ' SHRINK done aether-test/deployment/svc-5 restored to replicas=3$' "$R/churn.log"; then
	pass "churn term in a call: TERM during the SHRINK's scale-to-zero call still restores the replicas before the driver exits"
else
	fail "churn term in a call: TERM during the scale to zero (call '${k:-not found}'): its scale calls were '$(grep ' scale ' "$R/calls.tsv" 2>/dev/null | cut -f2 | tr '\n' '|')', log tail: $(tail -n 2 "$R/churn.log" 2>/dev/null | tr '\n' '|')"
fi
# A scale to zero that fails may have been applied all the same (a call that
# timed out): the driver restores, then aborts. RED before: `leaving it at 3`,
# and no restore.
R="$TMP/churn-scale-refused"
churn_start "$R" full SOAK_NEWSA=0 SOAK_UDSCSI=0 SOAK_PROXY_RSS=0 FAKE_FAIL_SCALE_DOWN=1
wait "$cpid"
rc=$?
left=$(survivors "$csid")
if [ "$rc" -eq 1 ] && [ -z "$left" ] && [ "$(grep -c ' scale deployment/svc-5 --replicas=3$' "$R/calls.tsv")" -eq 1 ] &&
	grep -q ' SHRINK done aether-test/deployment/svc-5 restored to replicas=3$' "$R/churn.log" && grep -q ' CHURN ABORTED: SHRINK could not scale ' "$R/churn.log"; then
	pass "churn: a scale to zero that fails is restored all the same, and the run aborts (exit 1)"
else
	fail "churn: a failed scale to zero: exit $rc, left behind: ${left:-nothing}, scale calls '$(grep ' scale ' "$R/calls.tsv" 2>/dev/null | cut -f2 | tr '\n' '|')', log tail: $(tail -n 3 "$R/churn.log" 2>/dev/null | tr '\n' '|')"
fi

# The sampler alone, as churn.sh queues it and as run.sh starts it: TERM during
# each of its three kubectl calls. Untrapped, TERM killed the shell and left the
# call; and its last listing fed a `while read` loop through a process
# substitution, which bash does not wait for (RED: call 3 was not waited for).
bad=""
for n in 1 2 3; do
	S="$TMP/rss-$n"
	rm -rf "$S" && mkdir -p "$S"
	echo "$CHURN_EPOCH" >"$S/clock"
	# shellcheck disable=SC2016 # $$ and $@ are the inner shell's
	env PATH="$TMP/churn-bin-real:$PATH" FAKE_STATE="$S" FAKE_CLOCK="$S/clock" FAKE_EPOCH="$CHURN_EPOCH" FAKE_TERM_AT_CALL="$n" \
		SOAK_PROXY_RSS_TSV="$S/rss.tsv" SOAK_PROXY_RSS_MAX_WAIT=0 \
		timeout -s KILL 10 setsid bash -c 'echo "$$" >"$1"; shift; exec bash "$@"' _ "$S/pid" \
		"$HERE/sample-proxy-rss.sh" --context fake --at-age 1800 >"$S/stdout" 2>"$S/stderr" &
	spid=$!
	wait "$spid"
	rc=$?
	left=$(survivors "$(cat "$S/pid")")
	if [ "$rc" -ne 143 ] || [ -n "$left" ] || [ ! -e "$S/waited-for" ]; then
		bad="$bad
  TERM during the sampler's kubectl call $n: exit $rc, the call was $([ -e "$S/waited-for" ] && echo "waited for" || echo "NOT waited for"), left behind: ${left:-nothing}"
	fi
done
if [ -z "$bad" ]; then
	pass "sampler term: TERM during each of sample-proxy-rss.sh's 3 kubectl calls: the call is waited for, exit 143, no process left"
else
	fail "sampler term:$bad"
fi
# ========================================================== churn.sh (end)

# ================================================= prober-grade.sh (begin)
# --- prober-grade.sh: the prober SLI and the unpinned-cluster counter, from raw
# counters (#1390, #1423) ---------------------------------------------------------
# Against canned Prometheus query responses in testdata/prober-grade/<scenario>/
# (the HTTP API's shape; the numbers are made up, except that `born` has the
# 2026-10-08 run's two timeout series: 21 + 19 raw, 37 by increase()). A fake
# curl serves <metric>.<start|start-time|window>.json by what the query asks for
# (the metric, `timestamp(metric)`, `metric[Ns]`) and logs each query; a fake
# kubectl serves the same through the API-server proxy path.
#
# A <metric>.window.json holds the samples the store has. The fake answers
# `metric[Ns]` at time T with the ones a server returns for it, and with no
# series that has none of them. Since Prometheus 3.0 that is the samples in
# (T - N, T]: a range selector is left-open, and a sample exactly at T - N is
# NOT returned. Before 3.0 it was [T - N, T]; FAKE_PROM_CLOSED_LEFT=1 answers
# that way. (Held against the pinned Prometheus, 3.15.0: README.md, "Grading".)
GRADE="$HERE/prober-grade.sh"
GF="$HERE/testdata/prober-grade"
GT0=1791418200 # 2026-10-08T00:10:00Z
mkdir -p "$TMP/grade-bin"
cat >"$TMP/grade-bin/range" <<'EOF'
#!/usr/bin/env bash
# range FILE N T: the samples of FILE that a range query of N seconds at T returns.
exec jq -c --argjson n "$2" --argjson t "$3" --arg closed "${FAKE_PROM_CLOSED_LEFT:-}" '
	.data.result |= (map(.values |= map(select(.[0] <= $t and (.[0] > $t - $n or ($closed != "" and .[0] == $t - $n)))))
		| map(select((.values | length) > 0)))' "$1"
EOF
cat >"$TMP/grade-bin/curl" <<'EOF'
#!/usr/bin/env bash
# curl -fsS --max-time N --get URL --data-urlencode query=Q --data-urlencode time=T
# (the log store's: ... start=S ... end=E, logged as S..E)
url="" q="" t=""
for a in "$@"; do
	case "$a" in
	http*) url="$a" ;;
	query=*) q="${a#query=}" ;;
	time=*) t="${a#time=}" ;;
	start=*) t="${a#start=}..$t" ;;
	end=*) t="$t${a#end=}" ;;
	esac
done
printf '%s\t%s\t%s\n' "$url" "$t" "$q" >>"$FAKE_PROM_LOG"
case "$url" in
*/select/logsql/query) f="$FAKE_PROM_DIR/logs.jsonl" ;;
*/api/v1/query)
	case "$q" in
	aether_probe_requests_total* | "timestamp(aether_probe_requests_total"*) m=probe ;;
	aether_agent_identity_cluster_unpinned_total* | "timestamp(aether_agent_identity_cluster_unpinned_total"*) m=unpinned ;;
	'aether_agent_snapshot_tls_clusters{pin="unpinned"'*) m=published ;;
	*) m=unknown ;;
	esac
	case "$q" in "timestamp("*) k=start-time ;; *"["*) k=window ;; *) k=start ;; esac
	f="$FAKE_PROM_DIR/$m.$k.json"
	;;
*) f=/nonexistent ;;
esac
if [ ! -r "$f" ]; then
	echo "curl: (22) The requested URL returned error: 503" >&2
	exit 22
fi
case "$f" in
*.window.json)
	n="${q##*[}"
	exec "$(dirname "$0")/range" "$f" "${n%s]}" "$t"
	;;
esac
cat "$f"
EOF
cat >"$TMP/grade-bin/kubectl" <<'EOF'
#!/usr/bin/env bash
# kubectl --context C get --raw /api/v1/namespaces/NS/services/SVC:PORT/proxy/api/v1/query?query=ENC&time=T
printf '%s\n' "$*" >>"$FAKE_PROM_LOG"
case "$*" in
"--context "*" get --raw /api/v1/namespaces/"*"/services/"*"/proxy/api/v1/query?query="*) ;;
*)
	echo "fake kubectl: unexpected call: $*" >&2
	exit 1
	;;
esac
case "$*" in *query=aether_probe_requests_total* | *query=timestamp%28aether_probe_requests_total*) m=probe ;; *query=aether_agent_snapshot_tls_clusters%7Bpin%3D%22unpinned%22*) m=published ;; *) m=unpinned ;; esac
case "$*" in *query=timestamp%28*) k=start-time ;; *%5B*) k=window ;; *) k=start ;; esac
if [ "$k" = window ]; then
	a="$*"
	n="${a##*%5B}"
	exec "$(dirname "$0")/range" "$FAKE_PROM_DIR/$m.$k.json" "${n%%s%5D*}" "${a##*&time=}"
fi
cat "$FAKE_PROM_DIR/$m.$k.json"
EOF
chmod +x "$TMP/grade-bin/curl" "$TMP/grade-bin/kubectl" "$TMP/grade-bin/range"
# run_grade <scenario> <out> [prober-grade args...]: leaves $grc and $TMP/grade-queries.tsv
run_grade() {
	local scen="$1" outf="$2"
	shift 2
	: >"$TMP/grade-queries.tsv"
	FAKE_PROM_DIR="$GF/$scen" FAKE_PROM_LOG="$TMP/grade-queries.tsv" PROBER_GRADE_CURL="$TMP/grade-bin/curl" PROBER_GRADE_KUBECTL="$TMP/grade-bin/kubectl" \
		bash "$GRADE" "$@" >"$outf" 2>&1
	grc=$?
}
# A run directory as run.sh and churn.sh leave it: T0 is the churn log's first line.
GD="$TMP/grade-rundir"
mkdir -p "$GD"
printf 'RUN_TAG=soak-20261008T000530Z\nMODE=soak\nCTX=some-cluster\nNS=aether-test\nT_LOAD=1791417960\nDURATION_S=30600\n' >"$GD/run.env"
printf '2026-10-08T00:10:00Z churn driver start T0=2026-10-08T00:10:00Z build=nightly/2.4.18-abc1234\n2026-10-08T00:10:00Z churn driver context=some-cluster (pre-flight passed)\n' >"$GD/churn.log"

G="$TMP/grade-born.log"
run_grade born "$G" --dir "$GD" --prometheus http://prom.example:9090/
show "prober-grade: two timeout series born inside the window (the 2026-10-08 shape)" "$G"
if [ "$grc" -eq 1 ]; then pass "grade born: exit 1 (there is something to attribute)"; else fail "grade born: exit $grc, want 1"; fi
expect "$G" "grade born: the window is T0 of the churn log + 8h" '^WINDOW  start=2026-10-08T00:10:00Z end=2026-10-08T08:10:00Z seconds=28800  \(T0 from churn\.log\)$' 1
expect "$G" "grade born: the queries are printed as they were asked, with their time" '^QUERY   probe/(start  time=2026-10-08T00:10:00Z  aether_probe_requests_total|window  time=2026-10-08T08:10:00Z  aether_probe_requests_total\[28800s\])$' 2
expect "$G" "grade born: 40 timeouts, summed over pods (raw: 21 + 19)" '^TOTAL   tier=mesh_dns result=timeout count=40 rate=0/s series=2$' 1
expect "$G" "grade born: by tier, result and SOURCE node" '^FAILED  tier=mesh_dns result=timeout node=worker-0(4 count=21 pods=prober-d|2 count=19 pods=prober-b) targets=echo\.aether-test\.aether\.internal:18081$' 2
expect "$G" "grade born: each born series is named, with the sample that created it" '^BORN    tier=mesh_dns result=timeout node=worker-0(4 pod=prober-d .* first=2@2026-10-08T02:16:00Z count=21|2 pod=prober-b .* first=1@2026-10-08T05:28:00Z count=19)  \(absent at the start: counted from 0; ' 2
expect "$G" "grade born: a failure series from before the run that did not move counts 0, and is no FAILED line" '^TOTAL   tier=mesh_dns result=http_error count=0 ' 1
expect "$G" "grade born: the success control, per tier (5 pods x 25/s x 8h)" '^TOTAL   tier=(liveness|mesh_dns) result=success count=3600000 rate=125/s series=5$' 2
expect "$G" "grade born: the prober verdict" '^PROBER  verdict=FAIL non_success=40 liveness_non_success=0 dns_class_non_success=0 success=7200000 series=13 born_in_window=2 resets=0$' 1
expect "$G" "grade born: the pod set did not change" '^PODS    at_start=5 at_end=5 gone=0 new=0 nodes=5$' 1
expect "$G" "grade born: the unpinned-cluster counter rests at its seeded zero on every node (#1423)" '^UNPINNED verdict=PASS increase=0 series=5 nodes=5 resets=0$' 1
expect "$G" "grade born: one VERDICT line" '^VERDICT prober=FAIL unpinned=PASS logs=not-checked$' 1
expect "$G" "grade born: the time of each sample at the start is asked for too (#1469)" '^QUERY   (probe|unpinned)/start-time  time=2026-10-08T00:10:00Z  timestamp\(aether_(probe_requests|agent_identity_cluster_unpinned)_total\)$' 2
if [ "$(cut -f1 "$TMP/grade-queries.tsv" | sort -u)" = "http://prom.example:9090/api/v1/query" ] && [ "$(grep -c '' "$TMP/grade-queries.tsv")" -eq 7 ] &&
	[ "$(cut -f2 "$TMP/grade-queries.tsv" | sort -u | tr '\n' ' ')" = "$GT0 $((GT0 + 28800)) " ]; then
	pass "grade born: seven instant queries, to the URL it was given, at the window's two ends"
else
	fail "grade born: the queries were: $(tr '\n' '|' <"$TMP/grade-queries.tsv")"
fi
expect "$G" "grade born: the seventh is the gauge of what the agents published, its unpinned series over the window and the two minutes before it (#1491)" '^QUERY   unpinned/published  time=2026-10-08T08:10:00Z  aether_agent_snapshot_tls_clusters\{pin="unpinned"\}\[28921s\]$' 1
# These agents have no `reason` label (before #1424) and so no gauge: that is
# said, and it is not UNPROVEN -- an agent that old never had one.
# shellcheck disable=SC2016 # the backticks are the output's, not a substitution
expect "$G" "grade born: agents with no reason label have no gauge, and that is said, not graded" '^UNPINNED published: not graded: nodes=worker-01,worker-02,worker-03,worker-04,worker-05  \(no `reason` label and no gauge: agents from before #1424\. ' 1
# The RED reading: what increase() can see of the same samples. It needs two
# samples of a series, so it counts from a series' FIRST sample in the window --
# the count that created the series is not in it. (Prometheus's increase() also
# extrapolates to the window's edges; this is its arithmetic without that.)
red=$(jq '[.data.result[] | select(.metric.result != "success") | (.values[-1][1] | tonumber) - (.values[0][1] | tonumber)] | add' "$GF/born/probe.window.json")
if [ "$red" = 37 ]; then
	pass "grade born: last-minus-first-sample, as increase() counts, reads 37 on the same samples (the #1390 bug, seen red); the script reads 40"
else
	fail "grade born: the increase()-style count of the fixture is $red, want 37: the red reading is gone"
fi
# ... and `x - x offset 8h` at the window's end drops both series outright:
# neither exists at the offset.
red2=$(jq -n --slurpfile s "$GF/born/probe.start.json" --slurpfile w "$GF/born/probe.window.json" '
	($s[0].data.result | map(.metric | tojson)) as $at0
	| [$w[0].data.result[] | select(.metric.result == "timeout") | select((.metric | tojson) as $k | $at0 | index($k))] | length')
if [ "$red2" = 0 ]; then pass "grade born: neither timeout series exists at the window's start (so \`x - x offset 8h\` drops both)"; else fail "grade born: $red2 timeout series exist at T0 in the fixture"; fi

# The cross-check against the prober's own lines: 39 detail lines and one
# `suppressed` count inside the window, one line before it.
run_grade born "$G" --dir "$GD" --prometheus http://prom.example:9090 --logs-file "$GF/born/prober.log"
expect "$G" "grade logs: 39 lines + 1 suppressed = the 40 the counters say; the line before T0 is not counted" '^LOGS    tier=mesh_dns result=timeout lines=39 suppressed=1 counters=40 match$' 1
expect "$G" "grade logs: verdict" '^VERDICT prober=FAIL unpinned=PASS logs=MATCH$' 1
grep -v '2026-10-08T05:28:0[56]' "$GF/born/prober.log" >"$TMP/grade-short.log"
run_grade born "$G" --dir "$GD" --prometheus http://prom.example:9090 --logs-file "$TMP/grade-short.log"
expect "$G" "grade logs: two lines short is a MISMATCH, with both numbers" '^LOGS    tier=mesh_dns result=timeout lines=37 suppressed=1 counters=40 MISMATCH$' 1
# RED before: exit 1, the prober's FAIL, as if the 40 were a count to attribute.
if [ "$grc" -eq 2 ]; then pass "grade logs: a MISMATCH is exit 2 although the prober verdict is FAIL"; else fail "grade logs: exit $grc with logs=MISMATCH, want 2"; fi
expect "$G" "grade logs: ... in the verdict too" '^VERDICT prober=FAIL unpinned=PASS logs=MISMATCH$' 1
run_grade born "$G" --dir "$GD" --prometheus http://prom.example:9090 --logs-url http://logs.example:9428 --logs-query '"AETHER_PROBE_FAIL" AND k8s.namespace.name:aether-test'
expect "$G" "grade logs: from a log store (one JSON record a line, the prober's line in _msg)" '^LOGS    verdict=MATCH lines=39 suppressed=1 boundary=0 counters=40 ' 1
if grep -q "^http://logs.example:9428/select/logsql/query	$GT0\.\.$((GT0 + 28800 + 120))	\"AETHER_PROBE_FAIL\" AND k8s.namespace.name:aether-test\$" "$TMP/grade-queries.tsv"; then
	pass "grade logs: the log query is the one it was given, at the URL it was given, from T0 to two minutes past the window's end (a summary closed there counts failures inside it)"
else
	fail "grade logs: the log store was asked: $(grep logsql "$TMP/grade-queries.tsv")"
fi
# A `suppressed` summary carries the time its window was CLOSED, not the times
# of the failures it counts (review of #1458). Closed 30 s after the window's
# end, the one suppressed failure of this fixture may lie on either side of the
# end. RED before: the summary was dropped for its `t` and the row read
# `lines=39 suppressed=0 counters=40 MISMATCH` -- a mismatch that is not one.
sed 's/"t":"2026-10-08T02:16:10.000Z"\(.*"suppressed":1\)/"t":"2026-10-08T08:10:30.000Z"\1/' "$GF/born/prober.log" >"$TMP/grade-edge-end.log"
run_grade born "$G" --dir "$GD" --prometheus http://prom.example:9090 --logs-file "$TMP/grade-edge-end.log"
expect "$G" "grade logs: a summary closed 30 s after the window's end is a boundary summary, named with the span it can cover" '^LOGS    boundary: tier=mesh_dns result=timeout pod=prober-d suppressed=1 closed=2026-10-08T08:10:30Z covers=2026-10-08T08:08:30Z\.\.2026-10-08T08:10:30Z  \(it may count failures on both sides of the end of the window: not in the sum\)$' 1
expect "$G" "grade logs: ... and the row is UNPROVEN with both bounds, not a MISMATCH" '^LOGS    tier=mesh_dns result=timeout lines=39 suppressed=0 boundary=1 counters=40 UNPROVEN  \(the logs say between 39 and 40\)$' 1
expect "$G" "grade logs: ... in the verdict too" '^VERDICT prober=FAIL unpinned=PASS logs=UNPROVEN$' 1
if [ "$grc" -eq 2 ]; then pass "grade logs: logs=UNPROVEN is exit 2 although the prober verdict is FAIL"; else fail "grade logs: exit $grc with logs=UNPROVEN, want 2"; fi
# ... and closed 30 s after the window's START, a summary of five failures from
# before T0. RED before: all five were added, `suppressed=6 counters=40 MISMATCH`.
{
	cat "$GF/born/prober.log"
	echo 'AETHER_PROBE_FAIL {"t":"2026-10-08T00:10:30.000Z","tier":"mesh_dns","result":"timeout","suppressed":5,"window_s":60,"pod":"prober-d","node":"worker-04"}'
} >"$TMP/grade-edge-start.log"
run_grade born "$G" --dir "$GD" --prometheus http://prom.example:9090 --logs-file "$TMP/grade-edge-start.log"
expect "$G" "grade logs: a summary closed 30 s after the window's start is not added to the sum" '^LOGS    tier=mesh_dns result=timeout lines=39 suppressed=1 boundary=5 counters=40 UNPROVEN  \(the logs say between 40 and 45\)$' 1
expect "$G" "grade logs: ... it is named as the start's" '^LOGS    boundary: .* suppressed=5 closed=2026-10-08T00:10:30Z covers=2026-10-08T00:08:30Z\.\.2026-10-08T00:10:30Z  \(it may count failures on both sides of the start of the window' 1
# A boundary summary excuses only what it can hold: two detail lines short and
# one boundary failure is still a MISMATCH.
grep -v '2026-10-08T05:28:0[56]' "$TMP/grade-edge-end.log" >"$TMP/grade-edge-short.log"
run_grade born "$G" --dir "$GD" --prometheus http://prom.example:9090 --logs-file "$TMP/grade-edge-short.log"
expect "$G" "grade logs: a count the boundary summary cannot explain is still a MISMATCH (37 + at most 1 against 40)" '^LOGS    tier=mesh_dns result=timeout lines=37 suppressed=0 boundary=1 counters=40 MISMATCH$' 1
expect "$G" "grade logs: ... in the verdict too" '^VERDICT prober=FAIL unpinned=PASS logs=MISMATCH$' 1
# A prober since #1463 says when the summary's window OPENED (`window_start`):
# the failures it counts lie between that and `t`, and no two-window guess is
# needed. Closed 90 s after the window's start, with a window that opened 10 s
# after it: every failure it counts is inside. RED before: the guess put the
# span at 00:09:30..00:11:30, across T0 -- `boundary=1 ... UNPROVEN`, exit 2.
sed 's/"t":"2026-10-08T02:16:10.000Z"\(.*"suppressed":1,"window_s":60\)/"t":"2026-10-08T00:11:30.000Z"\1,"window_start":"2026-10-08T00:10:10.250Z"/' "$GF/born/prober.log" >"$TMP/grade-window-start.log"
run_grade born "$G" --dir "$GD" --prometheus http://prom.example:9090 --logs-file "$TMP/grade-window-start.log"
expect "$G" "grade logs: a summary that carries its window_start is counted when that window is inside the run's" '^LOGS    tier=mesh_dns result=timeout lines=39 suppressed=1 counters=40 match$' 1
expect "$G" "grade logs: ... no boundary line" '^LOGS    boundary: ' 0
if [ "$grc" -eq 1 ]; then pass "grade logs: ... exit 1 (the prober's FAIL, and a count that matches)"; else fail "grade logs: exit $grc with a window_start inside the window, want 1"; fi
# ... and one whose window opened before T0 is still a boundary summary, with
# its own span, not the guess (which would be 00:08:30..00:10:30).
sed 's/"t":"2026-10-08T02:16:10.000Z"\(.*"suppressed":1,"window_s":60\)/"t":"2026-10-08T00:10:30.000Z"\1,"window_start":"2026-10-08T00:09:50.000Z"/' "$GF/born/prober.log" >"$TMP/grade-window-start-edge.log"
run_grade born "$G" --dir "$GD" --prometheus http://prom.example:9090 --logs-file "$TMP/grade-window-start-edge.log"
expect "$G" "grade logs: a window that opened before T0 and closed after it is a boundary summary, with the span the line gives" '^LOGS    boundary: tier=mesh_dns result=timeout pod=prober-d suppressed=1 closed=2026-10-08T00:10:30Z covers=2026-10-08T00:09:50Z\.\.2026-10-08T00:10:30Z  \(it may count failures on both sides of the start of the window: not in the sum\)$' 1

G="$TMP/grade-clean.log"
run_grade clean "$G" --start 2026-10-08T00:10:00Z --window 8h --prometheus http://prom.example:9090
show "prober-grade: a clean window" "$G"
if [ "$grc" -eq 0 ]; then pass "grade clean: exit 0"; else fail "grade clean: exit $grc, want 0"; fi
expect "$G" "grade clean: verdict" '^VERDICT prober=PASS unpinned=PASS logs=not-checked$' 1
# This scenario's agents all have the `reason` label (#1424): four series each
# (the fourth reason, tls_not_published, since #1482).
expect "$G" "grade clean: the unpinned counter at rest, one series per node and reason" '^UNPINNED verdict=PASS increase=0 series=20 nodes=5 resets=0$' 1
expect "$G" "grade clean: per node, four reasons" '^UNPINNED node=worker-0[1-5] count=0 series=4 born_in_window=0 resets=0$' 5
expect "$G" "grade clean: no reason line when nothing moved" '^UNPINNED (reason=|moved:)' 0
expect "$G" "grade clean: no FAILED, BORN or RESET line" '^(FAILED|BORN|RESET|GONE) ' 0
expect "$G" "grade clean: the prober line" '^PROBER  verdict=PASS non_success=0 liveness_non_success=0 dns_class_non_success=0 success=7200000 series=10 born_in_window=0 resets=0$' 1

# A counter reset and a replaced pod, each handled and each said. prober-c's
# container restarted (same series: 5,288,000 then 120,000); prober-e was
# replaced by prober-f at T0+4h, four connection errors before it went. And an
# agent rolled inside the window reported two entries without a pin before it
# knew its trust domain: a NEW series, born at 2. The new agent is one with the
# `reason` label (#1424), so it brings four series, one per reason, three of
# them resting at their seeded zero; the agents that did not roll still have
# the one label-less series each. Under trust_domain_unknown the agent
# published no TLS, and its gauge is at zero in every sample: the state was
# over within an export interval. That is an agent start, reported and not a
# failure (#1491). RED before: `UNPINNED verdict=FAIL ... (a TLS cluster was
# published without its server-identity pin`, VERDICT unpinned=FAIL.
G="$TMP/grade-reset.log"
run_grade reset "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: a counter reset, a replaced prober pod, and an unpinned cluster after an agent roll" "$G"
if [ "$grc" -eq 1 ]; then pass "grade reset: exit 1"; else fail "grade reset: exit $grc, want 1"; fi
expect "$G" "grade reset: the reset is said, with both values" '^RESET   tier=liveness result=success node=worker-03 pod=prober-c at=2026-10-08T04:58:00Z before=5288000 after=120000 ' 1
# prober-c: 288,000 before the reset + 408,000 after it. prober-e: 351,000 up to
# its last sample. prober-f: 361,500, counted from 0. The other three: 720,000.
expect "$G" "grade reset: success is counted across the reset and across the replaced pod (3 x 720,000 + 696,000 + 351,000 + 361,500)" '^TOTAL   tier=liveness result=success count=3568500 ' 1
expect "$G" "grade reset: a failure counter that reset: 2 before it and 1 after (9 - 7, then 1)" '^FAILED  tier=liveness result=timeout node=worker-03 count=3 pods=prober-c ' 1
expect "$G" "grade reset: the failures of the pod that is gone are still counted (an instant query at the end no longer returns its series)" '^FAILED  tier=mesh_dns result=connection_error node=worker-05 count=4 pods=prober-e ' 1
expect "$G" "grade reset: the replaced pod is named" '^GONE    pod=prober-e node=worker-05 last_sample=2026-10-08T04:04:00Z ' 1
expect "$G" "grade reset: the pod set" '^PODS    at_start=5 at_end=5 gone=1 new=1 nodes=5$' 1
expect "$G" "grade reset: the prober verdict counts liveness apart" '^PROBER  verdict=FAIL non_success=7 liveness_non_success=3 dns_class_non_success=0 ' 1
expect "$G" "grade reset: the unpinned counter moved on one node, on a series born in the window (RED for increase(): 2 - 2 = 0)" '^UNPINNED moved: node=worker-03 .*pod=aether-agent-new reason=trust_domain_unknown count=2 first=2@2026-10-08T05:10:00Z last=2@2026-10-08T08:10:00Z \(absent at the start: counted from 0\)$' 1
expect "$G" "grade reset: only the series that moved is a moved line (the two reasons at their seeded zero are not)" '^UNPINNED moved: ' 1
expect "$G" "grade reset: per reason, for the reason that moved (#1424)" '^UNPINNED reason=trust_domain_unknown count=2 nodes=worker-03 class=no_tls$' 1
expect "$G" "grade reset: no line for a reason that did not move" '^UNPINNED reason=' 1
expect "$G" "grade reset: per node, the old agent's one series and the new agent's four" '^UNPINNED node=worker-03 count=2 series=5 born_in_window=4 resets=0$' 1
# shellcheck disable=SC2016 # the backticks are the output's, not a substitution
expect "$G" "grade reset: UNPINNED verdict PASS, its own line (#1423, #1491): the sum is over every series, and what moved is a reason under which no TLS is published" '^UNPINNED verdict=PASS increase=2 series=9 nodes=5 resets=0  \(no TLS cluster was published without its pin: reason=trust_domain_unknown count=2 nodes=worker-03 -- under these the agent published no TLS at all, which is expected at an agent start, and no such state stood for 300 s in the samples of the gauge\. Compare `UNPINNED steps:` with the agent rolls in churn\.log\)$' 1
expect "$G" "grade reset: when it stepped, to hold against the churn log's agent rolls" '^UNPINNED steps: node=worker-03 job=aether-agent reason=trust_domain_unknown at=2026-10-08T05:10:00Z\(\+2\)  ' 1
expect "$G" "grade reset: the gauge at zero is no published line" '^UNPINNED published: node=' 0
expect "$G" "grade reset: the four agents that did not roll have no reason label and no gauge: said, not graded" '^UNPINNED published: not graded: nodes=worker-01,worker-02,worker-04,worker-05  ' 1
expect "$G" "grade reset: verdict" '^VERDICT prober=FAIL unpinned=PASS logs=not-checked$' 1
red=$(jq '[.data.result[] | (.values[-1][1] | tonumber) - (.values[0][1] | tonumber)] | add' "$GF/reset/unpinned.window.json")
if [ "$red" = 0 ]; then pass "grade reset: last-minus-first-sample reads 0 for the unpinned counter on these samples (seen red)"; else fail "grade reset: the increase()-style unpinned count is $red, want 0"; fi

# A series the query at T0 returned and the window query did not (#1469). The
# window query returns only series that have a sample in the window, and the
# audit was built from it alone: such a series was in no line and no verdict.
# Each scenario is `clean` with one thing changed. RED before, for each: noted.
#
# silent: worker-04's agent is there at T0 (its sample 30 s old) and exports
# nothing for the whole window. RED before: `UNPINNED verdict=PASS increase=0
# series=12 nodes=4` (three reasons per agent then), VERDICT unpinned=PASS, exit 0
# -- a pass on four nodes of five.
G="$TMP/grade-silent.log"
run_grade silent "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: an agent that exports nothing for the whole window" "$G"
if [ "$grc" -eq 2 ]; then pass "grade silent: exit 2, not a pass on the nodes that did report"; else fail "grade silent: exit $grc, want 2"; fi
expect "$G" "grade silent: the node and job are named, with the age of its sample at the start" '^UNPINNED silent: node=worker-04 job=aether-agent series=4 last_sample=2026-10-08T00:09:30Z  \(alive at the start, its sample there 30 s old, and no sample in the window from this node and job: its counter was not seen\)$' 1
expect "$G" "grade silent: UNPROVEN, and the sum it does have is still printed" '^UNPINNED verdict=UNPROVEN increase=0 series=16 nodes=4 resets=0  \(no sample in the window from worker-04: the sum is over the nodes that reported, not the fleet\)$' 1
expect "$G" "grade silent: it is one finding, not two (its prober reports, but the node is already named)" '^UNPINNED missing: ' 0
expect "$G" "grade silent: the prober grade is not touched" '^PROBER  verdict=PASS non_success=0 liveness_non_success=0 dns_class_non_success=0 success=7200000 series=10 born_in_window=0 resets=0$' 1
expect "$G" "grade silent: verdict" '^VERDICT prober=PASS unpinned=UNPROVEN logs=not-checked$' 1
# ... and a counter that moved elsewhere does not make it a FAIL to attribute:
# unproven comes first, in the verdict as in the exit status. The reset
# scenario's samples (worker-03 moved by 2), without worker-04's.
mkdir -p "$TMP/grade-silent-moved"
cp "$GF/reset/"*.json "$TMP/grade-silent-moved/"
jq -c '.data.result |= map(select(.metric.node != "worker-04"))' "$GF/reset/unpinned.window.json" >"$TMP/grade-silent-moved/unpinned.window.json"
GF="$TMP" run_grade grade-silent-moved "$G" --dir "$GD" --prometheus http://prom.example:9090
expect "$G" "grade silent + moved: UNPROVEN, and it says the counter moved" '^UNPINNED verdict=UNPROVEN increase=2 series=8 nodes=4 resets=0  \(no sample in the window from worker-04: the sum is over the nodes that reported, not the fleet; and the counter moved on those\)$' 1
expect "$G" "grade silent + moved: what moved is still listed" '^UNPINNED reason=trust_domain_unknown count=2 nodes=worker-03 class=no_tls$' 1
if [ "$grc" -eq 2 ]; then pass "grade silent + moved: exit 2"; else fail "grade silent + moved: exit $grc, want 2"; fi

# missing: worker-04's agent has no series at all, at T0 or after -- silent
# since more than the lookback before the run -- while the prober on that node
# reports. Nothing at T0 names it; the prober's node does. RED before: the same
# `UNPINNED verdict=PASS ... nodes=4`, exit 0.
G="$TMP/grade-missing.log"
run_grade missing "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: a node whose prober reports and whose agent never does" "$G"
if [ "$grc" -eq 2 ]; then pass "grade missing: exit 2"; else fail "grade missing: exit $grc, want 2"; fi
expect "$G" "grade missing: the node is named, from the prober's series" '^UNPINNED missing: node=worker-04  \(a prober on this node has samples in the window and the counter has none from it: its agent was not seen\)$' 1
expect "$G" "grade missing: UNPROVEN" '^UNPINNED verdict=UNPROVEN increase=0 series=16 nodes=4 resets=0  \(no sample in the window from worker-04: ' 1
expect "$G" "grade missing: verdict" '^VERDICT prober=PASS unpinned=UNPROVEN logs=not-checked$' 1

# replaced: pods replaced shortly BEFORE T0. The query at T0 still returns the
# old pod's series (an instant query answers with a sample up to five minutes
# old), and it has no sample in the window: exactly what a silent node looks
# like, except for the AGE of that sample. None of this is unproven:
#   prober-old (worker-05), last sample 200 s before T0; prober-e replaced it
#   aether-agent-05-old, 200 s before T0; aether-agent-05 replaced it
#   aether-agent-04-old, 30 s before T0 -- as fresh as a live one -- and
#     aether-agent-04 replaced it: the node and job have samples in the window
#   worker-06, an agent 200 s before T0 and nothing since: a node that left
# RED before: exit 0 as well, and not a word about any of them (no ENDED line,
# no `UNPINNED ended:` line). With the age test taken out of the fixed script
# (FRESH_S=300, every sample the query returns counts as alive) this scenario
# is exit 2: `GONE pod=prober-old` and `UNPINNED silent: node=worker-06`.
G="$TMP/grade-replaced.log"
run_grade replaced "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: pods replaced shortly before the window" "$G"
if [ "$grc" -eq 0 ]; then pass "grade replaced: exit 0 (a pod replaced before T0 is not an unproven window)"; else fail "grade replaced: exit $grc, want 0"; fi
expect "$G" "grade replaced: verdict" '^VERDICT prober=PASS unpinned=PASS logs=not-checked$' 1
expect "$G" "grade replaced: the old prober pod is said to have ended before the window, with the age that says so" '^ENDED   pod=prober-old node=worker-05 last_sample=2026-10-08T00:06:40Z  \(the query at the start still returned it, with a sample 200 s old: more than 120 s, so it had stopped before the window and is not in it\)$' 1
expect "$G" "grade replaced: it is not GONE" '^GONE ' 0
expect "$G" "grade replaced: ... and not in the pod set: four at the start, its replacement new" '^PODS    at_start=4 at_end=5 gone=0 new=1 nodes=5$' 1
expect "$G" "grade replaced: the replacement is counted from 0, whole" '^TOTAL   tier=(liveness|mesh_dns) result=success count=3600000 rate=125/s series=5$' 2
expect "$G" "grade replaced: an agent replaced before T0, old (worker-05) or fresh (worker-04), is no silent node: its node and job report" '^UNPINNED (silent|missing): ' 0
expect "$G" "grade replaced: per node, the replacement's four series, born in the window" '^UNPINNED node=worker-0[45] count=0 series=4 born_in_window=4 resets=0$' 2
expect "$G" "grade replaced: a node that left before the window is said, and is not a verdict" '^UNPINNED ended: node=worker-06 job=aether-agent series=4 last_sample=2026-10-08T00:06:40Z  \(the query at the start still returned it, with a sample 200 s old: more than 120 s, so it had stopped before the window and is not in it\)$' 1
expect "$G" "grade replaced: only that one" '^UNPINNED ended: ' 1
expect "$G" "grade replaced: the unpinned verdict" '^UNPINNED verdict=PASS increase=0 series=20 nodes=5 resets=0$' 1

# gone: prober-e (worker-05) is alive at T0 (its sample 20 s old) and deleted
# right after it, before it exports again; prober-f replaces it. RED before:
# no GONE line and `PODS at_start=4 at_end=5 gone=0 new=1` -- a pod that was
# there at the start, missing from the audit.
G="$TMP/grade-gone.log"
run_grade gone "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: a prober pod deleted right after the window's start" "$G"
if [ "$grc" -eq 0 ]; then pass "grade gone: exit 0 (a GONE pod is a line, not a verdict)"; else fail "grade gone: exit $grc, want 0"; fi
expect "$G" "grade gone: the pod is GONE, with its last sample, which is before the window" '^GONE    pod=prober-e node=worker-05 last_sample=2026-10-08T00:09:40Z  \(alive at the start, its sample there 20 s old, and no sample in the window: nothing of it is in the totals\)$' 1
expect "$G" "grade gone: the pod set has it at the start" '^PODS    at_start=5 at_end=5 gone=1 new=1 nodes=5$' 1
expect "$G" "grade gone: no ENDED line (it was alive at T0)" '^ENDED ' 0
expect "$G" "grade gone: the counts are of the series that have samples: ten, the gone pod's two not among them" '^PROBER  verdict=PASS non_success=0 liveness_non_success=0 dns_class_non_success=0 success=7200000 series=10 born_in_window=0 resets=0$' 1
expect "$G" "grade gone: verdict" '^VERDICT prober=PASS unpinned=PASS logs=not-checked$' 1

# The verdict per reason (#1491). The counter's `reason` is a closed set of two
# kinds of fact: under no_namespace_metadata and pin_not_rendered TLS went out
# without its server-identity pin, the validation gap; under
# trust_domain_unknown and tls_not_published the agent published no TLS at
# all, which every agent start does for a moment -- and a soak rolls the agents
# twice. Any movement used to fail, with a text that was untrue for the second
# kind. Each scenario is `clean` with one thing changed, built here; RED
# before, for each: noted (the same assertions against the script as it was).
#
# grade_case <name> [base]: a copy of a scenario to change, served from $TMP.
grade_case() { mkdir -p "$TMP/$1" && cp "$GF/${2:-clean}/"*.json "$TMP/$1/"; }
# grade_edit <name> <file> <jq filter> [jq args...]: rewrites one response.
grade_edit() {
	local d="$TMP/$1" f="$2" prog="$3"
	shift 3
	jq -c "$@" "$prog" "$d/$f.json" >"$d/$f.tmp" && mv "$d/$f.tmp" "$d/$f.json"
}
# grade_last <name> <file> <node> <reason> <value>: the LAST sample of that
# node's series for that reason (`-`: the series with no reason label).
grade_last() {
	# shellcheck disable=SC2016 # jq's variables, not the shell's
	grade_edit "$1" "$2" '(.data.result[] | select(.metric.node == $n and (.metric.reason // "-") == $r) | .values[-1][1]) = $v' --arg n "$3" --arg r "$4" --arg v "$5"
}
# grade_gauge <name> <node> <reason> <values>: every sample of that gauge series.
grade_gauge() {
	# shellcheck disable=SC2016 # jq's variables, not the shell's
	grade_edit "$1" published.window '(.data.result[] | select(.metric.node == $n and .metric.reason == $r) | .values) = $v' --arg n "$2" --arg r "$3" --argjson v "$4"
}
# gauge_run <from> <samples> <value>: zero a minute before, <samples> non-zero
# samples a minute apart (they span (samples - 1) x 60 s), zero after, zero at
# the window's end.
gauge_run() {
	jq -nc --argjson f "$1" --argjson k "$2" --arg v "$3" --argjson e "$((GT0 + 28800))" \
		'[[$f - 60, "0"]] + [range(0; $k) | [$f + . * 60, $v]] + [[$f + $k * 60, "0"], [$e, "0"]]'
}
GRUN=$((GT0 + 18000)) # 05:10:00Z, the TRIPLE's agent roll (T0+300m)

# gap: worker-02 published TLS without a pin (6 on the counter, in no gauge
# sample: it came and went within an export interval), worker-04 reported
# pin_not_rendered once, and worker-01's agent start counted 180 under
# trust_domain_unknown. The first two fail, by name; the third is listed and
# is not in the verdict's text.
# RED before: `UNPINNED verdict=FAIL increase=187 ... (a TLS cluster was
# published without its server-identity pin: the agent logged which and why`
# -- no reason, no count per reason, no node, and 180 of the 187 were not that.
grade_case grade-gap
grade_last grade-gap unpinned.window worker-02 no_namespace_metadata 6
grade_last grade-gap unpinned.window worker-04 pin_not_rendered 1
grade_last grade-gap unpinned.window worker-01 trust_domain_unknown 180
G="$TMP/grade-gap.log"
GF="$TMP" run_grade grade-gap "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: TLS published without its pin, beside an agent start" "$G"
if [ "$grc" -eq 1 ]; then pass "grade gap: exit 1"; else fail "grade gap: exit $grc, want 1"; fi
expect "$G" "grade gap: each reason that moved, with its class" '^UNPINNED reason=(no_namespace_metadata count=6 nodes=worker-02 class=gap|pin_not_rendered count=1 nodes=worker-04 class=gap|trust_domain_unknown count=180 nodes=worker-01 class=no_tls)$' 3
# shellcheck disable=SC2016 # the backticks are the output's, not a substitution
expect "$G" "grade gap: FAIL, and the text names the gap reasons, their counts and their nodes -- and only those" '^UNPINNED verdict=FAIL increase=187 series=20 nodes=5 resets=0  \(TLS was published without its server-identity pin, the mTLS validation gap: reason=no_namespace_metadata count=6 nodes=worker-02, reason=pin_not_rendered count=1 nodes=worker-04 -- the agent logged which clusters, `mesh clusters published with no server-identity SAN pin`; docs/runbook\.md, "The unpinned-cluster signal"\)$' 1
expect "$G" "grade gap: verdict" '^VERDICT prober=PASS unpinned=FAIL logs=not-checked$' 1
# ... the same with three entries in the gauge's last sample: said too.
grade_last grade-gap published.window worker-02 no_namespace_metadata 3
GF="$TMP" run_grade grade-gap "$G" --dir "$GD" --prometheus http://prom.example:9090
expect "$G" "grade gap: the gauge's non-zero sample is a published line" '^UNPINNED published: node=worker-02 job=aether-agent reason=no_namespace_metadata class=gap nonzero_samples=1 longest=0s max=3 at_end=3 series=1  \(the gauge held it: fails the gate\)$' 1
expect "$G" "grade gap: ... and in the verdict's text, after the counter's" '^UNPINNED verdict=FAIL increase=187 .* reason=pin_not_rendered count=1 nodes=worker-04; in the gauge: reason=no_namespace_metadata node=worker-02 published=3 -- the agent logged ' 1
# ... and a gap that stood through the window with no new snapshot: the counter
# adds on a snapshot, so it does not move; the gauge is exported every minute.
# RED before: `UNPINNED verdict=PASS increase=0`, exit 0 (the gauge was not read).
grade_case grade-gap-held
grade_edit grade-gap-held published.window '(.data.result[] | select(.metric.node == "worker-02" and .metric.reason == "no_namespace_metadata") | .values[][1]) = "4"'
GF="$TMP" run_grade grade-gap-held "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 1 ]; then pass "grade gap held: exit 1 although the counter did not move"; else fail "grade gap held: exit $grc, want 1"; fi
expect "$G" "grade gap held: FAIL from the gauge alone" '^UNPINNED verdict=FAIL increase=0 series=20 nodes=5 resets=0  \(TLS was published without its server-identity pin, the mTLS validation gap: reason=no_namespace_metadata node=worker-02 published=4 -- ' 1

# no-tls: two agent starts and nothing else. worker-01 counted 180 under
# trust_domain_unknown and worker-05 12 under tls_not_published; neither state
# is in a gauge sample. RED before: `UNPINNED verdict=FAIL increase=192`,
# VERDICT unpinned=FAIL, exit 1.
grade_case grade-notls
grade_last grade-notls unpinned.window worker-01 trust_domain_unknown 180
grade_last grade-notls unpinned.window worker-05 tls_not_published 12
G="$TMP/grade-notls.log"
GF="$TMP" run_grade grade-notls "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: agent starts, and no TLS published without a pin" "$G"
if [ "$grc" -eq 0 ]; then pass "grade no-tls: exit 0 (an agent start does not fail a soak that rolls the agents)"; else fail "grade no-tls: exit $grc, want 0"; fi
expect "$G" "grade no-tls: reported, per reason, with count and nodes" '^UNPINNED reason=(trust_domain_unknown count=180 nodes=worker-01|tls_not_published count=12 nodes=worker-05) class=no_tls$' 2
expect "$G" "grade no-tls: PASS, and the text says what the count is" '^UNPINNED verdict=PASS increase=192 series=20 nodes=5 resets=0  \(no TLS cluster was published without its pin: reason=tls_not_published count=12 nodes=worker-05, reason=trust_domain_unknown count=180 nodes=worker-01 -- under these the agent published no TLS at all, ' 1
expect "$G" "grade no-tls: verdict" '^VERDICT prober=PASS unpinned=PASS logs=not-checked$' 1
# ... the same with the state in the window's LAST gauge sample: an agent start
# the window cut. Not a failure, and said.
grade_last grade-notls published.window worker-05 tls_not_published 12
GF="$TMP" run_grade grade-notls "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 0 ]; then pass "grade no-tls at the end: exit 0"; else fail "grade no-tls at the end: exit $grc, want 0"; fi
expect "$G" "grade no-tls at the end: one sample is not a state, and it is said that the window ends on it" '^UNPINNED published: node=worker-05 job=aether-agent reason=tls_not_published class=no_tls nonzero_samples=1 longest=0s max=12 at_end=12 series=1  \(less than 300 s: reported, not a failure\) -- and still non-zero at the end of the window: whether it recovered is after it$' 1

# standing: worker-03's agent did not get its trust domain for five minutes
# after the TRIPLE: 90 entries in six gauge samples, 300 s from the first to
# the last. Longer than an agent start (PENDING_S), so it fails, with its own
# text. RED before: FAIL too, but as `a TLS cluster was published without its
# server-identity pin`, which did not happen.
grade_case grade-standing
grade_last grade-standing unpinned.window worker-03 trust_domain_unknown 630
grade_gauge grade-standing worker-03 trust_domain_unknown "$(gauge_run "$GRUN" 6 90)"
G="$TMP/grade-standing.log"
GF="$TMP" run_grade grade-standing "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: an agent that published no TLS for five minutes" "$G"
if [ "$grc" -eq 1 ]; then pass "grade standing: exit 1"; else fail "grade standing: exit $grc, want 1"; fi
expect "$G" "grade standing: the run, from the gauge's samples" '^UNPINNED published: node=worker-03 job=aether-agent reason=trust_domain_unknown class=no_tls nonzero_samples=6 longest=300s max=90 at_end=0 series=1  \(no TLS published for 300 s or more: not an agent start; fails the gate\)$' 1
expect "$G" "grade standing: FAIL, and the text is about no TLS, not about a missing pin" '^UNPINNED verdict=FAIL increase=630 series=20 nodes=5 resets=0  \(no TLS was published for 300 s or more: reason=trust_domain_unknown node=worker-03 seconds=300 published=90 -- longer than an agent start: ' 1
expect "$G" "grade standing: ... it does not claim the validation gap" 'UNPINNED verdict=.*(without its server-identity pin|validation gap)' 0
# ... one sample fewer is 240 s: under the bound, reported.
grade_gauge grade-standing worker-03 trust_domain_unknown "$(gauge_run "$GRUN" 5 90)"
GF="$TMP" run_grade grade-standing "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 0 ]; then pass "grade standing: 240 s is under the bound: exit 0"; else fail "grade standing: a 240 s run gave exit $grc, want 0"; fi
expect "$G" "grade standing: ... and printed with its length" '^UNPINNED published: node=worker-03 .* nonzero_samples=5 longest=240s max=90 at_end=0 series=1  \(less than 300 s: reported, not a failure\)$' 1
# ... and two 240 s runs with ten minutes of no sample between them are two
# runs, not one of 19 minutes: nothing says the state held while the exporter
# was silent (the bound for that is Prometheus's own lookback, STALE_S).
grade_gauge grade-standing worker-03 trust_domain_unknown "$(jq -nc --argjson f "$GRUN" --argjson e "$((GT0 + 28800))" '[range(0; 5) | [$f + . * 60, "90"]] + [range(0; 5) | [$f + 900 + . * 60, "90"]] + [[$e, "0"]]')"
GF="$TMP" run_grade grade-standing "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 0 ]; then pass "grade standing: a run does not span a gap in the samples: exit 0"; else fail "grade standing: two runs around a gap gave exit $grc, want 0"; fi
expect "$G" "grade standing: ... ten samples, the longest run 240 s" '^UNPINNED published: node=worker-03 .* nonzero_samples=10 longest=240s ' 1
# ... never recovered, and no snapshot in the window: worker-05 publishes two
# entries with no TLS in every sample from T0 to the end, and its counter does
# not move. RED before: `UNPINNED verdict=PASS increase=0`, exit 0.
# The store holds 481 samples, the first exactly at T0. A server since
# Prometheus 3.0 does not return that one for `[28800s]` (review of #1494: this
# fixture used to hand it over, as no such server does), and the script as it
# was then read `nonzero_samples=480 longest=28740s`. The state at T0 is now
# asked for (the range reaches FRESH_S before the window) and is the sample at
# T0, so it is 481 samples and 28800 s on either server.
grade_case grade-never
grade_gauge grade-never worker-05 tls_not_published "$(jq -nc --argjson t "$GT0" '[range(0; 481) | [$t + . * 60, "2"]]')"
GF="$TMP" run_grade grade-never "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 1 ]; then pass "grade never recovered: exit 1 although the counter did not move"; else fail "grade never recovered: exit $grc, want 1"; fi
expect "$G" "grade never recovered: the whole window, and still there at its end" '^UNPINNED published: node=worker-05 job=aether-agent reason=tls_not_published class=no_tls nonzero_samples=481 longest=28800s max=2 at_end=2 series=1  \(no TLS published for 300 s or more: ' 1
expect "$G" "grade never recovered: FAIL" '^UNPINNED verdict=FAIL increase=0 series=20 nodes=5 resets=0  \(no TLS was published for 300 s or more: reason=tls_not_published node=worker-05 seconds=28800 published=2 -- ' 1

# The gauge at the window's start (review of #1494). A range selector is
# left-open since Prometheus 3.0: `gauge[28800s]` at the window's end does not
# return a sample exactly at T0, and on no version one from before it.
# worker-03's agent has no trust domain from before the run: 90 entries in the
# samples at T0-60, T0, T0+60 ... T0+300, zero from T0+360. In the window that
# state stood for 300 s. RED before: the server returned T0+60 ... T0+300, the
# script read `nonzero_samples=5 longest=240s` and PASS, exit 0 -- a minute
# short, and under the bound.
grade_case grade-at-start
grade_gauge grade-at-start worker-03 trust_domain_unknown "$(jq -nc --argjson t "$GT0" '[range(-1; 6) | [$t + . * 60, "90"]] + [[$t + 360, "0"], [$t + 28800, "0"]]')"
G="$TMP/grade-at-start.log"
GF="$TMP" run_grade grade-at-start "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: a no-TLS state that is already standing at the window's start" "$G"
if [ "$grc" -eq 1 ]; then pass "grade at the start: exit 1 (300 s from T0, the sample at T0 included)"; else fail "grade at the start: exit $grc, want 1"; fi
expect "$G" "grade at the start: the run starts at T0, six samples" '^UNPINNED published: node=worker-03 job=aether-agent reason=trust_domain_unknown class=no_tls nonzero_samples=6 longest=300s max=90 at_end=0 series=1  \(no TLS published for 300 s or more: not an agent start; fails the gate\)$' 1
expect "$G" "grade at the start: FAIL" '^UNPINNED verdict=FAIL increase=0 series=20 nodes=5 resets=0  \(no TLS was published for 300 s or more: reason=trust_domain_unknown node=worker-03 seconds=300 published=90 -- ' 1
if grep -qF 'aether_agent_snapshot_tls_clusters{pin="unpinned"}[28921s]' "$TMP/grade-queries.tsv"; then
	pass "grade at the start: the gauge is asked for over the window and FRESH_S (120 s) and a second before it"
else
	fail "grade at the start: the gauge query was: $(grep snapshot_tls "$TMP/grade-queries.tsv")"
fi
# ... the same grade from a server before 3.0, whose range is closed at both ends.
FAKE_PROM_CLOSED_LEFT=1 GF="$TMP" run_grade grade-at-start "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 1 ]; then pass "grade at the start: exit 1 from a server whose range includes its start (before 3.0)"; else fail "grade at the start, closed range: exit $grc, want 1"; fi
expect "$G" "grade at the start: ... and the same line: the sample at T0 is counted once" '^UNPINNED published: node=worker-03 .* nonzero_samples=6 longest=300s max=90 at_end=0 series=1  ' 1
# ... the usual case, no sample exactly at T0: samples at T0-30, T0+30 ...
# T0+270. At T0 the state is the one of the sample 30 s before it (what an
# instant query at T0 answers), so the run is T0 to T0+270: under the bound,
# and not counted from before the window. RED before: `longest=240s`.
grade_gauge grade-at-start worker-03 trust_domain_unknown "$(jq -nc --argjson t "$GT0" '[range(0; 6) | [$t - 30 + . * 60, "90"]] + [[$t + 330, "0"], [$t + 28800, "0"]]')"
GF="$TMP" run_grade grade-at-start "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 0 ]; then pass "grade at the start: 270 s from T0 is under the bound: exit 0"; else fail "grade at the start: a 270 s run from T0 gave exit $grc, want 0"; fi
expect "$G" "grade at the start: ... the run is counted from T0, not from the sample before it" '^UNPINNED published: node=worker-03 .* nonzero_samples=6 longest=270s max=90 at_end=0 series=1  \(less than 300 s: reported, not a failure\)$' 1
# ... a sample EXACTLY FRESH_S (120 s) before T0 is still the state at T0: "at
# most FRESH_S old" includes it, and a left-open range of the window plus
# FRESH_S would not return it (review of #1494, third round). Samples at
# T0-120 and T0+60 ... T0+300: 300 s from T0. RED before: the range was
# `[28920s]`, the sample was not returned, `longest=240s`, PASS, exit 0.
grade_gauge grade-at-start worker-03 trust_domain_unknown "$(jq -nc --argjson t "$GT0" '[[$t - 120, "90"]] + [range(1; 6) | [$t + . * 60, "90"]] + [[$t + 360, "0"], [$t + 28800, "0"]]')"
GF="$TMP" run_grade grade-at-start "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 1 ]; then pass "grade at the start: a sample exactly 120 s before T0 is the state at T0: exit 1"; else fail "grade at the start: a sample exactly FRESH_S old gave exit $grc, want 1"; fi
expect "$G" "grade at the start: ... 300 s from T0" '^UNPINNED published: node=worker-03 .* nonzero_samples=6 longest=300s max=90 at_end=0 series=1  ' 1
# ... and one a second older is not: the exporter had stopped (FRESH_S).
grade_gauge grade-at-start worker-03 trust_domain_unknown "$(jq -nc --argjson t "$GT0" '[[$t - 121, "90"]] + [range(1; 6) | [$t + . * 60, "90"]] + [[$t + 360, "0"], [$t + 28800, "0"]]')"
FAKE_PROM_CLOSED_LEFT=1 GF="$TMP" run_grade grade-at-start "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 0 ]; then pass "grade at the start: a sample 121 s before T0 is not the state at T0, even from a server that returns it: exit 0"; else fail "grade at the start: a sample 121 s old gave exit $grc, want 0"; fi
expect "$G" "grade at the start: ... 240 s" '^UNPINNED published: node=worker-03 .* nonzero_samples=5 longest=240s ' 1
# ... and a state that was over before T0 is not a state in the window.
grade_gauge grade-at-start worker-03 trust_domain_unknown "$(jq -nc --argjson t "$GT0" '[[$t - 90, "90"], [$t - 30, "0"], [$t + 30, "0"], [$t + 28800, "0"]]')"
GF="$TMP" run_grade grade-at-start "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 0 ]; then pass "grade at the start: a state that was over before T0: exit 0"; else fail "grade at the start: a state over before T0 gave exit $grc, want 0"; fi
expect "$G" "grade at the start: ... and no published line" '^UNPINNED published: node=' 0

# One state, two pods (review of #1494). worker-03's agent is replaced at the
# TRIPLE while it has no trust domain, and the collector keeps a per-pod
# label: the old pod's series says 90 at 05:10, 05:11 and 05:12 and ends; the
# new pod's begins at 05:13 and says 90 until 05:15, then zero. Neither series
# holds it for 300 s (120 s each); the node did, from 05:10 to 05:15, which is
# what the alert rule's `sum by (job, node, reason)` sees. RED before: runs
# were per series, `nonzero_samples=6 longest=120s`, PASS, exit 0.
#
# grade_handover <name> <the new pod's gauge samples, a JSON array>: worker-03's
# old pod ends at 05:12 and aether-agent-03b follows it.
grade_handover() {
	local f
	grade_case "$1"
	for f in unpinned.window published.window; do
		# shellcheck disable=SC2016 # jq's variables, not the shell's
		grade_edit "$1" "$f" '
			(.data.result | map(select(.metric.node == "worker-03"))) as $old
			| .data.result |= map(if .metric.node == "worker-03" then .values |= map(select(.[0] < $run)) else . end)
			| .data.result += [$old[] | .metric.pod = "aether-agent-03b" | .values = ($new | map([.[0], "0"]))]' \
			--argjson run "$GRUN" --argjson new "$2"
	done
	# shellcheck disable=SC2016
	grade_edit "$1" published.window '
		(.data.result[] | select(.metric.pod == "aether-agent-03" and .metric.reason == "trust_domain_unknown") | .values) += [[$run, "90"], [$run + 60, "90"], [$run + 120, "90"]]
		| (.data.result[] | select(.metric.pod == "aether-agent-03b" and .metric.reason == "trust_domain_unknown") | .values) = $new' \
		--argjson run "$GRUN" --argjson new "$2"
	# shellcheck disable=SC2016
	grade_edit "$1" unpinned.window '
		(.data.result[] | select(.metric.pod == "aether-agent-03" and .metric.reason == "trust_domain_unknown") | .values) += [[$run, "90"], [$run + 60, "180"], [$run + 120, "270"]]
		| (.data.result[] | select(.metric.pod == "aether-agent-03b" and .metric.reason == "trust_domain_unknown") | .values[][1]) = "270"' \
		--argjson run "$GRUN"
}
grade_handover grade-handover "$(jq -nc --argjson f "$GRUN" --argjson e "$((GT0 + 28800))" '[[$f + 180, "90"], [$f + 240, "90"], [$f + 300, "90"], [$f + 360, "0"], [$e, "0"]]')"
G="$TMP/grade-handover.log"
GF="$TMP" run_grade grade-handover "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: a no-TLS state that outlives the pod that first showed it" "$G"
if [ "$grc" -eq 1 ]; then pass "grade handover: exit 1 (300 s on the node, over two pods)"; else fail "grade handover: exit $grc, want 1"; fi
expect "$G" "grade handover: one run over both series" '^UNPINNED published: node=worker-03 job=aether-agent reason=trust_domain_unknown class=no_tls nonzero_samples=6 longest=300s max=90 at_end=0 series=2  \(no TLS published for 300 s or more: not an agent start; fails the gate\)$' 1
expect "$G" "grade handover: FAIL" '^UNPINNED verdict=FAIL increase=540 series=24 nodes=5 resets=0  \(no TLS was published for 300 s or more: reason=trust_domain_unknown node=worker-03 seconds=300 published=90 -- ' 1
# ... but a new pod whose FIRST sample is zero ended the state: two runs of
# 120 s, not one of 360. (Not red before: per series it was two runs already.)
grade_handover grade-handover-zero "$(jq -nc --argjson f "$GRUN" --argjson e "$((GT0 + 28800))" '[[$f + 180, "0"], [$f + 240, "90"], [$f + 300, "90"], [$f + 360, "90"], [$f + 420, "0"], [$e, "0"]]')"
GF="$TMP" run_grade grade-handover-zero "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 0 ]; then pass "grade handover: a zero from the new pod between them is two runs: exit 0"; else fail "grade handover with a zero between: exit $grc, want 0"; fi
expect "$G" "grade handover: ... the longest of them 120 s" '^UNPINNED published: node=worker-03 .* nonzero_samples=6 longest=120s max=90 at_end=0 series=2  \(less than 300 s: reported, not a failure\)$' 1
# ... and two pods side by side (a surge roll): one says 90 for 300 s; the
# other, sampled half a minute apart, says zero throughout. A zero between two
# non-zero samples of the other series does not end its run. (Not red before.)
grade_case grade-beside
grade_gauge grade-beside worker-03 trust_domain_unknown "$(gauge_run "$GRUN" 6 90)"
# shellcheck disable=SC2016 # jq's variables, not the shell's
grade_edit grade-beside published.window '.data.result += [.data.result[] | select(.metric.node == "worker-03" and .metric.reason == "trust_domain_unknown") | .metric.pod = "aether-agent-03b" | .values = [range(-2; 9) | [$run + 30 + . * 60, "0"]]]' --argjson run "$GRUN"
# shellcheck disable=SC2016
grade_edit grade-beside unpinned.window '.data.result += [.data.result[] | select(.metric.node == "worker-03") | .metric.pod = "aether-agent-03b" | .values = [range(-2; 9) | [$run + 30 + . * 60, "0"]]]' --argjson run "$GRUN"
GF="$TMP" run_grade grade-beside "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 1 ]; then pass "grade beside: a pod at zero beside one that holds the state does not end it: exit 1"; else fail "grade beside: exit $grc, want 1"; fi
expect "$G" "grade beside: ... 300 s" '^UNPINNED published: node=worker-03 .* nonzero_samples=6 longest=300s max=90 at_end=0 series=2  ' 1

# The switch for a cluster where tls_not_published stands by design (review of
# #1494): a mesh run without SPIRE never leaves it, and a healthy agent holds
# a TCP floor entry in it for as long as the floor is not published. The
# default stays strict (`grade never recovered` above: FAIL). With
# --expect-tls-not-published the operator declares it, and the standing state
# is reported with how long, how many entries and the count, and does not fail.
# RED before: `unknown argument '--expect-tls-not-published'`, exit 2.
grade_last grade-never unpinned.window worker-05 tls_not_published 12
G="$TMP/grade-expected.log"
GF="$TMP" run_grade grade-never "$G" --dir "$GD" --prometheus http://prom.example:9090 --expect-tls-not-published
show "prober-grade: tls_not_published stands, and the operator declared that expected" "$G"
if [ "$grc" -eq 0 ]; then pass "grade expected: exit 0 with --expect-tls-not-published"; else fail "grade expected: exit $grc, want 0"; fi
expect "$G" "grade expected: the declaration is in the output, before the queries" '^EXPECT  reason=tls_not_published may stand on this cluster \(declared with --expect-tls-not-published\): reported with how long it stood, not a failure$' 1
expect "$G" "grade expected: its own line: how long, how many entries, the count" '^UNPINNED expected: node=worker-05 job=aether-agent reason=tls_not_published seconds=28800 published=2 count=12  \(it stood for 300 s or more and --expect-tls-not-published declares that expected on this cluster: ' 1
expect "$G" "grade expected: the published line says why it does not fail" '^UNPINNED published: node=worker-05 job=aether-agent reason=tls_not_published class=no_tls nonzero_samples=481 longest=28800s max=2 at_end=2 series=1  \(no TLS published for 300 s or more: declared expected on this cluster, --expect-tls-not-published; reported, not a failure\)$' 1
expect "$G" "grade expected: PASS, and the verdict names it as expected by declaration" '^UNPINNED verdict=PASS increase=12 series=20 nodes=5 resets=0  \(no TLS cluster was published without its pin: reason=tls_not_published count=12 nodes=worker-05 -- .* no such state stood for 300 s in the samples of the gauge but the one declared expected\. .* \| expected by declaration \(--expect-tls-not-published\), not a failure: reason=tls_not_published node=worker-05 seconds=28800 published=2 count=12\)$' 1
expect "$G" "grade expected: verdict" '^VERDICT prober=PASS unpinned=PASS logs=not-checked$' 1
# ... without the switch the same samples fail, and no line speaks of a declaration.
GF="$TMP" run_grade grade-never "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 1 ]; then pass "grade expected: the default is strict: the same samples without the switch are exit 1"; else fail "grade expected: without the switch exit $grc, want 1"; fi
expect "$G" "grade expected: ... and nothing is declared" '^(EXPECT |UNPINNED expected:)' 0
# ... the switch is for tls_not_published only: an agent without its trust
# domain for five minutes fails with it too,
grade_gauge grade-standing worker-03 trust_domain_unknown "$(gauge_run "$GRUN" 6 90)"
GF="$TMP" run_grade grade-standing "$G" --dir "$GD" --prometheus http://prom.example:9090 --expect-tls-not-published
if [ "$grc" -eq 1 ]; then pass "grade expected: a standing trust_domain_unknown fails with the switch too: exit 1"; else fail "grade expected: trust_domain_unknown with the switch gave exit $grc, want 1"; fi
expect "$G" "grade expected: ... as itself" '^UNPINNED verdict=FAIL .*\(no TLS was published for 300 s or more: reason=trust_domain_unknown node=worker-03 seconds=300 published=90 -- ' 1
# ... and so does the validation gap, beside a declared standing state.
grade_last grade-never unpinned.window worker-02 no_namespace_metadata 6
GF="$TMP" run_grade grade-never "$G" --dir "$GD" --prometheus http://prom.example:9090 --expect-tls-not-published
if [ "$grc" -eq 1 ]; then pass "grade expected: the gap fails beside a declared state: exit 1"; else fail "grade expected: a gap with the switch gave exit $grc, want 1"; fi
expect "$G" "grade expected: ... the verdict is about the gap, and the declared state keeps its line" '^(UNPINNED verdict=FAIL increase=18 .*\(TLS was published without its server-identity pin, the mTLS validation gap: reason=no_namespace_metadata count=6 nodes=worker-02 -- .*\)$|UNPINNED expected: node=worker-05 )' 2

# unknown: an agent newer than this script reports a fifth reason. The script
# cannot say which kind of fact it is, so it fails closed and says so. RED
# before: FAIL, as `a TLS cluster was published without its server-identity pin`.
grade_case grade-unknown
for f in unpinned.window published.window; do
	grade_edit grade-unknown "$f" '.data.result += [.data.result[] | select(.metric.node == "worker-02" and .metric.reason == "pin_not_rendered") | .metric.reason = "some_new_reason"]'
done
G="$TMP/grade-unknown.log"
GF="$TMP" run_grade grade-unknown "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 0 ]; then pass "grade unknown reason at rest: exit 0 (nothing was counted under it)"; else fail "grade unknown reason at rest: exit $grc, want 0"; fi
grade_last grade-unknown unpinned.window worker-02 some_new_reason 1
GF="$TMP" run_grade grade-unknown "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: a reason the script does not know" "$G"
if [ "$grc" -eq 1 ]; then pass "grade unknown reason: exit 1"; else fail "grade unknown reason: exit $grc, want 1"; fi
expect "$G" "grade unknown reason: its class" '^UNPINNED reason=some_new_reason count=1 nodes=worker-02 class=unknown$' 1
expect "$G" "grade unknown reason: FAIL, and the text says the script does not know it" '^UNPINNED verdict=FAIL increase=1 series=21 nodes=5 resets=0  \(this script does not know reason=some_new_reason count=1 nodes=worker-02: an agent newer than this script reports a reason that is in neither GAP_REASONS \(no_namespace_metadata,pin_not_rendered\) nor NO_TLS_REASONS \(trust_domain_unknown,tls_not_published\), and it is not taken for a harmless one\)$' 1
# ... and the unknown reason in the gauge alone, its counter at rest: the same
# closed door. A non-zero gauge sample is a state the agent published under a
# reason this script cannot class, whether or not a snapshot was set in the
# window. RED before: PASS, exit 0 (the gauge was not read).
grade_case grade-unknown-held
grade_edit grade-unknown-held published.window '.data.result += [.data.result[] | select(.metric.node == "worker-02" and .metric.reason == "pin_not_rendered") | .metric.reason = "some_new_reason"]'
grade_last grade-unknown-held published.window worker-02 some_new_reason 5
GF="$TMP" run_grade grade-unknown-held "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 1 ]; then pass "grade unknown reason in the gauge: exit 1 although no counter moved"; else fail "grade unknown reason in the gauge: exit $grc, want 1"; fi
expect "$G" "grade unknown reason in the gauge: FAIL, and the text says the script does not know it" '^UNPINNED verdict=FAIL increase=0 series=20 nodes=5 resets=0  \(this script does not know reason=some_new_reason node=worker-02 published=5: an agent newer than this script ' 1
expect "$G" "grade unknown reason in the gauge: its published line" '^UNPINNED published: node=worker-02 job=aether-agent reason=some_new_reason class=unknown nonzero_samples=1 longest=0s max=5 at_end=5 series=1  \(the gauge held it: fails the gate\)$' 1

# unlabelled: agents from before #1424 (the `born` scenario's: one series per
# node, no reason). Whether TLS was published under what they counted is not
# in the counter, so any movement fails, as it always did, with the text it
# always had -- and now the count and the node. This rule is the old one: the
# old script fails this too; what is new is the class and what follows `--`.
grade_case grade-unlabelled
cp "$GF/born/unpinned."*.json "$GF/born/published.window.json" "$TMP/grade-unlabelled/"
grade_last grade-unlabelled unpinned.window worker-02 - 3
G="$TMP/grade-unlabelled.log"
GF="$TMP" run_grade grade-unlabelled "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: the counter of an agent with no reason label moved" "$G"
if [ "$grc" -eq 1 ]; then pass "grade unlabelled: exit 1"; else fail "grade unlabelled: exit $grc, want 1"; fi
expect "$G" "grade unlabelled: its class" '^UNPINNED reason=- count=3 nodes=worker-02 class=unlabelled$' 1
# shellcheck disable=SC2016 # the backticks are the output's, not a substitution
expect "$G" "grade unlabelled: FAIL with the text it always had, then the count and the node" '^UNPINNED verdict=FAIL increase=3 series=5 nodes=5 resets=0  \(a TLS cluster was published without its server-identity pin: the agent logged which and why, `mesh clusters published with no server-identity SAN pin`; what each reason means is in docs/runbook\.md, "The unpinned-cluster signal" -- reason=- count=3 nodes=worker-02, from agents with no `reason` label \(before #1424\): whether TLS was published under them is not in the counter, so any movement fails\)$' 1

# no gauge: worker-04's counter has the reason label and its gauge has no
# sample in the window. Both arrived in one agent and the gauge is written on
# every snapshot, zeros included: an absent one is not a zero, so how long a
# state stood there was not seen. RED before: PASS, exit 0 (the gauge was not
# read at all).
grade_case grade-nogauge
grade_edit grade-nogauge published.window '.data.result |= map(select(.metric.node != "worker-04"))'
G="$TMP/grade-nogauge.log"
GF="$TMP" run_grade grade-nogauge "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: an agent whose gauge does not reach Prometheus" "$G"
if [ "$grc" -eq 2 ]; then pass "grade no gauge: exit 2, not a pass on a gauge read as zero"; else fail "grade no gauge: exit $grc, want 2"; fi
# shellcheck disable=SC2016 # the backticks are the output's, not a substitution
expect "$G" "grade no gauge: the node and job are named" '^UNPINNED published: absent: node=worker-04 job=aether-agent pod=aether-agent-04  \(its counter has the `reason` label and aether_agent_snapshot_tls_clusters\{pin="unpinned"\} has no sample in the window from it: ' 1
expect "$G" "grade no gauge: UNPROVEN" '^UNPINNED verdict=UNPROVEN increase=0 series=20 nodes=5 resets=0  \(the gauge aether_agent_snapshot_tls_clusters has no sample in the window from worker-04: how long a state stood there was not seen\)$' 1
expect "$G" "grade no gauge: verdict" '^VERDICT prober=PASS unpinned=UNPROVEN logs=not-checked$' 1
# ... and unproven comes before failed here too: the gap moved on worker-02.
grade_last grade-nogauge unpinned.window worker-02 no_namespace_metadata 6
GF="$TMP" run_grade grade-nogauge "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 2 ]; then pass "grade no gauge + gap: exit 2"; else fail "grade no gauge + gap: exit $grc, want 2"; fi
expect "$G" "grade no gauge + gap: UNPROVEN, and it says the counter moved" '^UNPINNED verdict=UNPROVEN increase=6 series=20 nodes=5 resets=0  \(the gauge aether_agent_snapshot_tls_clusters has no sample in the window from worker-04: how long a state stood there was not seen; and the counter moved\)$' 1
expect "$G" "grade no gauge + gap: what moved is still listed, with its class" '^UNPINNED reason=no_namespace_metadata count=6 nodes=worker-02 class=gap$' 1

# A replaced agent is not covered by the gauge of the one before it (review of
# #1494). worker-04's agent is replaced at 05:10. The old pod's counter and
# gauge end there; the new pod exports its seeded counter (four reasons at
# zero) and never sets a snapshot, so it has no gauge. RED before: coverage was
# by node and job, the old pod's samples covered the new one: PASS, exit 0.
grade_case grade-replaced-nogauge
for f in unpinned.window published.window; do
	# shellcheck disable=SC2016 # jq's variables, not the shell's
	grade_edit grade-replaced-nogauge "$f" '.data.result |= map(if .metric.node == "worker-04" then .values |= map(select(.[0] < $run)) else . end)' --argjson run "$GRUN"
done
# shellcheck disable=SC2016
grade_edit grade-replaced-nogauge unpinned.window '.data.result += [.data.result[] | select(.metric.node == "worker-04") | .metric.pod = "aether-agent-04b" | .values = [[$run, "0"], [$end, "0"]]]' --argjson run "$GRUN" --argjson end "$((GT0 + 28800))"
G="$TMP/grade-replaced-nogauge.log"
GF="$TMP" run_grade grade-replaced-nogauge "$G" --dir "$GD" --prometheus http://prom.example:9090
show "prober-grade: a replaced agent whose own gauge does not reach Prometheus" "$G"
if [ "$grc" -eq 2 ]; then pass "grade replaced, no gauge: exit 2, not a pass on the gauge of the pod before it"; else fail "grade replaced, no gauge: exit $grc, want 2"; fi
# shellcheck disable=SC2016 # the backticks are the output's, not a substitution
expect "$G" "grade replaced, no gauge: the new pod is named" '^UNPINNED published: absent: node=worker-04 job=aether-agent pod=aether-agent-04b  \(its counter has the `reason` label and aether_agent_snapshot_tls_clusters\{pin="unpinned"\} has no sample in the window from it: ' 1
expect "$G" "grade replaced, no gauge: ... and only it" '^UNPINNED published: absent: ' 1
expect "$G" "grade replaced, no gauge: UNPROVEN" '^UNPINNED verdict=UNPROVEN increase=0 series=24 nodes=5 resets=0  \(the gauge aether_agent_snapshot_tls_clusters has no sample in the window from worker-04: ' 1
# ... and the pod stays in an exporter's key when ANOTHER exporter in the same
# answer has no per-pod label (review of #1494, second round). worker-04's old
# pod runs on for two minutes beside its replacement (a surge roll) and its
# gauge has samples after the new pod's counter was born; worker-01's series
# carry no `pod` at all. RED before: the key was the labels common to EVERY
# series, so worker-01 took `pod` out of it, the old pod's later samples
# covered the new pod, and the grade was PASS, exit 0.
mkdir -p "$TMP/grade-mixed-labels" && cp "$TMP/grade-replaced-nogauge/"*.json "$TMP/grade-mixed-labels/"
for f in unpinned.window published.window; do
	# shellcheck disable=SC2016 # jq's variables, not the shell's
	grade_edit grade-mixed-labels "$f" '(.data.result[] | select(.metric.pod == "aether-agent-04") | .values) += [[$run, "0"], [$run + 60, "0"], [$run + 120, "0"]]' --argjson run "$GRUN"
done
for f in unpinned.start unpinned.start-time unpinned.window published.window; do
	grade_edit grade-mixed-labels "$f" '.data.result |= map(if .metric.node == "worker-01" then del(.metric.pod) else . end)'
done
GF="$TMP" run_grade grade-mixed-labels "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 2 ]; then pass "grade mixed labels: exit 2: an exporter with no pod label does not take the pod out of another's key"; else fail "grade mixed labels: exit $grc, want 2"; fi
# shellcheck disable=SC2016 # the backticks are the output's, not a substitution
expect "$G" "grade mixed labels: the new pod is named, and only it" '^UNPINNED published: absent: node=worker-04 job=aether-agent pod=aether-agent-04b  \(its counter has the `reason` label and aether_agent_snapshot_tls_clusters\{pin="unpinned"\} has no sample in the window from it: ' 1
expect "$G" "grade mixed labels: ... the exporter with no pod label is covered by its own gauge" '^UNPINNED published: absent: ' 1
# ... the same where the collector has no per-pod label: restarts fall into one
# series per node, job and reason, and no label says whose samples they are.
# The counter does: worker-04's stood at 4 and reads 0 from 05:10, a reset, so
# the process started again there, and its gauge has no sample since.
# RED before: PASS, exit 0.
grade_case grade-collapsed
for f in unpinned.start unpinned.start-time unpinned.window published.window; do
	grade_edit grade-collapsed "$f" '.data.result |= map(del(.metric.pod))'
done
grade_edit grade-collapsed unpinned.start '(.data.result[] | select(.metric.node == "worker-04" and .metric.reason == "trust_domain_unknown") | .value[1]) = "4"'
# shellcheck disable=SC2016 # jq's variables, not the shell's
grade_edit grade-collapsed unpinned.window '(.data.result[] | select(.metric.node == "worker-04" and .metric.reason == "trust_domain_unknown") | .values) = [[$t + 5760, "4"], [$run - 60, "4"], [$run, "0"], [$end, "0"]]' --argjson t "$GT0" --argjson run "$GRUN" --argjson end "$((GT0 + 28800))"
cp "$TMP/grade-collapsed/published.window.json" "$TMP/grade-collapsed.published"
# shellcheck disable=SC2016
grade_edit grade-collapsed published.window '.data.result |= map(if .metric.node == "worker-04" then .values |= map(select(.[0] < $run)) else . end)' --argjson run "$GRUN"
GF="$TMP" run_grade grade-collapsed "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 2 ]; then pass "grade restarted, one series: exit 2 when the gauge has no sample since the counter's reset"; else fail "grade restarted, one series: exit $grc, want 2"; fi
# shellcheck disable=SC2016 # the backticks are the output's, not a substitution
expect "$G" "grade restarted, one series: the line says since when" '^UNPINNED published: absent: node=worker-04 job=aether-agent  \(its counter has the `reason` label and aether_agent_snapshot_tls_clusters\{pin="unpinned"\} has no sample from it since its counter started again at 2026-10-08T05:10:00Z: the samples it has are from the process before: ' 1
expect "$G" "grade restarted, one series: the reset is counted as ever" '^UNPINNED node=worker-04 count=0 series=4 born_in_window=0 resets=1$' 1
# ... and with gauge samples after the reset it is covered: a restart alone is
# not unproven. (Not red before.)
cp "$TMP/grade-collapsed.published" "$TMP/grade-collapsed/published.window.json"
GF="$TMP" run_grade grade-collapsed "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 0 ]; then pass "grade restarted, one series: a gauge sample after the reset covers it: exit 0"; else fail "grade restarted, one series, gauge present: exit $grc, want 0"; fi
expect "$G" "grade restarted, one series: ... no absent line" '^UNPINNED published: absent: ' 0

# A zero that is not a zero: a Prometheus that answers and has neither metric.
G="$TMP/grade-empty.log"
run_grade empty "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 2 ]; then pass "grade empty: exit 2, not a pass"; else fail "grade empty: exit $grc, want 2"; fi
expect "$G" "grade empty: no prober series is UNPROVEN" '^PROBER  verdict=UNPROVEN non_success=0 .* \(no prober series in the window: ' 1
expect "$G" "grade empty: no unpinned series is UNPROVEN (the counter is seeded: absent is not zero)" '^UNPINNED verdict=UNPROVEN increase=0 series=0 nodes=0 resets=0  \(no series: the counter is seeded at zero' 1
# ... and one that does not answer.
G="$TMP/grade-down.log"
run_grade nonexistent "$G" --dir "$GD" --prometheus http://prom.example:9090
if [ "$grc" -eq 2 ]; then pass "grade down: a query that fails is exit 2"; else fail "grade down: exit $grc, want 2"; fi
expect "$G" "grade down: UNPROVEN, with curl's own words, and nothing graded" "^VERDICT UNPROVEN the query 'probe/start' failed \(exit 22\): curl: \(22\) The requested URL returned error: 503" 1
expect "$G" "grade down: no PROBER or UNPINNED line" '^(PROBER|UNPINNED|TOTAL) ' 0

# The endpoint is a parameter, never a default; through the API server it needs a context.
G="$TMP/grade-args.log"
run_grade born "$G" --dir "$GD"
if [ "$grc" -eq 2 ] && grep -q 'no Prometheus: pass --prometheus URL, or --prometheus-service' "$G" && [ ! -s "$TMP/grade-queries.tsv" ]; then
	pass "grade: no endpoint given -> exit 2 and no query made (there is no default cluster)"
else
	fail "grade: no endpoint gave exit $grc: $(cat "$G")"
fi
run_grade born "$G" --dir "$GD" --prometheus-service monitoring/prometheus:9090 --match 'cluster="east"'
if [ "$grc" -eq 1 ] && grep -q '^VERDICT prober=FAIL unpinned=PASS ' "$G"; then pass "grade service: the same grade through the API-server proxy"; else fail "grade service: exit $grc: $(tail -n 2 "$G")"; fi
expect "$G" "grade service: --match goes into the selector" '^QUERY   probe/window  time=2026-10-08T08:10:00Z  aether_probe_requests_total\{cluster="east"\}\[28800s\]$' 1
expect "$G" "grade service: ... and into the gauge's, beside its own matcher" '^QUERY   unpinned/published  time=2026-10-08T08:10:00Z  aether_agent_snapshot_tls_clusters\{pin="unpinned",cluster="east"\}\[28921s\]$' 1
if grep -qF -- '--context some-cluster get --raw /api/v1/namespaces/monitoring/services/prometheus:9090/proxy/api/v1/query?query=aether_probe_requests_total%7Bcluster%3D%22east%22%7D%5B28800s%5D&time='"$((GT0 + 28800))" "$TMP/grade-queries.tsv"; then
	pass "grade service: kubectl is given the run's own context (run.env) and the query, URL-encoded"
else
	fail "grade service: kubectl was called as: $(head -n 2 "$TMP/grade-queries.tsv")"
fi
# An e2e run has no churn log: its window is the load's own.
GE="$TMP/grade-e2e"
mkdir -p "$GE"
printf 'MODE=e2e\nCTX=some-cluster\nT_LOAD=%s\nDURATION_S=900\n' "$GT0" >"$GE/run.env"
run_grade clean "$G" --dir "$GE" --prometheus http://prom.example:9090
expect "$G" "grade e2e: without a churn log the window is run.env's T_LOAD + DURATION_S" '^WINDOW  start=2026-10-08T00:10:00Z end=2026-10-08T00:25:00Z seconds=900  \(T0 from run\.env T_LOAD\)$' 1
# A window that is not over is not graded.
run_grade clean "$G" --start "$(($(date +%s) - 60))" --window 8h --prometheus http://prom.example:9090
if [ "$grc" -eq 2 ] && grep -q 'a grade of a window that is not over is a grade of part of it' "$G" && [ ! -s "$TMP/grade-queries.tsv" ]; then
	pass "grade: a window that ends in the future is refused (exit 2, no query)"
else
	fail "grade: an unfinished window gave exit $grc: $(cat "$G")"
fi
# =================================================== prober-grade.sh (end)

echo
if [ "$FAILS" -eq 0 ]; then
	echo "harness_test: all passed"
else
	echo "harness_test: $FAILS FAILED"
	exit 1
fi
