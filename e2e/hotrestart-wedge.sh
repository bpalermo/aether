#!/usr/bin/env bash
# Kind leg for aether#1050: the hot-restart main-thread deadlock, and proof that
# the chart's mitigation (proxy.hotRestart.skipParentStats, which passes Envoy
# --skip-hot-restart-parent-stats) removes it.
#
# The production symptom: a hot-restart successor whose main thread goes silent
# right after `starting workers` while the draining parent goes silent right
# after `closing and draining listeners`, until the supervisor's 30 s liveness
# watchdog kills both epochs (~1 in 60 proxy rolls on talos, h3 inbound on).
#
# The mechanism (pinned Envoy 726d7ac + carried patches; confirmed with the
# per-thread kernel stacks and gdb backtraces this leg captures): a
# cross-process deadlock on the two hot-restart domain sockets.
#   * From the parent's drainListeners (the child's DrainListeners RPC, sent from
#     the startWorkers completion) every QUIC/UDP datagram the PARENT's workers
#     read that no parent session owns is posted to the parent MAIN thread and
#     sent to the child with a BLOCKING sendmsg on the `_udp` AF_UNIX datagram
#     socket (hot_restarting_parent.cc:60-85, hot_restarting_base.cc:84-143).
#     The child's UDP listeners are paused until the parent is terminated
#     (udp_listener_impl.cc:39-58), so for that whole ~15 s window EVERY packet of
#     every new QUIC connection takes that path.
#   * The child's main thread asks the parent for stats on every stats flush
#     (5 s) with a BLOCKING recvmsg and no timeout (hot_restarting_child.cc:161-176,
#     server.cc:272-274).
#   * If the child's `_udp` receive queue is full when it enters that recvmsg,
#     the parent's main thread is parked in sendmsg and never reads the Stats
#     request: both main threads wait on each other forever. Workers keep
#     running (existing connections serve), admin (main thread) is dead, and
#     SIGTERM (a main-dispatcher signal event) is ignored by both processes.
#
# What it proves (the red-then-green check; e2e/soak/README.md, #1050):
#   red    WEDGE_SKIP_PARENT_STATS=false WEDGE_FREEZE_S=6 on a proxy image
#          WITHOUT the carried #1060 patch -> WEDGES > 0 (4/4 on 2026-09-28):
#          the forced fault reproduces the production wedge
#   green  the same on the patched image (the chart's pin since #1061; the
#          chart default is WEDGE_SKIP_PARENT_STATS=false again) -> WEDGES = 0
#          (0/8), and WEDGE_SKIP_PARENT_STATS=true (the emergency switch) -> 0
#          (0/6), because the child never makes the blocking getParentStats call
# Unforced (WEDGE_FREEZE_S=0) the race is rare on kind (0/30 at ~1,900 rps).
# The knob is applied at `up` (a re-run of `up` on a live cluster only upgrades
# the aether release), and `run` reports what the live Envoy was started with.
#
# What this leg does, on the eastwest-quic bring-up (single kind node, SIGHUP =
# same-pod hot restart, the same Envoy protocol as a cross-pod roll):
#   W0  scale the h3 destinations to WEDGE_REPLICAS pods each (one h3 inbound
#       listener per pod), warm every (source, destination) twin
#   W1  drive WEDGE_LOOPS keep-alive HTTP/1.1 loops at WEDGE_RATE/s from both
#       sources to every h3 destination, so new QUIC connections from the child
#       flow through the parent's forwarding path during each drain window
#   W2  WEDGE_RESTARTS SIGHUP hot restarts, WEDGE_GAP s apart; after each the
#       child's admin is polled every 0.5 s for 20 s. Unresponsive for
#       >= WEDGE_SILENT_S s while the child process is alive = a WEDGE. On a
#       wedge, capture per-thread comm / syscall / wchan / kernel stack of every
#       envoy process (the kind node is privileged; the images are distroless)
#       into $WEDGE_OUT, then wait for the watchdog to recover the pod.
#
# Usage: e2e/hotrestart-wedge.sh {up|run|down}   (up = the eastwest-quic-hotrestart
#        bring-up; run needs it; bare = up + run)
# Env: WEDGE_SKIP_PARENT_STATS (false: the chart default; read at `up`),
#      WEDGE_RESTARTS (30), WEDGE_GAP (24), WEDGE_REPLICAS (6), WEDGE_LOOPS (24),
#      WEDGE_RATE (20), WEDGE_SILENT_S (5), WEDGE_OUT (mktemp -d),
#      WEDGE_STOP_ON_FIRST (1), WEDGE_HAMMER (0: N parallel admin /stats readers,
#      which lengthen the main thread's busy periods the way a big prod stat set,
#      xDS churn and the OTel sink flush do -- widening the race window),
#      WEDGE_FREEZE_S (0: if >0, SIGSTOP the NEW epoch for that many seconds 3 s
#      after the SIGHUP -- inside the parent's forwarding window -- so the
#      child's `_udp` queue fills, the parent's main thread parks in sendmsg,
#      and the child's 5 s stats-flush timer is due the moment it resumes. A
#      forced-fault probe of the mechanism, not a production shape),
#      WEDGE_GDB_IMAGE (unset: an image with gdb; on a wedge the main thread of
#      every envoy is backtraced from a privileged container sharing the kind
#      node's pid namespace, e.g. one built from debian + `apt-get install gdb`).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export EWQ_CLUSTER="${EWQ_CLUSTER:-eastwest-quic-hr}"
# shellcheck source=e2e/eastwest-quic.sh
. "$HERE/eastwest-quic.sh"

