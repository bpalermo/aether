#!/usr/bin/env bash
# 8-hour soak churn driver: 33 rolling restarts (31 schedule entries; the TRIPLE
# fires three at once), including three aether-mesh-dns DaemonSet rolls (surge +
# SO_REUSEPORT handoff), two aether-uds-csi DaemonSet rolls with a UDS pod deleted
# mid-roll (#1109), and one CONCURRENT triple (agent + proxy + svc at the same
# instant) as the stress peak -- followed by a deliberate 90-minute NO-ROLL WINDOW
# and a demand-set SHRINK.
#
# Pre-flight in the foreground first (loud, exits non-zero on any problem), then
# run detached -- it waits between rolls for ~7h32m:
#   bash "$PWD/e2e/soak/churn.sh" --context talos-main --preflight &&
#     nohup setsid bash "$PWD/e2e/soak/churn.sh" --context talos-main "rev192/0.92.0" >/dev/null 2>&1 &
#
# Progress is appended to $LOG with UTC timestamps; grep ROLLED to count.
#
# ------------------------------------------------- context, pre-flight, abort (#951)
#
#   --context NAME   kubeconfig context for EVERY kubectl call (also SOAK_CONTEXT;
#                    default talos-main). Never the kubeconfig's current-context:
#                    `kind delete cluster` clears it, and on 2026-09-26 the driver
#                    then ran 58 minutes with zero rolls -- every `rollout restart`
#                    hit localhost:8080, logged FAILED, and the schedule carried on
#                    while k6 and the prober made the soak look healthy.
#   --preflight      run the pre-flight only, print the result, exit.
#   --new-sa-once    run ONE new-ServiceAccount step now (see below) and exit: the
#                    pre-run check that the step works on this cluster, and the
#                    way to exercise it on kind. Writes its own fresh $LOG.
#   --uds-csi-once   run ONE uds-csi roll step now (see below) and exit; same
#                    purpose and the same fresh-$LOG behaviour as --new-sa-once.
#
# Pre-flight runs BEFORE the log is touched and before T0: the API server must answer
# /readyz, every workload the schedule rolls (and the SHRINK target) must exist, and
# the context must be allowed to patch them. Any failure prints to stderr and exits 2
# with $LOG untouched -- a bad launch leaves no half-run log behind.
#
# The first FAILED roll (or a SHRINK that cannot scale) logs `CHURN ABORTED` and exits
# 1, after restoring the shrink target. A schedule with holes is not a soak: stop,
# fix, relaunch with a fresh T0.
#
# ------------------------------------------------- stopping it (#1419, #1418)
#
# `kill -TERM <pid>`, and again until it has exited (INT is the same, 130 for
# 143). It then, in this order: waits for the kubectl call that is in the
# foreground, if any (a call is never cut in two); restores a SHRINK in progress
# and deletes a new-SA step's objects; stops what it left in the background -- a
# queued RSS sampler, a uds-csi step's plugin watch -- and waits for a TRIPLE's
# rolls that are already under way; exits 143. No process of the driver is left.
#
# It waits WITHOUT a child process (lib-wait.sh: `read -t` on a private fifo),
# so a TERM is taken within two seconds wherever the schedule is. It used to
# `sleep` in the foreground, and bash runs a trap only once the foreground child
# has ended: TERM took effect up to 12 minutes later, 90 in the no-roll window.
#
# "Again until it has exited": bash 5.2 can drop a trapped signal that arrives
# while it expands a `$(...)`. The log's timestamp and the waits no longer use
# one. A step still does while it runs, for its kubectl calls and its deadlines
# (`$(date +%s)`): a single TERM that lands there can be dropped.
#
# ---------------------------------------------------------------- the schedule
#
#   T0+   what                       why
#   ----  -------------------------  --------------------------------------------
#   12    svc-1        24  svc-2
#   36    mesh-dns     48  svc-3
#   60    proxy  *     72  agent      the ONLY scheduled agent roll besides TRIPLE
#   84    svc-5        96  proxy  *
#   108   svc-1        120 edge
#   132   svc-2        144 mesh-dns
#   156   svc-3        168 svc-4
#   162   UDS-CSI      the plugin DaemonSet + a uds-echo pod deleted mid-roll
#   180   svc-5        192 svc-1
#   204   edge         216 proxy  *
#   228   svc-2        240 svc-4
#   252   proxy  *     264 svc-3
#   270   UDS-CSI      the plugin DaemonSet + a uds-cr-echo pod deleted mid-roll
#   276   svc-1
#   300   TRIPLE *     agent + proxy + svc-3 concurrently -- the stress peak, and
#                      the LAST agent roll of the run (see the no-roll window)
#   312   mesh-dns     324 svc-4
#   336   proxy  *     348 svc-2
#   360   svc-1        the last roll of any kind
#   ----  -------------------------  --------------------------------------------
#   126   NEW-SA #1    a pod under a brand-new ServiceAccount (#1008/#1014),
#   288   NEW-SA #2    ~2 min of traffic, then deleted -- see below
#   ----  -------------------------  --------------------------------------------
#   360   NO-ROLL WINDOW BEGIN       nothing is rolled for 90 minutes
#   450   NO-ROLL WINDOW END
#   450   SHRINK                     svc-5 -> 0 replicas for 90s, then restored
#   ~452  churn driver complete      (k6 runs 8h30m: both land under load AND the runners cannot self-restart inside the graded T0+8h window)
#
#   * = 30 minutes later an age-matched proxy RSS sample is taken in the
#       background (sample-proxy-rss.sh --at-age 1800), for #628.
#
# ------------------------------------------- why the first proxy roll is at T0+60
#
# It used to be at T0+36. On 2026-09-19 that first proxy roll cost 15 mesh_dns
# timeouts across all five probers while the other five proxy rolls of the same run,
# with identical echo placement, cost 0/1/0/0/0. The one thing that set it apart is
# that it replaced the only Envoy generation born BEFORE the load started. Two
# explanations fit: the pre-load generation itself (connections that predate k6), or
# a roll landing before the load had settled. Moving it to a full hour after T0 (k6
# starts ~4 minutes before T0) separates them: if the first roll still costs an order
# of magnitude more than the rest, it is the pre-load generation, not the timing.
# mesh-dns took the T0+36 slot; the set of rolls, and the tally, were unchanged.
#
# ------------------------------------------------ why the window and the shrink
#
# #682: the node agent's demand-scoped dependency set holds each observed upstream
# for 1h, and rolling aether-agent rebuilds that set with a fresh TTL. The old
# schedule rolled the agent at T0+90m and again at T0+300m, so the TTL could never
# expire inside a soak -- the harness was STRUCTURALLY BLIND to the whole
# TTL-expiry -> ODCDS-stall class for its entire history, and the defect only ever
# surfaced in the quiet hours between runs. The no-roll window sits after the last
# agent roll and lasts longer than the TTL, so the expiry now happens under load,
# inside the graded window, on the nodes with no local replica of the upstream.
#
# The SHRINK is the cheap second trigger the 2026-09-05 grading found: ANY demand-set
# shrink -- not the 1h TTL specifically -- drops the cluster on a node with no local
# replica and exposes the ODCDS stall, in seconds instead of an hour. svc-5 is the
# target on purpose: it is the one service k6 declares as an upstream but never
# actually drives, so bouncing it cannot pollute the k6 error rate. It is NOT free on
# the prober SLI, though this header used to say so: on 2026-09-19 the restore cost 18
# mesh_dns timeouts (43% of that run's total), all client-side deadline expiries (`DC`
# in the access log, no UF/UH/URX/NR) ~20s after the agents logged `inbound chain
# references a secret absent from the snapshot` for svc-5's returning pods. Grade the
# SHRINK as its own episode.
#
# Both steps are opt-out (default ON):  SOAK_NO_ROLL_WINDOW=0   SOAK_SHRINK=0
#
# ------------------------------------------------ the new-ServiceAccount step
#
# #1008: every other step rolls a workload whose ServiceAccount the node has already
# seen, so the run never makes a node proxy build clusters for an identity that is new
# to it -- and that is exactly where a QUIC twin added after its base was subscribed sat
# warming for its full 15s initial_fetch_timeout (1,060 x 503/NC on rev242, visible only
# because the k6 loaders happened to start 58s before T0; fixed in #1012). Each NEW-SA
# step creates, together, a ServiceAccount + ConfigMap + 1-replica Deployment named
# sa-new-<epoch> in $NEWSA_NS, pinned to one worker node (round-robin across steps), whose
# pod (newsa-client.sh) drives ~20 rps at two destinations (one of them QUIC on the proving run; both QUIC-eligible since #979) for 2
# minutes from its first instant, with user agent aether-soak-newsa/sa-new-<epoch>. The
# driver then reads the pod's tally, logs
#   ROLLED newsa/sa-new-<epoch> <node> ready=<s>s <dst>:ok=..,non2xx=..,connerr=..,codes=.. ...
# (or FAILED newsa/... with the same tally when any request was not a 2xx), and deletes
# all three objects together. A FAILED newsa step is a GATE FINDING, not a hole in the
# schedule, so it does not abort the run; a step that could not produce a tally at all
# (apply refused, pod never Ready, no final line) is a hole and aborts like a failed roll.
#
#   SOAK_NEWSA=0                 opt out (default ON)
#   SOAK_NEWSA_OFFSETS="126 288" minutes after T0; nudged off T0+120 (edge roll) and
#                                T0+300 (TRIPLE) so neither confounds the other. Leave
#                                >= 4 minutes before the next roll: the step blocks.
#   SOAK_NEWSA_SECONDS=120  SOAK_NEWSA_RPS=20  SOAK_NEWSA_NS=aether-test
#   SOAK_NEWSA_TARGETS="svc-1=http://svc-1.aether-test.aether.internal:18081/ svc-3=..."
#   SOAK_NEWSA_UPSTREAMS="svc-1,svc-3"  SOAK_NEWSA_IMAGE=curlimages/curl:8.22.0
#
# ------------------------------------------------------- the uds-csi roll (#1109)
#
# The csi.aether.io node plugin (aether-uds-csi, proposal 039) is a per-node
# privileged component since chart 2.0.0, and nothing else here rolls it. Each
# UDS-CSI step exercises the three things a plugin roll can break: the per-pod
# mounts of running UDS pods persisting across it (k6 and uds-client keep
# driving them), NodeUnpublishVolume for a pod that is deleted WHILE its node's
# plugin is down (the kubelet retries until the new plugin registers), and a new
# pod mounting right after. The step:
#   1. picks a Running pod of the step's UDS Deployment (round-robin over
#      SOAK_UDSCSI_VICTIMS: uds-echo = the annotation path, uds-cr-echo = the
#      EndpointPolicy path) and the plugin pod on that pod's node;
#   2. starts a `kubectl get pods -w` on that node's plugin pods, `rollout
#      restart`s the DaemonSet and, as soon as the watch shows the node's plugin
#      DOWN (the old pod terminating or gone, no Ready replacement yet), deletes
#      the UDS pod (#1243: the step synchronises on that event, not on a timer).
#      The window is then one of:
#        window=down    the delete was accepted while the plugin was down,
#                       confirmed by a read AFTER the delete -- the leg ran;
#        window=late    the plugin was Ready again by the time the delete was
#                       accepted -- the leg may not have run;
#        window=missed  the plugin was never seen down within
#                       SOAK_UDSCSI_WINDOW_TIMEOUT, or a replacement was already
#                       Ready when first seen -- the leg did not run.
#      late/missed also log a `UDSCSI window=<w> ... NOT exercised` line with
#      the reason and what the watch saw. Never retried: a retry would roll the
#      DaemonSet a second time and change the roll count;
#   3. waits for the DaemonSet rollout, the deleted pod to finish Terminating, and
#      its replacement to become Ready; then checks every node running the plugin
#      lists csi.aether.io in its CSINode, and counts FailedMount events in the
#      victim's namespace during the step and in the 30s after it (the latter
#      MUST be 0: a FailedMount that outlives the roll);
#   4. logs ONE line:
#        ROLLED aether-system/daemonset/aether-uds-csi victim=<ns>/<pod>@<node> window=<down|late|missed>
#          rollout=<s>s terminated=<s>s replacement=<pod> ready=<s>s csinode=<k>/<n>
#          failedmount_during=<n> failedmount_after=<n>
#      or FAILED ... with the same fields plus bad=<what>. Like a FAILED newsa
#      step, a FAILED uds-csi step is a GATE FINDING and does not abort the run;
#      only a refused `rollout restart` (or no victim pod to delete) is a hole
#      and aborts.
#
#   SOAK_UDSCSI=0                   opt out (default ON)
#   SOAK_UDSCSI_OFFSETS="162 270"   minutes after T0: midway between two svc rolls,
#                                   clear of the TRIPLE (300), the new-SA steps
#                                   (126, 288), every proxy/agent roll and the
#                                   no-roll window (360-450). A step takes ~2-4 min.
#   SOAK_UDSCSI_VICTIMS="aether-test/uds-echo aether-test/uds-cr-echo"
#                                   <ns>/<Deployment>, pods labelled app=<Deployment>
#   SOAK_UDSCSI_TIMEOUT=600         seconds for the DaemonSet rollout; 300 each for
#                                   the pod to terminate and its replacement
#   SOAK_UDSCSI_WINDOW_TIMEOUT=300  seconds to wait for the victim node's plugin
#                                   to go down (the roll reaches nodes one by one)
#
# Needs bash 4.2+ and kubectl.
set -uo pipefail

