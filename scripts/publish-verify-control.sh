#!/usr/bin/env bash
# The expected-red control for publish-verify (#930).
#
# WHY THIS EXISTS
#
# scripts/verify-published-artifacts.sh is the gate that catches a commit whose
# publish never landed (#880). Its only demonstrated red state used to be a real
# defect — 1e31e3a (#924) published unsigned — and that red AGED OUT of the
# sweep's 24h window three runs later with nothing fixed (#929, #930). From then
# on the gate's failure path was asserted, not shown: the #853 shape, one level
# up from "a gate that checks nothing".
#
# So every publish-verify run first proves the gate CAN fail. It hands the
# verifier a commit that can never have artifacts and asserts the verifier
# reports it red — for the right reason — before the real gate runs.
#
# THE CONTROL COMMIT
#
# Built here, at run time, with `git commit-tree`: the tree of <base> (default
# origin/main, else HEAD), <base> as its parent, a fixed identity and fixed
# dates, and a message naming it as this control. It exists only in this
# checkout's object store — no ref points at it and it is never pushed — so no
# publish run can ever have built it, and its sha is in no GHCR tag list.
#
# Why not a real commit on main: the natural candidates (a stack-merge
# intermediate such as ef44437, #975; or 1e31e3a) are pinned facts about the
# past. A pinned sha ages out of every window it is looked up through, and
# picking "the newest non-push-head on main" from the activity log finds nothing
# on the (normal) day no stack merge happened in the last month. A constructed
# commit is available on every run, forever. It is deterministic per <base>: the
# same base always yields the same sha, so a log line can be reproduced locally.
# Its tree IS base's tree, so the verifier reads real Chart.yaml versions and
# asks the registry for real, correctly shaped coordinates — only the sha is one
# that was never published.
#
# WHAT "RED FOR THE RIGHT REASON" MEANS — every one of these must hold:
#
#   1. the verifier exits EXACTLY 1 (missing). 0 is a vacuous gate; 2 is
#      "inconclusive" (registry unreadable, bad commit) — a control that went
#      red because GHCR was down proves nothing about detection, so it is
#      reported as inconclusive (exit 2), not as a passing control.
#   2. it printed exactly <expected> MISSING lines — 4 charts + 8 images + 8
#      signatures, derived from scripts/ghcr-lib.sh, not typed here — and no
#      `ok` line. A partial red would mean part of the gate can no longer fail.
#   3. EVERY MISSING line names the control sha. A MISSING line about some other
#      commit is a red for the wrong reason.
#   4. every chart and image absence carries a witness: `witness <tag>: 200`,
#      a tag the same repository lists, looked up the same way, answering
#      present (#985 — the verifier looks tags up by name and no longer scans
#      a listing). A 404 with no witness could be an unreadable repository or
#      a lookup that 404s everything — exactly the failure that makes a real
#      gate report a present artifact as missing.
#   5. the summary line reads `FAIL: <expected> of <expected> artifact(s)
#      missing across 1 commit(s)`.
#
# USAGE
#
#   scripts/publish-verify-control.sh [<base-commit-ish>]
#
# VERIFIER overrides the verifier path; it exists for
# scripts/check-publish-verify-control.sh, which runs this script against the
# real verifier on a fake registry and against deliberately wrong verifiers.
#
# EXIT CODES
#   0  the verifier went red, for the right reason: the gate can fail
#   1  it did NOT — the gate is vacuous or partially blind; do not trust a green
#   2  inconclusive (verifier exit 2, no base commit)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ghcr-lib.sh
. "${here}/ghcr-lib.sh"
verifier="${VERIFIER:-${here}/verify-published-artifacts.sh}"

base="${1:-}"
if [ -z "$base" ]; then
	for candidate in origin/main main HEAD; do
		if git rev-parse --verify --quiet "${candidate}^{commit}" >/dev/null; then
			base="$candidate"
			break
		fi
	done
fi
if [ -z "$base" ] || ! base_sha="$(git rev-parse --verify --quiet "${base}^{commit}")"; then
	echo "::error::expected-red control: no base commit to build the control on (${base:-none})" >&2
	exit 2
