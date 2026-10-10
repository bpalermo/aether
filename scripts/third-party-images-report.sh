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
#   checked (ERROR)           know the whole set, so it never closes an open
#                             issue and never rewrites what it says is behind;
#                             with none open, the pins it did find behind open
#                             one. It is a warning on the run and exit 0: a
#                             registry having a bad minute must not turn a
#                             check red, and must not open an issue either.
#
# A lookup that fails on EVERY run is another matter (#1570): a repository gone
# private, a tag deleted, a rate limit that never lifts. Nobody reads the
# warning of a green run, so the pin would go unchecked for good. The rolling
# issue therefore also holds, in a hidden line of its body, in how many runs in
# a row each pin's lookup has failed:
#
#   <!-- third-party-images-state: errors@<open|closed> <name>:<tag>=<runs> ... -->
#
# A pin that failed in ERROR_RUNS runs in a row is reported: the issue is opened
# for it (or, when one is open, gains a section and a comment), and stays open
# until a run checks every pin. A pin that answers again is forgotten at once,
# so only an unbroken streak counts. Where the count lives:
#   - in the open rolling issue, when there is one. An incomplete run replaces
#     that hidden line and the section under it, and nothing else of the body;
#   - otherwise in the newest CLOSED rolling issue, whose body is edited and
#     which is not reopened: a streak shorter than ERROR_RUNS notifies nobody;
#   - and when there has never been a rolling issue, one is created and closed
#     at once to hold it. That happens once in the repository's life.
# The count is never a second issue. `@open` / `@closed` says which state of the
# issue the line was written in: a count written while the issue was open and
# found once it is closed predates the complete run that closed it, and is not
# continued.
#
# Newer tags (#1569). The report above is about the tag each pin names: it says
# nothing when 8.23.0 exists next to a pinned 8.22.0. `third-party-images.sh
# newer` lists those, and its answer is a section of the issue for whoever reads
# it. That section never opens the issue, never keeps it open, never closes it
# and never causes a comment (it is no part of the set the marker holds); when
# the tag lists cannot be read the section says so and the run goes on.
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
# --dry-run runs `outdated` and `newer` for real and writes nothing: it prints
# the issue it would write and calls `gh` not at all (a pull request's run has
# no `issues: write`), so it knows no count of earlier runs.
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
#   THIRD_PARTY_IMAGES  the script whose `outdated` and `newer` are run
#                       (default: the one next to this file); the test's seam
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
# The hidden line that holds the failed-lookup counts, and the line that ends
# the part of the body an incomplete run replaces.
STATE_PREFIX="<!-- third-party-images-state: errors@"
STATE_END="<!-- third-party-images-state-end -->"
# In how many runs in a row a pin's lookup must have failed before it is
# reported. The schedule is daily: three days.
ERROR_RUNS=3

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

# Newer tags (#1569): a second question to the registries, and a section of the
# issue. Its answer is believed only when its exit status, its lines and its
# summary agree; otherwise the section says the lists were not read. Nothing
# below decides anything from it.
nrc=0
"$IMAGES" newer >"$tmp/newer" 2>"$tmp/newer.err" || nrc=$?
cat "$tmp/newer"
grep -E '^NEWER ' "$tmp/newer" >"$tmp/newer.lines" || true
grep -E '^ERROR ' "$tmp/newer" >"$tmp/newer.errors" || true
nsummary="$(grep -Ex '[0-9]+ checked: [0-9]+ with newer tags, [0-9]+ could not be listed\.' "$tmp/newer" | tail -n 1 || true)"
nnewer="$(wc -l <"$tmp/newer.lines" | tr -d ' ')"
nunlisted="$(wc -l <"$tmp/newer.errors" | tr -d ' ')"
newer_ok=0
if [ -n "$nsummary" ]; then
	read -r _ _ said_newer _ _ _ said_unlisted _ <<<"$nsummary"
	want_nrc=0
	[ "$nunlisted" -eq 0 ] || want_nrc=2
	if [ "$said_newer" = "$nnewer" ] && [ "$said_unlisted" = "$nunlisted" ] && [ "$nrc" -eq "$want_nrc" ]; then newer_ok=1; fi
fi
if [ "$newer_ok" -eq 0 ]; then
	echo "::warning title=third-party image newer tags not listed::'newer' exited ${nrc}$(head -n 1 "$tmp/newer.err" | sed 's/^/: /')"
