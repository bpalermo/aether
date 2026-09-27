#!/usr/bin/env bash
# Gate for scripts/ci-bazel-warmup.sh (#1001): drives it against a stub bazel
# and asserts the retry contract, so a refactor cannot quietly turn it into a
# single attempt (no retry) or an unbounded one (a real failure retried).
#
#   1. a fetch failure that clears on attempt 2  -> success, exactly 2 runs
#   2. a fetch failure every time                -> failure, exactly 3 runs
#   3. a non-fetch failure (broken BUILD)        -> failure, exactly 1 run
#   4. success                                   -> success, exactly 1 run, and
#      the remote endpoints are cleared AFTER the caller's flags
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The stub records each invocation's argv and fails per $STUB_MODE.
cat >"$tmp/bazel" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$STUB_CALLS"
n="$(wc -l <"$STUB_CALLS")"
case "$STUB_MODE" in
ok) exit 0 ;;
flaky) [ "$n" -ge 2 ] && exit 0 ;;
esac
case "$STUB_MODE" in
flaky | down)
	echo "ERROR: An error occurred during the fetch of repository 'gazelle++go_deps+org_golang_google_genproto_googleapis_api':"
	echo "Error in fail: fetch_repo: Get \"https://proxy.golang.org/...\": net/http: TLS handshake timeout"
	exit 1
	;;
broken)
	echo "ERROR: /src/BUILD.bazel:3:11: no such attribute 'srcz' in 'go_library' rule"
	exit 1
	;;
esac
EOF
chmod +x "$tmp/bazel"

fail=0
run() { # mode want_rc want_calls
	local mode="$1" want_rc="$2" want_calls="$3" rc=0 calls
	: >"$tmp/calls"
	STUB_MODE="$mode" STUB_CALLS="$tmp/calls" BAZEL="$tmp/bazel" WARMUP_BACKOFF_SECONDS=0 \
		"$here/ci-bazel-warmup.sh" --config=ci //... >"$tmp/out" 2>&1 || rc=$?
	calls="$(wc -l <"$tmp/calls")"
	if [ "$rc" -ne "$want_rc" ] || [ "$calls" -ne "$want_calls" ]; then
		echo "FAIL ${mode}: exit ${rc} after ${calls} attempt(s), want exit ${want_rc} after ${want_calls}" >&2
		sed 's/^/  | /' "$tmp/out" >&2
		fail=1
	else
		echo "ok   ${mode}: exit ${rc} after ${calls} attempt(s)"
	fi
}

run flaky 0 2
run down 1 3
run broken 1 1
run ok 0 1

# The caller's flags come first and the remote endpoints are cleared after them,
# so `--config=ci` cannot re-enable BuildBuddy for the warm-up.
want="build --nobuild --config=ci //... --remote_executor= --remote_cache= --bes_backend="
got="$(head -1 "$tmp/calls")"
if [ "$got" != "$want" ]; then
	echo "FAIL argv: got '${got}', want '${want}'" >&2
	fail=1
else
	echo "ok   argv: ${got}"
fi

exit "$fail"
