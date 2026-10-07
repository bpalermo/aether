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
#     divisibility) and the rendered plan of both profiles;
#   - sortie-gate.sh (proposal 042) against canned sortie JSON reports in
#     testdata/sortie/: clean, one target with stream resets, a missing target,
#     pool_overflow, a dead engine, a short pool.
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
expect "$P" "plan soak: the sequencer does not spin" '^        value: SLEEP$' 1
expect "$P" "plan soak: an execution does not stop at its first failure (all four default predicates lifted)" '^        benchmark\.(http_4xx|http_5xx|pool_connection_failure|stream_resets): 1000000000000$' 4
expect "$P" "plan soak: due requests queue under the CPU limit" '^    max_pending_requests: 16$' 1
expect "$P" "plan soak: the 2xx total rides in the report" '"counter:benchmark.http_2xx > 0"' 1
expect "$P" "plan soak: no stats block unless asked" '^stats:' 0
expect "$P" "plan soak: floor = 99% x 3 rps x 5 nodes" '"rate:benchmark.http_2xx >= 14.85"' 1
for c in http_4xx http_5xx stream_resets pool_connection_failure pool_failure_timeout pool_overflow; do
	expect "$P" "plan soak: zero-failure threshold $c" "\"counter:benchmark.$c == 0\"" 1
done
if [ "$(sed -n 's/.*weight: \([0-9]*\)}.*/\1/p' "$P" | awk '{s += $1} END {print s}')" = 60 ]; then pass "plan soak: the weights are the shares (sum 60)"; else fail "plan soak: weights do not sum to the rate"; fi
P="$TMP/plan-e2e.yaml"
bash "$PLAN" render --profile e2e --backends 2 --dns x.y.svc.cluster.local:8443 --statsd 10.106.234.157:8125 >"$P"
expect "$P" "plan e2e: 15 minutes" '^      duration: 900s$' 1
expect "$P" "plan e2e: floor follows the backend count" '"rate:benchmark.http_2xx >= 5.94"' 1
expect "$P" "plan e2e: statsd address templated in" '^    address: "10.106.234.157:8125"$' 1
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
GATE="$HERE/sortie-gate.sh"
SF="$HERE/testdata/sortie"
run_gate() { # run_gate <fixture> <out>: canned report, 3 targets, 2 backends
	bash "$GATE" --report "$SF/$1.json" --shares "$SF/shares.tsv" --backends 2 --engines "$SF/engines.tsv" >"$2" 2>&1
}
G="$TMP/gate-clean.log"
run_gate clean "$G"
rc=$?
show "gate: clean report" "$G"
if [ "$rc" -eq 0 ]; then pass "gate clean: exit 0"; else fail "gate clean: exit $rc, want 0"; fi
expect "$G" "gate clean: one PASS per target" '^PASS  (svc-1|echo|uds-echo)  rate=.* failures=none$' 3
expect "$G" "gate clean: rate and count come from the report" '^PASS  svc-1  rate=17.99/18rps http_2xx=2685 backends=2/2 ' 1
expect "$G" "gate clean: verdict" '^VERDICT PASS targets=3 passed=3 failed=0 backends=2 report_pass=true$' 1

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
expect "$G" "gate missing: verdict FAIL although the report itself says pass" '^VERDICT FAIL targets=3 passed=2 failed=1 backends=2 report_pass=true$' 1

G="$TMP/gate-overflow.log"
run_gate pool-overflow "$G"
rc=$?
show "gate: pool_overflow (the client refused its own requests)" "$G"
if [ "$rc" -eq 1 ]; then pass "gate overflow: exit 1"; else fail "gate overflow: exit $rc, want 1"; fi
expect "$G" "gate overflow: per-node counts" '^FAIL  echo .* -- failures pool_overflow=271 \[main-worker-01=130 main-worker-02=141\]' 1
expect "$G" "gate overflow: the target's OWN rate floor (16.19 passes the plan-wide 5.94)" 'rate 16.19 rps < 99% of 18$' 1
expect "$G" "gate overflow: only that target" '^FAIL ' 1

G="$TMP/gate-died.log"
run_gate engine-died "$G"
rc=$?
show "gate: an engine died mid-run" "$G"
if [ "$rc" -eq 1 ]; then pass "gate died: exit 1"; else fail "gate died: exit $rc, want 1"; fi
expect "$G" "gate died: the execution error is the reason" '^FAIL  svc-1 .* -- error: backend 10.10.1.14:8443: awaiting execution response' 1

G="$TMP/gate-backends.log"
bash "$GATE" --report "$SF/clean.json" --shares "$SF/shares.tsv" --backends 5 >"$G" 2>&1
rc=$?
show "gate: the pool resolved 2 engines, 5 expected" "$G"
if [ "$rc" -eq 1 ]; then pass "gate backends: exit 1"; else fail "gate backends: exit $rc, want 1"; fi
expect "$G" "gate backends: a short pool fails every target" ' -- backends 2/5 \| rate ' 3

echo '{"not":"a report"}' >"$TMP/garbage.json"
bash "$GATE" --report "$TMP/garbage.json" --shares "$SF/shares.tsv" --backends 2 >"$TMP/gate-garbage.log" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then pass "gate: not a sortie report -> exit 2, not a verdict"; else fail "gate: garbage report gave exit $rc, want 2"; fi
bash "$GATE" --report "$TMP/nope.json" --shares "$SF/shares.tsv" --backends 2 >"$TMP/gate-nofile.log" 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then pass "gate: no report file -> exit 2"; else fail "gate: missing report gave exit $rc, want 2"; fi

echo
if [ "$FAILS" -eq 0 ]; then
	echo "harness_test: all passed"
else
	echo "harness_test: $FAILS FAILED"
	exit 1
fi
