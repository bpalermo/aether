#!/usr/bin/env bash
# Grades a sortie run's JSON report against the soak's zero-failure set
# (proposal 042): one PASS/FAIL line per target, then one VERDICT line. This is
# what replaces reading `http_req_failed` and the "by target" table out of each
# k6 runner's end-of-test summary.
#
#   sortie-gate.sh --dir RUN_DIR [--per-node]
#   sortie-gate.sh --report report.json --shares shares.tsv --backends 5 [--engines engines.tsv] [--log sortie.log]
#   sortie-gate.sh --dir RUN_DIR --stream RUN_DIR/results.jsonl     # no report: grade the results stream
#
# It reads what sortie 9fcbb81 and newer put in the report per execution:
#   results         each backend's benchmark.* counters, its elapsed time, when
#                   its first worker started (started_at) and its latency
#                   statistics
#   totals          the counters, summed over the backends that returned results
#   backend_errors  the backends that did not finish cleanly (a dead engine, a
#                   node that went away or went silent); one listed there and
#                   absent from `results` returned nothing
#   started_at, ended_at   when sortie began dispatching the execution and when
#                   its last backend had answered or been given up on
#   not_run         the execution was never attempted
#   refused         (sortie f0750ec) "execution_cap": the execution belongs to a
#                   stage an engine refused at its execution cap; also on the
#                   backend_errors entries of the engines that refused
# A report whose executions have no `started_at` is from an older sortie, which
# did not count the requests still in flight at the end of a run: the gate says
# so and exits 2, it does not grade it.
#
# --stream FILE grades the results stream instead (sortie's --results-stream,
# saved as results.jsonl): one JSON object per line, each the object that
# execution is in the report's `executions`. It is what is left of a run whose
# pod died before the report was written. sortie appends to that file and never
# truncates it, and a run that died can leave a last line that does not parse:
# the gate skips every line that is not a JSON object with a `label`, says how
# many it skipped, and FAILS a label that occurs twice (two runs in one file).
# The stream has no overall `pass`; the verdict line says `source=stream`.
#
# A target PASSES only if ALL of these hold; each line says which did not:
#   present    the report has an execution labelled <scenario>/<target>, once. A
#              target that is missing was never driven, and its zeros mean
#              nothing.
#   run        the execution is not marked `not_run`. sortie lists a stage it
#              never attempted (one after a stage refused at an engine's
#              execution cap) with pass=false and no counters; in its text
#              summary that is `SKIP` and `N/M executions passed, K not run`.
#              The gate FAILS it as NOT RUN and counts it in the verdict
#              (`not_run=K`). With --dir (or --log) it also reads the sortie
#              pod's log: a `SKIP` line there, or a summary with `K not run`,
#              K > 0, fails the gate even if the report does not show it. The
#              refused stage itself fails as `REFUSED AT THE EXECUTION CAP`,
#              with the engines that refused named (`[refused by: <node> ...]`):
#              they were not lost. It is known by sortie's own mark, `"refused":
#              "execution_cap"` on the execution and on those engines' entries
#              in backend_errors (sortie f0750ec). An entry without the mark is
#              a lost backend, in a refused stage too. A report with no such
#              field anywhere is sortie 9fcbb81's, which worded a refusal and
#              did not mark it: only there is the error text read instead.
#   lost       no backend is in `backend_errors`. A lost backend FAILS the
#              target and is named, with its node; the others are still judged,
#              each on its own counters, and the line says how they did.
#   ran        the execution has no `error` beyond the lost backends', its
#              elapsed time is not under its duration, and every backend's own
#              elapsed time is within 5 % of the plan's, either side. (The
#              engine fixes that time when the run stops and it can read a hair
#              under the configured duration: 899,999 ms of 900,000 on
#              talos-main. A node that froze and thawed hands back several
#              times the duration.)
#   backends   it was dispatched to exactly the expected number of engines: the
#              dns pool is resolved once, so a node that was not ready is absent
#              for the whole run.
#   failures   every counter of the zero-failure set is 0 on every backend:
#              http_4xx, http_5xx, grpc_error, stream_resets (and its _<phase>
#              / _<reason> refinements), pool_connection_failure, the
#              pool_failure_* family (which holds pool_failure_timeout),
#              pool_overflow and http_inflight_lost. The counters OVERLAP (a
#              reset is in stream_resets and in one phase and one reason
#              counter): they are listed per node, never summed across classes.
#   saturated  pool_overflow is in that set and fails the target, but it is
#              said in words, apart from the mesh's classes: `driver saturated:
#              N request(s) not sent [node=N]`. The engine's own pool refused
#              them because its client queue (the plan's max_pending_requests)
#              was full: that node stalled for longer than the plan's stall
#              budget. The requests never reached the mesh.
#   in flight  http_inflight_lost is in that set too and is also said apart:
#              `in flight at the end: N request(s) with no outcome [node=N]`.
#              They were sent, or queued for a connection, and had neither a
#              response nor a reset when the run was over and the request
#              timeout (30 s) had passed. Not a reset and not a status; they
#              are in no other counter. One hung request is now a number.
#   sent       EVERY backend's http_2xx is within --floor-pct (99) and
#              200 - floor (101) percent of what the plan asked of it: the
#              target's share x the CONFIGURED duration. A range, and against
#              the configured duration on purpose: the backend's own elapsed
#              time can read a hair under it, and a worker that is woken late
#              ends a run one request short (sortie's README: 999 of 1,000 in
#              about one short run in four on a busy machine; the second
#              talos-main run read 40,497 to 40,500 of 40,500 per target with
#              every class at zero, by this or by requests still in flight,
#              which sortie 94cf103 did not count), so an equality would fail a
#              clean run, while a rate over the
#              backend's own elapsed time would pass a run that stopped early.
#              1 % of the plan is 81 requests of a 9 rps target's 8,100 in 15
#              minutes, 2,754 of its 275,400 in 8h30m. The plan can only carry
#              one floor for the whole scenario and judges it against the
#              pool's total; this is per target and per node, so one slow node
#              is named instead of being averaged away.
#   thresholds no threshold of the plan failed for it.
#
# Not judged, printed: `lat p50<=A p99<=B max<=C`, the WORST backend's latency,
# from each backend's `statistics` in the report (benchmark_http_client.
# latency_2xx; the plan carries no latency threshold any more). A soft signal:
# see README.md before leaning on it. And one `WINDOW` line: the first
# started_at and the last ended_at of the run, to line it up with roll times.
#
# --per-node adds one `NODE` line per target and backend: 2xx against the planned
# count, the rate, when its first worker started, the latency, the non-zero
# failure counters.
#
# FAIL here is not by itself a failed soak: under 33 rolls a k6 run also ended
# with a handful of failures, each attributed to a roll bracket from the access
# logs. It IS the list of what has to be attributed, per target, per class, per
# node. See README.md, "Load driver: sortie".
#
# Exit: 0 every target passed; 1 at least one failed; 2 unusable input.
# Needs bash, jq and awk; //e2e/soak:harness_test runs it on canned reports.
set -uo pipefail

