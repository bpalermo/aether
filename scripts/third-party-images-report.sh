#!/usr/bin/env bash
# The scheduled half of the third-party image pins (#1478): run
# `scripts/third-party-images.sh outdated` and keep ONE rolling issue that lists
# the pins whose tag now points at another digest.
#
# `outdated` tells which pins the registries have moved past, but only when
# someone runs it, and nothing moves a pin by itself (Dependabot opens no pull
# requests here, there is no Renovate). .github/workflows/third-party-images.yaml
# runs this once a day. A pin that is behind still pulls the digest it names, so
# nothing is broken and no check fails: the issue is how it becomes visible.
#
# Usage:
#   scripts/third-party-images-report.sh [--dry-run]
#
# What a run does, from what `outdated` said:
#
#   every pin current         an open issue gets a closing comment and is closed
#   a pin behind (MOVED,      no open issue: one is opened. An open issue: its
#   NOT-MULTI-ARCH)           body is rewritten, so it always says where the
#                             tags point NOW, and it gets a comment only when
#                             the SET of pins behind changed (a hidden marker in
#                             the body holds the set; a tag that moves again
#                             under a pin already listed is no news)
#   a pin could not be        a registry did not answer. Such a run does not
#   checked (ERROR)           know the whole set, so it never closes and never
#                             rewrites an open issue; with none open, the pins
#                             it did find behind open one. It is a warning on
#                             the run and exit 0: a registry having a bad
#                             minute must not turn a check red.
#
# The rolling issue is the shape scripts/stuck-runs.sh uses: found by exact
# title among the open issues, reused, closed when there is nothing to report.
# Two things on top of it. Only an issue the workflow's own token opened
# (author `app/github-actions`) is the rolling issue: the repository is public,
# anyone can open an issue under this title, and this job would otherwise
# rewrite, comment on and close a stranger's issue. And the issue is opened with
# its kind and area labels (ISSUE_LABELS); a label that no longer exists fails
# the run instead of filing an issue nobody triages.
#
# --dry-run runs `outdated` for real and writes nothing: it prints the issue it
# would write and calls `gh` not at all (a pull request's run has no
# `issues: write`).
#
# Exit 0 whether or not a pin is behind or a registry failed (the issue and the
# run's warnings are the report). Exit 2 when the check itself is broken:
# `outdated` died without its summary line, or its exit status, its lines and
# its summary disagree, or `gh` failed. A check that cannot tell must not look
# like "every pin is current".
#
# Environment:
#   GH_TOKEN, GH_REPO   for `gh` (issues: write); not read in a dry run
#   RUN_URL             the workflow run, for the issue text
#   THIRD_PARTY_IMAGES  the script whose `outdated` is run (default: the one
#                       next to this file); the test's seam
#
# Tests: scripts/third_party_images_report_test.sh
# (//scripts:third_party_images_report_test), a fake `outdated` and a fake `gh`.
set -euo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
IMAGES="${THIRD_PARTY_IMAGES:-$HERE/third-party-images.sh}"
ISSUE_TITLE="CI: third-party image pins are behind their tags"
# How `gh` spells the author of an issue the workflow token opened.
ISSUE_AUTHOR="app/github-actions"
ISSUE_LABELS=(enhancement ci)
MARKER_PREFIX="<!-- third-party-images:"

die() {
	echo "::error::third-party-images-report: $*" >&2
	exit 2
}

dry=0
case "${1:-}" in
--dry-run) dry=1 ;;
"") ;;
*) die "unknown argument '$1' (want --dry-run or nothing)" ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

rc=0
"$IMAGES" outdated >"$tmp/out" || rc=$?
cat "$tmp/out"

# The lines that say a pin is behind, the lines that say one could not be
# checked, and the summary `outdated` ends with.
grep -E '^(MOVED|NOT-MULTI-ARCH) ' "$tmp/out" >"$tmp/behind" || true
grep -E '^ERROR ' "$tmp/out" >"$tmp/errors" || true
summary="$(grep -Ex '[0-9]+ checked: [0-9]+ behind, [0-9]+ could not be checked\.' "$tmp/out" | tail -n 1 || true)"
nbehind="$(wc -l <"$tmp/behind" | tr -d ' ')"
nerrors="$(wc -l <"$tmp/errors" | tr -d ' ')"

# Three accounts of one run (exit status, lines, summary) have to agree before
# any of them is believed: an `outdated` that died early (no jq, a broken
# inventory, exit 2 like a registry error) prints no summary, and a report built
# from half an output would close an issue that should stay open.
[ -n "$summary" ] || die "'outdated' exited $rc without its summary line: the check itself is broken, nothing was reported"
read -r _ _ said_behind _ said_errors _ <<<"$summary"
[ "$said_behind" = "$nbehind" ] && [ "$said_errors" = "$nerrors" ] ||
	die "'outdated' says '$summary' but printed $nbehind line(s) for a pin behind and $nerrors ERROR line(s)"
