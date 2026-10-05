#!/usr/bin/env bash
# Which commits on `main` were the HEAD of a push (#975).
#
# This file is SOURCED, never executed: `. scripts/push-heads-lib.sh`. It is
# sourced by scripts/verify-published-artifacts.sh (the `--recent` sweep) and by
# scripts/check-push-head-selection.sh (its unit check).
#
# WHY THE SWEEP NEEDS THIS
#
# `.github/workflows/publish.yaml` runs once per PUSH to main, for the push's
# head commit — not once per commit. A push normally carries one commit, so the
# two used to be indistinguishable, and the `--recent` sweep in
# verify-published-artifacts.sh checked every commit on main. An atomic stack
# merge breaks that: gh-stack #963 landed #959, #962, #964, #965 and #966 as
# five squash commits in ONE push whose head was 4930733. publish ran for
# 4930733 and published it completely; d74d77a, 690dbb5, 937a8fd and ef44437
# were never the tree of any publish run, so nothing could have published them,
# nobody can pin a deploy to them, and the sweep filed #975 about them anyway.
#
# So the sweep checks exactly the commits publish was obliged to publish: the
# push heads. Everything else in the window is printed as skipped, by name, so
# the narrowing is visible in every run's log rather than silent.
#
# WHERE THE PUSH HEADS COME FROM, AND WHY NOT `gh run list`
#
# GitHub's repository activity log (GET /repos/{repo}/activity), which records
# every update of refs/heads/main with its before/after sha. The obvious
# alternative — the head shas of `publish` workflow runs — would narrow the gate
# to "commits a publish run exists for", and "a run that never existed" is one
# of the three failures the verifier exists to catch (#880). The activity log
# records the PUSH, independently of whether any workflow reacted to it. A push
# with no publish run at all is still a head here, so it is still checked and
# still goes red. (A push whose publish run was SUPERSEDED — #880's f332061, run
# `cancelled` at queue time by a newer merge — is a head too; since #1282 it is
# skipped with a notice by select_unsuperseded below, because the newer publish
# carries its change. Main's own head is never skipped.)
#
# Both functions fail closed: an unreadable or empty activity log is an
# inconclusive check (the caller exits 2), never "nothing to check".

# Print the full sha of every push head on refs/heads/main, one per line.
#
# The repository is $GITHUB_REPOSITORY (set in Actions), else bpalermo/aether.
# PUSH_HEADS_PERIOD bounds how far back the log is read (day|week|month|quarter
# |year); `month` comfortably covers the sweep's 24h window and its "newest push
# head older than the grace period" fallback on a quiet week.
#
# PUSH_HEADS_FILE, when set, is read INSTEAD of the API: one sha per line. It
# exists so the selection can be exercised offline against a fabricated list —
# including a list naming a head whose artifacts are missing, which is how the
# gate is shown to still go red.
github_push_heads() {
	local repo="${GITHUB_REPOSITORY:-bpalermo/aether}"
	if [ -n "${PUSH_HEADS_FILE:-}" ]; then
		cat -- "$PUSH_HEADS_FILE"
		return
	fi
	# Branch deletion's `after` is the zero sha; nothing else is excluded — a
	# force push or a merge-queue merge leaves a head publish ran for, same as a
	# plain push or a PR merge.
	gh api --paginate \
		"/repos/${repo}/activity?ref=refs/heads/main&per_page=100&time_period=${PUSH_HEADS_PERIOD:-month}" \
		-q '.[] | select(.activity_type != "branch_deletion") | .after'
}

# Classify candidate commits against a push-head list.
#
#   select_push_heads <heads-file>   < candidate shas, one per line
#
# prints, in input order, one line per candidate:
#   check <sha>    — a push head: its artifacts MUST exist
#   skip <sha>     — not a push head: publish never ran for it
#
# Pure: no network, no git. Returns 2 (inconclusive) if the head list is empty
# or holds anything but full 40-char lowercase shas — an empty list would
# otherwise classify every candidate as `skip` and pass by checking nothing
# (#853), and an abbreviated sha would silently match nothing.
select_push_heads() {
	local heads="$1" sha
	if ! grep -q . "$heads"; then
		echo "::error::push-head list is empty; refusing to skip every commit" >&2
		return 2
	fi
	if grep -vqxE '[0-9a-f]{40}' "$heads"; then
		echo "::error::push-head list holds a line that is not a full 40-char sha:" >&2
		grep -vxE '[0-9a-f]{40}' "$heads" | head -3 >&2
		return 2
	fi
	while IFS= read -r sha; do
		[ -n "$sha" ] || continue
		if grep -qxF -- "$sha" "$heads"; then
			printf 'check %s\n' "$sha"
		else
			printf 'skip %s\n' "$sha"
		fi
	done
}

# ---------------------------------------------------------------------------
# SUPERSEDED PUSH HEADS (#1282)
#
# A push head whose publish run did not succeed, on a main that has since moved
# past it, was superseded (publish.yaml keeps one pending run per concurrency
# group, so the next merge cancels it) — the newer push's publish carries its
# change. That is the rule the workflow_run path applies
# (scripts/publish-verify-superseded.sh, #1277), and the sweep applies the SAME
# function to every push head rather than restating it: a superseded head is
# printed with a notice and not looked up, everything else is still checked.
# Fail-closed, as there: no publish run on record, an unknown main head or an
# unreadable run list all mean `check`.

_push_heads_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Print `<head sha> <conclusion>` for main's most recent publish runs, newest
# first (conclusion empty while a run has none). One page of 100: a busy day is
# ~40 publishes, and a head older than the page reads as "no run on record",
# which is checked, never skipped.
#
# PUBLISH_RUNS_FILE, when set, is read INSTEAD of the API (same format).
github_publish_conclusions() {
	local repo="${GITHUB_REPOSITORY:-bpalermo/aether}"
	if [ -n "${PUBLISH_RUNS_FILE:-}" ]; then
		cat -- "$PUBLISH_RUNS_FILE"
		return
	fi
	gh api "/repos/${repo}/actions/workflows/publish.yaml/runs?branch=main&per_page=100" \
		-q '.workflow_runs[] | "\(.head_sha) \(.conclusion // "")"'
}

# publish_conclusion <runs-file> <sha>: `success` if ANY publish run for <sha>
# succeeded (a re-run that went green wins over the cancelled attempt), else the
# newest non-empty conclusion, else empty (no finished run on record).
publish_conclusion() {
	awk -v s="$2" '$1 == s { if ($2 == "success") ok = 1; else if (c == "" && $2 != "") c = $2 }
		END { print (ok ? "success" : c) }' "$1"
}

# Classify push heads against the publish runs and main's head.
#
#   select_unsuperseded <runs-file> <main head>   < push-head shas, one per line
#
# prints, in input order:
#   check <sha>                     — verify it
#   superseded <sha> <conclusion>   — publish did not succeed and main has moved
#                                     on: skip it, with a notice
select_unsuperseded() {
	local runs="$1" head="$2" sha conclusion
	while IFS= read -r sha; do
		[ -n "$sha" ] || continue
		conclusion="$(publish_conclusion "$runs" "$sha")"
		if [ "$(bash "${_push_heads_dir}/publish-verify-superseded.sh" decide "$sha" "$conclusion" "$head")" = superseded ]; then
			printf 'superseded %s %s\n' "$sha" "$conclusion"
		else
			printf 'check %s\n' "$sha"
		fi
	done
}
