#!/usr/bin/env bash
# Grades a sortie run's JSON report against the soak's zero-failure set
# (proposal 042): one PASS/FAIL line per target, then one VERDICT line. This is
# what replaces reading `http_req_failed` and the "by target" table out of each
# k6 runner's end-of-test summary.
#
#   sortie-gate.sh --dir RUN_DIR [--per-node]
#   sortie-gate.sh --report report.json --shares shares.tsv --backends 5 [--engines engines.tsv]
#
# It reads what sortie 94cf103 and newer put in the report per execution:
#   results         each backend's benchmark.* counters and its elapsed time
#   totals          the same, summed over the backends that returned results
#   backend_errors  the backends that did not finish cleanly (a dead engine, a
#                   node that went away); one listed there and absent from
#                   `results` returned nothing
# A report without `results` is from an older sortie: the gate says so and
# exits 2, it does not grade it.
#
# A target PASSES only if ALL of these hold; each line says which did not:
#   present    the report has an execution labelled <scenario>/<target>. A target
#              that is missing was never driven, and its zeros mean nothing.
#   lost       no backend is in `backend_errors`. A lost backend FAILS the
#              target and is named, with its node; the others are still judged,
#              each on its own counters, and the line says how they did.
#   ran        the execution ran its whole duration, has no `error` beyond the
#              lost backends', and every backend's own elapsed time is within
#              5 % of the plan's. (A node that froze and thawed hands back a
#              result that took several times the duration: sortie waits for it.)
#   backends   it was dispatched to exactly the expected number of engines: the
#              dns pool is resolved once, so a node that was not ready is absent
#              for the whole run.
#   failures   every counter of the zero-failure set is 0 on every backend:
#              http_4xx, http_5xx, grpc_error, stream_resets (and its _<phase>
#              / _<reason> refinements), pool_connection_failure, the
#              pool_failure_* family (which holds pool_failure_timeout) and
#              pool_overflow. The counters OVERLAP (a reset is in stream_resets
#              and in one phase and one reason counter): they are listed per
#              node, never summed across classes.
#   saturated  pool_overflow is in that set and fails the target, but it is
#              said in words, apart from the mesh's classes: `driver saturated:
#              N request(s) not sent [node=N]`. The engine's own pool refused
#              them because its client queue (the plan's max_pending_requests)
#              was full: that node stalled for longer than the plan's stall
#              budget. The requests never reached the mesh, so this is not a
#              data-plane failure; it is that target's run on that node being
#              short of what it was meant to send.
#   rate       EVERY backend's 2xx rate (its http_2xx over its own elapsed time)
#              is at least --floor-pct (99) of the target's share. The plan can
#              only carry one floor for the whole scenario and judges it against
#              the pool's total; this is per target and per node, so one slow
#              node is named instead of being averaged away.
#   thresholds no threshold of the plan failed for it.
#
# Not judged, printed: `lat p50<=A p99<=B`, the WORST backend's latency, when the
# plan carries the two latency carriers (sortie-plan.sh). A soft signal: see
# README.md before leaning on it.
#
# --per-node adds one `NODE` line per target and backend: 2xx against the planned
# count, the rate, the latency, the non-zero failure counters.
#
# FAIL here is not by itself a failed soak: under 33 rolls a k6 run also ended
# with a handful of failures, each attributed to a roll bracket from the access
# logs. It IS the list of what has to be attributed, per target, per class, per
# node. See README.md, "Load driver: sortie".
#
# Exit: 0 every target passed; 1 at least one failed; 2 unusable input.
# Needs bash, jq and awk; //e2e/soak:harness_test runs it on canned reports.
set -uo pipefail

DIR="" REPORT="" SHARES="" BACKENDS="" ENGINES="" SCENARIO="mesh" FLOOR_PCT=99 PER_NODE=false
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
	--per-node) PER_NODE=true && shift ;;
	*) die "unknown option '$1' (usage: sortie-gate.sh --dir RUN_DIR | --report F --shares F --backends N; [--per-node])" ;;
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
# An execution that was judged (it has thresholds) but carries no per-backend
# results is the report of a sortie older than 94cf103. Its counters would all
# read as absent, that is as zero: refuse it rather than pass it.
if jq -e 'any(.executions[]; ((.thresholds // []) | length) > 0 and (has("results") | not))' "$REPORT" >/dev/null 2>&1; then
	die "'$REPORT' is an OLD-FORMAT sortie report: its executions have thresholds but no per-backend \`results\` (sortie older than 94cf103, the release that added results / totals / backend_errors). This gate reads those and would see every counter as zero; grade the report with the sortie-gate.sh of the commit that produced it."
fi

# backend address -> node, when the run directory has the engine list.
NODES='{}'
if [ -n "$ENGINES" ] && [ -r "$ENGINES" ]; then
	NODES="$(awk -F'\t' 'BEGIN { printf "{" } NF >= 2 { printf "%s\"%s\":\"%s\"", (n++ ? "," : ""), $1, $2 } END { printf "}" }' "$ENGINES")"