# From the first line (as in restart-watch.sh): with a trap set, bash takes a
# signal between commands, after the foreground child has ended. Untrapped, TERM
# kills the shell where it stands and the kubectl it was waiting for lives on.
# Replaced below, once there is a SHRINK or a new-SA step to undo.
trap 'exit 143' TERM
trap 'exit 130' INT

CTX="${SOAK_CONTEXT:-talos-main}"
PREFLIGHT_ONLY=0
NEWSA_ONCE=0
UDSCSI_ONCE=0
BUILD_LABEL=""
while [ $# -gt 0 ]; do
	case "$1" in
	--context)
		CTX="${2:-}"
		shift 2 || shift
		;;
	--context=*)
		CTX="${1#--context=}"
		shift
		;;
	--preflight)
		PREFLIGHT_ONLY=1
		shift
		;;
	--new-sa-once)
		NEWSA_ONCE=1
		shift
		;;
	--uds-csi-once)
		UDSCSI_ONCE=1
		shift
		;;
	-*)
		echo "churn.sh: unknown flag '$1' (usage: churn.sh [--context NAME] [--preflight] [--new-sa-once] [--uds-csi-once] [BUILD_LABEL])" >&2
		exit 2
		;;
	*)
		if [ -n "$BUILD_LABEL" ]; then
			echo "churn.sh: more than one BUILD_LABEL ('$BUILD_LABEL', '$1')" >&2
			exit 2
		fi
		BUILD_LABEL="$1"
		shift
		;;
	esac
done
BUILD_LABEL="${BUILD_LABEL:-unspecified-build}"
if [ "$NEWSA_ONCE" = "1" ] && [ "$UDSCSI_ONCE" = "1" ]; then
	echo "churn.sh: --new-sa-once and --uds-csi-once are separate runs; pass one" >&2
	exit 2
