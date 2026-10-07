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
# publish run can ever have built it, and its sha is in no registry's tag list.
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
#      red because the registry was down proves nothing about detection, so it
#      is reported as inconclusive (exit 2), not as a passing control.
#   2. it printed exactly <expected> MISSING lines — 4 charts + 9 images + 9
#      signatures, derived from scripts/registry-lib.sh AS OF <base>, not typed
#      here — and no
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
#   6. EVERY MISSING line names the registry the control's tree names — the
#      `<host>/<namespace>/` of bazel/registry/registry.bzl as of <base> (proposal
#      040). The verifier reads that file per commit; a red reported against
#      another registry (the old one, after the Quay cut-over) is a red for the
#      wrong reason: the gate would be checking where the commit never
#      published.
#
# AN INCONCLUSIVE VERIFIER IS RUN AGAIN (#1340)
#
# Run 37586580592 filed "the expected-red control did not go red" for one 502
# from the registry's token endpoint: the verifier exited 2, the control said
# inconclusive, and the next five runs were fine. The verifier's lookups retry
# by themselves now (registry__fetch_json for the pull token and the witness
# listing, #1336; `curl --retry` for the tag HEADs), but an outage longer than
# one lookup's few seconds still ends it with exit 2. So a verifier that exits
# 2 is run again, whole: CONTROL_ATTEMPTS runs in all (default 3), waiting
# CONTROL_RETRY_INTERVAL seconds (default 10) and doubling: 10 s, then 20 s.
# ONLY exit 2 is re-run. A verifier that PASSED the control commit (0), or went
# red (1), has answered, and asking again could only hide the answer.
#
# THE VERDICT IS RECORDED for the issue steps of publish-verify.yaml
# (scripts/publish-verify-control-issue.sh, scripts/publish-verify-close-issue.sh):
# a file whose first line is one of
#   ok            red for the right reason (exit 0)
#   green         the verifier exited 0: the gate is vacuous (exit 1)
#   wrong         red, or some other exit, but not for the right reason (exit 1)
#   inconclusive  no verdict could be reached (exit 2)
# followed by the lines this script printed as errors. The path is
# `publish-verify-control-issue.sh result-file`: $CONTROL_RESULT_FILE, else
# $RUNNER_TEMP/publish-verify-control.result; a run by hand outside Actions
# records nothing.
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
#   2  inconclusive (verifier exit 2 on every attempt, no base commit)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/registry-lib.sh
. "${here}/registry-lib.sh"
verifier="${VERIFIER:-${here}/verify-published-artifacts.sh}"

# The verdict file (see THE VERDICT IS RECORDED above). Removed first, so a
# control that dies half-way leaves no verdict rather than an older one.
result_file="$("${here}/publish-verify-control-issue.sh" result-file)"
[ -z "$result_file" ] || rm -f "$result_file"
summary=""
# error <text>: an error annotation, kept for the verdict file too.
error() {
	echo "::error::expected-red control: $*" >&2
	summary="${summary}$*"$'\n'
}
# finish <verdict> <exit code>: record the verdict and leave.
finish() {
	if [ -n "$result_file" ]; then
		{
			printf '%s\n' "$1"
			printf '%s' "$summary"
		} >"$result_file" || echo "::warning::expected-red control: could not record the verdict in ${result_file}"
	fi
	exit "$2"
}

attempts="${CONTROL_ATTEMPTS:-3}"
interval="${CONTROL_RETRY_INTERVAL:-10}"
for knob in "$attempts" "$interval"; do
	if ! [[ "$knob" =~ ^[0-9]+$ ]]; then
		error "CONTROL_ATTEMPTS and CONTROL_RETRY_INTERVAL must be whole numbers (got '${knob}')"
		finish inconclusive 2
	fi
done
[ "$attempts" -ge 1 ] || attempts=1

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
	error "no base commit to build the control on (${base:-none})"
	finish inconclusive 2
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

