#!/usr/bin/env bash
# Single-cluster kind e2e for what an OPA policy that does not compile does to
# a node, when it is applied and at the restarts that follow (#1447, #1448).
#
# The chart's OPA preset runs `opa run --server --watch` as a native sidecar of
# the aether-proxy pod (#1275, #1383). Validating a policy is the job of whoever
# changes it, before the change; nothing in the chart refuses a bad one. A
# running sidecar does not load a policy that fails to compile (it keeps the
# last good one), but no OPA process can START with that file, and the last
# good policy lives only in the running process. e2e/authz.sh step d covers the
# running sidecar. This harness covers the rest, with the destructive steps:
#
#   a. baseline       — e2e/authz.sh's decisions (allow header -> 200, none ->
#                       403, both decided by the sidecar), the proxy pod is
#                       Ready, and the policy check (the one-pod Deployment
#                       aether-opa-policy-check, #1447) is available
#   b. deny-all       — a policy that COMPILES and denies everything is loaded:
#                       the allowed request turns 403 and it is a decision
#                       (ext_authz.denied moves, ext_authz.error does not). The
#                       policy check stays available: it is about compiling,
#                       not about what a policy decides
#   c. bad, running   — a policy that does not parse is applied. On the node
#                       the last good policy keeps deciding and nothing
#                       restarts (e2e/authz.sh step d). AND it is visible
#                       (#1447): the policy check's rollout does not complete,
#                       the Deployment has an unavailable replica, and an
#                       Unhealthy event of its pod carries the compile error.
#                       The proxy pod stays Ready throughout: a NotReady proxy
#                       pod is what a roll deletes (see e). With ABP_EXPECT=red
#                       (a chart without the check) the gate is inverted: no
#                       object in the namespace says anything
#   d. sidecar restart — the `authz` container is stopped with the bad file on
#                       the node. The restarted OPA exits 1 and crash-loops;
#                       `proxy` keeps running (no restart, same Envoy epoch) with
#                       nothing behind the authz socket, so every check is an
#                       ext_authz error: 403 under failureMode DENY. A valid
#                       policy recovers it in the same pod
#   e. a roll          — with the bad file on the node again, `rollout restart
#                       ds/aether-proxy`: the surged pod's sidecar cannot start,
#                       its `proxy` container is never started, and the roll
#                       stalls there with the OLD pod, still Ready, still
#                       deciding with its last good policy (200 / 403, zero
#                       ext_authz errors). This is the step a readiness probe
#                       on the sidecar breaks: the DaemonSet controller deletes
#                       a NotReady old pod as soon as its replacement exists
#   f. pod replaced    — the old pod is then deleted (what a node drain or a
#                       reboot does). Its Envoy serves on for its successor wait
#                       and exits; the new pod still cannot start its proxy, so
#                       the node has NO proxy and mesh requests from it fail.
#                       This is the behaviour today and it is pinned here on
#                       purpose: whether the proxy should start without its
#                       authz sidecar (and decide per failureMode) is an open
#                       owner decision (#1447); changing it changes this step
#   g. recovery        — a valid policy: the stuck pod starts at the kubelet's
#                       next restart attempt, the roll completes, the policy
#                       check is available again and (a) holds
#
# Red/green: AUTHZ_CHARTS=<dir holding an older charts/ tree> installs that
# chart instead, and ABP_EXPECT=red inverts the gate of (c) and skips the
# policy check's other gates.
#
# Usage: e2e/authz-bad-policy.sh {up|test|verify|down}   (bare = up + verify)
# Env: ABP_CLUSTER (authz-bad), ABP_EXPECT (green|red), ABP_CHECK_TIMEOUT
#      (90: seconds the policy check may take to report a policy, either
#      way, once it is applied), ABP_RESTART_TIMEOUT (420: seconds a
#      crash-looping sidecar may take to start once the file is valid again;
#      the kubelet's back-off doubles up to 5 minutes), ABP_SUCCESSOR_TIMEOUT
#      (300: seconds the deleted pod may take to go; its Envoy waits 155 s for
#      a successor at the chart's defaults), and every AUTHZ_* knob of
#      e2e/authz.sh (AUTHZ_SKIP_BUILD, AUTHZ_CHARTS, AUTHZ_RELOAD_TIMEOUT)
#
# Prereqs: as e2e/authz.sh.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Its own cluster: the steps are destructive, and the nightly `authz` job runs
# at the same time on another runner with the default name.
export AUTHZ_CLUSTER="${ABP_CLUSTER:-authz-bad}"
# The bring-up, the policies, the client and the Envoy readings.
# shellcheck source=e2e/authz.sh
. "$HERE/authz.sh"

