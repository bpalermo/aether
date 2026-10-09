#!/usr/bin/env bash
# The decision of the `ci` job of .github/workflows/ci.yaml, the one status
# check the `main` ruleset requires (#1460).
#
# It reads the `needs` context of that job as JSON and passes only when every
# job ended the way the workflow's own conditions say it must. "Skipped" is an
# answer only where something says so out loud:
#
#   - `changes` and the jobs with no condition must have succeeded;
#   - `changes` must have said control_plane=true or control_plane=false, in
#     those words. With `false` (a change to the proxy workspace only) every
#     other job must be skipped;
#   - with `true`, `diff` and the other control-plane jobs must have succeeded,
#     and `diff` must have set has_any, has_unit, has_integration and has_e2e to
#     `true` or `false`, in those words. An output that was never set reads as
#     the empty string in the workflow's `if:` conditions, which skips a leg
#     exactly as `false` does: here it is a failure;
#   - each test leg must then have succeeded when its output says `true`, and
#     be skipped when it says `false`. "Nothing impacted" is therefore a value
#     `diff` wrote (four times `false`), never the absence of one;
#   - a job that failed or was cancelled fails the check, as before, and so does
#     a job this script has no rule for, or a job missing from `needs`.
#
# The rules below repeat the `if:` of each job in ci.yaml.
# //scripts:ci_gate_test holds the two together: the job list, the `needs` of
# the `ci` job and every `if:` are compared with the workflow file.
#
# Env:
#   NEEDS_JSON  `${{ toJSON(needs) }}` of the `ci` job
#   JQ          jq binary (default: jq)
#
# Usage: ci-gate.sh          decide; exit 0 to pass, 1 to fail
#        ci-gate.sh --jobs   print the jobs the rules cover, one per line
# shellcheck disable=SC2016 # single-quoted $names here are jq variables
set -uo pipefail

JQ="${JQ:-jq}"

# No condition in ci.yaml: they run on every pull request.
ALWAYS="changes envoy-api-parity proxy-pin actionlint"
# `if: needs.changes.outputs.control_plane == 'true'`.
CONTROL_PLANE="diff chart-version-bump deps-audit shell format"
# The same, and an output of `diff`. Which one is in the jq program below.
LEGS="test race netns integration e2e"

if [ "${1:-}" = --jobs ]; then
	# shellcheck disable=SC2086 # three word lists
	printf '%s\n' $ALWAYS $CONTROL_PLANE $LEGS
	exit 0
fi

DECIDE='
def words: split(" ") | map(select(. != ""));
def shown: if . == null then "not set" else tojson end;
# An output is a decision only when it is exactly "true" or "false".
def flag: if . == "true" then true elif . == "false" then false else null end;

