#!/usr/bin/env bash
# Dry tests for the soak harness's parsing logic -- no cluster, no network:
#   - restart-watch.sh (#1242) against canned `kubectl get pods -o json`
#     fixtures in testdata/restart-watch/, through a fake kubectl;
#   - udscsi-window.awk (#1243), the uds-csi step's plugin-down detector,
#     against canned `kubectl get pods -w -o jsonpath` lines.
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

echo
if [ "$FAILS" -eq 0 ]; then
	echo "harness_test: all passed"
else
	echo "harness_test: $FAILS FAILED"
	exit 1
fi