ABP_EXPECT="${ABP_EXPECT:-green}"
CHECK_TIMEOUT="${ABP_CHECK_TIMEOUT:-90}"
RESTART_TIMEOUT="${ABP_RESTART_TIMEOUT:-420}"
SUCCESSOR_TIMEOUT="${ABP_SUCCESSOR_TIMEOUT:-300}"
CHECK_DEPLOY="aether-opa-policy-check"

case "$ABP_EXPECT" in
green | red) ;;
*) die "ABP_EXPECT must be green or red" ;;
esac

# A policy that compiles and allows nothing (b).
POLICY_DENY_ALL='package envoy.authz

default allow := false'

# --- readings ------------------------------------------------------------------

# proxy_pods — the proxy pods that are not being deleted, one name per line.
proxy_pods() {
	kc -n "$NS" get pods -l app.kubernetes.io/component=proxy --no-headers \
		-o custom-columns=N:.metadata.name,D:.metadata.deletionTimestamp 2>/dev/null |
		awk '$2 == "<none>" { print $1 }'
}

# authz_field POD JSONPATH-SUFFIX — a field of POD's `authz` sidecar status.
authz_field() {
	kc -n "$NS" get pod "$1" -o jsonpath="{.status.initContainerStatuses[?(@.name==\"authz\")]$2}" 2>/dev/null
}

# pod_ready POD — the pod's Ready condition (True/False).
pod_ready() {
	kc -n "$NS" get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null
}

# proxy_started_at POD — when POD's `proxy` container started; empty if never.
proxy_started_at() {
	kc -n "$NS" get pod "$1" -o jsonpath='{.status.containerStatuses[?(@.name=="proxy")].state.running.startedAt}' 2>/dev/null
}

# wait_for SECONDS CMD... — poll CMD every 2 s until it succeeds.
wait_for() {
	local deadline=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		sleep 2
	done
}

proxy_pod_is_ready() { [ "$(authz_field "$1" .ready)" = "true" ] && [ "$(pod_ready "$1")" = "True" ]; }
authz_crash_looping() {
	local n
	n="$(authz_field "$1" .restartCount)"
	[ "${n:-0}" -ge "$2" ] && [ "$(authz_field "$1" .lastState.terminated.exitCode)" = "1" ]
}
pod_gone() { ! kc -n "$NS" get pod "$1" >/dev/null 2>&1; }

# --- the policy check (#1447) --------------------------------------------------

# check_unavailable — the Deployment's unavailable replicas (0 when none).
check_unavailable() {
	local n
	n="$(kc -n "$NS" get deploy "$CHECK_DEPLOY" -o jsonpath='{.status.unavailableReplicas}' 2>/dev/null)"
	printf '%s' "${n:-0}"
}

# check_events — "<pod> <message>" for every Unhealthy event of a check pod,
# past pods included (an event outlives its pod).
check_events() {
	# shellcheck disable=SC2016 # a jsonpath template, not shell
	kc -n "$NS" get events --field-selector reason=Unhealthy \
		-o jsonpath='{range .items[*]}{.involvedObject.name}{" "}{.lastTimestamp}{" "}{.message}{"\n"}{end}' 2>/dev/null |
		{ grep "^$CHECK_DEPLOY-" || true; }
}

# check_reports_error SINCE — an Unhealthy event of a check pod, last seen at
# or after SINCE (RFC 3339 UTC, compared as a string), carries the parse error.
check_reports_error() {
	[ "$(check_events | awk -v since="$1" '$2 >= since' | grep -c 'rego_parse_error')" -gt 0 ]
}

# check_available — the check has rolled out for the current policy: one
# updated replica, available, nothing unavailable.
check_available() {
	kc -n "$NS" rollout status "deploy/$CHECK_DEPLOY" --timeout=5s >/dev/null 2>&1 &&
		[ "$(check_unavailable)" = 0 ]
}

