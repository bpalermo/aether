#!/usr/bin/env bash
# 8-hour soak churn driver: 31 rolling restarts (29 schedule entries; the TRIPLE
# fires three at once), including three aether-mesh-dns DaemonSet rolls (surge +
# SO_REUSEPORT handoff) and one CONCURRENT triple (agent + proxy + svc at the same
# instant) as the stress peak -- followed by a deliberate 90-minute NO-ROLL WINDOW
# and a demand-set SHRINK.
#
# Pre-flight in the foreground first (loud, exits non-zero on any problem), then
# run detached -- it sleeps between rolls for ~7h32m:
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
#   180   svc-5        192 svc-1
#   204   edge         216 proxy  *
#   228   svc-2        240 svc-4
#   252   proxy  *     264 svc-3
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
# mesh-dns took the T0+36 slot; the set of rolls, and the 31 tally, are unchanged.
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
set -uo pipefail

CTX="${SOAK_CONTEXT:-talos-main}"
PREFLIGHT_ONLY=0
NEWSA_ONCE=0
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
	-*)
		echo "churn.sh: unknown flag '$1' (usage: churn.sh [--context NAME] [--preflight] [--new-sa-once] [BUILD_LABEL])" >&2
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
NEWSA_IMAGE="${SOAK_NEWSA_IMAGE:-curlimages/curl:8.22.0}"
NEWSA_TARGETS="${SOAK_NEWSA_TARGETS:-svc-1=http://svc-1.aether-test.aether.internal:18081/ svc-3=http://svc-3.aether-test.aether.internal:18081/}"
NEWSA_UPSTREAMS="${SOAK_NEWSA_UPSTREAMS:-svc-1,svc-3}"
NEWSA_CLIENT="$HERE/newsa-client.sh"
NEWSA_ON="${SOAK_NEWSA:-1}"
NEWSA_STEP=0
NEWSA_LIVE=""

