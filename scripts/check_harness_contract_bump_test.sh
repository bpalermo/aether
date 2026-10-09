#!/usr/bin/env bash
# Hermetic test of scripts/check-harness-contract-bump.sh (#1541): the lock of
# the external-harness contract compared with the one at the pull request's
# base. No repository and no network: `git` is a fake that prints a canned
# file for `git show <the expected ref>:<the lock>` and fails for anything
# else.
#
# Run: bazel test //scripts:check_harness_contract_bump_test
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/check-harness-contract-bump.sh"
WORKFLOW="${TEST_SRCDIR:-}/${TEST_WORKSPACE:-_main}/.github/workflows/ci.yaml"
[ -f "$WORKFLOW" ] || WORKFLOW="$HERE/../.github/workflows/ci.yaml"
LOCK="test/harnesscontract/external-harness.lock.yaml"

TMP="$(mktemp -d "${TEST_TMPDIR:-/tmp}/harness-bump.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin"
cat >"$TMP/bin/git" <<'EOF'
#!/usr/bin/env bash
# The one commit this repository has is $FAKE_BASE_REF, and the one path in it
# is $FAKE_LOCK, when $FAKE_BASE_FILE exists. `cat-file -e` answers for those;
# `show <ref>:<path>` prints the file, unless $FAKE_SHOW_FAILS is set.
# Everything else fails as git would.
if [ "$1" = "cat-file" ] && [ "$2" = "-e" ]; then
	[ "$3" = "$FAKE_BASE_REF^{commit}" ] && exit 0
	[ "$3" = "$FAKE_BASE_REF:$FAKE_LOCK" ] && [ -f "$FAKE_BASE_FILE" ] && exit 0
	exit 1
fi
if [ "$1" = "show" ] && [ "$2" = "$FAKE_BASE_REF:$FAKE_LOCK" ] && [ -f "$FAKE_BASE_FILE" ] && [ -z "${FAKE_SHOW_FAILS:-}" ]; then
	cat "$FAKE_BASE_FILE"
	exit 0
fi
echo "fatal: unexpected git $*" >&2
exit 128
EOF
chmod +x "$TMP/bin/git"

BASE_LOCK='# a comment
version: 3
promises:
  "a value": "1111111111111111"
  "b fields t": "2222222222222222"
  "c containers agent args --mesh-domain": "3333333333333333"
'

fail=0
n=0

# run NAME WANT_EXIT WANT_SUBSTRING BASE_CONTENT HEAD_CONTENT [REF_ARG]
# BASE_CONTENT "-" means the base has no lock; HEAD_CONTENT "-" means this
# checkout has none.
run() {
	local name="$1" want_exit="$2" want="$3" base="$4" head="$5" ref="${6-abc123}"
	n=$((n + 1))
	local dir="$TMP/case$n"
	mkdir -p "$dir/work/$(dirname "$LOCK")"
	[ "$base" = "-" ] || printf '%s' "$base" >"$dir/base.yaml"
	[ "$head" = "-" ] || printf '%s' "$head" >"$dir/work/$LOCK"
	local out code
	out="$(cd "$dir/work" && PATH="$TMP/bin:$PATH" FAKE_BASE_REF="abc123" FAKE_LOCK="$LOCK" FAKE_BASE_FILE="$dir/base.yaml" \
		bash "$SCRIPT" ${ref:+"$ref"} 2>&1)"
	code=$?
	if [ "$code" -ne "$want_exit" ] || [[ "$out" != *"$want"* ]]; then
		echo "FAIL: $name: exit $code, want $want_exit and output containing '$want'. Output:"
		printf '%s\n' "$out" | sed 's/^/    /'
		fail=1
	else
		echo "ok: $name"
	fi
}

run "nothing changed" 0 "every promise" "$BASE_LOCK" "$BASE_LOCK"
run "a promise added" 0 "every promise" "$BASE_LOCK" "$BASE_LOCK"'  "d value": "4444444444444444"
'
run "the header and the order changed" 0 "every promise" "$BASE_LOCK" '# another comment
version: 3
promises:
  "c containers agent args --mesh-domain": "3333333333333333"
  "b fields t": "2222222222222222"
  "a value": "1111111111111111"
