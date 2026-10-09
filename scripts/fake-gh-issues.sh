#!/usr/bin/env bash
# A fake `gh` for the tests of the scripts that keep a rolling issue through
# scripts/rolling-issue-lib.sh. Test-only: nothing ships it, no workflow runs
# it. A test puts a `gh` on PATH that execs this file.
#
# It plays the issues half of the REST API, `gh api [--paginate] [-X <method>]
# <path> [-f key=value ...] [--jq <filter>]`, over one JSON file, and applies
# the caller's `--jq` filter with the real jq ($JQ). So which issue counts as
# the workflow's own, and which comment is read back, is decided by the filter
# of the script under test and by nothing here.
#
# The listing ignores `creator=` on purpose: it serves every issue it has. The
# query parameter narrows what GitHub sends; the caller's own select is what
# must hold, and a fake that filtered for it would hide a select that is gone.
#
# State and seams (environment):
#   FAKE_STATE   a directory. issues.json: the issues, as the API returns them
#                (number, title, state, state_reason, body, user{login,type},
#                labels[{name}], comments[{user,body}], and pull_request on a
#                pull request). labels: the labels the repository has, one per
#                line. race.json: an issue that "another run" opens while this
#                one is creating its own (consumed by the next create).
#   FAKE_LOG     every call is appended as `gh <args>`, every write as
#                `WRITE issue create <n> [<labels>]: <title>`,
#                `WRITE issue comment <n>`, `WRITE issue close <n> <reason>`,
#                `WRITE issue reopen <n>`.
#   FAKE_FAIL    words; a call of that kind answers HTTP 502: list, view,
#                comments, create, comment, patch.
#   FAKE_LABELS  how a create answers a label the repository does not have:
#                `reject` (HTTP 422, the default) or `drop` (the issue is
#                opened without it, and nothing says so).
#   FAKE_CLOSED_MEANWHILE  a comment lands on an issue that somebody closes in
#                that same moment: after the comment is stored, the issue is
#                closed (GitHub accepts a comment on a closed issue).
#   FAKE_GH_ELSE a program that gets every call this file does not know (the
#                actions API of a test that needs one); without it, such a call
#                fails.
# shellcheck disable=SC2016 # single-quoted $names here are jq variables, never shell expansions.
set -uo pipefail

: "${FAKE_STATE:?}" "${FAKE_LOG:?}" "${JQ:?}"
BOT='{"login":"github-actions[bot]","type":"Bot"}'
DB="$FAKE_STATE/issues.json"
[ -f "$DB" ] || echo '[]' >"$DB"

echo "gh $*" >>"$FAKE_LOG"

fails() {
	case " ${FAKE_FAIL:-} " in *" $1 "*)
		echo "gh: Bad Gateway (HTTP 502)" >&2
		exit 1
		;;
	esac
}
update() { # jq arguments...: rewrite the database
	"$JQ" "$@" "$DB" >"$DB.next" && mv "$DB.next" "$DB"
}
orig=("$@")
elsewhere() {
	[ -n "${FAKE_GH_ELSE:-}" ] || {
		echo "fake gh: unexpected ${orig[*]}" >&2
		exit 1
	}
	exec "$FAKE_GH_ELSE" "${orig[@]}"
}

[ "${1:-}" = api ] || elsewhere
shift
method=GET path="" filter="." title="" body="" state="" reason="" labels=()
while [ $# -gt 0 ]; do
	case "$1" in
	--paginate) ;;
	-X)
		method="$2"
		shift
		;;
	--jq)
		filter="$2"
		shift
		;;
	-f)
		case "$2" in
		title=*) title="${2#title=}" ;;
		body=*) body="${2#body=}" ;;
		state=*) state="${2#state=}" ;;
		state_reason=*) reason="${2#state_reason=}" ;;
		"labels[]="*) labels+=("${2#labels\[\]=}") ;;
		*)
			echo "fake gh: unexpected field $2" >&2
			exit 1
			;;
		esac
		shift
		;;
	-*)
		echo "fake gh: unexpected flag $1" >&2
		exit 1
		;;
	*) path="$1" ;;
	esac
	shift
