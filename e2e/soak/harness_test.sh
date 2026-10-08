#!/usr/bin/env bash
# Dry tests for the soak harness's parsing logic -- no cluster, no network:
#   - restart-watch.sh (#1242) against canned `kubectl get pods -o json`
#     fixtures in testdata/restart-watch/, through a fake kubectl;
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
#     execution cap with the stages after it not run, a results stream with a
#     broken last line and one holding two runs, and two old-format reports,
#     which are refused. The kind-* files are REAL: trimmed from kind runs of
#     this harness against sortie 9fcbb81 (a clean run, its results stream, a
#     capped staircase and the sortie pod's log of it). The others are canned:
#     the shape is sortie's, the numbers are made up;
#   - sortie-save.sh: its offline --times, and the save itself against a fake
#     kubectl (both files, a stream with a broken last line and no report, a
#     download that breaks off, a reader pod that cannot be deleted);
#   - sortie-values.yaml and run.sh, read: the three digest pins, no CPU limit
#     on the engine, the PriorityClass on the engines AND the sortie pod, and
#     the engine's name taken from its node.
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

# --- restart-watch: stopped early -----------------------------------------------
L="$TMP/term.log"
rm -rf "$TMP/state" && mkdir -p "$TMP/state"
FAKE_DIR="$FIX/clean" FAKE_STATE="$TMP/state" RESTART_WATCH_KUBECTL="$TMP/kubectl" \
	bash "$WATCH" --context fake --interval 3600 --duration 2h --log "$L" &
wpid=$!
for _ in $(seq 1 50); do
	if grep -q ' BASELINE count=' "$L" 2>/dev/null; then break; fi
	sleep 0.1
done
t0=$(date +%s)
kill -TERM "$wpid"
wait "$wpid"
rc=$?
show "TERM mid-interval" "$L"
if [ "$rc" -eq 143 ] && [ $(($(date +%s) - t0)) -lt 5 ]; then pass "term: exits 143 at once"; else fail "term: exit $rc after $(($(date +%s) - t0))s"; fi
expect "$L" "term: verdict UNPROVEN ended=TERM" ' SUMMARY restart-watch verdict=UNPROVEN new_restarts=0 containers=0 samples=0 error_samples=0 baseline=1 ended=TERM ' 1

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
*" apply -f -"*) sed -n 's/.*command: \["cat", "\/report\/\(.*\)"\].*/\1/p' >"$FAKE_SAVE/reading" ;;
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

echo
if [ "$FAILS" -eq 0 ]; then
	echo "harness_test: all passed"
else
	echo "harness_test: $FAILS FAILED"
	exit 1
fi
