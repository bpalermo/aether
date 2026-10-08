#!/usr/bin/env bash
# Sourced by the soak scripts that trap TERM and run for hours (churn.sh,
# restart-watch.sh, sample-proxy-rss.sh): how they read the clock and how they
# wait. Neither forks.
#
#   soak_now VAR      VAR = the time, epoch seconds
#   soak_stamp VAR    VAR = the time, UTC, 2026-10-08T16:04:41Z
#   nap_open NAME     once, before the first wait; exits 2 if it cannot
#   nap_until EPOCH   wait until then
#   nap SECONDS       wait that long (whole seconds)
#   nap_brief T       one wait of T seconds; T may be a fraction, at most a slice
#
# Why no fork, twice over.
#
# The clock (#1418). bash 5.2.21 drops a trapped signal that arrives while it
# expands a command substitution: `trap: line 2: unexpected EOF while looking
# for matching ')'`, the handler is not run, and the script carries on. It was
# seen in `$(date ...)` (2 of 1,680 timed TERMs to restart-watch.sh). So the
# time is read with printf's %(...)T, a builtin, and no `$(date)` is left in a
# loop that runs for hours. `TZ=UTC printf` is how the stamp is UTC whatever the
# caller's zone is; measured on bash 4.2.53, 4.4.23 and 5.2.21, with the
# environment's TZ three hours off.
#
# The wait (#1386, #1419). `sleep N & pid=$!; wait "$pid"` with a trap that
# kills $pid leaves the sleep behind when TERM lands before `pid=$!`, and
# sometimes when it lands just before `wait`. A foreground `sleep N` leaves
# nothing behind and instead makes TERM wait: bash runs a trap only when the
# foreground child has ended, which for churn.sh was up to 90 minutes. A wait
# with no child has neither fault: `read -t` on a fifo nobody writes to. fd 9
# is that fifo, opened read-write (the open does not block, the read never sees
# end-of-file) and unlinked at once. bash runs a trap that arrives during
# `read` at once.
#
# Needs bash 4.2+ (printf %(...)T; fractional read -t is older).

soak_now() { printf -v "$1" '%(%s)T' -1; }
soak_stamp() { TZ=UTC printf -v "$1" '%(%FT%TZ)T' -1; }

nap_open() {
	local dir
	dir="$(mktemp -d "${TMPDIR:-/tmp}/soak-nap.XXXXXX")" || dir=""
	if [ -z "$dir" ] || ! mkfifo "$dir/nap" || ! exec 9<>"$dir/nap"; then
		echo "$1: cannot make the fifo it waits on (mktemp -d and mkfifo under ${TMPDIR:-/tmp})" >&2
		exit 2
	fi
	rm -rf "$dir"
}

# In slices of NAP_SLICE seconds, never one long read. Whatever bash is doing
# when a signal arrives, the trap is run no later than the end of the builtin
# in hand; a slice makes that at most NAP_SLICE seconds, where one read of the
# whole wait would make it the wait. (SOAK_NAP_SLICE is the tests': a dry run
# on a clock of its own takes each wait in one step. A longer slice on a real
# run is only a slower TERM.)
NAP_SLICE="${SOAK_NAP_SLICE:-2}"
nap_until() {
	local now left
	while
		soak_now now
		left=$(($1 - now))
		[ "$left" -gt 0 ]
	do
		if [ "$left" -gt "$NAP_SLICE" ]; then left=$NAP_SLICE; fi
		nap_brief "$left"
	done
}
nap() {
	local now
	soak_now now
	nap_until $((now + $1))
}
nap_brief() {
	local rc
	read -r -t "$1" -u 9 _
	rc=$?
	# Above 128 is the timeout: the one way this read ends, since nothing
	# writes to the fifo. Anything else means fd 9 is no longer that fifo: a
	# foreground sleep then (a trap waits it out, at most a slice), rather
	# than a loop that spins.
	if [ "$rc" -le 128 ]; then sleep "$1"; fi
}
