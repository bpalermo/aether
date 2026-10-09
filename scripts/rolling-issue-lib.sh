#!/usr/bin/env bash
# The rolling issue a workflow keeps: found, opened, commented on and closed in
# one way (#1532, #1568). Sourced, never run:
#
#   . "$(dirname "${BASH_SOURCE[0]}")/rolling-issue-lib.sh"
#
# Used by scripts/stuck-runs.sh, scripts/watcher-runs.sh,
# scripts/publish-verify-control-issue.sh, scripts/publish-verify-close-issue.sh
# and scripts/e2e-report-failure.sh. Each used to find its issue with a title
# search (`gh issue list --search 'in:title ...'`). Three things were wrong
# with that, and this file is the one place they are put right:
#
#   WHOSE ISSUE. The repository is public: anyone can open an issue under one
#   of these titles, and anyone can comment on the workflow's own. A search by
#   title handed that issue to a job that then commented on it, read hidden
#   markers from it and closed it. Only an issue opened by the workflow token
#   (`github-actions[bot]`, an account of type `Bot`) is the rolling issue, and
#   only what that account wrote on it is read back (rolling_issue_text). An
#   issue someone else opened under the title is theirs: it is not written to,
#   and it does not stop the workflow opening its own.
#
#   WHEN IT IS FOUND. The search index lags a create by a few seconds, so two
#   failures close together each found nothing and each opened an issue. The
#   issues are LISTED (`GET /repos/{repo}/issues`, the bot's, paginated), which
#   has no index to wait for. Two runs can still both find nothing in the same
#   moment and both create: after a create the list is read again, and the run
#   that finds an older issue moves its report there and closes its own
#   (rolling_issue_create).
#
#   WHAT IT IS LABELLED. An issue is opened with its kind and area labels, as
#   AGENTS.md asks of every issue. A label that is gone must cost neither the
#   report nor the knowledge that it is gone: the issue is then opened without
#   labels and rolling_issue_create returns 3, which every caller turns into a
#   failed step AFTER the report is filed.
#
# Everything goes through `gh api` (REST): GH_TOKEN with `issues: write`, and
# GH_REPO naming the repository. The functions print what they find on stdout
# and say what went wrong on stderr; none of them exits.
#
# Tests: every caller's test runs these functions through scripts/fake-gh-issues.sh,
# which keeps the issues as JSON and applies the `--jq` filters below with the
# real jq, so who counts as the author is decided by this file's own filter.
# shellcheck shell=bash

# The account the workflow token writes as, and how a listing is asked for it.
ROLLING_ISSUE_BOT="github-actions[bot]"
ROLLING_ISSUE_BOT_QUERY="github-actions%5Bbot%5D"

_rolling_issue_repo() {
	[ -n "${GH_REPO:-}" ] || {
		echo "rolling-issue: GH_REPO must name the repository (owner/repo)" >&2
		return 1
	}
	printf '%s\n' "$GH_REPO"
}

# rolling_issue_list <open|closed|all> <title>
# The numbers of the workflow's own issues with exactly this title, oldest
# first. Prints nothing when there is none. Non-zero when the list could not be
# read: a caller must not take that for "none".
rolling_issue_list() {
	local state="$1" title="$2" repo out
	repo="$(_rolling_issue_repo)" || return 1
	case "$title" in
	*'"'* | *\\* | "")
		echo "rolling-issue: a title must be non-empty and hold no quote or backslash: '$title'" >&2
		return 1
		;;
	esac
	# `creator=` narrows the listing; the select is what decides. A pull request
	# is an issue to this endpoint, hence the first select.
	out="$(gh api --paginate "repos/${repo}/issues?state=${state}&creator=${ROLLING_ISSUE_BOT_QUERY}&per_page=100" \
		--jq ".[] | select(.pull_request == null) | select(.user.login == \"${ROLLING_ISSUE_BOT}\" and .user.type == \"Bot\") | select(.title == \"${title}\") | .number")" ||
		return 1
	[ -n "$out" ] || return 0
	sort -n <<<"$out"
}

# rolling_issue_text <number>
# What the workflow wrote on the issue: its body, then each comment of the
# bot's, oldest first. A comment by anyone else is not printed, so a marker a
# stranger pasted is never read. Non-zero when the issue could not be read.
rolling_issue_text() {
	local repo
	repo="$(_rolling_issue_repo)" || return 1
	gh api "repos/${repo}/issues/$1" --jq '.body // ""' || return 1
	gh api --paginate "repos/${repo}/issues/$1/comments?per_page=100" \
		--jq ".[] | select(.user.login == \"${ROLLING_ISSUE_BOT}\" and .user.type == \"Bot\") | .body"
}