DIR="" REPORT="" STREAM="" SHARES="" BACKENDS="" ENGINES="" LOG="" SCENARIO="mesh" FLOOR_PCT=99 PER_NODE=false
die() {
	echo "sortie-gate.sh: $*" >&2
	exit 2
}
while [ $# -gt 0 ]; do
	case "$1" in
	--dir | --report | --stream | --shares | --backends | --engines | --log | --scenario | --floor-pct)
		[ $# -ge 2 ] || die "$1 needs a value"
		case "$1" in
		--dir) DIR="$2" ;;
		--report) REPORT="$2" ;;
		--stream) STREAM="$2" ;;
		--shares) SHARES="$2" ;;
		--backends) BACKENDS="$2" ;;
		--engines) ENGINES="$2" ;;
		--log) LOG="$2" ;;
		--scenario) SCENARIO="$2" ;;
		--floor-pct) FLOOR_PCT="$2" ;;
		esac
		shift 2
		;;
	--per-node) PER_NODE=true && shift ;;
	*) die "unknown option '$1' (usage: sortie-gate.sh --dir RUN_DIR | --report F --shares F --backends N; [--stream F] [--log F] [--per-node])" ;;
	esac
done
[ -z "$REPORT" ] || [ -z "$STREAM" ] || die "--report and --stream are two sources for the same thing: give one"
if [ -n "$DIR" ]; then
	[ -r "$DIR/run.env" ] || die "$DIR holds no run.env"
	: "${SHARES:=$DIR/shares.tsv}"
	if [ -z "$STREAM" ]; then
		: "${REPORT:=$DIR/report.json}"
		if [ ! -r "$REPORT" ] && [ -s "$DIR/results.jsonl" ]; then
			die "$DIR has no report.json but it has the results stream: the run did not live to write its report. Grade what it recorded with: sortie-gate.sh --dir $DIR --stream $DIR/results.jsonl"
		fi
	fi
	[ -n "$BACKENDS" ] || BACKENDS="$(awk -F= '$1 == "BACKENDS" {print $2}' "$DIR/run.env")"
	[ -n "$ENGINES" ] || { [ -r "$DIR/engines.tsv" ] && ENGINES="$DIR/engines.tsv"; }
	[ -n "$LOG" ] || { [ -r "$DIR/sortie.log" ] && LOG="$DIR/sortie.log"; }
