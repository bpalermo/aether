#!/usr/bin/env bash
# Hermetic test of the soak guard of e2e/pressure/run.sh (`preflight_no_soak`,
# #1555): no cluster, a fake `kubectl` that answers from files and logs every
# call, and a fake `pgrep` that always matches.
#
# What it holds the guard to:
#   - a real run without the operator's acknowledgement (--no-soak-running or
#     NO_SOAK_RUNNING=1) aborts, before any kubectl call; a dry run warns and
#     goes on;
#   - the acknowledgement alone passes on a quiet cluster, and the script says
#     that no pod was looked for;
#   - SOAK_POD_SELECTOR refuses while a pod carries it, in any namespace, and
#     is passed to kubectl as given;
#   - a DaemonSet mid-roll refuses the run, in each of its three forms;
#   - a list that cannot be read is a refusal, never "nothing found";
#   - the guard asks for no object by a name this repository does not define,
#     and never looks at local processes (a `pgrep -f` matches its own waiter).
#
# Run: bazel test //e2e/pressure:preflight_test (jq is the Bazel-pinned one),
#      or bash e2e/pressure/preflight_test.sh with jq on PATH.
# shellcheck disable=SC2016 # the single-quoted programs given to `bash -c`
# expand in that shell, on purpose, not in this one.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${PREFLIGHT_TEST_SCRIPT:-$HERE/run.sh}"

# Under Bazel, JQ_RLOCATIONPATH names the pinned jq in the runfiles.
if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}

# --- the fakes ----------------------------------------------------------------
BIN="$TMP/bin"
CASES="$TMP/cases"
mkdir -p "$BIN" "$CASES"
ln -s "$JQ" "$BIN/jq"

# kubectl: every call is appended to $FAKE/calls. Answers:
#   get pods --all-namespaces -l <sel> -o name   -> $FAKE/pods  (exit 1 if $FAKE/pods.err)
#   -n <ns> get ds -o json                       -> $FAKE/ds.json (exit 1 if $FAKE/ds.err)
#   -n <ns> get job <name>                       -> exit 0 iff $FAKE/job exists
#   anything else                                -> exit 1 (NotFound)
cat >"$BIN/kubectl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE/calls"
case " $* " in
*" get pods "*)
	[ -e "$FAKE/pods.err" ] && exit 1
	cat "$FAKE/pods" 2>/dev/null
	exit 0
	;;
*" get ds -o json "*)
	[ -e "$FAKE/ds.err" ] && exit 1
	cat "$FAKE/ds.json"
	exit 0
	;;
*" get job "*)
	[ -e "$FAKE/job" ]
	exit
	;;
esac
exit 1
FAKE

# pgrep: always "finds" a process, as a `pgrep -f <pattern>` does when the
# pattern is in the command line of whatever waits on it.
cat >"$BIN/pgrep" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE/pgrep-calls"
exit 0
FAKE
chmod +x "$BIN/kubectl" "$BIN/pgrep"

# ds <name> <generation> <observed> <desired> <updated> <unavailable>
ds() {
	printf '{"metadata":{"name":"%s","generation":%s},"status":{"observedGeneration":%s,"desiredNumberScheduled":%s,"updatedNumberScheduled":%s,"numberUnavailable":%s}}' "$@"
}
ds_list() {
	local IFS=,
	printf '{"items":[%s]}' "$*"
}
QUIET="$(ds_list "$(ds aether-agent 4 4 5 5 0)" "$(ds aether-proxy 7 7 5 5 0)")"

prepare() { mkdir -p "$CASES/$1"; }

# guard <case> [VAR=value ...]: a fake cluster (quiet, unless the case prepared
# its directory first), then `preflight_no_soak` of the sourced script, after
# $BEFORE when a case sets it (the script assigns its own globals when it is
# sourced, so a global that has no environment variable is set after that).
# Sets RC, and OUT to the file of its output.
guard() {
	local name="$1"
	shift
	FAKE="$CASES/$name"
	mkdir -p "$FAKE"
	[ -e "$FAKE/ds.json" ] || printf '%s' "$QUIET" >"$FAKE/ds.json"
	: >"$FAKE/calls"
	: >"$FAKE/pgrep-calls"
	OUT="$FAKE/out"
	env -u NO_SOAK_RUNNING -u SOAK_POD_SELECTOR \
		PATH="$BIN:$PATH" FAKE="$FAKE" "$@" \
		bash -c 'source "$1" || exit 97; eval "$2"; preflight_no_soak' preflight_test "$SCRIPT" "${BEFORE:-}" >"$OUT" 2>&1
	RC=$?
	BEFORE=""
}

# want <case> <rc> <fixed string the output must contain>
want() {
	if [ "$RC" != "$2" ]; then
		fail "$1: exit $RC, want $2: $(cat "$OUT")"
	elif ! grep -qF -- "$3" "$OUT"; then
		fail "$1: output lacks '$3': $(cat "$OUT")"
	else
		pass "$1"
	fi
}

# --- the acknowledgement ------------------------------------------------------
guard no-ack
want no-ack 2 'refusing to run without --no-soak-running'
if [ -s "$FAKE/calls" ]; then
	fail "no-ack: the cluster was queried before the acknowledgement was checked: $(cat "$FAKE/calls")"
else
	pass "no-ack: aborts before any kubectl call"
fi

guard no-ack-zero NO_SOAK_RUNNING=0
want no-ack-zero 2 'refusing to run without --no-soak-running'