fi
if [ -z "$CTX" ]; then
	echo "churn.sh: --context needs a kubeconfig context name" >&2
	exit 2
fi
LOG="${SOAK_CHURN_LOG:-/tmp/soak-churn.log}"

# Every kubectl call in this script goes through k: the context is never implicit.
k() { kubectl --context "$CTX" "$@"; }

# Ready, schedulable, non-control-plane nodes, sorted -- the new-SA step's round-robin.
newsa_nodes() {
	k --request-timeout=15s get nodes -l '!node-role.kubernetes.io/control-plane' \
		-o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{" "}{.spec.unschedulable}{"\n"}{end}' 2>/dev/null |
		awk '$2 == "True" && $3 != "true" {print $1}' | sort
}

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# The clock and the wait, neither with a child process: soak_now, soak_stamp,
# nap_until, nap, nap_brief.
# shellcheck source=e2e/soak/lib-wait.sh
. "$HERE/lib-wait.sh"
SAMPLER="${SOAK_PROXY_RSS_SAMPLER:-$HERE/sample-proxy-rss.sh}"

# Age-matched proxy RSS sampling (#628): sleep 28 minutes after a proxy roll, then
# let the sampler poll the last couple of minutes until the youngest proxy pod is
# exactly 30 minutes old. Without the age match, a "higher" node is usually just an
# older incarnation.
PROXY_RSS_AGE="${SOAK_PROXY_RSS_AGE:-1800}"
PROXY_RSS_DELAY="${SOAK_PROXY_RSS_DELAY:-1680}"

# No-roll window (#682). Begins when the last scheduled roll returns.
NO_ROLL_END_MIN="${SOAK_NO_ROLL_END_MIN:-450}"

# Demand-set shrink (#682).
SHRINK_NS="${SOAK_SHRINK_NS:-aether-test}"
SHRINK_TARGET="${SOAK_SHRINK_TARGET:-deployment/svc-5}"
SHRINK_SECONDS="${SOAK_SHRINK_SECONDS:-90}"
SHRINK_PREV=""

# New-ServiceAccount step (#1008/#1014). See the header.
NEWSA_NS="${SOAK_NEWSA_NS:-aether-test}"
NEWSA_OFFSETS="${SOAK_NEWSA_OFFSETS:-126 288}"
NEWSA_SECONDS="${SOAK_NEWSA_SECONDS:-120}"
NEWSA_RPS="${SOAK_NEWSA_RPS:-20}"
NEWSA_IMAGE="${SOAK_NEWSA_IMAGE:-curlimages/curl:8.22.0@sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777}"
NEWSA_TARGETS="${SOAK_NEWSA_TARGETS:-svc-1=http://svc-1.aether-test.aether.internal:18081/ svc-3=http://svc-3.aether-test.aether.internal:18081/}"
NEWSA_UPSTREAMS="${SOAK_NEWSA_UPSTREAMS:-svc-1,svc-3}"
NEWSA_CLIENT="$HERE/newsa-client.sh"
NEWSA_ON="${SOAK_NEWSA:-1}"
NEWSA_STEP=0
NEWSA_LIVE=""

# uds-csi roll (#1109). See the header.
UDSCSI_ON="${SOAK_UDSCSI:-1}"
UDSCSI_OFFSETS="${SOAK_UDSCSI_OFFSETS:-162 270}"
UDSCSI_VICTIMS="${SOAK_UDSCSI_VICTIMS:-aether-test/uds-echo aether-test/uds-cr-echo}"
UDSCSI_TIMEOUT="${SOAK_UDSCSI_TIMEOUT:-600}"
UDSCSI_NS="aether-system"
UDSCSI_DS="daemonset/aether-uds-csi"
UDSCSI_SELECTOR="app.kubernetes.io/component=uds-csi"
UDSCSI_DRIVER="csi.aether.io"
UDSCSI_WINDOW_TIMEOUT="${SOAK_UDSCSI_WINDOW_TIMEOUT:-300}"
UDSCSI_WINDOW_AWK="$HERE/udscsi-window.awk"
# One line per plugin pod observation, the udscsi-window.awk input format. No
# {range} in it: kubectl 1.35's `get -w -o jsonpath` with a {range}...{end}
# template prints the initial list and then EXITS 0 at the first change (seen on
# kind while building #1243), so the watch would end exactly when it matters.
UDSCSI_POD_FMT='pod={.metadata.name} del={.metadata.deletionTimestamp} ready={.status.conditions[?(@.type=="Ready")].status}{"\n"}'
UDSCSI_STEP=0

# The two waits that were a `sleep`, which also took `90s` or `1.5`: the wait is
# now arithmetic on the clock, so say so before T0 instead of at T0+450m.
if ! [[ "$PROXY_RSS_DELAY" =~ ^[0-9]+$ ]] || ! [[ "$SHRINK_SECONDS" =~ ^[0-9]+$ ]]; then
	echo "churn.sh: SOAK_PROXY_RSS_DELAY and SOAK_SHRINK_SECONDS take whole seconds (got '$PROXY_RSS_DELAY', '$SHRINK_SECONDS')" >&2
	exit 2
fi
nap_open churn.sh

# Pre-flight (#951). Writes to stderr only -- never to $LOG -- and runs before T0.
# "<namespace> <kind>/<name>" for every workload the schedule or the SHRINK touches.
PREFLIGHT_TARGETS=(
	"aether-system daemonset/aether-agent"
	"aether-system daemonset/aether-proxy"
	"aether-system daemonset/aether-mesh-dns"
	"aether-system daemonset/aether-uds-csi"
	"aether-ingress deployment/aether-edge"
	"aether-test deployment/svc-1"
	"aether-test deployment/svc-2"
	"aether-test deployment/svc-3"
	"aether-test deployment/svc-4"
	"aether-test deployment/svc-5"
	"$SHRINK_NS $SHRINK_TARGET"
)
# The new-SA step creates and deletes objects instead of patching them, and needs a
# worker node to pin to and the pod's log to read its tally.
preflight_newsa() {
	local out fail=0 verb res
	if [ ! -r "$NEWSA_CLIENT" ]; then
		echo "churn.sh: PRE-FLIGHT FAILED: new-SA client script missing at $NEWSA_CLIENT" >&2
		return 1
	fi
	if ! out=$(k --request-timeout=15s get namespace "$NEWSA_NS" -o name 2>&1); then
		echo "churn.sh: PRE-FLIGHT FAILED: new-SA namespace $NEWSA_NS not found on context '$CTX': $out" >&2
		return 1
	fi
	for verb in create delete; do
		for res in serviceaccounts configmaps deployments.apps; do
			if ! out=$(k --request-timeout=15s -n "$NEWSA_NS" auth can-i "$verb" "$res" 2>&1); then
				echo "churn.sh: PRE-FLIGHT FAILED: context '$CTX' may not $verb $res in $NEWSA_NS ($out)" >&2
				fail=1
			fi
		done
	done
	if ! out=$(k --request-timeout=15s -n "$NEWSA_NS" auth can-i get pods --subresource=log 2>&1); then
		echo "churn.sh: PRE-FLIGHT FAILED: context '$CTX' may not read pod logs in $NEWSA_NS ($out)" >&2
		fail=1
	fi
	if [ -z "$(newsa_nodes)" ]; then
		echo "churn.sh: PRE-FLIGHT FAILED: no Ready worker node to pin the new-SA step to" >&2
		fail=1
	fi
	return "$fail"
}