want_rc=0
[ "$nbehind" -eq 0 ] || want_rc=1
[ "$nerrors" -eq 0 ] || want_rc=2
[ "$rc" -eq "$want_rc" ] || die "'outdated' exited $rc, but '$summary' means exit $want_rc"

# The set of pins behind: what each is and why, not where its tag points today.
marker="${MARKER_PREFIX} $(awk '{ print $1, $2 }' "$tmp/behind" | LC_ALL=C sort | sha256sum | awk '{ print $1 }') -->"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo '### Third-party image pins'
		echo
		echo '```'
		cat "$tmp/out"
		echo '```'
	} >>"$GITHUB_STEP_SUMMARY"
fi
while IFS= read -r line; do
	echo "::warning title=third-party image pin behind::${line}"
done <"$tmp/behind"
while IFS= read -r line; do
	echo "::warning title=third-party image pin not checked::${line}"
done <"$tmp/errors"

if [ "$nbehind" -gt 0 ]; then
	{
		echo "\`scripts/third-party-images.sh outdated\` found ${nbehind} pinned third-party image(s) behind what the registry serves under the pinned tag:"
		echo
		echo '```'
		cat "$tmp/behind"
		echo '```'
		echo
		if [ "$nerrors" -gt 0 ]; then
			echo "${nerrors} pin(s) could not be checked in this run and are not counted either way:"
			echo
			echo '```'
			cat "$tmp/errors"
			echo '```'
			echo
		fi
		echo "Nothing is broken: a pin pulls the digest it names. \`MOVED\` means the tag was pushed again (a rebuilt base, a patched image), so the pinned digest is no longer what the tag stands for. To move a pin: \`docs/runbook.md\`, \"Refreshing third-party image pins\"."
		echo
		echo "Check run: ${RUN_URL:-n/a}"
		echo
		echo "_Filed automatically by \`third-party-images\` (.github/workflows/third-party-images.yaml). This issue is rewritten while a pin stays behind, gets a comment when the set of pins behind changes, and is closed once every pin is current._"
		echo
		echo "$marker"
	} >"$tmp/body.md"
fi

if [ "$dry" -eq 1 ]; then
	if [ "$nbehind" -gt 0 ]; then
		echo "DRY RUN: would open or update the issue \"${ISSUE_TITLE}\" with:"
		sed 's/^/  | /' "$tmp/body.md"
	elif [ "$nerrors" -gt 0 ]; then
		echo "DRY RUN: ${nerrors} pin(s) could not be checked; would leave an open \"${ISSUE_TITLE}\" issue as it is"
	else
		echo "DRY RUN: every pin is current; would close an open \"${ISSUE_TITLE}\" issue if there is one"
	fi
	exit 0
fi

: "${GH_REPO:?GH_REPO must name the repository (owner/repo)}"

open_issue() {
	local args=() label
	for label in "${ISSUE_LABELS[@]}"; do args+=(--label "$label"); done
	gh issue create --title "$ISSUE_TITLE" --body-file "$tmp/body.md" "${args[@]}" || die "could not open the issue"
}

# Exact-title match (the search is fuzzy, the select is not), and only an issue
# this workflow opened: one a user filed under the same title is theirs.
num="$(gh issue list --state open --limit 100 --search "in:title \"${ISSUE_TITLE}\"" \
	--json number,title,author \
	--jq "[.[] | select(.title == \"${ISSUE_TITLE}\" and .author.is_bot == true and .author.login == \"${ISSUE_AUTHOR}\")] | .[0].number // empty")" ||
	die "could not list the open issues"

if [ "$nerrors" -gt 0 ]; then
	# An incomplete run: what it found behind is real, what it did not reach is
	# unknown. It may open the issue; it may not close or rewrite one.
	if [ "$nbehind" -gt 0 ] && [ -z "$num" ]; then
		open_issue
	elif [ -n "$num" ]; then
		echo "#${num} left as it is: ${nerrors} pin(s) could not be checked, so this run does not know the whole set"
	fi
	exit 0
fi

if [ "$nbehind" -eq 0 ]; then
	if [ -n "$num" ]; then
		gh issue comment "$num" --body "Every pin is current again. Closing; the next pin that falls behind opens a new issue. ${RUN_URL:-}" ||
			die "could not comment on #${num}"
		gh issue close "$num" || die "could not close #${num}"
		echo "closed #${num}"
	fi
	exit 0
fi

if [ -z "$num" ]; then
	open_issue
	exit 0
fi

# The body is rewritten on every run, so its marker is the set last reported.
# The comment goes first: if the rewrite then fails, the next run comments
# again, which is louder than a changed set nobody was told about.
last="$(gh issue view "$num" --json body --jq .body)" || die "could not read #${num}"
if ! grep -qF -- "$marker" <<<"$last"; then
	gh issue comment "$num" --body-file "$tmp/body.md" || die "could not comment on #${num}"
	echo "commented on #${num}: the set of pins behind changed"
fi
gh issue edit "$num" --body-file "$tmp/body.md" || die "could not rewrite #${num}"
echo "rewrote #${num}"