# expect_check_available WHAT — green only: within CHECK_TIMEOUT.
expect_check_available() {
	[ "$ABP_EXPECT" = green ] || return 0
	wait_for "$CHECK_TIMEOUT" check_available ||
		die "the policy check is not available ${CHECK_TIMEOUT}s after $1 (status: $(kc -n "$NS" get deploy "$CHECK_DEPLOY" -o jsonpath='{.status}' 2>&1)); events: $(check_events | tail -n 3)"
	ok "the policy check is available ($1)"
}

# deliver POLICY — change the policy the operator's way and wait until the
# node's ConfigMap volume holds it (the kubelet syncs it on its own schedule).
deliver() {
	local deadline
	apply_policy "$1"
	deadline=$((SECONDS + RELOAD_TIMEOUT))
	until node_has_policy "$1"; do
		[ "$SECONDS" -lt "$deadline" ] || die "the policy never reached the node's ConfigMap volume in ${RELOAD_TIMEOUT}s"
		sleep 2
	done
}

# node_has_policy (e2e/authz.sh) reads the FIRST proxy pod's volume. With two
# pods on the node (e, f) the policy must be in each one's.
node_pods_have_policy() {
	local pod uid
	for pod in $(kc -n "$NS" get pods -l app.kubernetes.io/component=proxy -o name); do
		uid="$(kc -n "$NS" get "$pod" -o jsonpath='{.metadata.uid}')"
		[ "$(docker exec "$NODE" cat "/var/lib/kubelet/pods/$uid/volumes/kubernetes.io~configmap/opa-policy/policy.rego" 2>/dev/null)" = "$1" ] || return 1
	done
}

the_proxy_pod() {
	local pods
	pods="$(proxy_pods)"
	[ "$(wc -l <<<"$pods")" = 1 ] && [ -n "$pods" ] || die "expected exactly one proxy pod, got: $(tr '\n' ' ' <<<"$pods")"
	printf '%s' "$pods"
}

# --- steps ---------------------------------------------------------------------

verify_baseline() {
	local pod
	verify_decisions
	pod="$(the_proxy_pod)"
	wait_for 90 proxy_pod_is_ready "$pod" ||
		die "$pod: not Ready with a policy that compiles (authz ready='$(authz_field "$pod" .ready)', pod Ready='$(pod_ready "$pod")')"
	ok "$pod is Ready"
	expect_check_available "the install"
}

# b. A policy that compiles and denies everything.
verify_deny_all() {
	log "b. a policy that compiles and denies everything is loaded, and is not a failure"
	local pod id0 code den0 den1 err0 err1
	pod="$(the_proxy_pod)"
	id0="$(proxy_identity)"
	den0="$(authz_sum denied)"
	err0="$(authz_sum error)"
	apply_policy "$POLICY_DENY_ALL"
	code="$(await_code 403 "$RELOAD_TIMEOUT" "$ALLOW_HEADER")" ||
		die "the allowed request still answers $code ${RELOAD_TIMEOUT}s after the deny-all policy was applied"
	den1="$(authz_sum denied)"
	err1="$(authz_sum error)"
	[ "$den1" -gt "$den0" ] || die "ext_authz.denied did not move ($den0 -> $den1): the 403 is not the sidecar's decision"
	[ "$err1" = "$err0" ] || die "ext_authz.error grew by $((err1 - err0)) with a deny-all policy: a denial was counted as a failure"
	[ "$(proxy_identity)" = "$id0" ] || die "the deny-all policy restarted or replaced something: before [$id0], after [$(proxy_identity)]"
	ok "deny-all: 403 by decision (denied $den0 -> $den1, error +0), same pod"
	expect_check_available "a policy that compiles and denies everything"

	apply_policy "$POLICY"
	code="$(await_code 200 "$RELOAD_TIMEOUT" "$ALLOW_HEADER")" ||
		die "the first policy did not come back after deny-all (allow header answers $code)"
	ok "the first policy decides again"
	expect_check_available "the first policy again"
}