fi
{
	if [ "$newer_ok" -eq 0 ]; then
		echo "Newer tags were not listed in this run."
	else
		if [ "$nnewer" -gt 0 ]; then
			echo "${nnewer} pin(s) have newer tags of the same shape as the pinned one (\`scripts/third-party-images.sh newer\`). This section is for the reader: a newer tag never opens, changes or closes this issue."
			echo
			echo '```'
			cat "$tmp/newer.lines"
			echo '```'
		else
			echo "No pin has a newer tag of the same shape as the pinned one."
		fi
		if [ "$nunlisted" -gt 0 ]; then
			echo
			echo "The tag list of ${nunlisted} pin(s) was not read in this run:"
			echo
			echo '```'
			cat "$tmp/newer.errors"
			echo '```'
		fi
	fi
} >"$tmp/newer.md"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo '### Third-party image pins'
		echo
		echo '```'
		cat "$tmp/out"
		echo '```'
		echo
		echo '### Newer tags'
		echo
		cat "$tmp/newer.md"
	} >>"$GITHUB_STEP_SUMMARY"
fi
while IFS= read -r line; do
	echo "::warning title=third-party image pin behind::${line}"
done <"$tmp/behind"
while IFS= read -r line; do
	echo "::warning title=third-party image pin not checked::${line}"
done <"$tmp/errors"

# --- the failed-lookup counts (#1570) ------------------------------------------

# state_of <body file> <open|closed>: prints "<name>:<tag> <runs>" for every
# count the body holds that still stands for an issue in that state. The body
# is text a maintainer can edit: a token that is not a pin and a count is
# dropped, never run and never trusted.
state_of() {
	local line written
	line="$(grep -F -m 1 -- "$STATE_PREFIX" "$1" || true)"
	[ -n "$line" ] || return 0
	line="${line#*"$STATE_PREFIX"}"
	written="${line%% *}"
	# Written while open, found closed: it predates the run that closed it.
	[ "$written" = open ] && [ "$2" = closed ] && return 0
	printf '%s\n' "$line" | tr ' ' '\n' |
		{ grep -E '^[a-z0-9][A-Za-z0-9._/:-]{0,300}=[0-9]{1,6}$' || true; } |
		sed 's/=\([0-9]*\)$/ \1/'
}

# next_state <old state file>: prints "<name>:<tag> <runs> <last answer>" for
# every pin this run could not check: one more than it had, or 1.
next_state() {
	awk '
		FILENAME == ARGV[1] {
			old[$1] = $2
			next
		}
		!($2 in seen) {
			seen[$2] = 1
			why = $0
			sub(/^ERROR[ \t]+[^ \t]+[ \t]*/, "", why)
			gsub(/`/, "", why)
			print $2, old[$2] + 1, why
		}
	' "$1" "$tmp/errors" | LC_ALL=C sort
}

# region <state file> <open|closed>: the part of the body that holds the
# counts, and the section for the pins whose streak reached ERROR_RUNS.
region() {
	local n
	printf '%s%s%s -->\n' "$STATE_PREFIX" "$2" "$(awk '{ printf " %s=%s", $1, $2 }' "$1")"
	n="$(awk -v min="$ERROR_RUNS" '$2 >= min' "$1" | wc -l | tr -d ' ')"
	if [ "$n" -gt 0 ]; then
		echo "${n} pin(s) could not be checked in ${ERROR_RUNS} or more runs in a row: the registry did not answer for the pinned tag (a repository gone private, a tag deleted, a rate limit that does not lift). One failed lookup is only a warning on its run; a lookup that keeps failing means nobody knows whether the pin is current:"
		echo
		echo '```'
		awk -v min="$ERROR_RUNS" '$2 >= min { pin = $1; runs = $2; $1 = $2 = ""; sub(/^ +/, ""); printf "%s  %d runs in a row; last answer: %s\n", pin, runs, $0 }' "$1"
		echo '```'
		echo
	fi
	echo "$STATE_END"
}

# with_region <body file> <region file>: the body with its region replaced, or
# with the region appended when it has none (an issue written before #1570).
with_region() {
	awk -v begin="$STATE_PREFIX" -v end="$STATE_END" -v region="$2" '
		function put(    l) {
			while ((getline l < region) > 0) print l
			close(region)
			done = 1
		}
		skip {
			# A body saved from the web editor ends its lines with CR LF.
			line = $0
			sub(/\r$/, "", line)
			if (line == end) skip = 0
			next
		}
		!done && index($0, begin) == 1 {
			put()
			skip = 1
			next
		}
		{ print }
		END { if (!done) put() }
	' "$1"
}

# reported <state file>: the pins whose streak has reached ERROR_RUNS.
reported() {
	awk -v min="$ERROR_RUNS" '$2 >= min { print $1 }' "$1" | LC_ALL=C sort
}

# body <kind>: the whole issue text. `behind`: a pin is behind. `unreachable`:
# none is known to be, and a lookup keeps failing. `holder`: neither; the issue
# exists to hold the counts.
body() {
	case "$1" in
	behind)
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
		;;
	unreachable)
		# Which pins is the region's to say: an incomplete run rewrites that
		# and not this.
		echo "\`scripts/third-party-images.sh outdated\` found no pinned third-party image behind its tag among the pins it could check."
		echo
		;;
	holder)
		echo "No pinned third-party image is known to be behind its tag, and this issue is closed. It was opened to hold, in a hidden line below, in how many runs in a row a pin's registry lookup has failed: one failed lookup is only a warning on its run, ${ERROR_RUNS} in a row open a new issue under this title (\`scripts/third-party-images-report.sh\`)."
		echo
		;;
	esac
	cat "$tmp/region"
	echo
	if [ "$1" = behind ]; then
		echo "Nothing is broken: a pin pulls the digest it names. \`MOVED\` means the tag was pushed again (a rebuilt base, a patched image), so the pinned digest is no longer what the tag stands for. To move a pin: \`docs/runbook.md\`, \"Refreshing third-party image pins\"."
		echo
	fi
	if [ "$1" != holder ]; then
		cat "$tmp/newer.md"
		echo
	fi
	echo "Check run: ${RUN_URL:-n/a}"
	echo
	echo "_Filed automatically by \`third-party-images\` (.github/workflows/third-party-images.yaml). This issue is rewritten while a pin stays behind, gets a comment when the set of pins behind changes, and is closed once every pin is current._"
	echo
	echo "$marker"
}