WEDGE_RESTARTS="${WEDGE_RESTARTS:-30}"
WEDGE_GAP="${WEDGE_GAP:-24}"
WEDGE_REPLICAS="${WEDGE_REPLICAS:-6}"
WEDGE_LOOPS="${WEDGE_LOOPS:-24}"
WEDGE_RATE="${WEDGE_RATE:-20}"
WEDGE_SILENT_S="${WEDGE_SILENT_S:-5}"
WEDGE_OUT="${WEDGE_OUT:-$(mktemp -d)}"
WEDGE_STOP_ON_FIRST="${WEDGE_STOP_ON_FIRST:-1}"
WEDGE_HAMMER="${WEDGE_HAMMER:-0}"
WEDGE_FREEZE_S="${WEDGE_FREEZE_S:-0}"
WEDGE_SKIP_PARENT_STATS="${WEDGE_SKIP_PARENT_STATS:-false}"
WEDGE_PATH="/echo?msg=$(printf 'x%.0s' $(seq 1 700))"

# node_sh CMD — run CMD as root in the kind node (shares the proxy's host netns
# and sees its processes).
node_sh() { docker exec "$NODE" sh -c "$1"; }

# ENVOY_ARGV0 is the proxy image's Envoy path. Every process match below anchors
# on it as argv[0]: the node-side `sh -c` running the match has the pattern in
# its OWN argv, so an unanchored `*envoy*` glob matches itself.
ENVOY_ARGV0="/usr/local/bin/envoy"

# envoy_pids — "<pid> <restart-epoch>" per live envoy process on the node.
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

supervisor_pid() {
	# shellcheck disable=SC2016
	node_sh '
		for p in /proc/[0-9]*; do
			a0=$({ tr "\0" "\n" <"$p/cmdline"; } 2>/dev/null | head -n 1)
			if [ "$a0" = /opt/aether/supervisor ]; then echo "${p#/proc/}"; fi
		done; exit 0' | head -n 1
}

# skip_parent_stats — "yes" iff a live envoy was started with
# --skip-hot-restart-parent-stats (what the chart knob actually rendered).
skip_parent_stats() {
	# shellcheck disable=SC2016  # evaluated by the node's shell
	if node_sh '
		for p in /proc/[0-9]*; do
			c=$({ tr "\0" " " <"$p/cmdline"; } 2>/dev/null) || continue
			case "$c" in
			"'"$ENVOY_ARGV0"' "*--skip-hot-restart-parent-stats*) exit 0 ;;
			esac
		done; exit 1'; then
		echo yes
	else
		echo no
	fi
}

epoch_now() {
	docker exec "$NODE" curl -s --max-time 1 http://127.0.0.1:9901/server_info 2>/dev/null |
		tr -d ' \n' | { grep -o '"restart_epoch":[0-9]*' || true; } | cut -d: -f2 || true
}

# wait_recovered — block until a supervisor runs and its Envoy admin answers
# (up to 5 min).
wait_recovered() {
	local i
	for i in $(seq 1 150); do
		if [ -n "$(supervisor_pid)" ] && [ -n "$(epoch_now)" ]; then
			echo "  recovered after ~$((i * 2))s (epoch $(epoch_now))"
			return 0
		fi
		sleep 2
	done
	die "the proxy did not recover within 5 min of a wedge"
}