# The image components the control's tree names (its tree IS base's), counted
# the way the verifier counts them per commit (registry_image_components_at).
n_images="$(registry_image_components_at "$base_sha" | grep -c . || true)"
expected=$((${#REGISTRY_CHARTS[@]} + 2 * n_images))

# The registry the control's tree names (its tree IS base's): where every
# MISSING line must point.
base_bzl="$(mktemp)"
if ! registry_setting_at "$base_sha" "$base_bzl" ||
	! want_prefix="$(IMAGE_REGISTRY_BZL="$base_bzl" "${here}/image-registry.sh" prefix)"; then
	rm -f "$base_bzl"
	error "cannot read the registry setting (${REGISTRY_SETTING_PATHS[*]}) as of ${base_sha}"
	finish inconclusive 2
fi
rm -f "$base_bzl"

echo "expected-red control: commit ${control}"
echo "  built from ${base_sha} (${base}); never pushed, so nothing can have published it"
echo "  expecting: exit 1, ${expected} MISSING lines naming ${control} on ${want_prefix}/, every absence witnessed"

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

# The control's red must not land in the job summary as a publish failure, and
# nothing it prints may be mistaken for the gate's own verify.log.
# PROXY_PIN_CHECK=0: the control asserts EXACTLY the 22 per-commit coordinates;
# the constructed commit pins main's (signed, present) aether-proxy digest,
# which is not what this control is about — see verify-published-artifacts.sh
# step 5 and case 7 of scripts/check-publish-verify-control.sh for the proxy
# pin's own red.
#
# Run again ONLY on exit 2 (see AN INCONCLUSIVE VERIFIER IS RUN AGAIN above).
attempt=1
wait="$interval"
while :; do
	rc=0
	env -u GITHUB_STEP_SUMMARY PROXY_PIN_CHECK=0 "$verifier" "$control" >"$log" 2>&1 || rc=$?
	sed 's/^/  | /' "$log"
	if [ "$rc" -ne 2 ] || [ "$attempt" -ge "$attempts" ]; then
		break
	fi
	echo "expected-red control: the verifier could not complete (exit 2) on attempt ${attempt} of ${attempts}; running it again in ${wait}s"
	sleep "$wait"
	wait=$((wait * 2))
	attempt=$((attempt + 1))
done

bad=0
fail() {
	error "$@"
	bad=1
}

case "$rc" in
1) ;;
0)
	fail "the verifier PASSED a commit that was never published — the gate is vacuous"
	;;
2)
	error "INCONCLUSIVE — the verifier could not complete (exit 2) on any of ${attempt} attempt(s); a red caused by an unreadable registry proves nothing about detection"
	# Why, in the verifier's own words: its errors and the registry library's
	# lines (which name the URL and the status, never a token), for the issue.
	why="$(grep -E '^(::error::|registry-lib: |registry_tag_exists: |registry_referrers: )' "$log" | sed 's/^::error:://' | tail -n 5 || true)"
	[ -z "$why" ] || summary="${summary}the verifier's last attempt said:"$'\n'"${why}"$'\n'
	finish inconclusive 2
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
# Each offending set is collected ONCE, with `|| true` on the whole pipeline,
# and tested with `[ -n ]` (#1046). Not `if producer | grep -q`: under pipefail
# an early-exiting `grep -q` can SIGPIPE its producer and read a match as a
# miss, skipping the fail(). And the first three are shown with `sed -n 1,3p`,
# which reads all of its input, not `head -3`: a producer killed by head's early
# exit fails its pipeline, and `set -e` would end this script mid-report.
wrong_sha="$(printf '%s\n' "$missing_lines" | grep . | grep -vF -- "$control" || true)"
if [ -n "$wrong_sha" ]; then
	fail "a MISSING line does not name the control sha — red for the wrong reason:"
	printf '%s\n' "$wrong_sha" | sed -n 1,3p >&2
fi
wrong_registry="$(printf '%s\n' "$missing_lines" | grep . | grep -vF -- "MISSING ${want_prefix}/" || true)"
if [ -n "$wrong_registry" ]; then
	fail "a MISSING line names a registry other than ${want_prefix}/ (bazel/registry/registry.bzl as of the control's tree) — red for the wrong reason:"
	printf '%s\n' "$wrong_registry" | sed -n 1,3p >&2
fi
if grep -qE '^[[:space:]]*ok[[:space:]]' "$log"; then
	fail "the verifier reported an artifact PRESENT for a commit that was never published:"
	grep -E '^[[:space:]]*ok[[:space:]]' "$log" | sed -n 1,3p >&2
fi
# One witness per chart and per image absence (a "no image to sign" line rides
# on its image's). Fewer means some absence was never shown to be one: the
# lookup behind it was never seen to answer "present" in that repository.
n_witness="$(printf '%s\n' "$missing_lines" | grep -cE '; witness [^ ]+: 200\)$' || true)"
want_witness=$((${#REGISTRY_CHARTS[@]} + n_images))
if [ "$n_witness" -ne "$want_witness" ]; then
	fail "${n_witness} witnessed absence(s), expected ${want_witness} — cannot tell a real absence from an unread registry or a lookup that 404s everything"
fi
if ! grep -qxF "FAIL: ${expected} of ${expected} artifact(s) missing across 1 commit(s)" "$log"; then
	fail "no 'FAIL: ${expected} of ${expected} artifact(s) missing across 1 commit(s)' summary line"
fi

if [ "$bad" -ne 0 ]; then
	echo "expected-red control: FAILED — do not trust a green publish-verify until this is fixed"
	# Two different defects (#1340): a verifier that PASSED the commit is a
	# vacuous gate; one that went red for the wrong reason is partly blind.
	if [ "$rc" -eq 0 ]; then
		finish green 1
	fi
	finish wrong 1
fi
echo "expected-red control: OK — verifier exited 1 with ${n_missing}/${expected} MISSING, all naming ${control}"
finish ok 0
