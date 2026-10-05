#!/usr/bin/env bash
# Hermetic test of sweep_since() and github_last_green_sweep()
# (scripts/push-heads-lib.sh): where the scheduled publish-verify sweep starts
# reading (#1281). The sweep re-verified every push head of the last day at each
# of the day's twelve runs until three in a row hit the 30-minute timeout; it now
# starts where the last GREEN scheduled sweep left off. What must hold:
#   - never earlier than the full window, never later than the last green
#     sweep's start minus the grace period minus the overlap (no gap);
#   - anything it cannot trust (no green sweep, an unparsable or future
#     timestamp) reads the full window: fail closed.
# No network: LAST_GREEN_SWEEP stands in for the API; times are literals.
#
# Run: bazel test //scripts:sweep_since_test
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091 # a runfile next to this test, not a path shellcheck can follow
. "$HERE/push-heads-lib.sh"

# 2026-10-05T22:44:54Z, the first sweep to finish after the timeouts (37384419419).
now=1791240294
window=$((now - 24 * 3600))
grace=7200
overlap=1800

FAILS=0
N=0
expect() { # expect <name> <want> <got>
	N=$((N + 1))
	if [ "$3" = "$2" ]; then
		echo "PASS  $1"
	else
		printf 'FAIL  %s\n  want: %s\n  got:  %s\n' "$1" "$2" "$3"
		FAILS=$((FAILS + 1))
	fi
}

# Anchored: the previous green sweep two hours earlier.
last="2026-10-05T20:44:54Z"
last_s=$((now - 2 * 3600))
expect "anchored: last green start - grace - overlap" \
	"$((last_s - grace - overlap))" "$(sweep_since "$now" "$window" "$grace" "$overlap" "$last")"
expect "anchored: reads back 4.5h (2.5h of commits once the 2h grace is cut off), not 24h" \
	"16200" "$((now - $(sweep_since "$now" "$window" "$grace" "$overlap" "$last")))"

# Clamped to the full window: the last green sweep is old (10-04 19:41, before
# the three timeouts) — every head since it must be read, up to a day back.
expect "last green older than the window: the full window" \
	"$window" "$(sweep_since "$now" "$window" "$grace" "$overlap" "2026-10-04T19:41:56Z")"
expect "last green just inside the window, but its overlap is not: the full window" \
	"$window" "$(sweep_since "$now" "$window" "$grace" "$overlap" "2026-10-05T00:00:00Z")"

# Fail closed.
expect "no green sweep on record: the full window" \
	"$window" "$(sweep_since "$now" "$window" "$grace" "$overlap" "")"
expect "unparsable timestamp: the full window" \
	"$window" "$(sweep_since "$now" "$window" "$grace" "$overlap" "yesterday-ish?")"
expect "a timestamp in the future: the full window" \
	"$window" "$(sweep_since "$now" "$window" "$grace" "$overlap" "2026-10-06T22:00:00Z")"

# LAST_GREEN_SWEEP stands in for the API, set-but-empty included (no green
# sweep on record must not fall through to a network call).
expect "LAST_GREEN_SWEEP is read instead of the API" \
	"$last" "$(LAST_GREEN_SWEEP="$last" github_last_green_sweep)"
expect "LAST_GREEN_SWEEP set but empty: empty, no API call" \
	"" "$(LAST_GREEN_SWEEP="" GITHUB_REPOSITORY=invalid/none PATH=/nonexistent github_last_green_sweep)"

if [ "$N" -ne 9 ]; then
	echo "ran ${N} cases, expected 9 -- a test that checks nothing passes"
	exit 2
fi
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS failure(s)"
	exit 1
fi
echo "all ${N} passed"