# Pre-flight (#951). Writes to stderr only -- never to $LOG -- and runs before T0.
# "<namespace> <kind>/<name>" for every workload the schedule or the SHRINK touches.
PREFLIGHT_TARGETS=(
	"aether-system daemonset/aether-agent"
	"aether-system daemonset/aether-proxy"
	"aether-system daemonset/aether-mesh-dns"
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

preflight() {
	local out fail=0 ns obj t
	if ! out=$(k --request-timeout=15s get --raw /readyz 2>&1); then
		echo "churn.sh: PRE-FLIGHT FAILED: context '$CTX' cannot reach a ready API server:" >&2
		printf '  %s\n' "$out" >&2
		echo "churn.sh: refusing to start; $LOG untouched, no T0 written. Check \`kubectl config get-contexts\` and pass --context." >&2
		return 1
	fi
	if [ "$NEWSA_ONCE" = "1" ]; then PREFLIGHT_TARGETS=(); fi
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
	if [ "$NEWSA_ON" != "0" ] || [ "$NEWSA_ONCE" = "1" ]; then
		preflight_newsa || fail=1
	fi
	if [ "$fail" -ne 0 ]; then
		echo "churn.sh: refusing to start; $LOG untouched, no T0 written." >&2
		return 1
	fi
	local newsa_state="new-SA step ON at T0+{${NEWSA_OFFSETS// /,}}m"
	if [ "$NEWSA_ONCE" = "1" ]; then newsa_state="new-SA step once, now"; elif [ "$NEWSA_ON" = "0" ]; then newsa_state="new-SA step OFF"; fi
	echo "churn.sh: pre-flight OK on context '$CTX' (API ready; ${#PREFLIGHT_TARGETS[@]} targets present and patchable; $newsa_state)" >&2
}

if ! preflight; then exit 2; fi
if [ "$PREFLIGHT_ONLY" = "1" ]; then exit 0; fi

T0=$(date +%s)

# Start a FRESH log, archiving any previous run alongside it. Without this the driver
# appends to the last soak's file, and `grep -c ROLLED` -- which teardown uses to confirm
# 31 rolls -- silently double-counts, so a run looks complete when it is not.
if [ -s "$LOG" ]; then
	mv -f "$LOG" "$LOG.$(date -u +%Y%m%dT%H%M%SZ).prev"
fi
: >"$LOG"

log() { echo "$(date -u +%FT%TZ) $*" >>"$LOG"; }

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

# Fail fast (#951): the first failure ends the run, loudly. Queued RSS samplers are
# killed so nothing appends to the log after the ABORTED line; the EXIT trap restores
# a SHRINK in progress.
abort() {
	log "CHURN ABORTED: $* -- stop, fix, relaunch with a fresh T0 (context=$CTX)"
	local j
	for j in $(jobs -p); do kill "$j" 2>/dev/null; done
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
trap 'restore_shrink; cleanup_newsa' EXIT
trap 'restore_shrink; cleanup_newsa; exit 130' INT
trap 'restore_shrink; cleanup_newsa; exit 143' TERM

# Sleep until T0 + $1 minutes.
waituntil() {
	local target now delta
	target=$((T0 + $1 * 60))
	now=$(date +%s)
	delta=$((target - now))
	if [ "$delta" -gt 0 ]; then sleep "$delta"; fi
}

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
	(
		sleep "$PROXY_RSS_DELAY"
		bash "$SAMPLER" --context "$CTX" --at-age "$PROXY_RSS_AGE"
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
	if ! k -n "$SHRINK_NS" scale "$SHRINK_TARGET" --replicas=0 >>"$LOG" 2>&1; then
		log "SHRINK FAILED to scale $SHRINK_NS/$SHRINK_TARGET down; leaving it at $prev"
		abort "SHRINK could not scale $SHRINK_NS/$SHRINK_TARGET down"
	fi
	SHRINK_PREV="$prev"
	sleep "$SHRINK_SECONDS"
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
	local name node nodes n t_apply ready_after final tally bad
	NEWSA_STEP=$((NEWSA_STEP + 1))
	name="sa-new-$(date +%s)"
	mapfile -t nodes < <(newsa_nodes)
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
		sleep 10
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

# The new-SA steps join the schedule by offset (a stable numeric sort keeps a roll
# that shares a minute with a step ahead of it).
NEWSA_COUNT=0
if [ "$NEWSA_ON" = "0" ]; then
	log "new-SA step SKIPPED (SOAK_NEWSA=0)"
else
	for off in $NEWSA_OFFSETS; do
		SCHED+=("$off newsa")
		NEWSA_COUNT=$((NEWSA_COUNT + 1))
	done
	mapfile -t SCHED < <(printf '%s\n' "${SCHED[@]}" | sort -s -n -k1,1)
	log "new-SA step scheduled at T0+{${NEWSA_OFFSETS// /,}}m ($NEWSA_COUNT steps, ${NEWSA_SECONDS}s each, #1014)"
fi

for entry in "${SCHED[@]}"; do
	read -r off kind name <<<"$entry"
	waituntil "$off"
	case "$kind" in
	newsa) newsa || abort "new-SA step produced no tally" ;;
	svc) roll aether-test "deployment/$name" || abort "roll failed: aether-test/deployment/$name" ;;
	proxy) roll_proxy ;;
	agent) roll aether-system daemonset/aether-agent || abort "roll failed: aether-system/daemonset/aether-agent" ;;
	meshdns) roll aether-system daemonset/aether-mesh-dns || abort "roll failed: aether-system/daemonset/aether-mesh-dns" ;;
	edge) roll aether-ingress deployment/aether-edge || abort "roll failed: aether-ingress/deployment/aether-edge" ;;
	TRIPLE)
		log "CONCURRENT triple begin"
		# Wait on these three PIDs specifically: a bare `wait` would also block on
		# any RSS sampler still parked in its --at-age poll, delaying the log.
		roll aether-system daemonset/aether-agent &
		t_agent=$!
		roll aether-system daemonset/aether-proxy &
		t_proxy=$!
		roll aether-test deployment/svc-3 &
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

# 29 schedule entries, but the TRIPLE logs its three concurrent rolls separately, so
# the rolls alone are 31. (The old footer said 30: it forgot the TRIPLE's proxy. The
# set of rolls is unchanged -- only the tally is now honest.) Each new-SA step adds
# one `ROLLED newsa/` (or `FAILED newsa/`) line: `grep -c ROLLED` is 33 by default.
log "churn driver complete (31 rolls: 6 proxy incl triple, 2 agent incl triple, 2 edge, 3 meshdns, 18 svc incl triple; + $NEWSA_COUNT new-SA steps)"
