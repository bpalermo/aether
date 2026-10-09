#!/usr/bin/env bash
# Hermetic test of e2e/pressure/run.sh: no cluster, a fake `kubectl` and a fake
# `curl` that answer from files and log every call, and a fake `pgrep` that
# always matches.
#
# What it holds the soak guard to (`preflight_ack` and `preflight_no_soak`, #1555):
#   - a real run without the operator's acknowledgement (--no-soak-running or
#     NO_SOAK_RUNNING=1) aborts, and the script run as an operator runs it
#     (`main`, not one function) has called neither kubectl nor curl by then; a
#     dry run warns and goes on;
#   - the acknowledgement alone passes on a quiet cluster, and the script says
#     that no pod was looked for;
#   - SOAK_POD_SELECTOR refuses while a pod carries it, in any namespace, and
#     is passed to kubectl as given;
#   - a DaemonSet mid-roll refuses the run, in each of its three forms;
#   - a list that cannot be read is a refusal, never "nothing found";
#   - the guard asks for no object by a name this repository does not define,
#     and never looks at local processes (a `pgrep -f` matches its own waiter).
#
# What it holds every other kubectl and curl call to (#1578): "could not ask"
# is not "absent", and it is not a verdict. For each call, one case makes the
# fake fail the way an unreachable API server does and requires
#   - exit 2 (INCONCLUSIVE), never 1 (FAIL: "#662 is back") and never 0;
#   - a message that says the question could not be asked, next to the tool's
#     own error, and not the message for an absent object;
# and, where absence is a legitimate answer (no node, no agent pod yet, no
# GOMEMLIMIT, no sample), a second case that it still reads as absence.
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

# The error an unreachable API server gives, and the one a closed port gives.
export KUBE_ERR='Unable to connect to the server: dial tcp 192.0.2.10:6443: i/o timeout'
export CURL_ERR='curl: (7) Failed to connect to 127.0.0.1 port 4242: Connection refused'

# kubectl: every call is appended to $FAKE/calls, and gets a key by its shape:
#   config current-context -> context        get node -> node
#   get deploy             -> deploy (deploy-json with -o json)
#   get pods               -> pods (--all-namespaces), agent-pods (by
#                             spec.nodeName), collector-pods (the rest)
#   get pod <name>         -> gomemlimit, memlimit, status-ready,
#                             status-restarts, status-waiting,
#                             status-term-reason, status-term-exit
#   get ds -> ds   get job -> job   delete job -> delete-job
#   delete pod -> delete-pod   apply -> apply   logs -> logs
#   port-forward -> port-forward
# The n-th call of a key answers from, in this order:
#   $FAKE/<key>.err.<n> or $FAKE/<key>.err  -> $KUBE_ERR on stderr, exit 1
#   $FAKE/<key>.out.<n> or $FAKE/<key>.out  -> that file, exit 0
#   neither -> what kubectl does on a cluster that has no such object: exit 0
#              with no output for a list, a jsonpath or --ignore-not-found;
#              NotFound (exit 1) for a named `get` without it; "index out of
#              bounds" (exit 1) for a jsonpath that indexes .items[0] of an
#              empty list; no current context.
# Kept from #1555: `pods` and `ds` answer from $FAKE/pods and $FAKE/ds.json,
# and the Job exists iff $FAKE/job does. `apply` leaves what it was given in
# $FAKE/applied.
cat >"$BIN/kubectl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE/calls"
a=" $* "
key=unknown
case "$a" in
*" config current-context "*) key=context ;;
*" get node "*) key=node ;;
*" get deploy "*)
	key=deploy
	case "$a" in *" -o json "*) key=deploy-json ;; esac
	;;
*" get pods "*)
	key=collector-pods
	case "$a" in
	*" --all-namespaces "*) key=pods ;;
	*"spec.nodeName="*) key=agent-pods ;;
	esac
	;;
*" get pod "*)
	case "$a" in
	*GOMEMLIMIT*) key=gomemlimit ;;
	*limits.memory*) key=memlimit ;;
	*.restartCount*) key=status-restarts ;;
	*.state.waiting.reason*) key=status-waiting ;;
	*.lastState.terminated.reason*) key=status-term-reason ;;
	*.lastState.terminated.exitCode*) key=status-term-exit ;;
	*.ready*) key=status-ready ;;
	esac
	;;