($always | words) as $always
| ($control_plane | words) as $cp_jobs
| ($legs | words) as $legs
| ($always + $cp_jobs + $legs) as $known
| if type != "object" then
    {errors: ["the needs context is not a JSON object"], notes: []}
  else
    . as $n
    | ($known - ($n | keys)) as $missing
    | (($n | keys) - $known) as $extra
    | [$known[] | select($n[.] != null) | {job: ., result: $n[.].result}] as $seen
    | ($n.changes.outputs.control_plane? | flag) as $cp
    | ({
        has_any: ($n.diff.outputs.has_any? | flag),
        has_unit: ($n.diff.outputs.has_unit? | flag),
        has_integration: ($n.diff.outputs.has_integration? | flag),
        has_e2e: ($n.diff.outputs.has_e2e? | flag)
      }) as $has
    | ($has | to_entries | map(select(.value == null) | .key)) as $unset
    | ($cp == true and $n.diff.result? == "success" and ($unset | length) == 0) as $decided
    # Which legs must have run. Each line is the `if:` of that job in ci.yaml.
    | ({
        test: $has.has_any,
        race: ($has.has_unit or $has.has_integration),
        netns: $has.has_unit,
        integration: $has.has_integration,
        e2e: $has.has_e2e
      }) as $want
    | {
        errors: (
          [$missing[] | "job \(.) is not in the needs of the ci job: its result cannot be checked"]
          + [$extra[] | "job \(.) is in the needs of the ci job and scripts/ci-gate.sh has no rule for it"]
          + [$seen[] | select(.result as $r | ["success", "failure", "cancelled", "skipped"] | index($r) | not)
              | "job \(.job) has the result \(.result | shown), which is not one a job can end with"]
          + [$seen[] | select(.result == "failure" or .result == "cancelled")
              | "job \(.job) ended as \(.result)"]
          + [$seen[] | select(.result == "skipped") | select(.job as $j | $always | index($j))
              | "job \(.job) was skipped, and it has no condition: it runs on every pull request"]
          + (if $n.changes.result? == "success" and $cp == null then
               ["job changes succeeded and its output control_plane is \($n.changes.outputs.control_plane? | shown), not \"true\" or \"false\": nothing says whether the other jobs had to run"]
             else [] end)
          + (if $cp == false then
               [$seen[] | select(.result == "success") | select(.job as $j | ($cp_jobs + $legs) | index($j))
                 | "job \(.job) ran, and changes says control_plane=false"]
             else [] end)
          + (if $cp == true then
               [$seen[] | select(.result == "skipped") | select(.job as $j | $cp_jobs | index($j))
                 | "job \(.job) was skipped, and changes says control_plane=true"]
             else [] end)
          + (if $cp == true and $n.diff.result? == "success" then
               [$unset[] | "job diff succeeded and its output \(.) is \($n.diff.outputs[.]? | shown), not \"true\" or \"false\": a leg skipped on it tested nothing, and nothing says there was nothing to test"]
             else [] end)
          + (if $decided and $has.has_any == false and ($has.has_unit or $has.has_integration or $has.has_e2e) then
               ["job diff says has_any=false and has_unit=\($has.has_unit) has_integration=\($has.has_integration) has_e2e=\($has.has_e2e): a test cannot be impacted when no target is"]
             else [] end)
          + (if $decided then
               [$seen[] | select(.job as $j | $legs | index($j)) | . as $s
                 | if $want[$s.job] and $s.result == "skipped" then
                     "job \($s.job) was skipped, and diff says it had to run"
                   elif ($want[$s.job] | not) and $s.result == "success" then
                     "job \($s.job) ran, and diff says it had nothing to run"
                   else empty end]
             else [] end)
        ),
        notes: (
          [$seen[] | "\(.job): \(.result // "not set")"]
          + ["changes: control_plane=\($n.changes.outputs.control_plane? | shown)"]
          + [$has | keys_unsorted[] as $k | "diff: \($k)=\($n.diff.outputs[$k]? | shown)"]
          + (if $cp == false then
               ["decision: the control plane is untouched (changes wrote control_plane=false); every other job is skipped by design"]
             elif $decided and ($has | [.[]] | any | not) then
               ["decision: nothing impacted (diff wrote false to has_any, has_unit, has_integration and has_e2e); the test legs are skipped by design"]
             elif $decided then
               ["decision: impacted; had to run: \([$legs[] | select($want[.])] | join(" "))"]
             else
               ["decision: none, see the errors"]
             end)
        )
      }
  end
'

if [ -z "${NEEDS_JSON:-}" ]; then
	echo "::error::ci-gate: NEEDS_JSON is empty: the results of the jobs were not passed in"
	exit 1
fi

# The context as GitHub rendered it, for whoever reads a red `ci` job. Results
# and job outputs only: GitHub does not hand a secret on as a job output.
echo "::group::the needs context"
printf '%s\n' "$NEEDS_JSON"
echo "::endgroup::"

if ! verdict="$(printf '%s' "$NEEDS_JSON" | "$JQ" -c \
	--arg always "$ALWAYS" --arg control_plane "$CONTROL_PLANE" --arg legs "$LEGS" \
	"$DECIDE")" || [ -z "$verdict" ]; then
	echo "::error::ci-gate: NEEDS_JSON could not be read as the needs context (jq failed)"
	exit 1
fi

if ! notes="$(printf '%s' "$verdict" | "$JQ" -r '.notes[]')" ||
	! errors="$(printf '%s' "$verdict" | "$JQ" -r '.errors[]')" ||
	! count="$(printf '%s' "$verdict" | "$JQ" -r '.errors | length')"; then
	echo "::error::ci-gate: the verdict could not be read (jq failed)"
	exit 1
fi

printf '%s\n' "$notes"
if [ "$count" != 0 ]; then
	while IFS= read -r line; do
		echo "::error::ci-gate: $line"
	done <<<"$errors"
	exit 1
fi
echo "ci-gate: every job ended the way its conditions say it must"