# The uds-csi step patches the plugin DaemonSet (a PREFLIGHT_TARGETS entry when it
# is on) and deletes one pod of each victim Deployment; it reads CSINodes and the
# victims' FailedMount events.
preflight_udscsi() {
	local out fail=0 spec vns vdep running
	for spec in $UDSCSI_VICTIMS; do
		vns="${spec%%/*}" vdep="${spec#*/}"
		if ! out=$(k --request-timeout=15s -n "$vns" get "deployment/$vdep" -o name 2>&1); then
			echo "churn.sh: PRE-FLIGHT FAILED: UDS victim $vns/deployment/$vdep not found on context '$CTX' (install charts/udsecho, or SOAK_UDSCSI=0): $out" >&2
			fail=1
			continue
		fi
		running=$(k --request-timeout=15s -n "$vns" get pods -l "app=$vdep" --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
		if [ "$running" -eq 0 ]; then
			echo "churn.sh: PRE-FLIGHT FAILED: no Running pod labelled app=$vdep in $vns to delete mid-roll" >&2
			fail=1
		fi
		if ! out=$(k --request-timeout=15s -n "$vns" auth can-i delete pods 2>&1); then
			echo "churn.sh: PRE-FLIGHT FAILED: context '$CTX' may not delete pods in $vns ($out)" >&2
			fail=1
		fi
		if ! out=$(k --request-timeout=15s -n "$vns" auth can-i list events 2>&1); then
			echo "churn.sh: PRE-FLIGHT FAILED: context '$CTX' may not list events in $vns ($out)" >&2
			fail=1
		fi
	done
	if [ ! -r "$UDSCSI_WINDOW_AWK" ]; then
		echo "churn.sh: PRE-FLIGHT FAILED: uds-csi window detector missing at $UDSCSI_WINDOW_AWK" >&2
		fail=1
	fi
	if ! out=$(k --request-timeout=15s -n "$UDSCSI_NS" auth can-i watch pods 2>&1); then
		echo "churn.sh: PRE-FLIGHT FAILED: context '$CTX' may not watch pods in $UDSCSI_NS ($out)" >&2
		fail=1
	fi
	if ! out=$(k --request-timeout=15s auth can-i list csinodes.storage.k8s.io 2>&1); then
		echo "churn.sh: PRE-FLIGHT FAILED: context '$CTX' may not list CSINodes ($out)" >&2
		fail=1
	fi
	return "$fail"
}

preflight() {
	local out fail=0 ns obj t
	if ! out=$(k --request-timeout=15s get --raw /readyz 2>&1); then
		echo "churn.sh: PRE-FLIGHT FAILED: context '$CTX' cannot reach a ready API server:" >&2
		printf '  %s\n' "$out" >&2
		echo "churn.sh: refusing to start; $LOG untouched, no T0 written. Check \`kubectl config get-contexts\` and pass --context." >&2
		return 1
	fi
	if [ "$NEWSA_ONCE" = "1" ]; then PREFLIGHT_TARGETS=(); fi
	if [ "$UDSCSI_ONCE" = "1" ]; then PREFLIGHT_TARGETS=("$UDSCSI_NS $UDSCSI_DS"); fi
	if [ "$UDSCSI_ON" = "0" ] && [ "$UDSCSI_ONCE" != "1" ]; then
		local keep=()
		for t in "${PREFLIGHT_TARGETS[@]}"; do
			if [ "$t" != "$UDSCSI_NS $UDSCSI_DS" ]; then keep+=("$t"); fi
		done
		PREFLIGHT_TARGETS=("${keep[@]}")
	fi
	for t in "${PREFLIGHT_TARGETS[@]}"; do
		read -r ns obj <<<"$t"
		if ! out=$(k --request-timeout=15s -n "$ns" get "$obj" -o name 2>&1); then
			echo "churn.sh: PRE-FLIGHT FAILED: $ns/$obj not found on context '$CTX': $out" >&2
			fail=1
			continue
		fi
		if ! out=$(k --request-timeout=15s -n "$ns" auth can-i patch "$obj" 2>&1); then
			echo "churn.sh: PRE-FLIGHT FAILED: context '$CTX' may not patch $ns/$obj ($out)" >&2
			fail=1
		fi
	done
	if { [ "$NEWSA_ON" != "0" ] && [ "$UDSCSI_ONCE" != "1" ]; } || [ "$NEWSA_ONCE" = "1" ]; then
		preflight_newsa || fail=1
	fi
	if { [ "$UDSCSI_ON" != "0" ] && [ "$NEWSA_ONCE" != "1" ]; } || [ "$UDSCSI_ONCE" = "1" ]; then
		preflight_udscsi || fail=1
	fi
	if [ "$fail" -ne 0 ]; then
		echo "churn.sh: refusing to start; $LOG untouched, no T0 written." >&2
		return 1
	fi
	local newsa_state="new-SA step ON at T0+{${NEWSA_OFFSETS// /,}}m"
	if [ "$NEWSA_ONCE" = "1" ]; then newsa_state="new-SA step once, now"; elif [ "$NEWSA_ON" = "0" ] || [ "$UDSCSI_ONCE" = "1" ]; then newsa_state="new-SA step OFF"; fi
	local udscsi_state="uds-csi step ON at T0+{${UDSCSI_OFFSETS// /,}}m"
	if [ "$UDSCSI_ONCE" = "1" ]; then udscsi_state="uds-csi step once, now"; elif [ "$UDSCSI_ON" = "0" ] || [ "$NEWSA_ONCE" = "1" ]; then udscsi_state="uds-csi step OFF"; fi
	echo "churn.sh: pre-flight OK on context '$CTX' (API ready; ${#PREFLIGHT_TARGETS[@]} targets present and patchable; $newsa_state; $udscsi_state)" >&2
}

if ! preflight; then exit 2; fi
if [ "$PREFLIGHT_ONLY" = "1" ]; then exit 0; fi

soak_now T0

# Start a FRESH log, archiving any previous run alongside it. Without this the driver
# appends to the last soak's file, and `grep -c ROLLED` -- which teardown uses to confirm
# 33 rolls -- silently double-counts, so a run looks complete when it is not.
if [ -s "$LOG" ]; then
	mv -f "$LOG" "$LOG.$(date -u +%Y%m%dT%H%M%SZ).prev"
fi
: >"$LOG"

log() {
	local ts
	soak_stamp ts
	echo "$ts $*" >>"$LOG"
}

# Never leave the shrink target scaled to zero, even if the driver is killed mid-shrink.
restore_shrink() {
	if [ -n "$SHRINK_PREV" ]; then
		local prev="$SHRINK_PREV"
		SHRINK_PREV=""
		if k -n "$SHRINK_NS" scale "$SHRINK_TARGET" --replicas="$prev" >>"$LOG" 2>&1; then
			log "SHRINK done $SHRINK_NS/$SHRINK_TARGET restored to replicas=$prev"
		else
			log "SHRINK FAILED to restore $SHRINK_NS/$SHRINK_TARGET to replicas=$prev -- RESTORE BY HAND"
			return 1
		fi
	fi
}

# Stop what the driver has running in the background (#1419): a queued RSS
# sampler (still waiting, or sampling), a uds-csi step's plugin watch, a
# TRIPLE's rolls.
#
# The children are asked for, not remembered: `pid=$!` after a `&` is a second
# command, and a TERM between the two leaves a child nobody has the PID of.
# `jobs -pr` is bash's own list of its children that are still running, so it
# also never names a PID that has since become some other process.
#
# TERM, and again every 0.2 s until the child is gone. One TERM can miss: a
# child that is signalled before it has stopped being a copy of this shell
# takes the signal with this shell's handler and then drops it. The sampler
# traps TERM and exits once its kubectl call has returned; the watch is kubectl
# itself and dies; a TRIPLE's roll ignores TERM and ends when its `rollout
# restart` has returned, so this waits for it.
#
# It returns only when the list is empty: the driver never exits ahead of a
# child. A kubectl call that does not return (none of these has a request
# timeout) therefore holds the driver, and that is the lesser harm: a driver
# that is still there shows in `pgrep`, and a roll it had left behind would
# not. After STOP_CHILDREN_SAY tries (20 s) it names what it is waiting for in
# the log, once.
STOP_CHILDREN_SAY=100
stop_children() {
	local pid tries=0 left
	while :; do
		left=""
		for pid in $(jobs -pr); do
			if kill -TERM "$pid" 2>/dev/null; then left="$left $pid"; fi
		done
		if [ -z "$left" ]; then return 0; fi
		tries=$((tries + 1))
		if [ "$tries" -eq "$STOP_CHILDREN_SAY" ]; then
			log "STOP: still waiting after $((STOP_CHILDREN_SAY / 5))s for:$left -- the driver exits when they have ended (ps -o pid,args -p <pid>; kill by hand a call that will never return)"
		fi
		nap_brief 0.2
	done
}

# Fail fast (#951): the first failure ends the run, loudly. Queued RSS samplers are
# stopped so nothing appends to the log after the ABORTED line; the EXIT trap restores
# a SHRINK in progress.
abort() {
	log "CHURN ABORTED: $* -- stop, fix, relaunch with a fresh T0 (context=$CTX)"
	stop_children
	exit 1
}
# Split on purpose (#835). `trap restore_shrink EXIT INT TERM` made `kill -TERM`
# do the OPPOSITE of stopping the driver: restore_shrink RETURNS rather than
# exiting, the signal had already interrupted the in-flight `sleep` inside
# waituntil, so control fell straight through to the NEXT roll. The driver kept
# running, one roll ahead of schedule -- observed live on 2026-09-19 as an
# unscheduled `ROLLED aether-test/deployment/svc-1`. In the log that reads as
# "the tool ignored my signal", which sends you debugging the wrong thing.
#
# EXIT stays as the idempotent safety net: restore_shrink clears SHRINK_PREV
# before it scales, so the EXIT trap firing again after an INT/TERM handler has
# already run is a no-op. 130/143 are the conventional 128+SIGINT / 128+SIGTERM.
#
# A stop is one pass (#1419): the cluster first (the SHRINK, the new-SA
# objects), then the driver's own children, then exit. A further TERM or INT
# during that pass is ignored rather than starting a second one in the middle
# of the first. On EXIT alone nothing is stopped: a driver that has run its
# schedule leaves a queued RSS sampler to take its sample, as it always has.
on_signal() {
	trap '' TERM INT
	restore_shrink
	cleanup_newsa
	stop_children
	exit "$1"
}
trap 'restore_shrink; cleanup_newsa' EXIT
trap 'on_signal 130' INT
trap 'on_signal 143' TERM

# Wait until T0 + $1 minutes. No child process, so a TERM is taken at once.
waituntil() { nap_until $((T0 + $1 * 60)); }

# Returns non-zero on failure; the caller aborts (a roll inside the TRIPLE runs in
# a subshell, where exiting would only end the subshell).
roll() {
	if k rollout restart "$2" -n "$1" >>"$LOG" 2>&1; then
		log "ROLLED $1/$2"
	else
		log "FAILED $1/$2"
		return 1
	fi
}

# Queue an age-matched proxy RSS sample 30 minutes from now, detached, so it can
# never delay the schedule.
schedule_rss_sample() {
	if [ "${SOAK_PROXY_RSS:-1}" = "0" ]; then return; fi
	if [ ! -r "$SAMPLER" ]; then
		log "RSS SAMPLE skipped: no sampler at $SAMPLER"
		return
	fi
	# The wait has no child, and the sampler is exec'd: this job is one process
	# from start to end, the one stop_children signals. (It was a subshell
	# around a `sleep` and then around the sampler: TERM to it killed the
	# subshell and left whichever of the two was running.)
	(
		nap "$PROXY_RSS_DELAY"
		exec bash "$SAMPLER" --context "$CTX" --at-age "$PROXY_RSS_AGE"
	) >>"$LOG" 2>&1 &
	log "RSS SAMPLE queued for T+${PROXY_RSS_AGE}s after this aether-proxy roll (#628)"
}

roll_proxy() {
	roll aether-system daemonset/aether-proxy || abort "roll failed: aether-system/daemonset/aether-proxy"
	schedule_rss_sample
}

shrink() {
	local prev
	prev=$(k -n "$SHRINK_NS" get "$SHRINK_TARGET" -o jsonpath='{.spec.replicas}' 2>>"$LOG")
	if ! [[ "$prev" =~ ^[0-9]+$ ]] || [ "$prev" -eq 0 ]; then
		log "SHRINK skipped: cannot read a non-zero .spec.replicas from $SHRINK_NS/$SHRINK_TARGET (got '${prev}')"
		abort "SHRINK could not read $SHRINK_NS/$SHRINK_TARGET"
	fi
	log "SHRINK begin $SHRINK_NS/$SHRINK_TARGET replicas=$prev -> 0 for ${SHRINK_SECONDS}s (demand-set shrink, #682)"
	# Before the call, not after it: a TERM that arrives during the call is taken
	# when the call has returned, and the cluster has the scale by then. The
	# stop (and the EXIT trap) restore whatever SHRINK_PREV names.
	SHRINK_PREV="$prev"
	if ! k -n "$SHRINK_NS" scale "$SHRINK_TARGET" --replicas=0 >>"$LOG" 2>&1; then
		# A call that failed may have been applied all the same (a timeout):
		# SHRINK_PREV stays set, and the abort's EXIT trap scales back to it.
		# Restoring a value that is already there changes nothing.
		log "SHRINK FAILED to scale $SHRINK_NS/$SHRINK_TARGET down; restoring it to $prev in case the call was applied"
		abort "SHRINK could not scale $SHRINK_NS/$SHRINK_TARGET down"
	fi
	nap "$SHRINK_SECONDS"
	restore_shrink || abort "SHRINK could not restore $SHRINK_NS/$SHRINK_TARGET"
}

# Delete a new-SA step's three objects together. Idempotent (NEWSA_LIVE is cleared
# first), so the EXIT trap firing after an INT/TERM handler is a no-op.
cleanup_newsa() {
	if [ -n "$NEWSA_LIVE" ]; then
		local name="$NEWSA_LIVE"
		NEWSA_LIVE=""
		k -n "$NEWSA_NS" delete "deployment/$name" "configmap/$name" "serviceaccount/$name" \
			--ignore-not-found --wait=false >>"$LOG" 2>&1 ||
			log "NEWSA cleanup FAILED for $NEWSA_NS/$name -- DELETE BY HAND (deployment, configmap, serviceaccount)"
	fi
}

# One new-ServiceAccount step (#1008/#1014). See the header. Returns non-zero only
# when the step produced no tally (a hole); a tally with any non-2xx logs FAILED and
# returns 0, because that is the gate reading, not a harness failure.
newsa() {
	local name node nodes=() nodes_list n t_apply ready_after final tally bad
	NEWSA_STEP=$((NEWSA_STEP + 1))
	name="sa-new-$(date +%s)"
	# Not `mapfile < <(newsa_nodes)` (#1419): `mapfile` returns as soon as a
	# trapped signal arrives and bash does not wait for a process substitution,
	# so a TERM during it ended the driver at once and left the kubectl call
	# running. A command substitution is waited for.
	nodes_list=$(newsa_nodes)
	if [ -n "$nodes_list" ]; then mapfile -t nodes <<<"$nodes_list"; fi
	n=${#nodes[@]}
	if [ "$n" -eq 0 ]; then
		log "FAILED newsa/$name - no Ready worker node"
		return 1
	fi
	# Round-robin across the steps of a run, offset by T0 so successive runs do not
	# always start on the same node.
	node="${nodes[$(((T0 / 60 + NEWSA_STEP - 1) % n))]}"
	log "NEWSA begin $NEWSA_NS/$name on $node for ${NEWSA_SECONDS}s at ${NEWSA_RPS} rps -> $NEWSA_TARGETS"
	NEWSA_LIVE="$name"
	t_apply=$(date +%s)
	if ! {
		k -n "$NEWSA_NS" create configmap "$name" --from-file=newsa-client.sh="$NEWSA_CLIENT" \
			--dry-run=client -o yaml
		echo "---"
		newsa_manifest "$name" "$node"
	} | k -n "$NEWSA_NS" apply -f - >>"$LOG" 2>&1; then
		log "FAILED newsa/$name $node - apply refused"
		cleanup_newsa
		return 1
	fi
	if ! k -n "$NEWSA_NS" rollout status "deployment/$name" --timeout=180s >>"$LOG" 2>&1; then
		log "FAILED newsa/$name $node - pod not Ready within 180s"
		k -n "$NEWSA_NS" get pods -l "aether.io/soak-newsa=$name" -o wide >>"$LOG" 2>&1
		cleanup_newsa
		return 1
	fi
	ready_after=$(($(date +%s) - t_apply))
	# The pod prints its final line after NEWSA_SECONDS; allow two minutes of slack.
	final=""
	while [ $(($(date +%s) - t_apply)) -lt $((NEWSA_SECONDS + ready_after + 120)) ]; do
		nap 10
		final=$(k -n "$NEWSA_NS" logs "deployment/$name" 2>/dev/null | grep '^AETHER_NEWSA_FINAL ' | tail -1)
		if [ -n "$final" ]; then break; fi
	done
	# Keep the pod's own 10-second tallies and its JSON line with the step.
	k -n "$NEWSA_NS" logs "deployment/$name" 2>/dev/null | grep -E '^AETHER_(NEWSA|NEWSA_FINAL|METRIC) ' >>"$LOG"
	cleanup_newsa
	if [ -z "$final" ]; then
		log "FAILED newsa/$name $node - no AETHER_NEWSA_FINAL line within $((NEWSA_SECONDS + 120))s of Ready"
		return 1
	fi
	# "svc-1:ok=1200,non2xx=0,connerr=0,codes=- svc-3:..."
	tally="${final#*node=* }"
	# A step is clean only if every destination answered and nothing was not a 2xx.
	bad=$(tr ' ' '\n' <<<"$tally" | awk -F'[:=,]' '
		/:ok=/ { if ($3 == 0 || $5 != 0 || $7 != 0) b++ }
		END { print b + 0 }')
	if [ "$bad" -eq 0 ]; then
		log "ROLLED newsa/$name $node ready=${ready_after}s $tally"
	else
		log "FAILED newsa/$name $node ready=${ready_after}s $tally"
	fi
}

# ServiceAccount + Deployment for one new-SA step; the ConfigMap is prepended by newsa.
newsa_manifest() {
	local name="$1" node="$2"
	cat <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: $name
  labels:
    app.kubernetes.io/name: soak-newsa
    aether.io/soak-newsa: $name
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $name
  labels:
    app.kubernetes.io/name: soak-newsa
    aether.io/soak-newsa: $name
spec:
  replicas: 1
  selector:
    matchLabels:
      aether.io/soak-newsa: $name
  template:
    metadata:
      labels:
        # REQUIRED, same as the k6 runner: without it no mesh DNS, no capture.
        aether.io/managed: "true"
        app.kubernetes.io/name: soak-newsa
        aether.io/soak-newsa: $name
      annotations:
        config.aether.io/upstreams: "$NEWSA_UPSTREAMS"
    spec:
      serviceAccountName: $name
      nodeSelector:
        kubernetes.io/hostname: $node
      terminationGracePeriodSeconds: 5
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: client
          image: $NEWSA_IMAGE
          imagePullPolicy: IfNotPresent
          command: ["sh", "/newsa/newsa-client.sh"]
          env:
            - { name: NEWSA_STEP, value: "$name" }
            - { name: NEWSA_SECONDS, value: "$NEWSA_SECONDS" }
            - { name: NEWSA_RPS, value: "$NEWSA_RPS" }
            - { name: NEWSA_TARGETS, value: "$NEWSA_TARGETS" }
            - name: NEWSA_NODE
              valueFrom: { fieldRef: { fieldPath: spec.nodeName } }
          volumeMounts:
            - { name: script, mountPath: /newsa, readOnly: true }
            - { name: tmp, mountPath: /tmp }
          resources:
            requests: { cpu: 20m, memory: 16Mi }
            limits: { memory: 64Mi }
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
      volumes:
        - name: script
          configMap: { name: $name }
        - name: tmp
          emptyDir: {}
EOF
}

# FailedMount events in namespace $1 last seen at or after $2 (and, with $3, at or
# before $3), all ISO-8601 UTC. lastTimestamp for core events, eventTime for the
# events.k8s.io shape; ISO strings compare lexically. Prints "?" when the events
# cannot be listed, so an API error never reads as a clean 0.
failedmount_count() {
	local ev
	if ! ev=$(k --request-timeout=15s -n "$1" get events --field-selector reason=FailedMount \
		-o jsonpath='{range .items[*]}{.lastTimestamp}{" "}{.eventTime}{"\n"}{end}' 2>/dev/null); then
		echo "?"
		return
	fi
	printf '%s\n' "$ev" |
		awk -v from="$2" -v to="${3:-9999}" '
			{ t = ($1 != "" && $1 != "<nil>") ? $1 : $2 }
			t != "" && t >= from && t <= to { n++ }
			END { print n + 0 }'
}

# "<ok>/<total>": nodes running a uds-csi plugin pod whose CSINode lists the driver.
udscsi_csinodes() {
	local nodes node ok=0 total=0
	nodes=$(k -n "$UDSCSI_NS" get pods -l "$UDSCSI_SELECTOR" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | sort -u)
	for node in $nodes; do
		total=$((total + 1))
		case " $(k get csinode "$node" -o jsonpath='{.spec.drivers[*].name}' 2>/dev/null) " in
		*" $UDSCSI_DRIVER "*) ok=$((ok + 1)) ;;
		esac
	done
	echo "$ok/$total"
}

# The node's plugin state from the watch file: unseen|up|down|back (see the awk).
udscsi_window() { awk -v orig="$1" -f "$UDSCSI_WINDOW_AWK" "$2"; }

# Stop a step's plugin watch and drop its file.
udscsi_unwatch() {
	if [ -n "$1" ]; then kill "$1" 2>/dev/null; fi
	rm -f "$2"
}

# One uds-csi roll step (#1109). See the header. Returns non-zero only for a hole
# (restart refused, or nothing to delete); every other outcome is a tally line.
udscsi() {
	local spec vns vdep victims n line victim vnode before plugin t_begin since t_del t_done
	local window="missed" window_why="" rollout_s=- term_s=- ready_s=- replacement=- csinode fm_during fm_after bad="" deadline
	local wfile wpid how state
	UDSCSI_STEP=$((UDSCSI_STEP + 1))
	read -r -a victims <<<"$UDSCSI_VICTIMS"
	n=${#victims[@]}
	spec="${victims[$(((UDSCSI_STEP - 1) % n))]}"
	vns="${spec%%/*}" vdep="${spec#*/}"
	# A Running, not-terminating pod of the victim Deployment, and its node.
	# SIGPIPE rule (#1121, e2e/README.md): this script runs under pipefail, so
	# no pipeline may end in a reader that exits before its writer is done
	# (`head`, `grep -q`/`-m`, `awk '...; exit'`): the writer dies of SIGPIPE
	# and the pipeline fails with 141. awk keeps the first match, no `exit`.
	line=$(k -n "$vns" get pods -l "app=$vdep" --field-selector=status.phase=Running \
		-o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{" "}{.metadata.deletionTimestamp}{"\n"}{end}' 2>>"$LOG" |
		awk '!f && NF == 2 {print; f = 1}')
	read -r victim vnode <<<"$line"
	if [ -z "${victim:-}" ]; then
		log "FAILED $UDSCSI_NS/$UDSCSI_DS - no Running pod labelled app=$vdep in $vns to delete mid-roll"
		return 1
	fi
	before=$(k -n "$vns" get pods -l "app=$vdep" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)
	plugin=$(k -n "$UDSCSI_NS" get pods -l "$UDSCSI_SELECTOR" --field-selector "spec.nodeName=$vnode" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
	# Watch the node's plugin pods from BEFORE the restart (#1243). The old
	# 1-second poll of the old plugin pod alone logged window=missed whenever
	# that pod was already gone at the next poll -- although the node's plugin
	# stays down until the REPLACEMENT is Ready (maxSurge 0), which is the
	# window that matters. The watch delivers the terminating update itself,
	# so the step acts on the event, not on a timer.
	wfile=$(mktemp)
	wpid=""
	if [ -n "$plugin" ]; then
		# exec, not `k ... &`: a function in the background is a subshell with
		# kubectl as its child, and killing $! then left the watch running until
		# the API server closed it. This way $! is kubectl.
		(exec kubectl --context "$CTX" -n "$UDSCSI_NS" get pods -l "$UDSCSI_SELECTOR" --field-selector "spec.nodeName=$vnode" \
			-w -o jsonpath="$UDSCSI_POD_FMT") >"$wfile" 2>>"$LOG" &
		wpid=$!
		# The watch lists the existing pods first; wait (bounded) until it has.
		deadline=$(($(date +%s) + 15))
		while [ "$(udscsi_window "$plugin" "$wfile")" = "unseen" ] && [ "$(date +%s)" -lt "$deadline" ]; do
			nap_brief 0.2
		done
		if [ "$(udscsi_window "$plugin" "$wfile")" = "unseen" ]; then
			log "UDSCSI watch on $vnode listed nothing in 15s; polling instead"
			kill "$wpid" 2>/dev/null
		fi
	fi
	t_begin=$(date +%s)
	since=$(date -u +%FT%TZ)
	log "UDSCSI begin $UDSCSI_NS/$UDSCSI_DS; will delete $vns/$victim on $vnode once its plugin ${plugin:-<none>} is down"
	if ! k -n "$UDSCSI_NS" rollout restart "$UDSCSI_DS" >>"$LOG" 2>&1; then
		log "FAILED $UDSCSI_NS/$UDSCSI_DS - rollout restart refused"
		udscsi_unwatch "$wpid" "$wfile"
		return 1
	fi
	# Delete the victim while ITS node's plugin is down, so the kubelet's
	# NodeUnpublishVolume has to wait for the new plugin to register. Down =
	# the old plugin pod terminating or gone AND no Ready replacement on the
	# node. Bounded: the roll reaches the node within the rollout, or never.
	deadline=$((t_begin + UDSCSI_WINDOW_TIMEOUT))
	how="watch"
	while [ -n "$plugin" ]; do
		if [ -n "$wpid" ] && ! kill -0 "$wpid" 2>/dev/null; then
			# The watch ended (API error, server-side timeout): poll the same
			# lines into the same file; the latest line per pod wins.
			if [ "$how" = "watch" ]; then log "UDSCSI plugin watch on $vnode ended; polling every 0.2s instead"; fi
			how="poll"
			k --request-timeout=10s -n "$UDSCSI_NS" get pods -l "$UDSCSI_SELECTOR" \
				--field-selector "spec.nodeName=$vnode" -o jsonpath="{range .items[*]}$UDSCSI_POD_FMT{end}" \
				>>"$wfile" 2>/dev/null
		fi
		state=$(udscsi_window "$plugin" "$wfile")
		case "$state" in
		down)
			window="down"
			break
			;;
		back)
			window_why="replacement already Ready when first seen"
			break
			;;
		esac
		if [ "$(date +%s)" -ge "$deadline" ]; then
			window_why="plugin $plugin not down within ${UDSCSI_WINDOW_TIMEOUT}s (last state: $state)"
			break
		fi
		nap_brief 0.2
	done
	t_del=$(date +%s)
	if ! k -n "$vns" delete pod "$victim" --wait=false >>"$LOG" 2>&1; then
		log "FAILED $UDSCSI_NS/$UDSCSI_DS - could not delete $vns/$victim"
		udscsi_unwatch "$wpid" "$wfile"
		return 1
	fi
	# Confirm AFTER the delete was accepted: still down = the delete provably
	# landed inside the window. Back up already = window=late, the leg may not
	# have been exercised (conservative: it may also have been a near miss).
	if [ "$window" = "down" ]; then
		k --request-timeout=10s -n "$UDSCSI_NS" get pods -l "$UDSCSI_SELECTOR" \
			--field-selector "spec.nodeName=$vnode" -o jsonpath="{range .items[*]}$UDSCSI_POD_FMT{end}" \
			>>"$wfile" 2>/dev/null
		if [ "$(udscsi_window "$plugin" "$wfile")" != "down" ]; then
			window="late"
			window_why="plugin on $vnode Ready again by the time the delete was accepted"
		fi
	fi
	if [ "$window" = "down" ]; then
		log "UDSCSI window=down on $vnode: $vns/$victim deleted $((t_del - t_begin))s after the restart while the plugin was down ($how)"
	else
		# Keep what the driver saw, so a miss can be read from the log.
		log "UDSCSI window=$window on $vnode: ${window_why:-no plugin pod on $vnode} -- the mid-roll NodeUnpublishVolume leg was NOT exercised in this step ($how)"
		sed 's/^/  plugin-watch /' "$wfile" >>"$LOG"
	fi
	udscsi_unwatch "$wpid" "$wfile"
	if k -n "$UDSCSI_NS" rollout status "$UDSCSI_DS" --timeout="${UDSCSI_TIMEOUT}s" >>"$LOG" 2>&1; then
		rollout_s=$(($(date +%s) - t_begin))
	else
		bad="$bad,rollout"
	fi
	if k -n "$vns" wait --for=delete "pod/$victim" --timeout=300s >>"$LOG" 2>&1; then
		term_s=$(($(date +%s) - t_del))
	else
		bad="$bad,terminating"
	fi
	# The replacement: a pod of the Deployment that did not exist before the delete.
	deadline=$(($(date +%s) + 300))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		replacement=$(k -n "$vns" get pods -l "app=$vdep" \
			-o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' 2>/dev/null |
			awk -v before=" $before " '!f && $2 == "True" && index(before, " " $1 " ") == 0 {print $1; f = 1}')
		if [ -n "$replacement" ]; then
			ready_s=$(($(date +%s) - t_del))
			break
		fi
		nap 3
	done
	if [ -z "$replacement" ]; then
		replacement=-
		bad="$bad,replacement"
	fi
	# Registration is asynchronous to pod readiness: give the kubelet a minute.
	deadline=$(($(date +%s) + 60))
	while :; do
		csinode=$(udscsi_csinodes)
		if [ "${csinode%/*}" = "${csinode#*/}" ] && [ "${csinode#*/}" != 0 ]; then break; fi
		if [ "$(date +%s)" -ge "$deadline" ]; then
			bad="$bad,csinode"
			break
		fi
		nap 5
	done
	t_done=$(date -u +%FT%TZ)
	fm_during=$(failedmount_count "$vns" "$since" "$t_done")
	# Settle, then any FailedMount stamped after the step finished outlived the roll.
	nap 30
	fm_after=$(failedmount_count "$vns" "$t_done")
	if [ "$fm_after" != 0 ]; then bad="$bad,failedmount"; fi
	line="$UDSCSI_NS/$UDSCSI_DS victim=$vns/$victim@$vnode window=$window rollout=${rollout_s}s terminated=${term_s}s replacement=$replacement ready=${ready_s}s csinode=$csinode failedmount_during=$fm_during failedmount_after=$fm_after"
	if [ -z "$bad" ]; then
		log "ROLLED $line"
	else
		log "FAILED $line bad=${bad#,}"
	fi
}

if [ "$UDSCSI_ONCE" = "1" ]; then
	log "churn driver uds-csi-once start T0=$(date -u +%FT%TZ) build=$BUILD_LABEL context=$CTX"
	udscsi || abort "uds-csi step could not run"
	log "churn driver uds-csi-once complete"
	exit 0
fi

if [ "$NEWSA_ONCE" = "1" ]; then
	log "churn driver new-sa-once start T0=$(date -u +%FT%TZ) build=$BUILD_LABEL context=$CTX"
	newsa || abort "new-SA step produced no tally"
	log "churn driver new-sa-once complete"
	exit 0
fi

log "churn driver start T0=$(date -u +%FT%TZ) build=$BUILD_LABEL"
log "churn driver context=$CTX (pre-flight passed)"

# "<offset-minutes> <kind> [name]"
SCHED=(
	"12 svc svc-1" "24 svc svc-2" "36 meshdns" "48 svc svc-3" "60 proxy"
	"72 agent" "84 svc svc-5" "96 proxy" "108 svc svc-1" "120 edge"
	"132 svc svc-2" "144 meshdns" "156 svc svc-3" "168 svc svc-4" "180 svc svc-5"
	"192 svc svc-1" "204 edge" "216 proxy" "228 svc svc-2" "240 svc svc-4"
	"252 proxy" "264 svc svc-3" "276 svc svc-1" "300 TRIPLE" "312 meshdns"
	"324 svc svc-4" "336 proxy" "348 svc svc-2" "360 svc svc-1"
)

# The new-SA and uds-csi steps join the schedule by offset (a stable numeric sort
# keeps a roll that shares a minute with a step ahead of it).
NEWSA_COUNT=0
if [ "$NEWSA_ON" = "0" ]; then
	log "new-SA step SKIPPED (SOAK_NEWSA=0)"
else
	for off in $NEWSA_OFFSETS; do
		SCHED+=("$off newsa")
		NEWSA_COUNT=$((NEWSA_COUNT + 1))
	done
	log "new-SA step scheduled at T0+{${NEWSA_OFFSETS// /,}}m ($NEWSA_COUNT steps, ${NEWSA_SECONDS}s each, #1014)"
fi
UDSCSI_COUNT=0
if [ "$UDSCSI_ON" = "0" ]; then
	log "uds-csi step SKIPPED (SOAK_UDSCSI=0)"
else
	for off in $UDSCSI_OFFSETS; do
		SCHED+=("$off udscsi")
		UDSCSI_COUNT=$((UDSCSI_COUNT + 1))
	done
	log "uds-csi step scheduled at T0+{${UDSCSI_OFFSETS// /,}}m ($UDSCSI_COUNT steps; victims round-robin over: $UDSCSI_VICTIMS; #1109)"
fi
mapfile -t SCHED < <(printf '%s\n' "${SCHED[@]}" | sort -s -n -k1,1)

for entry in "${SCHED[@]}"; do
	read -r off kind name <<<"$entry"
	waituntil "$off"
	case "$kind" in
	newsa) newsa || abort "new-SA step produced no tally" ;;
	udscsi) udscsi || abort "uds-csi step could not run" ;;
	svc) roll aether-test "deployment/$name" || abort "roll failed: aether-test/deployment/$name" ;;
	proxy) roll_proxy ;;
	agent) roll aether-system daemonset/aether-agent || abort "roll failed: aether-system/daemonset/aether-agent" ;;
	meshdns) roll aether-system daemonset/aether-mesh-dns || abort "roll failed: aether-system/daemonset/aether-mesh-dns" ;;
	edge) roll aether-ingress deployment/aether-edge || abort "roll failed: aether-ingress/deployment/aether-edge" ;;
	TRIPLE)
		log "CONCURRENT triple begin"
		# Wait on these three PIDs specifically: a bare `wait` would also block on
		# any RSS sampler still parked in its --at-age poll, delaying the log.
		#
		# Each roll ignores TERM (#1419): a driver that is stopped during the
		# TRIPLE waits for the rolls already under way (stop_children) instead of
		# killing the subshell and leaving its kubectl, and its `ROLLED` line,
		# behind. kubectl inherits the ignore, so the call is not cut either.
		(
			trap '' TERM
			roll aether-system daemonset/aether-agent
		) &
		t_agent=$!
		(
			trap '' TERM
			roll aether-system daemonset/aether-proxy
		) &
		t_proxy=$!
		(
			trap '' TERM
			roll aether-test deployment/svc-3
		) &
		t_svc=$!
		# Wait on each PID separately: `wait a b c` returns only the LAST status, and
		# every one of the three must have rolled.
		t_fail=""
		wait "$t_agent" || t_fail="$t_fail agent"
		wait "$t_proxy" || t_fail="$t_fail proxy"
		wait "$t_svc" || t_fail="$t_fail svc-3"
		if [ -n "$t_fail" ]; then abort "CONCURRENT triple roll failed:$t_fail"; fi
		log "CONCURRENT triple done"
		# The TRIPLE-fresh proxy incarnation is #628's worst observed data point
		# (316Mi in 9 minutes on 08-03), so age-match it too.
		schedule_rss_sample
		;;
	*) abort "UNKNOWN kind $kind" ;;
	esac