fi
SOURCE="${REPORT:-$STREAM}"
[ -n "$SOURCE" ] && [ -n "$SHARES" ] && [ -n "$BACKENDS" ] || die "need --dir, or --report (or --stream), --shares and --backends"
[ -r "$SOURCE" ] || die "cannot read '$SOURCE' (was it saved? sortie-save.sh --dir ...)"
[ -r "$SHARES" ] || die "cannot read the share table '$SHARES'"
[ -z "$LOG" ] || [ -r "$LOG" ] || die "cannot read the sortie log '$LOG'"
case "$BACKENDS" in '' | *[!0-9]* | 0*) die "--backends must be a positive integer, got '$BACKENDS'" ;; esac
case "$FLOOR_PCT" in '' | *[!0-9]* | 0*) die "--floor-pct must be 1..100, got '$FLOOR_PCT'" ;; esac
[ "$FLOOR_PCT" -le 100 ] || die "--floor-pct must be 1..100, got '$FLOOR_PCT'"

OUT="$(mktemp "${TMPDIR:-/tmp}/sortie-gate.XXXXXX")" || die "mktemp failed"
DOC="$(mktemp "${TMPDIR:-/tmp}/sortie-gate.XXXXXX")" || die "mktemp failed"
trap 'rm -f "$OUT" "$DOC"' EXIT

# One document, whichever the source. From a stream: every line that is a JSON
# object with a label is an execution; anything else (the unfinished last line
# of a run that died, a fragment sortie closed off) is counted and skipped.
if [ -n "$STREAM" ]; then
	jq -Rn '
		[inputs | select(test("[^[:space:]]")) | (try fromjson catch null)] as $lines
		| ($lines | map(select(type == "object" and (.label | type) == "string"))) as $ok
		| {pass: null, source: "stream", skipped_lines: (($lines | length) - ($ok | length)), executions: $ok}' \
		"$STREAM" >"$DOC" 2>/dev/null || die "could not read the results stream '$STREAM'"
	jq -e '(.executions | length) > 0' "$DOC" >/dev/null 2>&1 ||
		die "'$STREAM' holds no execution: $(jq -r '.skipped_lines' "$DOC") line(s), none a JSON object with a label. Nothing to grade"
else
	jq -e '(.executions | type) == "array"' "$REPORT" >/dev/null 2>&1 || die "'$REPORT' is not a sortie JSON report (no .executions array)"
	jq '. + {source: "report", skipped_lines: 0}' "$REPORT" >"$DOC" || die "could not read '$REPORT'"
fi
# An execution that was judged (it has thresholds) but carries no per-backend
# results is the report of a sortie older than 94cf103. Its counters would all
# read as absent, that is as zero: refuse it rather than pass it.
if jq -e 'any(.executions[]; ((.thresholds // []) | length) > 0 and (has("results") | not))' "$DOC" >/dev/null 2>&1; then
	die "'$SOURCE' is an OLD-FORMAT sortie report: its executions have thresholds but no per-backend \`results\` (sortie older than 94cf103, the release that added results / totals / backend_errors). This gate reads those and would see every counter as zero; grade the report with the sortie-gate.sh of the commit that produced it."