BEFORE='DRY_RUN=1'
guard no-ack-dry-run
want no-ack-dry-run 0 'WARN: --no-soak-running not given'

guard ack NO_SOAK_RUNNING=1
want ack 0 'SOAK_POD_SELECTOR is not set: no pod was looked for'
if grep -q 'get pods' "$FAKE/calls"; then
	fail "ack: pods were listed with no selector set: $(cat "$FAKE/calls")"
else
	pass "ack: no pod list without a selector"
fi

# The flag is the same acknowledgement as the variable.
if out=$(env -u NO_SOAK_RUNNING PATH="$BIN:$PATH" FAKE="$CASES/ack" \
	bash -c 'source "$1" || exit 97; parse_args --node n1 --no-soak-running; echo "ack=$NO_SOAK_RUNNING"' preflight_test "$SCRIPT" 2>&1) &&
	[ "$out" = "ack=1" ]; then
	pass "flag: --no-soak-running sets the acknowledgement"
else
	fail "flag: --no-soak-running did not set the acknowledgement: $out"
fi

# --- SOAK_POD_SELECTOR --------------------------------------------------------
prepare selector-hit
printf 'pod/loader-abc\npod/loader-def\n' >"$CASES/selector-hit/pods"
guard selector-hit NO_SOAK_RUNNING=1 SOAK_POD_SELECTOR='role=load,tier!=x'
want selector-hit 2 "2 pod(s) carry SOAK_POD_SELECTOR='role=load,tier!=x'"
if grep -qxF -- 'get pods --all-namespaces -l role=load,tier!=x -o name' "$FAKE/calls"; then
	pass "selector-hit: the selector reaches kubectl as given, across namespaces"
else
	fail "selector-hit: unexpected pod query: $(cat "$FAKE/calls")"
fi

guard selector-miss NO_SOAK_RUNNING=1 SOAK_POD_SELECTOR='role=load'
want selector-miss 0 "no pod carries SOAK_POD_SELECTOR='role=load'"

prepare selector-error
: >"$CASES/selector-error/pods.err"
guard selector-error NO_SOAK_RUNNING=1 SOAK_POD_SELECTOR='role=load'
want selector-error 2 'could not list pods by SOAK_POD_SELECTOR'

# --- a DaemonSet mid-roll -----------------------------------------------------
roll_case() {
	prepare "$1"
	ds_list "$(ds aether-agent 4 4 5 5 0)" "$2" >"$CASES/$1/ds.json"
	guard "$1" NO_SOAK_RUNNING=1
	want "$1" 2 "DaemonSet(s) mid-roll in aether-system: $3 — "
}
roll_case roll-not-updated "$(ds aether-proxy 7 7 5 3 0)" aether-proxy
roll_case roll-unavailable "$(ds aether-proxy 7 7 5 5 1)" aether-proxy
roll_case roll-not-observed "$(ds aether-proxy 8 7 5 5 0)" aether-proxy
roll_case roll-two "$(ds aether-proxy 7 7 5 3 0),$(ds aether-mesh-dns 2 2 5 4 1)" 'aether-proxy aether-mesh-dns'

guard roll-other-namespace NO_SOAK_RUNNING=1 AGENT_NS=mesh-ns
want roll-other-namespace 0 'no DaemonSet of mesh-ns is mid-roll'
if grep -qxF -- '-n mesh-ns get ds -o json' "$FAKE/calls"; then
	pass "roll-other-namespace: AGENT_NS is the namespace asked"
else
	fail "roll-other-namespace: unexpected DaemonSet query: $(cat "$FAKE/calls")"
fi

prepare ds-error
: >"$CASES/ds-error/ds.err"
guard ds-error NO_SOAK_RUNNING=1
want ds-error 2 'could not list the DaemonSets of aether-system'

prepare ds-garbage
printf 'not json' >"$CASES/ds-garbage/ds.json"
guard ds-garbage NO_SOAK_RUNNING=1
want ds-garbage 2 'could not read the DaemonSets of aether-system'

# --- what the guard kept ------------------------------------------------------
prepare job-exists
: >"$CASES/job-exists/job"
guard job-exists NO_SOAK_RUNNING=1
want job-exists 2 'job aether-test/aether-collector-pressure already exists'

# --- what the guard must not do -----------------------------------------------
# No object is asked for by a name this repository does not define, and local
# processes are never consulted: the fake pgrep always matches, so a guard
# that asked it would have refused every case above that had to pass.
for c in "$CASES"/*/; do
	name="$(basename "$c")"
	if grep -q 'k6\|churn' "$c/calls"; then
		fail "$name: the guard asked the cluster for a harness's own name: $(grep 'k6\|churn' "$c/calls")"
	fi
	if [ -s "$c/pgrep-calls" ]; then
		fail "$name: the guard ran pgrep: $(cat "$c/pgrep-calls")"
	fi
done
pass "checked $(find "$CASES" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ') case(s) for harness names and pgrep"
if grep -n 'pgrep\|k6-soak-loader\|churn\.sh' "$SCRIPT"; then
	fail "run.sh still names pgrep, k6-soak-loader or churn.sh"
else
	pass "run.sh names no pgrep, k6-soak-loader or churn.sh"
fi

if [ "$FAILS" -gt 0 ]; then
	echo "$FAILS failure(s)"
	exit 1
fi
echo "all passed"
