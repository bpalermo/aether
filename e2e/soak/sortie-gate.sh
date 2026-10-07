#!/usr/bin/env bash
# Grades a sortie run's JSON report against the soak's zero-failure set
# (proposal 042): one PASS/FAIL line per target, then one VERDICT line. This is
# what replaces reading `http_req_failed` and the "by target" table out of each
# k6 runner's end-of-test summary.
#
#   sortie-gate.sh --dir RUN_DIR
#   sortie-gate.sh --report report.json --shares shares.tsv --backends 5 [--engines engines.tsv]
#
# A target PASSES only if ALL of these hold; each line says which did not:
#   present    the report has an execution labelled <scenario>/<target>. A target
#              that is missing was never driven, and its zeros mean nothing.
#   ran        the execution has no `error` (an engine died, the run was
#              cancelled, a name did not resolve) and ran its whole duration.
#   backends   it ran on exactly the expected number of engines: the dns pool is
#              resolved once, so a node that was not ready is absent for the
#              whole run.
#   failures   every counter of the zero-failure set is 0 on every backend:
#              http_4xx, http_5xx, grpc_error, stream_resets (and its _<phase>
#              / _<reason> refinements), pool_connection_failure, the
#              pool_failure_* family (which holds pool_failure_timeout) and
#              pool_overflow. The counters OVERLAP (a reset is in stream_resets
#              and in one phase and one reason counter): they are listed, never
#              summed.
#   rate       its achieved 2xx rate is at least --floor-pct (99) of ITS OWN
#              share x backends. The plan can only carry one floor for the
#              whole scenario (the smallest share's); this is the per-target one.
#   thresholds no threshold of the plan failed for it.
#
# FAIL here is not by itself a failed soak: under 33 rolls a k6 run also ended
# with a handful of failures, each attributed to a roll bracket from the access
# logs. It IS the list of what has to be attributed, per target, per class, per
# node. See README.md, "Load driver: sortie".
#
# Exit: 0 every target passed; 1 at least one failed; 2 unusable input.
# Needs bash, jq and awk; //e2e/soak:harness_test runs it on canned reports.
set -uo pipefail

DIR="" REPORT="" SHARES="" BACKENDS="" ENGINES="" SCENARIO="mesh" FLOOR_PCT=99
die() {
	echo "sortie-gate.sh: $*" >&2
	exit 2
}
while [ $# -gt 0 ]; do
	case "$1" in
	--dir | --report | --shares | --backends | --engines | --scenario | --floor-pct)
		[ $# -ge 2 ] || die "$1 needs a value"
		case "$1" in
		--dir) DIR="$2" ;;
		--report) REPORT="$2" ;;
		--shares) SHARES="$2" ;;
		--backends) BACKENDS="$2" ;;
		--engines) ENGINES="$2" ;;
		--scenario) SCENARIO="$2" ;;
		--floor-pct) FLOOR_PCT="$2" ;;
		esac
		shift 2
		;;
	*) die "unknown option '$1' (usage: sortie-gate.sh --dir RUN_DIR | --report F --shares F --backends N)" ;;
	esac
done
if [ -n "$DIR" ]; then
	[ -r "$DIR/run.env" ] || die "$DIR holds no run.env"
	: "${REPORT:=$DIR/report.json}" "${SHARES:=$DIR/shares.tsv}"
	[ -n "$BACKENDS" ] || BACKENDS="$(awk -F= '$1 == "BACKENDS" {print $2}' "$DIR/run.env")"
	[ -n "$ENGINES" ] || { [ -r "$DIR/engines.tsv" ] && ENGINES="$DIR/engines.tsv"; }
fi
[ -n "$REPORT" ] && [ -n "$SHARES" ] && [ -n "$BACKENDS" ] || die "need --dir, or --report, --shares and --backends"
[ -r "$REPORT" ] || die "cannot read the report '$REPORT' (was it saved? sortie-save.sh --dir ...)"
[ -r "$SHARES" ] || die "cannot read the share table '$SHARES'"
case "$BACKENDS" in '' | *[!0-9]* | 0*) die "--backends must be a positive integer, got '$BACKENDS'" ;; esac
jq -e '(.executions | type) == "array"' "$REPORT" >/dev/null 2>&1 || die "'$REPORT' is not a sortie JSON report (no .executions array)"

# backend address -> node, when the run directory has the engine list.
NODES='{}'
if [ -n "$ENGINES" ] && [ -r "$ENGINES" ]; then
	NODES="$(awk -F'\t' 'BEGIN { printf "{" } NF >= 2 { printf "%s\"%s\":\"%s\"", (n++ ? "," : ""), $1, $2 } END { printf "}" }' "$ENGINES")"