# c. A policy that does not parse, on a running sidecar.
verify_bad_running() {
	log "c. a policy that does not parse is applied: the node decides as before, and it shows (expect: $ABP_EXPECT)"
	local pod id0 err0 err1 code t0 since deadline
	pod="$(the_proxy_pod)"
	id0="$(proxy_identity)"
	err0="$(authz_sum error)"
	since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	t0="$SECONDS"
	apply_policy "$POLICY_BAD"

	if [ "$ABP_EXPECT" = green ]; then
		wait_for "$CHECK_TIMEOUT" check_reports_error "$since" ||
			die "${CHECK_TIMEOUT}s after a policy that does not parse was applied, no Unhealthy event of the policy check carries the compile error — the bad policy is invisible (#1447); events: $(check_events | tail -n 3)"
		ok "$((SECONDS - t0))s after the upgrade began an Unhealthy event carries the error: $(check_events | grep 'rego_parse_error' | tail -n 1 | tr -s '[:space:]' ' ' | cut -c1-200)"
		[ "$(check_unavailable)" -ge 1 ] ||
			die "the policy check reports no unavailable replica with a policy that does not parse (status: $(kc -n "$NS" get deploy "$CHECK_DEPLOY" -o jsonpath='{.status}'))"
		if kc -n "$NS" rollout status "deploy/$CHECK_DEPLOY" --timeout=20s >/dev/null 2>&1; then
			die "kubectl rollout status deploy/$CHECK_DEPLOY succeeded with a policy that does not parse"
		fi
		ok "deploy/$CHECK_DEPLOY: $(check_unavailable) unavailable replica(s), and its rollout does not complete"
	fi

	deadline=$((SECONDS + RELOAD_TIMEOUT))
	until node_has_policy "$POLICY_BAD"; do
		[ "$SECONDS" -lt "$deadline" ] || die "the bad policy never reached the node's ConfigMap volume in ${RELOAD_TIMEOUT}s"
		sleep 2
	done
	# Give the sidecar's watch time to act on it.
	sleep 10
	ok "the bad policy is on the node ($((SECONDS - t0))s after the upgrade began)"

	if [ "$ABP_EXPECT" = red ]; then
		# Nothing in the namespace reports it: no check exists, every workload
		# is as available as before.
		! kc -n "$NS" get deploy "$CHECK_DEPLOY" >/dev/null 2>&1 ||
			die "ABP_EXPECT=red but deploy/$CHECK_DEPLOY exists — this chart already shows a bad policy"
		# Events since the change only: a reused cluster may hold older ones.
		# shellcheck disable=SC2016 # a jsonpath template, not shell
		[ "$(kc -n "$NS" get events -o jsonpath='{range .items[*]}{.lastTimestamp}{" "}{.message}{"\n"}{end}' 2>/dev/null |
			awk -v since="$since" '$1 >= since' | grep -c 'rego_')" = 0 ] ||
			die "ABP_EXPECT=red but an event names the policy error"
		ok "RED as expected: no object and no event in $NS says the policy does not parse"
	fi

	code="$(mesh_code "$ALLOW_HEADER")"
	[ "$code" = 200 ] || die "with the bad policy on the node the allowed request answers $code, expected 200 — the last good policy was dropped"
	code="$(mesh_code)"
	[ "$code" = 403 ] || die "with the bad policy on the node the request without the header answers $code, expected 403"
	[ "$(proxy_identity)" = "$id0" ] || die "the bad policy restarted or replaced something: before [$id0], after [$(proxy_identity)]"
	err1="$(authz_sum error)"
	[ "$err1" = "$err0" ] || die "ext_authz.error grew by $((err1 - err0)) with the bad policy on a running sidecar"
	# The signal must not be the proxy pod's readiness (see e).
	proxy_pod_is_ready "$pod" ||
		die "$pod is NotReady with the bad policy on its node (authz ready='$(authz_field "$pod" .ready)'): a NotReady proxy pod is deleted by the next roll whether or not its successor can start"
	[ "$(kc -n "$NS" get ds aether-proxy -o jsonpath='{.status.numberUnavailable}')" = "" ] ||
		[ "$(kc -n "$NS" get ds aether-proxy -o jsonpath='{.status.numberUnavailable}')" = 0 ] ||
		die "the proxy DaemonSet counts a pod unavailable with the bad policy on a running sidecar"
	ok "the last good policy still decides (200 / 403); same pod, still Ready, no restart, same epoch, zero ext_authz errors"
}