*" get ds "*) key=ds ;;
*" get job "*) key=job ;;
*" delete job "*) key=delete-job ;;
*" delete pod "*) key=delete-pod ;;
*" apply "*) key=apply ;;
*" logs "*) key=logs ;;
*" port-forward "*) key=port-forward ;;
esac
n=$(($(cat "$FAKE/$key.n" 2>/dev/null || echo 0) + 1))
echo "$n" >"$FAKE/$key.n"
if [ -e "$FAKE/$key.err.$n" ] || [ -e "$FAKE/$key.err" ]; then
	echo "$KUBE_ERR" >&2
	exit 1
fi
out=""
if [ -e "$FAKE/$key.out.$n" ]; then
	out="$FAKE/$key.out.$n"
elif [ -e "$FAKE/$key.out" ]; then
	out="$FAKE/$key.out"
fi
case "$key" in
pods) [ -n "$out" ] || out="$FAKE/pods" ;;
ds) [ -n "$out" ] || out="$FAKE/ds.json" ;;
job)
	if [ -e "$FAKE/job" ]; then
		echo "job.batch/the-job"
		exit 0
	fi
	case "$a" in *" --ignore-not-found "*) exit 0 ;; esac
	echo 'Error from server (NotFound): jobs.batch not found' >&2
	exit 1
	;;
node)
	if [ -z "$out" ]; then
		case "$a" in *" --ignore-not-found "*) exit 0 ;; esac
		echo 'Error from server (NotFound): nodes not found' >&2
		exit 1
	fi
	;;
context)
	if [ -z "$out" ]; then
		echo 'error: current-context is not set' >&2
		exit 1
	fi
	;;
agent-pods)
	if { [ -z "$out" ] || [ ! -s "$out" ]; } && [[ "$a" == *"{.items[0]"* ]]; then
		echo 'error: error executing jsonpath "{.items[0].metadata.name}": array index out of bounds: index 0, length 0' >&2
		exit 1
	fi
	;;
apply)
	if [[ "$a" == *" -f - "* ]]; then
		cat >"$FAKE/applied"
	else
		f="${a##* -f }"
		cat "${f%% *}" >"$FAKE/applied"
	fi
	;;
unknown) exit 1 ;;
esac
if [ -n "$out" ] && [ -e "$out" ]; then cat "$out"; fi
exit 0
FAKE

# pgrep: always "finds" a process, as a `pgrep -f <pattern>` does when the
# pattern is in the command line of whatever waits on it.
cat >"$BIN/pgrep" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE/pgrep-calls"
exit 0
FAKE

# curl: recorded with kubectl's calls. A Prometheus query has the key `prom`,
# a collector's /metrics the key `metrics`; the n-th call of a key answers from
# <key>.err[.<n>] ($CURL_ERR on stderr, exit 7) or <key>.out[.<n>] (into the -o
# file when there is one), and exits 1 with neither.
cat >"$BIN/curl" <<'FAKE'
#!/usr/bin/env bash
printf 'curl %s\n' "$*" >>"$FAKE/calls"
key=other
case "$*" in
*/api/v1/query*) key=prom ;;
*/metrics*) key=metrics ;;
esac
n=$(($(cat "$FAKE/$key.n" 2>/dev/null || echo 0) + 1))
echo "$n" >"$FAKE/$key.n"
if [ -e "$FAKE/$key.err.$n" ] || [ -e "$FAKE/$key.err" ]; then
	echo "$CURL_ERR" >&2
	exit 7
fi
out=""
if [ -e "$FAKE/$key.out.$n" ]; then
	out="$FAKE/$key.out.$n"
elif [ -e "$FAKE/$key.out" ]; then
	out="$FAKE/$key.out"