fi
SHARES_JSON="$(awk -F'\t' 'BEGIN { printf "[" } NF >= 2 { printf "%s{\"name\":\"%s\",\"rps\":%d}", (n++ ? "," : ""), $1, $2 } END { printf "]" }' "$SHARES")"

OUT="$(mktemp "${TMPDIR:-/tmp}/sortie-gate.XXXXXX")" || die "mktemp failed"
trap 'rm -f "$OUT"' EXIT
jq -r --argjson shares "$SHARES_JSON" --argjson nodes "$NODES" --argjson n "$BACKENDS" \
	--arg scenario "$SCENARIO" --argjson floor "$FLOOR_PCT" --argjson pernode "$PER_NODE" '
	def zero_set: test("^benchmark\\.(http_4xx|http_5xx|grpc_error|stream_resets|pool_connection_failure|pool_failure_|pool_overflow)");
	def ip: sub(":[0-9]+$"; "");
	def node: (ip) as $ip | ($nodes[$ip] // $ip);
	def r2: (. * 100 | round / 100);
	# A Go duration ("412.5µs", "1.84ms", "2.1s") in milliseconds; null if it is
	# anything else (an error text, a compound like 1m2s).
	def ms: (capture("^(?<v>[0-9.]+)(?<u>ns|µs|us|ms|s)$")? // null) as $c
		| if $c == null then null
		  else ($c.v | tonumber) * ({"ns": 0.000001, "µs": 0.001, "us": 0.001, "ms": 1, "s": 1000}[$c.u]) end;
	def fmt_ms: if . == null then "?" elif . >= 100 then (round | tostring) + "ms" elif . >= 10 then (. * 10 | round / 10 | tostring) + "ms" else (r2 | tostring) + "ms" end;
	# One latency carrier`s values, in the order of .results (sortie evaluates a
	# statistic threshold against each backend that returned a result, in order).
	def carrier($e; $p): ([$e.thresholds[]? | select(.scope == "per-backend") | select(.expr | startswith("latency_2xx." + $p + " ")) | .actual][0] // null)
		| if . == null then [] else split(", ") | map(ms) end;

	(.executions | map({key: .label, value: .}) | from_entries) as $by
	| ($shares | map("\($scenario)/\(.name)")) as $want
	| [ $shares[] as $s
	    | ("\($scenario)/\($s.name)") as $label
	    | $by[$label] as $e
	    | if $e == null then
	        {name: $s.name, ok: false, lost: 0, why: ["missing from the report (never driven)"], detail: "", nodes: []}
	      else
	        ($e.backends // []) as $dispatched
	        | ($e.backend_errors // []) as $berr
	        | ([$berr[].backend]) as $lostaddrs
	        | carrier($e; "p50") as $p50 | carrier($e; "p99") as $p99
	        | (($e.duration_ms // 0) / 1000) as $dur
	        # One row per backend that returned a result.
	        | [ ($e.results // []) | to_entries[] | .key as $i | .value as $r
	            | ($r.counters["benchmark.http_2xx"] // 0) as $c
	            | (if ($r.elapsed_ms // 0) > 0 then $c / ($r.elapsed_ms / 1000) else null end) as $rate
	            | ([$r.counters | to_entries[] | select(.key | zero_set) | select(.value > 0) | {c: (.key | sub("^benchmark\\."; "")), v: .value}]) as $f
	            | {b: $r.backend, node: ($r.backend | node), count: $c, rate: $rate, fail: $f,
	               lost: (($lostaddrs | index($r.backend)) != null),
	               short: ($rate == null or $rate < ($s.rps * $floor / 100)),
	               secs: (($r.elapsed_ms // 0) / 1000),
	               off: ($dur > 0 and ((($r.elapsed_ms // 0) / 1000 - $dur) | if . < 0 then -. else . end) > ($dur * 0.05)),
	               p50: ($p50[$i] // null), p99: ($p99[$i] // null)} ] as $res
	        | ([$res[] | select(.lost | not)]) as $surv
	        | ([$res[] | .node as $nd | .fail[] | {c: .c, b: $nd, v: .v}]) as $fail
	        # pool_overflow apart from the rest: the driver`s own refusals are not
	        # a class of mesh failure and are worded differently.
	        | ([$fail[] | select(.c == "pool_overflow")]) as $sat
	        | ([$fail[] | select(.c != "pool_overflow")]) as $mesh
	        | ($s.rps * $n) as $planned
	        | (if ($res | length) > 0 then ([$res[] | .rate // 0] | add) else null end) as $rate
	        | ($e.totals["benchmark.http_2xx"] // ([$res[].count] | add) // null) as $count
	        | ([$e.thresholds[]? | select(.pass == false) | select(.expr | sub("^counter:"; "") | zero_set | not) | "\(.expr) (actual \(.actual))"]
	           + [$e.thresholds[]? | select(.pass == false) | select(.expr | sub("^counter:"; "") | zero_set)
	              | select(($fail | length) == 0) | "\(.expr) (actual \(.actual))"]) as $thr
	        | ([ ($berr[] | "LOST BACKEND \(.backend | node) (\(.backend))"
	                + (if (.backend as $b | $res | any(.b == $b)) then " [partial counters reported]" else " [returned nothing]" end)),
	             (if ($berr | length) > 0 and ($surv | length) > 0 then
	                "survivors " + ($surv | map("\(.node)=" + (if (.fail | length) == 0 and (.short | not) and (.off | not) then "ok" else "FAIL" end)) | join(" "))
	              elif ($berr | length) > 0 then "no backend survived"
	              else empty end),
	             (if ($e.error // "") != "" and ($berr | length) == 0 then "error: \($e.error)" else empty end),
	             (if ($e.error // "") == "" and ($e.elapsed_ms // 0) < ($e.duration_ms // 0) then "ran \($e.elapsed_ms)ms of \($e.duration_ms)ms" else empty end),
	             (if ([$surv[] | select(.off)] | length) > 0 then
	                "ran off plan (\($dur)s) on [" + ([$surv[] | select(.off) | "\(.node)=\(.secs | r2)s"] | join(" ")) + "]"
	              else empty end),
	             (if ($dispatched | length) != $n then "backends \($dispatched | length)/\($n)" else empty end),
	             (if ($mesh | length) > 0 then
	                "failures " + ($mesh | group_by(.c) | map("\(.[0].c)=\(map(.v) | add) [" + (map("\(.b)=\(.v)") | join(" ")) + "]") | join(" "))
	              else empty end),
	             (if ($sat | length) > 0 then
	                "driver saturated: \([$sat[].v] | add) request(s) not sent [" + ($sat | map("\(.b)=\(.v)") | join(" ")) + "] (pool_overflow: the engine refused them itself; not a mesh error)"
	              else empty end),
	             (if ([$surv[] | select(.short)] | length) > 0 then
	                "rate below \($floor)% of \($s.rps) rps on [" + ([$surv[] | select(.short) | "\(.node)=" + (if .rate == null then "?" else (.rate | r2 | tostring) end)] | join(" ")) + "]"
	              elif ($res | length) == 0 and ($berr | length) == 0 and ($e.error // "") == "" then "no per-backend results in the report (the 2xx rate went unchecked)"
	              else empty end),
	             (if ($thr | length) > 0 then "thresholds failed: " + ($thr | join("; ")) else empty end)
	           ]) as $why
	        | ([$res[].p50 | select(. != null)] | max) as $w50 | ([$res[].p99 | select(. != null)] | max) as $w99
	        | {name: $s.name, ok: (($why | length) == 0), lost: ($berr | length), why: $why,
	           detail: ("rate=" + (if $rate == null then "?" else ($rate | r2 | tostring) end) + "/\($planned)rps"
	                    + (if $count == null then "" else " http_2xx=\($count)/\($planned * $dur | round)" end)
	                    + " backends=\(($surv | length))/\($n)"),
	           lat: (if $w50 == null and $w99 == null then "" else "lat p50<=\($w50 | fmt_ms) p99<=\($w99 | fmt_ms)" end),
	           nodes: ([$res[] | "NODE  \($s.name)  \(.node)  http_2xx=\(.count)/\($s.rps * $dur | round) rate=\(if .rate == null then "?" else (.rate | r2 | tostring) end)/\($s.rps)rps"
	                             + (if .p50 == null and .p99 == null then "" else " p50=\(.p50 | fmt_ms) p99=\(.p99 | fmt_ms)" end)
	                             + (if (.fail | length) == 0 then " failures=none" else " failures " + (.fail | map("\(.c)=\(.v)") | join(" ")) end)
	                             + (if .lost then " LOST" else "" end)]
	                  + [$berr[] | select(.backend as $b | $res | any(.b == $b) | not) | "NODE  \($s.name)  \(.backend | node)  LOST, returned nothing"])}
	      end ] as $rows
	| ([.executions[].label] - $want) as $extra
	| ($rows[] | ((if .ok then "PASS  " else "FAIL  " end) + .name + (if .detail != "" then "  " + .detail else "" end)
	             + (if .ok then "  failures=none" else "  -- " + (.why | join(" | ")) end)
	             + (if (.lat // "") != "" then "  " + .lat else "" end)),
	            (if $pernode then .nodes[] else empty end)),
	  ($extra[] | "FAIL  \(.)  -- in the report but not in the share table (a different plan?)"),
	  # Each lost backend once, with what sortie said about it: the error is the
	  # same for every target of an engine that went away.
	  ([.executions[] | (.backend_errors // [])[]] | group_by(.backend)[]
	   | "LOST  \(.[0].backend | node) (\(.[0].backend))  targets=\(length)  error: \(.[0].error)"),
	  ("VERDICT " + (if ([$rows[] | select(.ok | not)] | length) == 0 and ($extra | length) == 0 then "PASS" else "FAIL" end)
	   + " targets=\($rows | length) passed=\([$rows[] | select(.ok)] | length) failed=\(([$rows[] | select(.ok | not)] | length) + ($extra | length))"
	   + " backends=\($n) lost_backends=\([.executions[] | (.backend_errors // [])[].backend] | unique | length) report_pass=\(.pass)")
	' "$REPORT" >"$OUT" || die "could not evaluate '$REPORT'"
cat "$OUT"
grep -q '^VERDICT PASS ' "$OUT"