fi
# An execution that ran but has no started_at is the report of a sortie older
# than 9fcbb81. That sortie ended a run with requests still in flight and
# counted them nowhere, so its http_inflight_lost is not zero, it is absent --
# and absent reads as zero. Same refusal, same reason.
if jq -e 'any(.executions[]; (.not_run // false | not) and (has("started_at") | not))' "$DOC" >/dev/null 2>&1; then
	die "'$SOURCE' is an OLD-FORMAT sortie report: its executions have no \`started_at\` (sortie older than 9fcbb81, the release that added the in-flight accounting, not_run and the timestamps). Such a sortie did not count the requests still in flight at the end of a run, so http_inflight_lost would read as zero here without having been counted; grade the report with the sortie-gate.sh of the commit that produced it."
fi

# sortie's own account of the run, from the pod's log: `SKIP` lines (one per
# stage it never attempted, in the narration and again in the summary) and the
# summary's `K not run`. A second witness for not_run, and the only one when the
# report in hand is not the report of that run.
LOG_SKIPS=0 LOG_NOTRUN=0 LOG_SUMMARY=""
if [ -n "$LOG" ]; then
	LOG_SKIPS="$(grep -cE '^[[:space:]]*SKIP[[:space:]]' "$LOG")"
	LOG_SUMMARY="$(grep -E '^(PASS|FAIL)  [0-9]+/[0-9]+ executions passed(, [0-9]+ not run)?$' "$LOG" | tail -n 1)"
	LOG_NOTRUN="$(printf '%s\n' "$LOG_SUMMARY" | sed -n 's/.* executions passed, \([0-9][0-9]*\) not run$/\1/p')"
	: "${LOG_NOTRUN:=0}"
fi

# backend address -> node, when the run directory has the engine list.
NODES='{}'
if [ -n "$ENGINES" ] && [ -r "$ENGINES" ]; then
	NODES="$(awk -F'\t' 'BEGIN { printf "{" } NF >= 2 { printf "%s\"%s\":\"%s\"", (n++ ? "," : ""), $1, $2 } END { printf "}" }' "$ENGINES")"
