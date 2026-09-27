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
# records the PUSH, independently of whether any workflow reacted to it. A
# superseded publish (#880's f332061: run `cancelled` at queue time) is still a
# push head here, so it is still checked and still goes red.
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