# forensics TAG — per-thread state of every envoy process into $WEDGE_OUT/TAG.
forensics() {
	local tag="$1" f
	f="$WEDGE_OUT/$tag.txt"
	{
		echo "## $(date -u +%H:%M:%S.%NZ) envoy processes (pid epoch):"
		envoy_pids
		# shellcheck disable=SC2016
		node_sh '
			for p in /proc/[0-9]*; do
				c=$({ tr "\0" " " <"$p/cmdline"; } 2>/dev/null) || continue
				case "$c" in "'"$ENVOY_ARGV0"' "*--restart-epoch*) ;; *) continue ;; esac
				echo "### pid ${p#/proc/}: $c"
				for t in "$p"/task/*; do
					printf "tid=%s comm=%s wchan=%s syscall=%s\n" "${t##*/}" "$(cat "$t/comm")" \
						"$(cat "$t/wchan" 2>/dev/null)" "$(cut -d" " -f1-3 "$t/syscall" 2>/dev/null)"
				done
				echo "#### main thread kernel stack"
				cat "$p/task/${p#/proc/}/stack" 2>/dev/null || echo "(no kernel stack readable)"
				echo "#### unix sockets (inode rq):"
				ls -l "$p/fd" 2>/dev/null | grep -c socket
			done
			echo "### /proc/net/unix hot-restart sockets"
			grep -E "hot|envoy_domain|_udp" /proc/net/unix || true'
		if [ -n "${WEDGE_GDB_IMAGE:-}" ]; then
			local pid
			for pid in $(envoy_pids 2>/dev/null | cut -d' ' -f1); do
				echo "### gdb main-thread backtrace, pid $pid"
				docker run --rm --privileged --pid="container:$NODE" "$WEDGE_GDB_IMAGE" \
					gdb -batch -ex "set sysroot /proc/$pid/root" "/proc/$pid/root/usr/local/bin/envoy" -p "$pid" -ex "thread apply 1 bt 30" 2>&1 | grep -E '^#' | cut -c1-240
			done
		fi
	} >"$f" 2>&1
	echo "$f"
}

# watch_restart BEFORE — poll the new epoch's admin for 20 s. Prints WEDGE or OK.
watch_restart() {
	local before="$1" t0 now e fail_since="" i
	t0=$(date +%s.%N)
	for i in $(seq 1 60); do
		e="$(epoch_now)"
		now=$(date +%s.%N)
		if [ -n "$e" ] && [ "$e" -gt "$before" ]; then
			fail_since=""
		else
			[ -n "$fail_since" ] || fail_since="$now"
			if awk -v a="$now" -v b="$fail_since" -v s="$WEDGE_SILENT_S" 'BEGIN { exit !(a - b >= s) }' &&
				awk -v a="$now" -v b="$t0" 'BEGIN { exit !(a - b >= 8) }'; then
				echo WEDGE
				return 0
			fi
		fi
		awk -v a="$now" -v b="$t0" 'BEGIN { exit !(a - b >= 22) }' && break
		sleep 0.5
	done
	echo OK
}

load_loop() {
	local src="$1" dst="$2" pod n
	pod="$(pod_of "$src")"
	n=$(((WEDGE_RESTARTS * WEDGE_GAP + 120) * WEDGE_RATE))
	# shellcheck disable=SC2016  # evaluated by the POD's shell
	kc -n "$TEST_NS" exec "$pod" -c curl -- sh -c '
		url="$1"; n="$2"; rate="$3"; loops="$4"
		cfg=/tmp/w-$$.cfg; : >"$cfg"; i=0
		while [ "$i" -lt "$n" ]; do printf "url = \"%s\"\noutput = \"/dev/null\"\n" "$url" >>"$cfg"; i=$((i + 1)); done
		j=0
		while [ "$j" -lt "$loops" ]; do
			curl -s --max-time 5 --rate "$rate/s" -K "$cfg" -w "%{http_code}\n" >"$cfg.$j" &
			j=$((j + 1))
		done
		wait; cat "$cfg".* | sort | uniq -c; rm -f "$cfg" "$cfg".*
	' sh "http://$(fqdn "$dst"):$OUTBOUND_PORT$WEDGE_PATH" "$n" "$WEDGE_RATE" "$WEDGE_LOOPS" 2>/dev/null
}

