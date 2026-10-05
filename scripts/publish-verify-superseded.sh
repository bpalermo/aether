#!/usr/bin/env bash
# Is the commit a finished publish run was for SUPERSEDED? (publish-verify's
# workflow_run path)
#
# publish.yaml serialises on one concurrency group and GitHub keeps one pending
# run per group, so a merge that lands while a publish is queued or running
# replaces the earlier pending run, which ends `cancelled` having pushed
# nothing. That commit is not lost: the newer push's publish builds main's tree,
# which contains it. Verifying the superseded commit's own coordinates then goes
# red ("not published: ...") on every such merge — permanent red noise on main
# that hides a real verification failure.
#
# So the commit is SUPERSEDED, and the registry check is skipped with a notice,
# when BOTH hold:
#   - its publish run did not conclude `success`, and
#   - it is no longer the head of main.
# Everything else is verified as before, and red stays red:
#   - publish reported success                  -> verify (missing artifacts or
#                                                  signatures are a real defect)
#   - the commit is still main's head           -> verify (nothing newer will
#                                                  publish it: a cancelled or
#                                                  failed head is a real gap)
#   - no publish conclusion (not workflow_run)  -> verify
#   - main's head unknown, or a malformed sha   -> verify (fail closed)
#
#   scripts/publish-verify-superseded.sh decide <commit> <conclusion> <main head>
#       prints `superseded` or `verify` (pure; the test drives this)
#   scripts/publish-verify-superseded.sh
#       the workflow step: reads TARGET and TRIGGERING_CONCLUSION, resolves
#       main's head with `git ls-remote origin` (MAIN_HEAD overrides), writes
#       skip=true|false to $GITHUB_OUTPUT and, when skipping, a ::notice:: and
#       a job-summary line naming the commit, the run and main's head.
#
# Test: scripts/publish_verify_superseded_test.sh (//scripts:publish_verify_superseded_test).
set -euo pipefail

is_sha() {
	case "$1" in
	*[!0-9a-f]* | "") return 1 ;;
	esac
	[ "${#1}" -eq 40 ]
}

decide() {
	local commit="$1" conclusion="$2" head="$3"
	if [ "$conclusion" = success ] || [ -z "$conclusion" ]; then
		echo verify
	elif ! is_sha "$commit" || ! is_sha "$head"; then
		echo verify
	elif [ "$commit" = "$head" ]; then
		echo verify
	else
		echo superseded
	fi
}

if [ "${1:-}" = decide ]; then
	[ "$#" -eq 4 ] || {
		echo "usage: $0 decide <commit> <conclusion> <main head>" >&2
		exit 2
	}
	decide "$2" "$3" "$4"
	exit 0
fi

target="${TARGET:-}"
conclusion="${TRIGGERING_CONCLUSION:-}"
head="${MAIN_HEAD:-}"
if [ -z "$head" ]; then
	head="$(git ls-remote origin refs/heads/main 2>/dev/null | cut -f1)" || head=""
fi

verdict="$(decide "$target" "$conclusion" "$head")"
echo "commit ${target:-<none>}, publish conclusion ${conclusion:-<none>}, main head ${head:-<unknown>}: ${verdict}"
if [ "$verdict" = superseded ]; then
	msg="${target} was superseded: its publish run ended ${conclusion} and main has moved on to ${head:0:12}, whose publish carries the change. Not verified; this run is not a publish failure. Publish run: ${TRIGGERING_RUN:-n/a}"
	echo "::notice title=superseded commit, not verified::${msg}"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		printf '%s\n' "**Superseded, not verified.** ${msg}" >>"$GITHUB_STEP_SUMMARY"
	fi
	echo "skip=true" >>"${GITHUB_OUTPUT:-/dev/null}"
else
	echo "skip=false" >>"${GITHUB_OUTPUT:-/dev/null}"
fi