# d. The sidecar container restarts with the bad file on the node.
verify_sidecar_restart() {
	log "d. the authz container is stopped with the bad policy on the node"
	local pod uid0 epoch0 err0 err1 code cid restarts0 p_restarts
	pod="$(the_proxy_pod)"
	uid0="$(kc -n "$NS" get pod "$pod" -o jsonpath='{.metadata.uid}')"
	epoch0="$(envoy_epoch)"
	err0="$(authz_sum error)"
	restarts0="$(authz_field "$pod" .restartCount)"
	cid="$(authz_field "$pod" .containerID)"
	cid="${cid#containerd://}"
	[ -n "$cid" ] || die "$pod: could not read the authz container's id"
	docker exec "$NODE" crictl stop "$cid" >/dev/null || die "could not stop the authz container $cid on $NODE"
	wait_for 120 authz_crash_looping "$pod" "$((restarts0 + 1))" ||
		die "$pod: the restarted authz sidecar is not exiting 1 (restarts $(authz_field "$pod" .restartCount), state $(authz_field "$pod" .state), last $(authz_field "$pod" .lastState))"
	ok "the restarted OPA exits 1: $(authz_field "$pod" .restartCount) restart(s), last exit code 1"
	[ "$(kc -n "$NS" logs "$pod" -c authz --tail=20 2>/dev/null | grep -c 'rego_parse_error')" -gt 0 ] ||
		[ "$(kc -n "$NS" logs "$pod" -c authz --previous --tail=20 2>/dev/null | grep -c 'rego_parse_error')" -gt 0 ] ||
		die "$pod: the exiting sidecar's log does not name the compile error"
	ok "its log names the error (load error ... rego_parse_error)"

	code="$(await_code 403 60 "$ALLOW_HEADER")" ||
		die "with no authz behind the socket the allowed request answers $code, expected 403 (failureMode DENY)"
	err1="$(authz_sum error)"
	[ "$err1" -gt "$err0" ] || die "ext_authz.error did not move ($err0 -> $err1): the 403 is not Envoy failing closed on an unreachable sidecar"
	p_restarts="$(kc -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[?(@.name=="proxy")].restartCount}')"
	[ "$p_restarts" = 0 ] || die "$pod: the proxy container restarted ($p_restarts) when its sidecar did"
	[ "$(envoy_epoch)" = "$epoch0" ] || die "the node Envoy's epoch moved ($epoch0 -> $(envoy_epoch)) when the sidecar restarted"
	ok "proxy keeps running (0 restarts, epoch $epoch0); every check fails closed: 403, ext_authz.error $err0 -> $err1"

	apply_policy "$POLICY"
	expect_check_available "a valid policy"
	code="$(await_code 200 "$((RELOAD_TIMEOUT + RESTART_TIMEOUT))" "$ALLOW_HEADER")" ||
		die "the allowed request still answers $code after a valid policy was applied — the crash-looping sidecar did not recover"
	[ "$(kc -n "$NS" get pod "$pod" -o jsonpath='{.metadata.uid}')" = "$uid0" ] || die "recovering the sidecar replaced the pod"
	wait_for 120 proxy_pod_is_ready "$pod" || die "$pod did not become Ready again after the valid policy"
	ok "a valid policy recovers it: 200 again, same pod, Ready"
}

# e. A proxy roll with the bad file on the node.
verify_roll_stalls() {
	log "e. a proxy roll with the bad policy on the node stalls with the old pod still deciding"
	local old new err0 err1 code i
	old="$(the_proxy_pod)"
	deliver "$POLICY_BAD"
	sleep 10
	proxy_pod_is_ready "$old" || die "$old is NotReady before the roll: the roll would delete it"
	err0="$(authz_sum error)"
	kc -n "$NS" rollout restart ds/aether-proxy >/dev/null
	for i in $(seq 1 60); do
		new="$(proxy_pods | grep -vx "$old" || true)"
		[ -n "$new" ] && break
		sleep 2
	done
	[ -n "$new" ] || die "the roll created no second proxy pod"
	wait_for 180 authz_crash_looping "$new" 2 ||
		die "$new: the surged pod's authz sidecar is not crash-looping (restarts $(authz_field "$new" .restartCount), state $(authz_field "$new" .state))"
	[ -z "$(proxy_started_at "$new")" ] || die "$new: the proxy container started although its authz sidecar never did"
	ok "$new: authz exits 1 ($(authz_field "$new" .restartCount) restarts), proxy never started"
	# Hold, then require the old pod to be untouched and still deciding.
	sleep 30
	[ "$(kc -n "$NS" get pod "$old" -o jsonpath='{.metadata.deletionTimestamp}')" = "" ] ||
		die "the roll deleted the old pod $old although its successor never became Ready"
	proxy_pod_is_ready "$old" || die "$old turned NotReady during the stalled roll"
	code="$(mesh_code "$ALLOW_HEADER")"
	[ "$code" = 200 ] || die "during the stalled roll the allowed request answers $code, expected 200 from the old pod's last good policy"
	code="$(mesh_code)"
	[ "$code" = 403 ] || die "during the stalled roll the request without the header answers $code, expected 403"
	err1="$(authz_sum error)"
	[ "$err1" = "$err0" ] || die "ext_authz.error grew by $((err1 - err0)) during the stalled roll"
	ok "the roll is stalled; $old is Ready and still decides with its last good policy (200 / 403, zero ext_authz errors)"
	OLD_POD="$old"
	NEW_POD="$new"
}

