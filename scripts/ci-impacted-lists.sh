#!/usr/bin/env bash
# What a job of .github/workflows/ci.yaml or main.yaml runs right after it
# downloads the impacted-targets artifact of its `diff` job, before it reads
# any list (#1459, #1460, #1488). It fails unless:
#
#   - impacted_build.txt, impacted_unit.txt, impacted_integration.txt and
#     impacted_commit.txt are all there. A list that is missing is not an empty
#     list: read as one, it turns into "nothing to run" in a job that then
#     passes (`cat ... 2>/dev/null`, `grep -q` on a file that is not there);
#   - impacted_commit.txt, the commit scripts/ci-impacted-targets.sh computed
#     the lists for, is the commit this job has checked out. A label listed on
#     one tree and built on another either does not exist there or misses what
#     only exists there;
#   - each has_* output of `diff` is `true` or `false`, in those words, and
#     agrees with its list: `true` with at least one target, `false` with none.
#
# Usage: ci-impacted-lists.sh <directory the artifact was downloaded to>
# Env:   HAS_ANY, HAS_UNIT, HAS_INTEGRATION   the outputs of the `diff` job
set -uo pipefail

dir="${1:-}"
[ -n "$dir" ] || {
	echo "::error::ci-impacted-lists: usage: ci-impacted-lists.sh <directory>" >&2
	exit 1
}

FAILED=0
err() {
	echo "::error::ci-impacted-lists: $1" >&2
	FAILED=1
}

for f in impacted_build.txt impacted_unit.txt impacted_integration.txt impacted_commit.txt; do
	[ -f "$dir/$f" ] && [ -r "$dir/$f" ] || err "$f is missing from ${dir}: a list that is not there is not an empty list"
done
[ "$FAILED" -eq 0 ] || exit 1

here="$(git rev-parse HEAD 2>/dev/null)" || {
	err "git rev-parse HEAD failed: not a git checkout"
	exit 1
}
commit="$(cat "$dir/impacted_commit.txt")"
if [[ ! "$commit" =~ ^[0-9a-f]{40}$ ]]; then
	err "impacted_commit.txt does not hold one full commit id: '${commit}'"
elif [ "$commit" != "$here" ]; then
	err "the impacted lists were computed for ${commit} and this job checked out ${here}: they are not lists of this tree"
fi

# agree <output name> <value> <list>
agree() {
	local count
	case "$2" in
	true | false) ;;
	*)
		err "$1 is \"$2\", not \"true\" or \"false\""
		return
		;;
	esac
	# grep -c prints the count and exits 1 on zero; above 1 it could not read.
	count="$(grep -cv '^[[:space:]]*$' "$dir/$3")"
	[ "$?" -le 1 ] || {
		err "could not read $3"
		return
	}
	if [ "$2" = true ] && [ "$count" -eq 0 ]; then
		err "$1 is true and $3 has 0 target(s)"
	elif [ "$2" = false ] && [ "$count" -ne 0 ]; then
		err "$1 is false and $3 has ${count} target(s)"
	else
		echo "  $1=$2, $3: ${count} target(s)"
	fi
}
agree has_any "${HAS_ANY:-}" impacted_build.txt
agree has_unit "${HAS_UNIT:-}" impacted_unit.txt
agree has_integration "${HAS_INTEGRATION:-}" impacted_integration.txt

[ "$FAILED" -eq 0 ] || exit 1
echo "ci-impacted-lists: the lists are for ${here}, the commit checked out, and agree with the outputs of diff"
