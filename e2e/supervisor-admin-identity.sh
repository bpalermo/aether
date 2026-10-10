#!/usr/bin/env bash
# Kind leg for aether#1127: the old proxy pod's supervisor must never send a
# state-changing admin request to ANOTHER pod's Envoy on the node-shared admin
# address.
#
# The incident (2026-10-01 4->2 worker rollout, w01/w03): the surge successor's
# Envoy (epoch N+1) took the old Envoy's admin over the hot-restart protocol and
# then crashed (#1126). The new pod's supervisor started a FRESH Envoy at epoch
# 0, which bound 127.0.0.1:9901 (proxy pods are hostNetwork: one address per
# node). The old pod's supervisor, in its SIGTERM successor wait, found nobody
# terminating its Envoy, hit `no successor within budget; draining listeners`
# and POSTed /drain_listeners?graceful to 127.0.0.1:9901 -- the NEW pod's
# Envoy. That Envoy then added no listeners for any pod created on the node
# until the next handoff (~12 min of dial timeouts / connection refused).
#
# This leg reproduces that shape without the #1126 crash:
#   S0  make sure the node proxy runs at epoch >= 1 (one plain roll): a fresh
#       epoch-0 Envoy can only start beside the old one if their base-id
#       domain sockets (per epoch) do not collide, which is the incident's
#       situation too (the old Envoy was a hot-restart epoch)
#   S1  roll the proxy DaemonSet; once the successor's Envoy (epoch N+1) owns
#       the admin -- i.e. the old Envoy has closed its admin to it -- SIGKILL it,
#       standing in for the #1126 crash
#   S2  wait for the new pod's supervisor to start Envoy FRESH at epoch 0 and
#       its admin to answer LIVE (the incident's `no live predecessor; starting
#       fresh at epoch 0`); the new pod goes Ready and the DaemonSet terminates
#       the old pod, whose supervisor enters its bounded successor wait
#       (proxy.terminationGracePeriodSeconds=$SAI_GRACE here, so the wait is
#       SAI_GRACE-25 s instead of 155 s)
#   S3  wait for the old pod to finish terminating, and record what its
#       supervisor did at the end of the wait (from its log): drained the
#       shared admin (main) or refused because a foreign Envoy answered
#       (#1127)
#   S4  the assertion: a pod created AFTER that is served by the node proxy.
#       The destination's pod is replaced, the node proxy must list the new
#       pod's inbound listener (it took an LDS update after the fallback), and
#       once the new pod has warmed up SAI_REQUESTS requests from a mesh client
#       to it must all answer 200.
#
# Red-then-green (main vs the #1127 branch supervisor image):
#   red    the old supervisor logs `listeners drained; stopping envoy`
#          drainAccepted=true, the fresh Envoy's listener_manager reports
#          its listeners stopped, the replaced pod's listener never shows up
#          and requests fail
#   green  the old supervisor logs `not sending envoy admin request: ... ANOTHER
#          envoy` with answeredBy=<new pod>, and S4 passes
#
# Usage: e2e/supervisor-admin-identity.sh {up|run|down}   (bare = up + run)
# Env: SAI_GRACE (45: the proxy's terminationGracePeriodSeconds, read at `up`),
#      SAI_REQUESTS (20), SAI_WARMUP_S (60: how long the new pod may take to
#      answer its first 200 before the batch must be all 200s), plus every EWQ_* knob of e2e/eastwest-quic.sh
#      (EWQ_SKIP_BUILD / EWQ_IMAGE_TAG to run pre-built images).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export EWQ_CLUSTER="${EWQ_CLUSTER:-aether-1127}"
SAI_GRACE="${SAI_GRACE:-45}"
SAI_REQUESTS="${SAI_REQUESTS:-20}"
SAI_WARMUP_S="${SAI_WARMUP_S:-60}"
# The successor-wait budget is grace - (drainTime 10s + shutdownGrace 5s +
# margin 10s); a shorter grace makes the old pod reach its fallback in seconds.
EWQ_EXTRA_HELM_ARGS=(--set "proxy.terminationGracePeriodSeconds=$SAI_GRACE")
# shellcheck source=e2e/eastwest-quic.sh
. "$HERE/eastwest-quic.sh"

ENVOY_ARGV0="/usr/local/bin/envoy"
DST="quic-a"
SRC="client-a"

node_sh() { docker exec "$NODE" sh -c "$1"; }