# rolling_issue_comment <number> <body>
rolling_issue_comment() {
	local repo
	repo="$(_rolling_issue_repo)" || return 1
	gh api -X POST "repos/${repo}/issues/$1/comments" -f "body=$2" --jq '.id // empty' >/dev/null
}

# rolling_issue_close <number> [completed|not_planned]
rolling_issue_close() {
	local repo
	repo="$(_rolling_issue_repo)" || return 1
	gh api -X PATCH "repos/${repo}/issues/$1" -f state=closed -f "state_reason=${2:-completed}" \
		--jq '.id // empty' >/dev/null
}

# rolling_issue_create <title> <body> [label...]
# Opens the issue and prints the number of the issue the report is on: the new
# one, or an older one that another run opened in the same moment (the report
# is then a comment there, and the new issue is closed as a duplicate).
#   0  opened (or folded into the older one), with every label
#   1  could not be opened: nothing was filed
#   3  filed, but WITHOUT one of its labels (it does not exist any more, or the
#      token may not set it). The caller must fail its step: the report is
#      there, and an issue nobody triages must not look like a clean run.
rolling_issue_create() {
	local title="$1" body="$2" repo answer created label missing="" args=() rc=0
	shift 2
	repo="$(_rolling_issue_repo)" || return 1
	for label in "$@"; do args+=(-f "labels[]=${label}"); done
	# number, then the labels the issue really has: GitHub may refuse an unknown
	# label or drop it without a word, and both must be noticed.
	if ! answer="$(gh api -X POST "repos/${repo}/issues" -f "title=${title}" -f "body=${body}" "${args[@]}" \
		--jq '[.number, (.labels // [] | map(.name) | join(","))] | @tsv')"; then
		[ "$#" -gt 0 ] || return 1
		echo "::warning title=rolling-issue::could not open \"${title}\" with the labels $* (is one gone?); opening it without labels" >&2
		answer="$(gh api -X POST "repos/${repo}/issues" -f "title=${title}" -f "body=${body}" \
			--jq '[.number, ""] | @tsv')" || return 1
	fi
	created="${answer%%$'\t'*}"
	[[ "$created" =~ ^[0-9]+$ ]] || {
		echo "rolling-issue: \"${title}\" was opened, but its number was not returned" >&2
		return 1
	}
	for label in "$@"; do
		case ",${answer#*$'\t'}," in
		*",${label},"*) ;;
		*) missing+="${missing:+, }${label}" ;;
		esac
	done
	if [ -n "$missing" ]; then
		echo "::error title=rolling-issue::#${created} \"${title}\" was opened without the label(s) ${missing}: create the label, or change the script that names it" >&2
		rc=3
	fi

	# Another run may have opened one in the same moment. The older issue wins.
	local numbers oldest
	if ! numbers="$(rolling_issue_list open "$title")"; then
		echo "::warning title=rolling-issue::could not list the issues again after opening #${created}; if two were opened at once, both stay open" >&2
		printf '%s\n' "$created"
		return "$rc"
	fi
	oldest="$(sed -n 1p <<<"$numbers")"
	if [ -n "$oldest" ] && [ "$oldest" != "$created" ]; then
		if rolling_issue_comment "$oldest" "$body"; then
			rolling_issue_comment "$created" "Opened in the same moment as #${oldest}, which is older: the report is there." || true
			rolling_issue_close "$created" not_planned ||
				echo "::warning title=rolling-issue::could not close #${created}, a duplicate of #${oldest}" >&2
			printf '%s\n' "$oldest"
			return "$rc"
		fi
		echo "::warning title=rolling-issue::could not move the report from #${created} to the older #${oldest}; both stay open" >&2
	fi
	printf '%s\n' "$created"
	return "$rc"
}

# rolling_issue_report <title> <body> [label...]
# The whole of a report that needs no memory: a comment on the workflow's open
# issue with this title (the oldest, if there are several), or a new issue.
# Says which on stdout. Returns as rolling_issue_create does.
rolling_issue_report() {
	local title="$1" body="$2" numbers num rc=0
	shift 2
	numbers="$(rolling_issue_list open "$title")" || {
		echo "rolling-issue: could not list the open issues; nothing was filed" >&2
		return 1
	}
	num="$(sed -n 1p <<<"$numbers")"
	if [ -n "$num" ]; then
		rolling_issue_comment "$num" "$body" || {
			echo "rolling-issue: could not comment on #${num}" >&2
			return 1
		}
		echo "commented on #${num}"
		return 0
	fi
	num="$(rolling_issue_create "$title" "$body" "$@")" || rc=$?
	[ "$rc" -ne 1 ] || {
		echo "rolling-issue: could not open \"${title}\"" >&2
		return 1
	}
	echo "opened #${num}"
	return "$rc"
}