fi
[ -n "$out" ] || exit 1
dest=""
while [ $# -gt 0 ]; do
	if [ "$1" = -o ]; then dest="$2"; fi
	shift
done
if [ -n "$dest" ]; then cat "$out" >"$dest"; else cat "$out"; fi
exit 0
FAKE
chmod +x "$BIN/kubectl" "$BIN/pgrep" "$BIN/curl"

# The inputs run.sh has no default for (#1579), as any caller gives them.
INPUTS=(EXPECT_CONTEXT=ctx-test COLLECTOR_NS=telemetry PROM_NS=monitoring)

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
# its directory first), then the two halves of the guard in the order `main`
# calls them (`preflight_ack`, `preflight_no_soak`) of the sourced script, after
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
		PATH="$BIN:$PATH" FAKE="$FAKE" "${INPUTS[@]}" "$@" \
		bash -c 'source "$1" || exit 97; eval "$2"; preflight_ack; preflight_no_soak' preflight_test "$SCRIPT" "${BEFORE:-}" >"$OUT" 2>&1
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

# lacks <case> <fixed string the output must not contain>
lacks() {
	if grep -qF -- "$2" "$OUT"; then
		fail "$1: output has '$2': $(cat "$OUT")"
	else
		pass "$1: does not say '$2'"
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

# The script as an operator runs it: `main` reaches the acknowledgement before
# it runs any command, so a run that was not acknowledged asks the cluster
# nothing. An acknowledged one goes on to the cluster (and ends there: the fake
# has no context to give).
# run_main <case> [VAR=value ...] -- <arguments of run.sh>
run_main() {
	local name="$1" envs=()
	shift
	while [ "$1" != -- ]; do
		envs+=("$1")
		shift
	done
	shift
	FAKE="$CASES/$name"
	mkdir -p "$FAKE"
	: >"$FAKE/calls"
	: >"$FAKE/pgrep-calls"
	OUT="$FAKE/out"
	env -u NO_SOAK_RUNNING -u SOAK_POD_SELECTOR \
		-u EXPECT_CONTEXT -u COLLECTOR_NS -u PROM_NS \
		PATH="$BIN:$PATH" FAKE="$FAKE" ${envs[@]+"${envs[@]}"} \
		bash "$SCRIPT" "$@" >"$OUT" 2>&1
	RC=$?
}

run_main main-no-ack "${INPUTS[@]}" -- --node n1
want main-no-ack 2 'refusing to run without --no-soak-running'
if [ -s "$FAKE/calls" ]; then
	fail "main-no-ack: the script ran a command before it checked the acknowledgement: $(cat "$FAKE/calls")"
else
	pass "main-no-ack: nothing was run before the refusal"
fi

for how in flag variable; do
	if [ "$how" = flag ]; then
		run_main "main-ack-$how" "${INPUTS[@]}" -- --node n1 --no-soak-running
	else
		run_main "main-ack-$how" "${INPUTS[@]}" NO_SOAK_RUNNING=1 -- --node n1
	fi
	if grep -qF 'refusing to run without' "$OUT"; then
		fail "main-ack-$how: refused although acknowledged: $(cat "$OUT")"
	elif [ "$(head -n 1 "$FAKE/calls")" != 'config current-context' ]; then
		fail "main-ack-$how: did not go on to the cluster (exit $RC): calls=[$(cat "$FAKE/calls")] $(cat "$OUT")"
	else
		pass "main-ack-$how: goes on to the cluster"
	fi
done

# The flag is the same acknowledgement as the variable.
if out=$(env -u NO_SOAK_RUNNING PATH="$BIN:$PATH" FAKE="$CASES/ack" \
	bash -c 'source "$1" || exit 97; parse_args --node n1 --no-soak-running; echo "ack=$NO_SOAK_RUNNING"' preflight_test "$SCRIPT" 2>&1) &&
	[ "$out" = "ack=1" ]; then
	pass "flag: --no-soak-running sets the acknowledgement"
else
	fail "flag: --no-soak-running did not set the acknowledgement: $out"
fi

# --- the inputs with no default (#1579) ----------------------------------------
# The context to expect and the namespaces of the collector and of Prometheus
# are the operator's: an acknowledged run without them names each one and asks
# the cluster nothing. A dry run needs them too.
run_main inputs-missing NO_SOAK_RUNNING=1 -- --node n1
want inputs-missing 2 'EXPECT_CONTEXT'
want inputs-missing 2 'COLLECTOR_NS'
want inputs-missing 2 'PROM_NS'
if [ -s "$FAKE/calls" ]; then
	fail "inputs-missing: the script ran a command without its inputs: $(cat "$FAKE/calls")"
else
	pass "inputs-missing: nothing was run"
fi
run_main inputs-missing-dry-run -- --node n1 --dry-run
want inputs-missing-dry-run 2 'EXPECT_CONTEXT'
run_main inputs-one-missing NO_SOAK_RUNNING=1 EXPECT_CONTEXT=ctx-test COLLECTOR_NS=telemetry -- --node n1
want inputs-one-missing 2 'PROM_NS'
lacks inputs-one-missing 'COLLECTOR_NS'

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

# The Job could not be asked for: that is not "no Job".
prepare job-error
: >"$CASES/job-error/job.err"
guard job-error NO_SOAK_RUNNING=1
want job-error 2 'could not ask for job aether-test/aether-collector-pressure'
want job-error 2 "$KUBE_ERR"

# --- every other call: "could not ask" is not "absent" (#1578) ------------------
# call <case> <program> [VAR=value ...]: sources run.sh and runs <program> (a
# function of the script, after whatever globals it needs) against the fake
# cluster the case prepared. `sleep` advances the shell's clock instead of
# waiting, so a wait loop runs to its deadline at once. Sets RC and OUT.
call() {
	local name="$1" prog="$2"
	shift 2
	FAKE="$CASES/$name"
	mkdir -p "$FAKE"
	: >"$FAKE/calls"
	OUT="$FAKE/out"
	env -u NO_SOAK_RUNNING -u SOAK_POD_SELECTOR \
		PATH="$BIN:$PATH" FAKE="$FAKE" "${INPUTS[@]}" "$@" \
		bash -c 'source "$1" || exit 97; sleep() { SECONDS=$((SECONDS + ${1%.*})); }; eval "$2"' preflight_test "$SCRIPT" "$prog" >"$OUT" 2>&1
	RC=$?
}
# put <case> <file> [content]: one answer of the case's fake cluster.
put() {
	mkdir -p "$CASES/$1"
	printf '%s' "${3:-}" >"$CASES/$1/$2"
}
# asked <case> <what the script must say>: the call failed, so the run is
# INCONCLUSIVE and the tool's own error is on the terminal.
asked() {
	want "$1" 2 "$2"
	want "$1" 2 "${3:-$KUBE_ERR}"
}

# preflight_cluster: the context, the node, the collector's Deployment.
put cluster-context-error context.err
call cluster-context-error 'NODE=n1; preflight_cluster'
asked cluster-context-error 'could not read the current kube context'

put cluster-context-other context.out 'some-other-context'
call cluster-context-other 'NODE=n1; preflight_cluster'
want cluster-context-other 2 "kubectl context is 'some-other-context', expected 'ctx-test'"

put cluster-node-error context.out ctx-test
put cluster-node-error node.err
call cluster-node-error 'NODE=n1; preflight_cluster'
asked cluster-node-error 'could not ask for node n1'
lacks cluster-node-error 'node n1 not found'

put cluster-node-absent context.out ctx-test
call cluster-node-absent 'NODE=n1; preflight_cluster'
want cluster-node-absent 2 'node n1 not found'

put cluster-deploy-error context.out ctx-test
put cluster-deploy-error node.out node/n1
put cluster-deploy-error deploy.err
call cluster-deploy-error 'NODE=n1; preflight_cluster'
asked cluster-deploy-error 'could not read Deployment telemetry/otel-collector'

put cluster-not-ready context.out ctx-test
put cluster-not-ready node.out node/n1
put cluster-not-ready deploy.out '2 1'
call cluster-not-ready 'NODE=n1; preflight_cluster'
want cluster-not-ready 2 'telemetry/otel-collector is 1/2 ready'

# readyReplicas is absent from a Deployment with none ready.
put cluster-none-ready context.out ctx-test
put cluster-none-ready node.out node/n1
put cluster-none-ready deploy.out '2 '
call cluster-none-ready 'NODE=n1; preflight_cluster'
want cluster-none-ready 2 'telemetry/otel-collector is 0/2 ready'

put cluster-ok context.out ctx-test
put cluster-ok node.out node/n1
put cluster-ok deploy.out '2 2'
call cluster-ok 'NODE=n1; preflight_cluster'
want cluster-ok 0 'context=ctx-test node=n1 collector=2/2 ready'

# preflight_agent: the node's agent pod and its status.
put agent-list-error agent-pods.err
call agent-list-error 'NODE=n1; preflight_agent'
asked agent-list-error 'could not list the aether-agent pods on node n1'
lacks agent-list-error 'no aether-agent pod on node n1'

call agent-none 'NODE=n1; preflight_agent'
want agent-none 2 'no aether-agent pod on node n1'

put agent-ready-error agent-pods.out aether-agent-aaa
put agent-ready-error status-ready.err
call agent-ready-error 'NODE=n1; preflight_agent'
asked agent-ready-error 'could not read the status of agent pod aether-agent-aaa'
lacks agent-ready-error 'is not Ready'

put agent-restarts-error agent-pods.out aether-agent-aaa
put agent-restarts-error status-ready.out true
put agent-restarts-error status-restarts.err
call agent-restarts-error 'NODE=n1; preflight_agent'
asked agent-restarts-error 'could not read the status of agent pod aether-agent-aaa'
lacks agent-restarts-error 'restarts — start from a clean node'

put agent-ok agent-pods.out aether-agent-aaa
put agent-ok status-ready.out true
put agent-ok status-restarts.out 0
call agent-ok 'NODE=n1; preflight_agent'
want agent-ok 0 'agent pod aether-agent-aaa Ready, restarts=0'

# discover_collector_pods: the Deployment's selector, then its Running pods.
put collector-deploy-error deploy-json.err
call collector-deploy-error 'discover_collector_pods'
asked collector-deploy-error 'could not read Deployment telemetry/otel-collector'

DEPLOY_JSON='{"spec":{"selector":{"matchLabels":{"app":"col","tier":"gw"}}}}'
put collector-pods-error deploy-json.out "$DEPLOY_JSON"
put collector-pods-error collector-pods.err
call collector-pods-error 'discover_collector_pods'
asked collector-pods-error 'could not list the collector pods (-l app=col,tier=gw) of telemetry'
lacks collector-pods-error 'no Running collector pods'

put collector-pods-none deploy-json.out "$DEPLOY_JSON"
call collector-pods-none 'discover_collector_pods'
want collector-pods-none 2 'no Running collector pods matched -l app=col,tier=gw in telemetry'

put collector-no-selector deploy-json.out '{"spec":{}}'
call collector-no-selector 'discover_collector_pods'
want collector-no-selector 2 'could not read .spec.selector.matchLabels from telemetry/otel-collector'

put collector-pods-ok deploy-json.out "$DEPLOY_JSON"
put collector-pods-ok collector-pods.out 'c-0
c-1
'
call collector-pods-ok 'discover_collector_pods'
want collector-pods-ok 0 'collector replicas (-l app=col,tier=gw): c-0 c-1'

# resolve_collector_limits: GOMEMLIMIT may be absent; "could not read it" is
# not that.
put limits-gomemlimit-error gomemlimit.err
put limits-gomemlimit-error memlimit.out 2Gi
call limits-gomemlimit-error 'COLLECTOR_PODS=(c-0); resolve_collector_limits'
asked limits-gomemlimit-error 'could not read GOMEMLIMIT of telemetry/c-0'
lacks limits-gomemlimit-error 'no literal GOMEMLIMIT'

put limits-memlimit-error gomemlimit.out 1638MiB
put limits-memlimit-error memlimit.err
call limits-memlimit-error 'COLLECTOR_PODS=(c-0); resolve_collector_limits'
asked limits-memlimit-error 'could not read the memory limit of telemetry/c-0'
lacks limits-memlimit-error 'has no memory limit'

put limits-gomemlimit-absent memlimit.out 2Gi
call limits-gomemlimit-absent 'COLLECTOR_PODS=(c-0); resolve_collector_limits; echo "gomemlimit=$GOMEMLIMIT_BYTES"'
want limits-gomemlimit-absent 0 'derived: no literal GOMEMLIMIT in the pod env'
want limits-gomemlimit-absent 0 'gomemlimit=2147483648'

call limits-memlimit-absent 'COLLECTOR_PODS=(c-0); resolve_collector_limits'
want limits-memlimit-absent 2 'has no memory limit'

# The collector's own /metrics: a port-forward that did not come up, a fetch
# that failed and an answer with no collector sample are three things. In
# `auto` each one falls back to Prometheus, and the reason is said.
put pf-error port-forward.err
call pf-error 'unset -f sleep; COLLECTOR_PODS=(c-0); SCRATCH="$FAKE"; start_collector_metrics && echo rc=0 || echo rc=1'
want pf-error 0 'rc=1'
want pf-error 0 "$KUBE_ERR"

put metrics-fetch-error metrics.err
call metrics-fetch-error 'start_pf() { echo "0 4242"; }; COLLECTOR_PODS=(c-0); SCRATCH="$FAKE"; start_collector_metrics && echo rc=0 || echo rc=1'
want metrics-fetch-error 0 'rc=1'
want metrics-fetch-error 0 'could not fetch c-0:8888/metrics'
want metrics-fetch-error 0 "$CURL_ERR"
lacks metrics-fetch-error 'did not serve otelcol_* samples'

put metrics-no-samples metrics.out 'go_goroutines 12
'
call metrics-no-samples 'start_pf() { echo "0 4242"; }; COLLECTOR_PODS=(c-0); SCRATCH="$FAKE"; start_collector_metrics && echo rc=0 || echo rc=1'
want metrics-no-samples 0 'rc=1'
want metrics-no-samples 0 'did not serve otelcol_* samples'

METRICS='# HELP x
otelcol_process_runtime_heap_alloc_bytes{a="b"} 1048576
otelcol_process_memory_rss_bytes 2097152
otelcol_processor_memory_limiter_refused_metric_points_total{p="x"} 7
otelcol_receiver_refused_log_records_total 3
'
SNAP='COLLECTOR_PODS=(c-0); CPF_PORTS=(4242); SCRATCH="$FAKE"; collector_snapshot; echo "heap=$M_HEAP rss=$M_RSS points=$M_POINTS logs=$M_LOGS"'
put snapshot-ok metrics.out "$METRICS"
call snapshot-ok "$SNAP"
want snapshot-ok 0 'heap=1048576 rss=2097152 points=7 logs=3'

put snapshot-fetch-error metrics.err
call snapshot-fetch-error "$SNAP"
asked snapshot-fetch-error "could not read c-0's /metrics" "$CURL_ERR"

# An answer that is not the collector's (a proxy's error page) is not a
# collector with a zero heap and no refusals.
put snapshot-no-samples metrics.out '<html>502 Bad Gateway</html>
'
call snapshot-no-samples "$SNAP"
want snapshot-no-samples 2 "c-0's /metrics answered with no otelcol_* sample"
lacks snapshot-no-samples 'heap=0'

# Prometheus: a query that could not be asked, one Prometheus refused, and one
# that returned no sample.
PROM_EMPTY='{"status":"success","data":{"resultType":"vector","result":[]}}'
prom_value() { printf '{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1760000000,"%s"]}]}}' "$1"; }
PROMQ='PROM_PORT=4242; v=$(promq_req up "the up series"); echo "v=$v"'

put prom-transport-error prom.err
call prom-transport-error "$PROMQ"
asked prom-transport-error 'could not query Prometheus for the up series' "$CURL_ERR"
lacks prom-transport-error 'no data for'

put prom-status-error prom.out '{"status":"error","errorType":"bad_data","error":"parse error: unexpected end of input"}'
call prom-status-error "$PROMQ"
want prom-status-error 2 'could not query Prometheus for the up series'
want prom-status-error 2 'parse error: unexpected end of input'
lacks prom-status-error 'no data for'

put prom-not-json prom.out '<html>502 Bad Gateway</html>'
call prom-not-json "$PROMQ"
want prom-not-json 2 'could not query Prometheus for the up series'
lacks prom-not-json 'no data for'

put prom-no-sample prom.out "$PROM_EMPTY"
call prom-no-sample "$PROMQ"
want prom-no-sample 2 'no data for the up series'
lacks prom-no-sample 'could not query'

put prom-value prom.out "$(prom_value 12.7)"
call prom-value "$PROMQ"
want prom-value 0 'v=12'

# preflight_signals: the prober's rate, then the age of the agent's series.
put signals-age-error prom.out.1 "$(prom_value 25)"
put signals-age-error prom.err.2
call signals-age-error 'NODE=n1; PROM_PORT=4242; preflight_signals'
asked signals-age-error 'could not query Prometheus for the age of aether_agent_storage_pods{node="n1"}' "$CURL_ERR"
lacks signals-age-error 'has no samples'

put signals-age-absent prom.out.1 "$(prom_value 25)"
put signals-age-absent prom.out.2 "$PROM_EMPTY"
call signals-age-absent 'NODE=n1; PROM_PORT=4242; preflight_signals'
want signals-age-absent 2 'aether_agent_storage_pods{node="n1"} has no samples'

put signals-ok prom.out.1 "$(prom_value 25)"
put signals-ok prom.out.2 "$(prom_value 14)"
call signals-ok 'NODE=n1; PROM_PORT=4242; preflight_signals'
want signals-ok 0 'prober ~25/s success, agent series age 14s'

# apply_job: a failed apply is not a verdict, and the Job it may have made is
# the cleanup trap's to delete. The Job is aimed at the collector the caller
# named.
APPLY='trap '\''echo "JOB_APPLIED=$JOB_APPLIED"'\'' EXIT; apply_job'
put apply-error apply.err
call apply-error "$APPLY"
asked apply-error 'could not apply'
want apply-error 2 'JOB_APPLIED=1'

call apply-ok "$APPLY"
want apply-ok 0 'JOB_APPLIED=1'
if grep -qF -- '--otlp-endpoint=otel-collector.telemetry.svc.cluster.local:4317' "$FAKE/applied" &&
	grep -qF 'namespaces: ["telemetry"]' "$FAKE/applied" && ! grep -q '__[A-Z_]*__' "$FAKE/applied"; then
	pass "apply-ok: the Job is aimed at the caller's collector"
else
	fail "apply-ok: the applied Job is not aimed at telemetry/otel-collector: $(grep -n 'otlp-endpoint\|namespaces:\|__' "$FAKE/applied")"
fi

call apply-endpoint "$APPLY" COLLECTOR_OTLP_ENDPOINT=gw.elsewhere:4317
if grep -qF -- '--otlp-endpoint=gw.elsewhere:4317' "$FAKE/applied"; then
	pass "apply-endpoint: COLLECTOR_OTLP_ENDPOINT is the endpoint flooded"
else
	fail "apply-endpoint: $(grep -n 'otlp-endpoint' "$FAKE/applied")"
fi

# restart_agent.
put restart-list-error agent-pods.err
call restart-list-error 'NODE=n1; restart_agent'
asked restart-list-error 'could not list the aether-agent pods on node n1'

put restart-delete-error agent-pods.out aether-agent-aaa
put restart-delete-error delete-pod.err
call restart-delete-error 'NODE=n1; restart_agent'
asked restart-delete-error 'could not delete agent pod aether-agent-aaa'

# wait_agent_replaced: FAIL (exit 1) says the replacement never appeared. A
# list that could not be read says nothing about the agent, and the moment
# between the old pod going and the new one being created is an empty list,
# not an error.
REPLACED='NODE=n1; OLD_POD=aether-agent-aaa; POD_APPEAR_TIMEOUT=20; wait_agent_replaced'
put replaced-errors agent-pods.err
call replaced-errors "$REPLACED"
asked replaced-errors 'could not list the aether-agent pods on node n1'
lacks replaced-errors 'no replacement agent pod appeared'

put replaced-gap agent-pods.out.1 ''
put replaced-gap agent-pods.out.2 aether-agent-bbb
call replaced-gap "$REPLACED"
want replaced-gap 0 'replacement agent pod aether-agent-bbb created'

put replaced-blip agent-pods.err.1
put replaced-blip agent-pods.out 'aether-agent-aaa
aether-agent-bbb
'
call replaced-blip "$REPLACED"
want replaced-blip 0 'replacement agent pod aether-agent-bbb created'

put replaced-never agent-pods.out aether-agent-aaa
call replaced-never "$REPLACED"
want replaced-never 1 'no replacement agent pod appeared on n1'

# wait_agent_ready.
READY='NEW_POD=aether-agent-bbb; AGENT_READY_TIMEOUT=20; wait_agent_ready'
put ready-errors status-waiting.err
call ready-errors "$READY"
asked ready-errors 'could not read the status of agent pod aether-agent-bbb'
lacks ready-errors 'did not become Ready'

put ready-blip status-waiting.err.1
put ready-blip status-ready.err.1
put ready-blip status-ready.out true
call ready-blip "$READY"
want ready-blip 0 'replacement agent pod aether-agent-bbb Ready'

put ready-never status-ready.out false
call ready-never "$READY"
want ready-never 1 'did not become Ready within 20s'

put ready-crashloop status-waiting.out CrashLoopBackOff
call ready-crashloop "$READY"
want ready-crashloop 1 'is in CrashLoopBackOff'

# verify_agent: the verdict is read off the pod's status and its log. One that
# could not be read is neither a PASS nor #662's signature.
VERIFY='NEW_POD=aether-agent-bbb; verify_agent'
put verify-restarts-error status-restarts.err
call verify-restarts-error "$VERIFY"
asked verify-restarts-error 'could not read the status of agent pod aether-agent-bbb'
lacks verify-restarts-error "#662's signature"

put verify-term-error status-restarts.out 0
put verify-term-error status-term-reason.err
call verify-term-error "$VERIFY"
asked verify-term-error 'could not read the status of agent pod aether-agent-bbb'

put verify-logs-error status-restarts.out 0
put verify-logs-error logs.err
call verify-logs-error "$VERIFY"
asked verify-logs-error 'could not read the log of agent pod aether-agent-bbb'
lacks verify-logs-error 'never logged'

put verify-no-spire status-restarts.out 0
put verify-no-spire logs.out 'starting
'
call verify-no-spire "$VERIFY"
want verify-no-spire 1 "never logged 'resolved workload trust domain from SPIRE'"

put verify-ok status-restarts.out 0
put verify-ok logs.out 'starting
resolved workload trust domain from SPIRE
'
call verify-ok "$VERIFY"
want verify-ok 0 'restarts=0, no terminated container, SPIRE Workload API source established'

# The Job's deletion: after the agent's checks, at a safety ceiling, and in the
# cleanup trap. A delete that failed did not delete, and the trap tries again.
put stop-error delete-job.err
call stop-error 'JOB_APPLIED=1; trap '\''echo "JOB_APPLIED=$JOB_APPLIED"'\'' EXIT; stop_pressure'
asked stop-error 'could not delete job aether-test/aether-collector-pressure'
want stop-error 2 'JOB_APPLIED=1'
lacks stop-error 'pressure job deleted'

CEILING='M_RSS=900; M_HEAP=900; ABORT_HEAP_BYTES=50; ABORT_RSS_BYTES=50; GOMEMLIMIT_BYTES=100; POD_MEM_LIMIT_BYTES=100; trap '\''echo "JOB_APPLIED=$JOB_APPLIED"'\'' EXIT; track_memory'
put ceiling-delete-error delete-job.err
call ceiling-delete-error "JOB_APPLIED=1; $CEILING"
asked ceiling-delete-error 'the job could NOT be deleted'
want ceiling-delete-error 2 'JOB_APPLIED=1'
lacks ceiling-delete-error 'job deleted'

call ceiling-delete-ok "JOB_APPLIED=1; $CEILING"
want ceiling-delete-ok 2 'job deleted'
want ceiling-delete-ok 2 'JOB_APPLIED=0'

# Before the Job is applied (the baseline, and every dry run) there is nothing
# to delete, and a dry run changes nothing.
call ceiling-no-job "JOB_APPLIED=0; $CEILING"
want ceiling-no-job 2 'no job had been applied'
if grep -q 'delete' "$FAKE/calls"; then
	fail "ceiling-no-job: deleted something before any Job was applied: $(cat "$FAKE/calls")"
else
	pass "ceiling-no-job: nothing was deleted"
fi

put cleanup-delete-error delete-job.err
call cleanup-delete-error 'JOB_APPLIED=1; trap cleanup EXIT; exit 1'
want cleanup-delete-error 1 'WARN: could not delete job aether-test/aether-collector-pressure'
want cleanup-delete-error 1 "$KUBE_ERR"

call cleanup-delete-ok 'JOB_APPLIED=1; trap cleanup EXIT; exit 0'
want cleanup-delete-ok 0 'cleanup: deleting job aether-test/aether-collector-pressure'
lacks cleanup-delete-ok 'WARN'

# wait_series_fresh: FAIL says the agent's telemetry did not resume.
FRESH='NODE=n1; PROM_PORT=4242; POLL_INTERVAL=5; SERIES_FRESH_TIMEOUT=20; wait_series_fresh'
put fresh-errors prom.err
call fresh-errors "$FRESH"
asked fresh-errors 'could not query Prometheus for the age of aether_agent_storage_pods{node="n1"}' "$CURL_ERR"
lacks fresh-errors 'did not go fresh'

put fresh-blip prom.err.1
put fresh-blip prom.out "$(prom_value 3)"
call fresh-blip "$FRESH"
want fresh-blip 0 'is fresh again (3s old)'

put fresh-stale prom.out "$(prom_value 500)"
call fresh-stale "$FRESH"
want fresh-stale 1 'did not go fresh within 20s (age: 500)'

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