# f. The old pod is deleted: nothing on the node can start a proxy.
verify_pod_replaced() {
	log "f. the old pod is deleted with the bad policy on the node (today: the node ends up with no proxy)"
	local code t0
	node_pods_have_policy "$POLICY_BAD" || die "the bad policy is not in both proxy pods' volumes"
	t0="$SECONDS"
	kc -n "$NS" delete pod "$OLD_POD" --wait=false >/dev/null
	sleep 15
	code="$(mesh_code "$ALLOW_HEADER")"
	[ "$code" = 200 ] || die "15s after its deletion the old pod's Envoy answers $code, expected 200 (it serves on while it waits for a successor)"
	ok "15s after the deletion the old Envoy still serves (200): it is waiting for its successor"
	wait_for "$SUCCESSOR_TIMEOUT" pod_gone "$OLD_POD" ||
		die "$OLD_POD is still there ${SUCCESSOR_TIMEOUT}s after its deletion"
	ok "the old pod is gone after $((SECONDS - t0))s"
	[ -z "$(proxy_started_at "$NEW_POD")" ] || die "$NEW_POD: the proxy container started although its authz sidecar cannot"
	# mesh_code prints curl's own 000 and then its fallback 000 when curl fails.
	code="$(mesh_code "$ALLOW_HEADER")"
	code="${code:0:3}"
	[ "$code" != 200 ] && [ "$code" != 403 ] ||
		die "with no proxy on the node the allowed request answers $code — something is serving; if the proxy now starts without its sidecar, that is the #1447 decision and this step must change with it"
	ok "the node has no proxy: the allowed request answers $code (not a decision)"
}

# g. A valid policy.
verify_recovery() {
	log "g. a valid policy recovers the node"
	local code t0 pod
	t0="$SECONDS"
	apply_policy "$POLICY"
	expect_check_available "a valid policy"
	code="$(await_code 200 "$((RELOAD_TIMEOUT + RESTART_TIMEOUT))" "$ALLOW_HEADER")" ||
		die "the allowed request still answers $code after a valid policy was applied — the node did not recover"
	ok "the allowed request answers 200 again $((SECONDS - t0))s after the upgrade began"
	kc -n "$NS" rollout status ds/aether-proxy --timeout=300s >/dev/null || die "the stalled roll never completed"
	pod="$(the_proxy_pod)"
	[ "$pod" = "$NEW_POD" ] || die "the proxy pod is $pod, expected the stalled pod $NEW_POD to have started"
	wait_for 120 proxy_pod_is_ready "$pod" || die "$pod is not Ready after recovery"
	ok "$pod (the stalled pod) started its proxy and is Ready"
	verify_decisions
}

abp_verify() {
	[ -n "$OPA_IMAGE" ] || die "could not read the OPA preset image from $CHARTS_SRC/aether/values.yaml"
	log "a. baseline"
	verify_baseline
	verify_deny_all
	verify_bad_running
	verify_sidecar_restart
	verify_roll_stalls
	verify_pod_replaced
	verify_recovery
	local seen="is reported by the policy check when it is applied"
	[ "$ABP_EXPECT" = green ] || seen="is reported by NOTHING when it is applied (this chart has no policy check)"
	log "all assertions passed (#1447/#1448: a policy that does not compile $seen, leaves a running sidecar deciding, fails checks closed after a sidecar restart, stalls a roll, and takes the node's proxy out when the pod is replaced; expect=$ABP_EXPECT)"
}

case "${1:-}" in
up) up ;;
test) abp_verify ;;
verify) abp_verify ;;
down) down ;;
"") up && abp_verify ;;
*) die "usage: $0 {up|test|verify|down}" ;;
esac