if [ "$dry" -eq 1 ]; then
	# No `gh`, so no count of earlier runs: every failed lookup is a first one.
	: >"$tmp/none"
	next_state "$tmp/none" >"$tmp/state"
	region "$tmp/state" open >"$tmp/region"
	if [ "$nbehind" -gt 0 ]; then
		body behind >"$tmp/body.md"
		echo "DRY RUN: would open or update the issue \"${ISSUE_TITLE}\" with:"
		sed 's/^/  | /' "$tmp/body.md"
	else
		echo "DRY RUN: newer tags (a section of the issue while it is open; it never opens or closes it):"
		sed 's/^/  | /' "$tmp/newer.md"
		if [ "$nerrors" -gt 0 ]; then
			echo "DRY RUN: ${nerrors} pin(s) could not be checked; would leave an open \"${ISSUE_TITLE}\" issue as it is, and count the failed lookup (${ERROR_RUNS} runs in a row are reported)"
		else
			echo "DRY RUN: every pin is current; would close an open \"${ISSUE_TITLE}\" issue if there is one"
		fi
	fi
	exit 0
fi

: "${GH_REPO:?GH_REPO must name the repository (owner/repo)}"

open_issue() { # <body file>: sets OPENED to what `gh` printed, the issue's URL
	local args=() label
	for label in "${ISSUE_LABELS[@]}"; do args+=(--label "$label"); done
	OPENED="$(gh issue create --title "$ISSUE_TITLE" --body-file "$1" "${args[@]}")" || die "could not open the issue"
	echo "$OPENED"
}

# find_issue <open|closed>: the newest rolling issue in that state. Exact-title
# match (the search is fuzzy, the select is not), and only an issue this
# workflow opened: one a user filed under the same title is theirs.
find_issue() {
	gh issue list --state "$1" --limit 100 --search "in:title \"${ISSUE_TITLE}\" sort:created-desc" \
		--json number,title,author \
		--jq "[.[] | select(.title == \"${ISSUE_TITLE}\" and .author.is_bot == true and .author.login == \"${ISSUE_AUTHOR}\")] | .[0].number // empty" ||
		die "could not list the $1 issues"
}

read_body() { # <number> <out>
	gh issue view "$1" --json body --jq .body >"$2" || die "could not read #$1"
}

num="$(find_issue open)"