done

# The no-roll window: the 1h demand-scoped TTL expires under load, with no agent or
# proxy roll to reset it. See the header. Nothing is rolled until NO_ROLL_END_MIN.
if [ "${SOAK_NO_ROLL_WINDOW:-1}" = "0" ]; then
	log "no-roll window SKIPPED (SOAK_NO_ROLL_WINDOW=0)"
else
	log "no-roll window begin T0+360m; nothing is rolled until T0+${NO_ROLL_END_MIN}m (#682 TTL expiry under load)"
	waituntil "$NO_ROLL_END_MIN"
	log "no-roll window end T0+${NO_ROLL_END_MIN}m"
fi

if [ "${SOAK_SHRINK:-1}" = "0" ]; then
	log "SHRINK skipped (SOAK_SHRINK=0)"
else
	shrink
fi

# 29 schedule entries plus the uds-csi steps, but the TRIPLE logs its three
# concurrent rolls separately, so the rolls are 31 + the uds-csi steps = 33 by
# default. (A footer once said 30: it forgot the TRIPLE's proxy.) Each new-SA step
# adds one `ROLLED newsa/` (or `FAILED newsa/`) line: `grep -c ROLLED` is 35 by
# default.
log "churn driver complete ($((31 + UDSCSI_COUNT)) rolls: 6 proxy incl triple, 2 agent incl triple, 2 edge, 3 meshdns, $UDSCSI_COUNT uds-csi, 18 svc incl triple; + $NEWSA_COUNT new-SA steps)"