fi
SHARES_JSON="$(awk -F'\t' 'BEGIN { printf "[" } NF >= 2 { printf "%s{\"name\":\"%s\",\"rps\":%d}", (n++ ? "," : ""), $1, $2 } END { printf "]" }' "$SHARES")"

jq -r --argjson shares "$SHARES_JSON" --argjson nodes "$NODES" --argjson n "$BACKENDS" \
	--arg scenario "$SCENARIO" --argjson floor "$FLOOR_PCT" '
	def zero_set: test("^benchmark\\.(http_4xx|http_5xx|grpc_error|stream_resets|pool_connection_failure|pool_failure_|pool_overflow)");
	def node($b): ($b | sub(":[0-9]+$"; "")) as $ip | ($nodes[$ip] // $ip);
	def num: (try tonumber catch null);
	def actual($e; $prefix): ([$e.thresholds[]? | select(.expr | startswith($prefix)) | .actual | num] | map(select(. != null)) | .[0]);

	(.executions | map({key: .label, value: .}) | from_entries) as $by
	| ($shares | map("\($scenario)/\(.name)")) as $want
	| [ $shares[] as $s
	    | ("\($scenario)/\($s.name)") as $label
	    | $by[$label] as $e
	    | if $e == null then
	        {name: $s.name, ok: false, why: ["missing from the report (never driven)"], detail: ""}
	      else
	        ($s.rps * $n) as $planned
	        | actual($e; "rate:benchmark.http_2xx") as $rate
	        | actual($e; "counter:benchmark.http_2xx") as $count
	        | ([$e.failures[]? | .backend as $b | .counters | to_entries[] | select(.key | zero_set) | select(.value > 0)
	            | {c: (.key | sub("^benchmark\\."; "")), b: node($b), v: .value}]) as $fail
	        | ([$e.thresholds[]? | select(.pass == false) | select(.expr | sub("^counter:"; "") | zero_set | not) | .expr]
	           + [$e.thresholds[]? | select(.pass == false) | select(.expr | sub("^counter:"; "") | zero_set)
	              | select(($fail | length) == 0) | "\(.expr) actual \(.actual)"]) as $thr
	        | ([ (if ($e.error // "") != "" then "error: \($e.error)" else empty end),
	             (if ($e.error // "") == "" and ($e.elapsed_ms // 0) < ($e.duration_ms // 0) then "ran \($e.elapsed_ms)ms of \($e.duration_ms)ms" else empty end),
	             (if (($e.backends // []) | length) != $n then "backends \(($e.backends // []) | length)/\($n)" else empty end),
	             (if ($fail | length) > 0 then
	                "failures " + ($fail | group_by(.c) | map("\(.[0].c)=\(map(.v) | add) [" + (map("\(.b)=\(.v)") | join(" ")) + "]") | join(" "))
	              else empty end),
	             (if ($e.error // "") == "" and $rate == null then "no rate:benchmark.http_2xx threshold in the report (the 2xx rate went unchecked)"
	              elif $rate != null and $rate < ($planned * $floor / 100) then "rate \($rate * 100 | round / 100) rps < \($floor)% of \($planned)"
	              else empty end),
	             (if ($thr | length) > 0 then "thresholds failed: " + ($thr | join("; ")) else empty end)
	           ]) as $why
	        | {name: $s.name, ok: (($why | length) == 0), why: $why,
	           detail: ("rate=" + (if $rate == null then "?" else ($rate * 100 | round / 100 | tostring) end) + "/\($planned)rps"
	                    + (if $count == null then "" else " http_2xx=\($count)" end)
	                    + " backends=\(($e.backends // []) | length)/\($n)")}
	      end ] as $rows
	| ([.executions[].label] - $want) as $extra
	| ($rows[] | (if .ok then "PASS  " else "FAIL  " end) + .name + (if .detail != "" then "  " + .detail else "" end)
	             + (if .ok then "  failures=none" else "  -- " + (.why | join(" | ")) end)),
	  ($extra[] | "FAIL  \(.)  -- in the report but not in the share table (a different plan?)"),
	  ("VERDICT " + (if ([$rows[] | select(.ok | not)] | length) == 0 and ($extra | length) == 0 then "PASS" else "FAIL" end)
	   + " targets=\($rows | length) passed=\([$rows[] | select(.ok)] | length) failed=\(([$rows[] | select(.ok | not)] | length) + ($extra | length))"
	   + " backends=\($n) report_pass=\(.pass)")
	' "$REPORT" >"${TMPDIR:-/tmp}/sortie-gate.$$" || {
	rm -f "${TMPDIR:-/tmp}/sortie-gate.$$"
	die "could not evaluate '$REPORT'"
}
cat "${TMPDIR:-/tmp}/sortie-gate.$$"
grep -q '^VERDICT PASS ' "${TMPDIR:-/tmp}/sortie-gate.$$"
rc=$?
rm -f "${TMPDIR:-/tmp}/sortie-gate.$$"
exit "$rc"