if [ "$nerrors" -gt 0 ]; then
	# An incomplete run: what it found behind is real, what it did not reach is
	# unknown. It may open the issue; it may not close one, and of an open one
	# it rewrites the failed-lookup counts and nothing else.
	holder="$num"
	holder_state=open
	if [ -z "$num" ]; then
		holder="$(find_issue closed)"
		holder_state=closed
	fi
	: >"$tmp/old.md"
	[ -z "$holder" ] || read_body "$holder" "$tmp/old.md"
	state_of "$tmp/old.md" "$holder_state" >"$tmp/old.state"
	next_state "$tmp/old.state" >"$tmp/state"
	reported "$tmp/old.state" >"$tmp/old.reported"
	reported "$tmp/state" >"$tmp/reported"
	nreported="$(wc -l <"$tmp/reported" | tr -d ' ')"

	# An OPEN issue whose count says `@closed` is the issue that holds the
	# count, left open: the run that opened it was cancelled, or failed, before
	# it closed it. Left alone it would hide the streak for good (an open issue
	# only has its count edited), so this run finishes the job: it closes it,
	# or, when there is something to report by now, makes it the report.
	stranded=0
	if [ -n "$num" ] && grep -qF -- "${STATE_PREFIX}closed" "$tmp/old.md"; then stranded=1; fi

	if [ "$stranded" -eq 1 ] && [ "$nbehind" -eq 0 ] && [ "$nreported" -eq 0 ]; then
		region "$tmp/state" closed >"$tmp/region"
		with_region "$tmp/old.md" "$tmp/region" >"$tmp/body.md"
		gh issue edit "$num" --body-file "$tmp/body.md" || die "could not rewrite #${num}"
		gh issue close "$num" || die "could not close #${num}"
		echo "closed #${num}: it holds the count of failed lookups and was left open by an earlier run"
	elif [ "$stranded" -eq 1 ]; then
		region "$tmp/state" open >"$tmp/region"
		if [ "$nbehind" -gt 0 ]; then body behind >"$tmp/body.md"; else body unreachable >"$tmp/body.md"; fi
		gh issue edit "$num" --body-file "$tmp/body.md" || die "could not rewrite #${num}"
		echo "rewrote #${num}: it held the count of failed lookups, and there is something to report now"
	elif [ -n "$num" ]; then
		region "$tmp/state" open >"$tmp/region"
		with_region "$tmp/old.md" "$tmp/region" >"$tmp/body.md"
		# The comment goes first: if the rewrite then fails, the next run
		# comments again, which is louder than a pin nobody was told about.
		if [ -n "$(LC_ALL=C comm -13 "$tmp/old.reported" "$tmp/reported")" ]; then
			gh issue comment "$num" --body-file "$tmp/region" || die "could not comment on #${num}"
			echo "commented on #${num}: a pin could not be checked in ${ERROR_RUNS} runs in a row"
		fi
		gh issue edit "$num" --body-file "$tmp/body.md" || die "could not rewrite #${num}"
		echo "#${num} left as it is: ${nerrors} pin(s) could not be checked, so this run does not know the whole set (only the count of failed lookups was updated)"
	elif [ "$nbehind" -gt 0 ] || [ "$nreported" -gt 0 ]; then
		region "$tmp/state" open >"$tmp/region"
		if [ "$nbehind" -gt 0 ]; then body behind >"$tmp/body.md"; else body unreachable >"$tmp/body.md"; fi
		open_issue "$tmp/body.md"
	elif [ -n "$holder" ]; then
		region "$tmp/state" closed >"$tmp/region"
		with_region "$tmp/old.md" "$tmp/region" >"$tmp/body.md"
		gh issue edit "$holder" --body-file "$tmp/body.md" || die "could not rewrite #${holder}"
		echo "counted the failed lookup(s) in the closed #${holder}; nothing to report before ${ERROR_RUNS} runs in a row"
	else
		# There has never been a rolling issue: one is made, closed, to hold
		# the count. Not left open: one failed lookup is no report.
		region "$tmp/state" closed >"$tmp/region"
		body holder >"$tmp/body.md"
		open_issue "$tmp/body.md"
		# Its number is the end of the URL `gh` printed (a search for it this
		# soon after it was opened may not find it yet).
		holder="${OPENED##*/}"
		[[ "$holder" =~ ^[0-9]+$ ]] || die "an issue was opened to hold the failed-lookup count, but 'gh' did not print its URL ('${OPENED}'): close it by hand"
		gh issue close "$holder" || die "could not close #${holder}"
		echo "opened and closed #${holder} to hold the count of failed lookups"
	fi
	exit 0
fi

# A complete run: every pin answered, so no streak is left.
: >"$tmp/state"
region "$tmp/state" open >"$tmp/region"

if [ "$nbehind" -eq 0 ]; then
	if [ -n "$num" ]; then
		gh issue comment "$num" --body "Every pin is current again. Closing; the next pin that falls behind opens a new issue. ${RUN_URL:-}" ||
			die "could not comment on #${num}"
		gh issue close "$num" || die "could not close #${num}"
		echo "closed #${num}"
		exit 0
	fi
	# No open issue. A closed one may still hold a streak this run has ended.
	holder="$(find_issue closed)"
	[ -n "$holder" ] || exit 0
	read_body "$holder" "$tmp/old.md"
	state_of "$tmp/old.md" closed >"$tmp/old.state"
	[ -s "$tmp/old.state" ] || exit 0
	region "$tmp/state" closed >"$tmp/region"
	with_region "$tmp/old.md" "$tmp/region" >"$tmp/body.md"
	gh issue edit "$holder" --body-file "$tmp/body.md" || die "could not rewrite #${holder}"
	echo "every pin answered: the count of failed lookups in the closed #${holder} is cleared"
	exit 0
fi

body behind >"$tmp/body.md"
if [ -z "$num" ]; then
	open_issue "$tmp/body.md"
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