fi

# Fixed identity and dates: the sha is a pure function of the base.
control="$(
	GIT_AUTHOR_NAME="publish-verify control" \
		GIT_AUTHOR_EMAIL="publish-verify-control@invalid" \
		GIT_AUTHOR_DATE="2026-09-27T00:00:00Z" \
		GIT_COMMITTER_NAME="publish-verify control" \
		GIT_COMMITTER_EMAIL="publish-verify-control@invalid" \
		GIT_COMMITTER_DATE="2026-09-27T00:00:00Z" \
		git commit-tree "${base_sha}^{tree}" -p "$base_sha" \
		-m "publish-verify expected-red control (#930): never pushed, never published"
)"

expected=$((${#GHCR_CHARTS[@]} + 2 * ${#GHCR_IMAGE_REPOS[@]}))

echo "expected-red control: commit ${control}"
echo "  built from ${base_sha} (${base}); never pushed, so nothing can have published it"
echo "  expecting: exit 1, ${expected} MISSING lines naming ${control}, every absence witnessed"

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

# The control's red must not land in the job summary as a publish failure, and
# nothing it prints may be mistaken for the gate's own verify.log.
rc=0
env -u GITHUB_STEP_SUMMARY "$verifier" "$control" >"$log" 2>&1 || rc=$?
sed 's/^/  | /' "$log"

bad=0
fail() {
	echo "::error::expected-red control: $*" >&2
	bad=1
}

case "$rc" in
1) ;;
0)
	fail "the verifier PASSED a commit that was never published — the gate is vacuous"
	;;
2)
	echo "::error::expected-red control: INCONCLUSIVE — the verifier could not complete (exit 2); a red caused by an unreadable registry proves nothing about detection" >&2
	exit 2
	;;
*)
	fail "the verifier exited ${rc}; only 1 (missing) is the expected red"
	;;
esac

missing_lines="$(grep -E '^[[:space:]]*MISSING ' "$log" || true)"
n_missing="$(printf '%s\n' "$missing_lines" | grep -c . || true)"
if [ "$n_missing" -ne "$expected" ]; then
	fail "${n_missing} MISSING line(s), expected ${expected} — part of the gate cannot fail"
fi
if printf '%s\n' "$missing_lines" | grep . | grep -qvF -- "$control"; then
	fail "a MISSING line does not name the control sha — red for the wrong reason:"
	printf '%s\n' "$missing_lines" | grep . | grep -vF -- "$control" | head -3 >&2
fi
if grep -qE '^[[:space:]]*ok[[:space:]]' "$log"; then
	fail "the verifier reported an artifact PRESENT for a commit that was never published:"
	grep -E '^[[:space:]]*ok[[:space:]]' "$log" | head -3 >&2
fi
# One witness per chart and per image absence (a "no image to sign" line rides
# on its image's). Fewer means some absence was never shown to be one: the
# lookup behind it was never seen to answer "present" in that repository.
n_witness="$(printf '%s\n' "$missing_lines" | grep -cE '; witness [^ ]+: 200\)$' || true)"
want_witness=$((${#GHCR_CHARTS[@]} + ${#GHCR_IMAGE_REPOS[@]}))
if [ "$n_witness" -ne "$want_witness" ]; then
	fail "${n_witness} witnessed absence(s), expected ${want_witness} — cannot tell a real absence from an unread registry or a lookup that 404s everything"
fi
if ! grep -qxF "FAIL: ${expected} of ${expected} artifact(s) missing across 1 commit(s)" "$log"; then
	fail "no 'FAIL: ${expected} of ${expected} artifact(s) missing across 1 commit(s)' summary line"
fi

if [ "$bad" -ne 0 ]; then
	echo "expected-red control: FAILED — do not trust a green publish-verify until this is fixed"
	exit 1
fi
echo "expected-red control: OK — verifier exited 1 with ${n_missing}/${expected} MISSING, all naming ${control}"