'
run "a promise removed with its line" 1 '"b fields t"' "$BASE_LOCK" "${BASE_LOCK/  \"b fields t\": \"2222222222222222\"$'\n'/}"
run "a removed promise is counted" 1 "1 promise(s) of the external-harness contract left or changed and its version is still 3" "$BASE_LOCK" "${BASE_LOCK/  \"b fields t\": \"2222222222222222\"$'\n'/}"
run "a promise changed and its digest recomputed" 1 '"a value"' "$BASE_LOCK" "${BASE_LOCK/1111111111111111/9999999999999999}"
run "a promise renamed" 1 '"c containers agent args --mesh-domain"' "$BASE_LOCK" "${BASE_LOCK/--mesh-domain/--domain}"
# A name with a quote in it, as the lock writes it.
QUOTED_LOCK="$BASE_LOCK"'  "c containers agent env_contains VAR say \"k\"": "5555555555555555"
'
run "a name with an escaped quote, unchanged" 0 "every promise" "$QUOTED_LOCK" "$QUOTED_LOCK"
run "a promise with an escaped quote in its name removed" 1 '"c containers agent env_contains VAR say \"k\""' "$QUOTED_LOCK" "$BASE_LOCK"
run "every promise removed" 1 "3 promise(s)" "$BASE_LOCK" 'version: 3
promises: {}
'
run "a promise removed and the version bumped" 0 "version was bumped: 3 -> 4" "$BASE_LOCK" 'version: 4
promises:
  "a value": "1111111111111111"
'
run "the version bumped and nothing else" 0 "every promise" "$BASE_LOCK" "${BASE_LOCK/version: 3/version: 4}"
run "the version bumped by two and nothing else" 1 "went from 3 at abc123 to 5 here: a bump is by one" "$BASE_LOCK" "${BASE_LOCK/version: 3/version: 5}"
run "a promise removed and the version bumped by two" 1 "a bump is by one" "$BASE_LOCK" 'version: 5
promises:
  "a value": "1111111111111111"
'
run "the version lowered" 1 "went back: 3 at abc123, 2 here" "$BASE_LOCK" "${BASE_LOCK/version: 3/version: 2}"
run "the version lowered to hide a removal" 1 "went back" "$BASE_LOCK" 'version: 2
promises: {}
'
run "no lock at the base" 0 "does not exist at abc123" "-" "$BASE_LOCK"
run "no lock in this checkout" 1 "is missing" "$BASE_LOCK" "-"
run "no version in this checkout's lock" 1 "no 'version: N' line" "$BASE_LOCK" "${BASE_LOCK/version: 3/versions: 3}"
# The base is the ref the caller names, and a base that is not here fails: the
# fake has the commit abc123 only. A lock removed in this checkout would
# otherwise pass for want of anything to compare.
run "a base this checkout does not have" 1 "other-ref is not a commit this checkout has" "$BASE_LOCK" "$BASE_LOCK" "other-ref"
run "a base this checkout does not have, with a promise removed" 1 "is not a commit this checkout has" "$BASE_LOCK" 'version: 3
promises: {}
' "other-ref"
FAKE_SHOW_FAILS=1 run "a lock at the base that git cannot read" 1 "exists at abc123 and git could not read it" "$BASE_LOCK" "$BASE_LOCK"

# The workflow runs it against the pull request's base, in a job with the full
# history.
# shellcheck disable=SC2016 # the workflow's own text, not a shell expansion
if ! grep -qF 'run: scripts/check-harness-contract-bump.sh "$BASE_SHA"' "$WORKFLOW" ||
	! grep -B3 -F 'run: scripts/check-harness-contract-bump.sh "$BASE_SHA"' "$WORKFLOW" | grep -qF 'BASE_SHA: ${{ github.event.pull_request.base.sha }}'; then
	echo "FAIL: .github/workflows/ci.yaml does not run scripts/check-harness-contract-bump.sh with the pull request's base sha"
	fail=1
else
	echo "ok: ci.yaml runs the check against the pull request's base"
fi

if [ "$fail" -ne 0 ]; then
	echo "check_harness_contract_bump_test FAILED"
	exit 1
fi
echo "check_harness_contract_bump_test passed ($n cases)."