fi
SHARES_JSON="$(awk -F'\t' 'BEGIN { printf "[" } NF >= 2 { printf "%s{\"name\":\"%s\",\"rps\":%d}", (n++ ? "," : ""), $1, $2 } END { printf "]" }' "$SHARES")"

jq -r --argjson shares "$SHARES_JSON" --argjson nodes "$NODES" --argjson n "$BACKENDS" \
	--arg scenario "$SCENARIO" --argjson floor "$FLOOR_PCT" --argjson pernode "$PER_NODE" \
	--argjson logskips "$LOG_SKIPS" --argjson lognotrun "$LOG_NOTRUN" --arg logsummary "$LOG_SUMMARY" \
	--arg haslog "$LOG" '
	def zero_set: test("^benchmark\\.(http_4xx|http_5xx|grpc_error|stream_resets|pool_connection_failure|pool_failure_|pool_overflow|http_inflight_lost)");
	# A stage refused at an engine`s execution cap: sortie stopped it on every
	# backend. Since sortie f0750ec the report SAYS so, in a field: "refused":
	# "execution_cap" on every execution of that stage, and on each entry of
	# backend_errors that is an engine which refused (an entry without it is a
	# backend that was lost, refused stage or not). The field is what is read.
	# The error TEXT is read only for an execution that has no such field at
	# all, which is the report of sortie 9fcbb81: that pin`s reports are still
	# graded here (the talos-main runs of 2026-10-08 are its), and it marked a
	# refusal in no other way. There, every backend the refused execution lists
	# is taken to have refused, as sortie 9fcbb81 listed no other kind.
	def says_refused: (.refused // "") == "execution_cap";
	def old_shape_refused: (has("refused") | not) and ((.error // "") | test("refused a start because the engine is at "));
	def capped: says_refused or old_shape_refused;
	# One entry of an execution`s backend_errors: did that engine refuse, or was it lost?
	def refused_by($e): says_refused or ($e | old_shape_refused);
	def ip: sub(":[0-9]+$"; "");
	def node: (ip) as $ip | ($nodes[$ip] // $ip);
	def r2: (. * 100 | round / 100);
	def fmt_ms: if . == null then "?" elif . >= 100 then (round | tostring) + "ms" elif . >= 10 then (. * 10 | round / 10 | tostring) + "ms" else (r2 | tostring) + "ms" end;
	# One field of a backend`s 2xx latency statistic, in milliseconds; null when
	# the backend recorded none (it answered nothing) or the unit is not time.
	def lat($r; $f): ($r.statistics["benchmark_http_client.latency_2xx"] // null)
		| if . == null or .unit != "ns" or .[$f] == null then null else .[$f] / 1000000 end;

	(.executions | group_by(.label) | map({key: .[0].label, value: .}) | from_entries) as $by
	| ($shares | map("\($scenario)/\(.name)")) as $want
	| [ $shares[] as $s
	    | ("\($scenario)/\($s.name)") as $label
	    | ($by[$label] // []) as $es
	    | $es[0] as $e
	    | if $e == null then
	        {name: $s.name, ok: false, lost: 0, notrun: false, why: ["missing from the report (never driven)"], detail: "", nodes: []}
	      elif ($es | length) > 1 then
	        {name: $s.name, ok: false, lost: 0, notrun: false, detail: "", nodes: [],
	         why: ["listed \($es | length) times (started_at " + ($es | map(.started_at // "?") | join(", ")) + "): more than one run in this file, and which one is being graded is not decidable. sortie appends to a results stream; a run needs a file of its own"]}
	      elif ($e.not_run // false) then
	        {name: $s.name, ok: false, lost: 0, notrun: true, detail: "", nodes: [],
	         why: ["NOT RUN: sortie never attempted it, so it has no counters and its zeros mean nothing (" + ($e.error // "no reason given") + ")"]}
	      else
	        ($e.backends // []) as $dispatched
	        # backend_errors, in two: the backends that were lost, and the engines
	        # that refused a start at their execution cap (not lost).
	        | ([($e.backend_errors // [])[] | select(refused_by($e) | not)]) as $berr
	        | ([($e.backend_errors // [])[] | select(refused_by($e))]) as $refb
	        | ([$berr[].backend]) as $lostaddrs
	        | (($e.duration_ms // 0) / 1000) as $dur
	        | ($s.rps * $dur) as $plan1
	        # One row per backend that returned a result.
	        | [ ($e.results // [])[] as $r
	            | ($r.counters["benchmark.http_2xx"] // 0) as $c
	            | (if ($r.elapsed_ms // 0) > 0 then $c / ($r.elapsed_ms / 1000) else null end) as $rate
	            | ([$r.counters | to_entries[] | select(.key | zero_set) | select(.value > 0) | {c: (.key | sub("^benchmark\\."; "")), v: .value}]) as $f
	            | {b: $r.backend, node: ($r.backend | node), count: $c, rate: $rate, fail: $f,
	               lost: (($lostaddrs | index($r.backend)) != null),
	               short: ($plan1 <= 0 or $c < ($plan1 * $floor / 100)),
	               over: ($plan1 > 0 and $c > ($plan1 * (200 - $floor) / 100)),
	               secs: (($r.elapsed_ms // 0) / 1000),
	               off: ($dur > 0 and ((($r.elapsed_ms // 0) / 1000 - $dur) | if . < 0 then -. else . end) > ($dur * 0.05)),
	               started: ($r.started_at // null),
	               p50: lat($r; "p50"), p99: lat($r; "p99"), max: lat($r; "max")} ] as $res
	        | ([$res[] | select(.lost | not)]) as $surv
	        | ([$res[] | .node as $nd | .fail[] | {c: .c, b: $nd, v: .v}]) as $fail
	        # Two classes apart from the rest. pool_overflow: the driver`s own
	        # refusals, never sent. http_inflight_lost: sent, and without an
	        # outcome when the run was over. Neither is a reset or a status.
	        | ([$fail[] | select(.c == "pool_overflow")]) as $sat
	        | ([$fail[] | select(.c == "http_inflight_lost")]) as $inflight
	        | ([$fail[] | select(.c != "pool_overflow" and .c != "http_inflight_lost")]) as $mesh
	        | ($s.rps * $n) as $planned
	        | (if ($res | length) > 0 then ([$res[] | .rate // 0] | add) else null end) as $rate
	        | ($e.totals["benchmark.http_2xx"] // ([$res[].count] | add) // null) as $count
	        | ([$e.thresholds[]? | select(.pass == false) | select(.expr | sub("^counter:"; "") | zero_set | not) | "\(.expr) (actual \(.actual))"]
	           + [$e.thresholds[]? | select(.pass == false) | select(.expr | sub("^counter:"; "") | zero_set)
	              | select(($fail | length) == 0) | "\(.expr) (actual \(.actual))"]) as $thr
	        | ($e | capped) as $cap
	        | ([ (if $cap then "REFUSED AT THE EXECUTION CAP, nothing of this stage ran: \($e.error // "no reason given")"
	                + (if ($refb | length) > 0 then " [refused by: " + ($refb | map(.backend | node) | join(" ")) + "]" else "" end)
	              else empty end),
	             ($berr[] | "LOST BACKEND \(.backend | node) (\(.backend))"
	                + (if (.backend as $b | $res | any(.b == $b)) then " [counters reported]" else " [returned nothing]" end)),
	             (if $cap then empty
	              elif ($berr | length) > 0 and ($surv | length) > 0 then
	                "survivors " + ($surv | map("\(.node)=" + (if (.fail | length) == 0 and (.short | not) and (.over | not) and (.off | not) then "ok" else "FAIL" end)) | join(" "))
	              elif ($berr | length) > 0 then "no backend survived"
	              else empty end),
	             (if ($e.error // "") != "" and ($berr | length) == 0 and ($cap | not) then "error: \($e.error)" else empty end),
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
	             (if ($inflight | length) > 0 then
	                "in flight at the end: \([$inflight[].v] | add) request(s) with no outcome [" + ($inflight | map("\(.b)=\(.v)") | join(" ")) + "] (http_inflight_lost: sent or queued, neither answered nor reset when the run and its request timeout were over; in no other counter)"
	              else empty end),
	             (if ([$surv[] | select(.short)] | length) > 0 then
	                "http_2xx below \($floor)% of the planned \($plan1 | round) on [" + ([$surv[] | select(.short) | "\(.node)=\(.count)"] | join(" ")) + "]"
	              elif ($res | length) == 0 and ($berr | length) == 0 and ($e.error // "") == "" then "no per-backend results in the report (the 2xx count went unchecked)"
	              else empty end),
	             (if ([$surv[] | select(.over)] | length) > 0 then
	                "http_2xx above \(200 - $floor)% of the planned \($plan1 | round) on [" + ([$surv[] | select(.over) | "\(.node)=\(.count)"] | join(" ")) + "] (more than the plan asked for: not this plan`s load)"
	              else empty end),
	             (if ($thr | length) > 0 then "thresholds failed: " + ($thr | join("; ")) else empty end)
	           ]) as $why
	        | ([$res[].p50 | select(. != null)] | max) as $w50 | ([$res[].p99 | select(. != null)] | max) as $w99
	        | ([$res[].max | select(. != null)] | max) as $wmax
	        | {name: $s.name, ok: (($why | length) == 0), lost: ($berr | length), notrun: false, why: $why,
	           detail: ("rate=" + (if $rate == null then "?" else ($rate | r2 | tostring) end) + "/\($planned)rps"
	                    + (if $count == null then "" else " http_2xx=\($count)/\($planned * $dur | round)" end)
	                    + " backends=\(($surv | length))/\($n)"),
	           lat: (if $w50 == null and $w99 == null then "" else "lat p50<=\($w50 | fmt_ms) p99<=\($w99 | fmt_ms) max<=\($wmax | fmt_ms)" end),
	           nodes: ([$res[] | "NODE  \($s.name)  \(.node)  http_2xx=\(.count)/\($plan1 | round) rate=\(if .rate == null then "?" else (.rate | r2 | tostring) end)/\($s.rps)rps"
	                             + (if .started == null then "" else " started=\(.started)" end)
	                             + (if .p50 == null and .p99 == null then "" else " p50=\(.p50 | fmt_ms) p99=\(.p99 | fmt_ms) max=\(.max | fmt_ms)" end)
	                             + (if (.fail | length) == 0 then " failures=none" else " failures " + (.fail | map("\(.c)=\(.v)") | join(" ")) end)
	                             + (if .lost then " LOST" else "" end)]
	                  + [$berr[] | select(.backend as $b | $res | any(.b == $b) | not) | "NODE  \($s.name)  \(.backend | node)  LOST, returned nothing"]
	                  + [$refb[] | "NODE  \($s.name)  \(.backend | node)  REFUSED the start: the engine was at its execution cap"])}
	      end ] as $rows
	| ([.executions[].label] | unique - $want) as $extra
	# Every execution sortie marked not_run, in the share table or not.
	| ([.executions[] | select(.not_run // false)] | length) as $notrun
	# sortie`s own summary disagrees with the report in hand, or shows a SKIP.
	| (if $lognotrun > 0 or $logskips > 0 then
	     ["FAIL  sortie.log  -- sortie itself reports executions NOT RUN: \($logskips) SKIP line(s)"
	      + (if $logsummary != "" then ", summary \"\($logsummary)\"" else "" end)
	      + ". A stage that was skipped was not tested; this run does not pass"]
	   else [] end) as $logfail
	| ($rows[] | ((if .ok then "PASS  " else "FAIL  " end) + .name + (if .detail != "" then "  " + .detail else "" end)
	             + (if .ok then "  failures=none" else "  -- " + (.why | join(" | ")) end)
	             + (if (.lat // "") != "" then "  " + .lat else "" end)),
	            (if $pernode then .nodes[] else empty end)),
	  ($extra[] as $x | "FAIL  \($x)  -- in the report but not in the share table (a different plan?)"
	     + (if ($by[$x] | any(.not_run // false)) then " and NOT RUN" else "" end)),
	  $logfail[],
	  # Each lost backend once, with what sortie said about it: the error is the
	  # same for every target of an engine that went away.
	  ([.executions[] as $e | ($e.backend_errors // [])[] | select(refused_by($e) | not)] | group_by(.backend)[]
	   | "LOST  \(.[0].backend | node) (\(.[0].backend))  targets=\(length)  error: \(.[0].error)"),
	  # When the run was, to line it up with rolls: sortie`s own clock for the
	  # dispatch bracket, each engine`s for when its first worker started.
	  ([.executions[] | .started_at // empty] | min) as $t0
	  | ([.executions[] | .ended_at // empty] | max) as $t1
	  | ([.executions[] | (.results // [])[] | .started_at // empty]) as $eng
	  | (if $t0 == null then empty else
	       "WINDOW  dispatched=\($t0) ended=\($t1 // "?")"
	       + (if ($eng | length) > 0 then " engines_started=\($eng | min)..\($eng | max)" else "" end)
	     end),
	  (if .skipped_lines > 0 then "NOTE  \(.skipped_lines) line(s) of the results stream are not an execution (an unfinished or torn line) and were skipped" else empty end),
	  (if $haslog != "" and $logsummary == "" and .source == "report" then "NOTE  the sortie log has no summary line (\"N/M executions passed\"): it could not be checked for stages that were not run" else empty end),
	  ("VERDICT " + (if ([$rows[] | select(.ok | not)] | length) == 0 and ($extra | length) == 0 and $notrun == 0 and ($logfail | length) == 0 then "PASS" else "FAIL" end)
	   + " targets=\($rows | length) passed=\([$rows[] | select(.ok)] | length) failed=\(([$rows[] | select(.ok | not)] | length) + ($extra | length))"
	   + " not_run=\([$notrun, $lognotrun] | max)"
	   + " backends=\($n) lost_backends=\([.executions[] as $e | ($e.backend_errors // [])[] | select(refused_by($e) | not) | .backend] | unique | length)"
	   + " report_pass=\(if .pass == null then "absent" else .pass end)"
	   + (if .source == "stream" then " source=stream skipped_lines=\(.skipped_lines)" else "" end))
	' "$DOC" >"$OUT" || die "could not evaluate '$SOURCE'"
cat "$OUT"
grep -q '^VERDICT PASS ' "$OUT"