# envoy_pids — "<pid> <restart-epoch>" per live envoy process on the node,
# anchored on argv[0] so the matching shell does not match itself.
envoy_pids() {
	# shellcheck disable=SC2016  # evaluated by the node's shell
	node_sh '
		for p in /proc/[0-9]*; do
			c=$({ tr "\0" " " <"$p/cmdline"; } 2>/dev/null) || continue
			case "$c" in
			"'"$ENVOY_ARGV0"' "*--restart-epoch*)
				e=$(printf "%s\n" "$c" | sed -n "s/.*--restart-epoch \([0-9]*\).*/\1/p")
				echo "${p#/proc/} $e" ;;
			esac
		done; exit 0'
}

# admin_epoch / admin_state — what the Envoy holding the node's admin reports.
server_info() { docker exec "$NODE" curl -s --max-time 2 http://127.0.0.1:9901/server_info 2>/dev/null | tr -d ' \n' || true; }
admin_epoch() { server_info | { grep -o '"restart_epoch":[0-9]*' || true; } | cut -d: -f2; }
admin_state() { server_info | { grep -o '"state":"[A-Z_]*"' || true; } | cut -d'"' -f4; }

proxy_pods() {
	kc -n "$NS" get pods -l app.kubernetes.io/component=proxy --no-headers \
		-o custom-columns=N:.metadata.name,D:.metadata.deletionTimestamp 2>/dev/null
}

wait_proxy_settled() {
	kc -n "$NS" rollout status ds/aether-proxy --timeout=300s >/dev/null || die "the proxy DaemonSet never settled"
	local i
	for i in $(seq 1 120); do
		[ "$(proxy_pods | wc -l)" = 1 ] && [ "$(admin_state)" = LIVE ] && return 0
		sleep 2
	done
	die "the proxy never settled to one pod with a LIVE admin"
}

s0_epoch_ge1() {
	log "S0 node proxy at a hot-restart epoch (>= 1)"
	wait_proxy_settled
	if [ "$(admin_epoch)" = 0 ]; then
		kc -n "$NS" rollout restart ds/aether-proxy >/dev/null
		sleep 5
		wait_proxy_settled
	fi
	local e
	e="$(admin_epoch)"
	[ -n "$e" ] && [ "$e" -ge 1 ] || die "S0: proxy still at epoch '${e}' after a roll"
	ok "proxy LIVE at epoch $e"
}

s1_kill_successor() {
	local old_epoch="$1" i pid
	log "S1 roll the proxy; SIGKILL the successor Envoy (epoch $((old_epoch + 1))) once it owns the admin"
	kc -n "$NS" rollout restart ds/aether-proxy >/dev/null
	for i in $(seq 1 600); do
		if [ "$(admin_epoch)" = "$((old_epoch + 1))" ]; then
			# SIGPIPE rule (#1121, e2e/README.md): this script runs under
			# pipefail, so no pipeline may end in a reader that exits before its
			# writer is done (`head`, `grep -q`/`-m`, `awk '...; exit'`): the
			# writer dies of SIGPIPE and the pipeline fails with 141 at a random
			# point. Read to EOF instead (awk keeps the first match, no `exit`).
			pid="$(envoy_pids | awk -v e="$((old_epoch + 1))" '!f && $2 == e { print $1; f = 1 }')"
			if [ -n "$pid" ]; then
				node_sh "kill -9 $pid"
				ok "killed successor envoy pid $pid (epoch $((old_epoch + 1))) after ~$((i / 5))s"
				return 0
			fi
		fi
		sleep 0.2
	done
	die "S1: the successor never took the admin at epoch $((old_epoch + 1))"
}

s2_fresh_start() {
	log "S2 the new pod starts envoy fresh at epoch 0; the old pod enters its successor wait"
	local i
	for i in $(seq 1 150); do
		if [ "$(admin_epoch)" = 0 ] && [ "$(admin_state)" = LIVE ]; then
			ok "fresh epoch-0 envoy LIVE on the node admin"
			echo "  envoy processes (pid epoch): $(envoy_pids | tr '\n' ' ')"
			return 0
		fi
		sleep 2
	done
	die "S2: no fresh epoch-0 envoy came up"
}

s3_old_pod_ends() {
	local old="$1" logf="$2" i
	log "S3 wait for the old pod ($old) to finish its termination"
	# Snapshot the old pod's log while it exists: the last snapshot before it is
	# deleted holds the end of its successor wait (a `logs -f` follower can be cut
	# off early by the container's exit).
	for i in $(seq 1 $((SAI_GRACE + 60))); do
		kc -n "$NS" logs "$old" -c proxy >"$logf.new" 2>/dev/null && mv "$logf.new" "$logf"
		kc -n "$NS" get pod "$old" >/dev/null 2>&1 || break
		sleep 1
	done
	kc -n "$NS" get pod "$old" >/dev/null 2>&1 && die "S3: old pod $old still exists"
	ok "old pod gone"
	echo "  old supervisor, end of its successor wait:"
	grep -E 'no successor|not sending envoy admin request|listeners drained|draining listeners|graceful' "$logf" |
		sed 's/^/    /' || echo "    (no matching line)"
}

s4_new_pod_served() {
	log "S4 a pod created after the old supervisor's fallback is served by the node proxy"
	local before after newpod i replies good
	before="$(pod_of "$DST")"
	kc -n "$TEST_NS" delete pod "$before" --wait=false >/dev/null
	for i in $(seq 1 90); do
		newpod="$(pod_of "$DST")"
		[ -n "$newpod" ] && [ "$newpod" != "$before" ] && break
		sleep 2
	done
	[ -n "$newpod" ] && [ "$newpod" != "$before" ] || die "S4: $DST was not replaced"
	kc -n "$TEST_NS" wait --for=condition=Ready "pod/$newpod" --timeout=120s >/dev/null || die "S4: $newpod never Ready"
	echo "  replaced $before -> $newpod"

	local listed=no
	for i in $(seq 1 30); do
		if grep -q "inbound_${TEST_NS}_${newpod}" <<<"$(admin /listeners 2>/dev/null)"; then
			listed=yes
			break
		fi
		sleep 1
	done
	echo "  node proxy lists inbound_${TEST_NS}_${newpod}: $listed"
	echo "  listener_manager: $(admin '/stats?filter=listener_manager.(total_listeners_active|listener_added|lds.update_success|total_listeners_draining)' | tr '\n' ' ')"

	[ "$listed" = yes ] || die "S4 RED: the node proxy never added the new pod's inbound listener (aether#1127)"

	# A new pod's first requests can fail for reasons unrelated to #1127 while it
	# warms up (its SVID is served to the proxy a few seconds after Ready, and
	# the first use of a (source, destination) pair builds its QUIC twin). Wait
	# up to SAI_WARMUP_S for the first 200; then every request must answer 200.
	local first=""
	for i in $(seq 1 "$SAI_WARMUP_S"); do
		first="$(req_batch "$SRC" "$DST" /hostname 1)"
		[ "${first%% *}" = 200 ] && break
		sleep 1
	done
	echo "  first 200 after ~${i}s of warm-up (last reply: $first)"
	replies="$(req_batch "$SRC" "$DST" /hostname "$SAI_REQUESTS")"
	good="$(printf '%s\n' "$replies" | awk -v p="$newpod" '$1 == "200" && $2 == p { n++ } END { print n + 0 }')"
	echo "  $SRC -> $DST: $good/$SAI_REQUESTS answered 200 by $newpod"
	after="$(printf '%s\n' "$replies" | awk '{ print $1 }' | sort | uniq -c | tr '\n' ' ')"
	echo "  status codes: $after"
	[ "$good" = "$SAI_REQUESTS" ] || die "S4 RED: requests to the new pod failed (aether#1127)"
	ok "the new pod is served: listener present, $good/$SAI_REQUESTS x 200"
}

run() {
	s0_epoch_ge1
	local old old_epoch logf
	old="$(proxy_pods | awk '!f && $2 == "<none>" { print $1; f = 1 }')"
	old_epoch="$(admin_epoch)"
	logf="$(mktemp)"
	# shellcheck disable=SC2064  # expand now: the path is known here
	trap "rm -f '$logf' '$logf.new'" EXIT
	s1_kill_successor "$old_epoch"
	s2_fresh_start
	s3_old_pod_ends "$old" "$logf"
	echo "  new proxy pod(s): $(proxy_pods | awk '{ print $1 }' | tr '\n' ' ')"
	s4_new_pod_served
	rm -f "$logf"
	log "aether#1127 GREEN: the old supervisor left the new pod's envoy alone"
}

case "${1:-}" in
up) up ;;
run) run ;;
down) down ;;
"") up && run ;;
*) die "usage: $0 {up|run|down}" ;;
esac