wedge_run() {
	node_sh 'rm -f /tmp/wedge-stop' || true
	wait_recovered
	log "W0 scaling ${QUIC_DSTS[*]} to $WEDGE_REPLICAS replicas; warming twins"
	local d s
	for d in "${QUIC_DSTS[@]}"; do kc -n "$TEST_NS" scale "deploy/$d" --replicas="$WEDGE_REPLICAS" >/dev/null; done
	for d in "${QUIC_DSTS[@]}"; do kc -n "$TEST_NS" rollout status "deploy/$d" --timeout=240s >/dev/null || die "scale $d"; done
	sleep 10
	for s in "${SOURCES[@]}"; do for d in "${QUIC_DSTS[@]}"; do req_batch "$s" "$d" "$WEDGE_PATH" 3 >/dev/null || true; done; done
	echo "  h3 inbound listeners: $(admin /listeners | grep -c '_h3::' || true)"
	echo "  forensics dir: $WEDGE_OUT"
	local skip
	skip="$(skip_parent_stats)"
	echo "  envoy --skip-hot-restart-parent-stats: $skip"

	log "W1 load: $WEDGE_LOOPS loops x ${WEDGE_RATE}/s per (source, h3 destination)"
	local pids=()
	for s in "${SOURCES[@]}"; do
		for d in "${QUIC_DSTS[@]}"; do
			load_loop "$s" "$d" >"$WEDGE_OUT/load-$s-$d.txt" &
			pids+=("$!")
		done
	done
	if [ "$WEDGE_HAMMER" -gt 0 ]; then
		# shellcheck disable=SC2016  # evaluated by the node's shell
		docker exec -d "$NODE" sh -c 'n=$1; j=0; while [ $j -lt $n ]; do
			(while [ ! -e /tmp/wedge-stop ]; do curl -s -o /dev/null --max-time 3 http://127.0.0.1:9901/stats; done) &
			j=$((j + 1)); done; wait' sh "$WEDGE_HAMMER"
	fi
	sleep 15

	log "W2 $WEDGE_RESTARTS hot restarts, ${WEDGE_GAP}s apart"
	local i before pid verdict wedges=0 f
	for i in $(seq 1 "$WEDGE_RESTARTS"); do
		before="$(epoch_now)"
		[ -n "$before" ] || {
			sleep 5
			before="$(epoch_now)"
		}
		pid="$(supervisor_pid)"
		[ -n "$pid" ] || die "no supervisor"
		node_sh "kill -HUP $pid"
		if [ "$WEDGE_FREEZE_S" -gt 0 ]; then
			sleep 3
			local child
			child="$(envoy_pids 2>/dev/null | sort -k2 -n | tail -n 1 | cut -d' ' -f1)"
			node_sh "kill -STOP $child; sleep $WEDGE_FREEZE_S; kill -CONT $child"
			echo "  froze epoch pid $child for ${WEDGE_FREEZE_S}s"
		fi
		verdict="$(watch_restart "${before:-0}")"
		if [ "$verdict" = WEDGE ]; then
			wedges=$((wedges + 1))
			f="$(forensics "wedge-$i")"
			printf '\033[1;31m  ✗ restart %s (epoch %s -> ?): WEDGE — forensics %s\033[0m\n' "$i" "$before" "$f"
			sed -n '1,200p' "$f" | grep -aE '^(##|###|tid=[0-9]+ comm=envoy )|sendmsg|recvmsg|unix' | head -40
			kc -n "$NS" logs -l app.kubernetes.io/component=proxy -c proxy --tail=200 --prefix >"$WEDGE_OUT/wedge-$i-proxy.log" 2>&1 || true
			if [ "$WEDGE_STOP_ON_FIRST" = 1 ]; then
				sleep 3
				forensics "wedge-$i-b" >/dev/null
				break
			fi
			# Let the watchdog (30 s) + drain kill (15 s) recover the pod. Each
			# recovery is a container restart, so kubelet's crash-loop backoff
			# grows with every wedge: wait for a live admin, not a fixed time.
			wait_recovered
			sleep 10
		else
			ok "restart $i: epoch $before -> $(epoch_now) responsive ($(date -u +%H:%M:%SZ))"
			sleep "$((WEDGE_GAP - 20))"
		fi
	done
	echo "  wedges: $wedges / $i restarts"
	node_sh 'touch /tmp/wedge-stop' || true
	# The loops outlive the restarts and are killed below, so their own summary
	# never prints: read each source pod's per-request status files first.
	for s in "${SOURCES[@]}"; do
		echo "  codes from $s: $(kc -n "$TEST_NS" exec "$(pod_of "$s")" -c curl -- sh -c 'cat /tmp/w-*.cfg.* 2>/dev/null | sort | uniq -c' | tr '\n' ' ')"
	done
	kill "${pids[@]}" 2>/dev/null || true
	wait 2>/dev/null || true
	for f in "$WEDGE_OUT"/load-*.txt; do echo "  $(basename "$f"): $(tr '\n' ' ' <"$f")"; done
	echo "WEDGES=$wedges RESTARTS=$i FREEZE_S=$WEDGE_FREEZE_S SKIP_PARENT_STATS=$skip"
}

case "${1:-}" in
up) HR_SKIP_PARENT_STATS="$WEDGE_SKIP_PARENT_STATS" "$HERE/eastwest-quic-hotrestart.sh" up ;;
run) wedge_run ;;
down) down ;;
"") HR_SKIP_PARENT_STATS="$WEDGE_SKIP_PARENT_STATS" "$HERE/eastwest-quic-hotrestart.sh" up && wedge_run ;;
*) die "usage: $0 {up|run|down}" ;;
esac