done

case "$method $path" in
"GET repos/"*"/issues?"*)
	fails list
	want="${path#*state=}"
	want="${want%%&*}"
	"$JQ" --arg s "$want" '[.[] | select($s == "all" or .state == $s) | del(.comments)]' "$DB" | "$JQ" -r "$filter"
	;;
"GET repos/"*"/issues/"*"/comments"*)
	fails comments
	n="${path#*/issues/}"
	n="${n%%/*}"
	"$JQ" --argjson n "$n" '[.[] | select(.number == $n) | .comments[]?]' "$DB" | "$JQ" -r "$filter"
	;;
"GET repos/"*"/issues/"*)
	fails view
	n="${path#*/issues/}"
	"$JQ" -e --argjson n "$n" '.[] | select(.number == $n) | del(.comments)' "$DB" >"$FAKE_STATE/one.json" || {
		echo "gh: Not Found (HTTP 404)" >&2
		exit 1
	}
	"$JQ" -r "$filter" "$FAKE_STATE/one.json"
	;;
"POST repos/"*"/issues")
	fails create
	have=()
	for l in "${labels[@]+"${labels[@]}"}"; do
		if grep -qxF -- "$l" "$FAKE_STATE/labels" 2>/dev/null; then
			have+=("$l")
		elif [ "${FAKE_LABELS:-reject}" = reject ]; then
			echo "gh: Validation Failed (HTTP 422)" >&2
			exit 1
		fi
	done
	n="$("$JQ" '([.[].number] | max // 6) + 1' "$DB")"
	lj="$(printf '%s\n' "${have[@]+"${have[@]}"}" | "$JQ" -R . | "$JQ" -s 'map(select(. != "") | {name: .})')"
	update --argjson n "$n" --arg t "$title" --arg b "$body" --argjson u "$BOT" --argjson l "$lj" \
		'. + [{number: $n, title: $t, state: "open", body: $b, user: $u, labels: $l, comments: []}]'
	echo "WRITE issue create $n [$(
		IFS=+
		echo "${have[*]+"${have[*]}"}"
	)]: $title" >>"$FAKE_LOG"
	# Another run opens its own in the same moment (an older number).
	if [ -f "$FAKE_STATE/race.json" ]; then
		update --slurpfile r "$FAKE_STATE/race.json" '. + $r'
		rm -f "$FAKE_STATE/race.json"
	fi
	"$JQ" --argjson n "$n" '.[] | select(.number == $n)' "$DB" | "$JQ" -r "$filter"
	;;
"POST repos/"*"/issues/"*"/comments")
	fails comment
	n="${path#*/issues/}"
	n="${n%%/*}"
	update --argjson n "$n" --arg b "$body" --argjson u "$BOT" \
		'map(if .number == $n then .comments += [{user: $u, body: $b}] else . end)'
	echo "WRITE issue comment $n" >>"$FAKE_LOG"
	if [ -n "${FAKE_CLOSED_MEANWHILE:-}" ]; then
		update --argjson n "$n" 'map(if .number == $n then .state = "closed" | .state_reason = "completed" else . end)'
	fi
	echo '{"id": 1}' | "$JQ" -r "$filter"
	;;
"PATCH repos/"*"/issues/"*)
	fails patch
	n="${path#*/issues/}"
	update --argjson n "$n" --arg s "$state" --arg r "$reason" \
		'map(if .number == $n then .state = $s | .state_reason = $r else . end)'
	if [ "$state" = closed ]; then
		echo "WRITE issue close $n $reason" >>"$FAKE_LOG"
	else
		echo "WRITE issue reopen $n" >>"$FAKE_LOG"
	fi
	echo '{"id": 1}' | "$JQ" -r "$filter"
	;;
*) elsewhere ;;
esac
