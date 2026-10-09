#!/usr/bin/env bash
# Grades the prober SLI and the agents' unpinned-cluster counter over a run's
# window, from RAW counter samples (#1390, #1423). The same way every time: it
# prints the window, the queries it ran, the totals, every non-success by tier,
# result and source node, and one VERDICT line.
#
#   prober-grade.sh --dir RUN_DIR --prometheus http://prometheus.example:9090
#   prober-grade.sh --dir RUN_DIR --prometheus-service monitoring/prometheus:9090   # through the API server
#   prober-grade.sh --start 2026-10-08T00:10:00Z --window 8h --prometheus URL        # no run directory
#   ... --logs-file prober-fail-lines.txt     # cross-check against AETHER_PROBE_FAIL lines
#
# Why not increase(). The grade used to read
#   increase(aether_probe_requests_total{result!="success"}[8h])
# A failure series does not exist until the first failure creates it, and
# increase() needs two samples of a series: the count that creates a series is
# never counted. On 2026-10-08 it reported 37 mesh_dns timeouts; the raw counters
# read 21 + 19 = 40, and the prober had logged 40 AETHER_PROBE_FAIL lines.
# increase() also extrapolates to the window's edges, so even the rest is not a
# count.
#
# What it computes, per series (one label set: tier, target, result, pod, node):
#   the value at the window's start   one instant query at T0; a series that is
#                                     ABSENT there starts from 0
#   the time of that value            `timestamp(metric)` at T0: an instant query
#                                     answers with a series' LAST sample, up to
#                                     five minutes old, and its age is what tells
#                                     a series that was alive at T0 from one that
#                                     had already ended (see "A series with no
#                                     sample in the window")
#   every raw sample in the window    one instant query of `metric[<window>s]`
#                                     at the window's end, which returns the
#                                     samples themselves, of every series that
#                                     has any -- a pod that was replaced mid-run
#                                     included. (The agents' gauge is read the
#                                     same way, this one query only, from
#                                     FRESH_S before the window: see "The gauge
#                                     at the window's start".)
#   the count                         the sum of the steps from one sample to
#                                     the next. With no reset that is exactly
#                                     end minus start. A sample BELOW the one
#                                     before it is a counter reset (the process
#                                     restarted and counts from 0 again): the
#                                     new value is counted whole, and the reset
#                                     is printed.
# Then the counts are summed over pods, per tier and result.
#
# The lines:
#   WINDOW   start, end, seconds, and where T0 came from
#   EXPECT   only with --expect-tls-not-published: what was declared
#   QUERY    each query, with the time it was evaluated at: paste it to re-run
#   TOTAL    tier, result, count, rate per second, series
#   FAILED   one per (tier, result, source node) with a non-success count: the
#            pods and targets. `node` is the node the PROBER ran on, the source
#            of the request. Where the request went is not in the metric: it is
#            found through the access logs (README, "Grading").
#   BORN     a non-success series that did not exist at the window's start, with
#            its first sample: the case increase() gets wrong
#   RESET    a counter that went down, and the two values
#   GONE     a prober pod with no sample at the window's end: replaced or
#            deleted in the window, or silent since T0
#   ENDED    a prober pod the query at T0 still returned although it had
#            stopped before the window: not in it, and said so
#   PODS     prober pods at the start, at the end, gone and new in between
#   PROBER   verdict=PASS|FAIL|UNPROVEN and the sums
#   UNPINNED per node, per reason when it moved, then
#            verdict=PASS|FAIL|UNPROVEN: the increase of
#            aether_agent_identity_cluster_unpinned_total, graded per reason
#            (see "UNPINNED verdict"). The counter counts mesh cluster entries
#            with no server-identity pin (#832), on every snapshot, and rests
#            at zero since #1421. An agent restarts in every soak (two rolls),
#            so its counter resets, or a new series is born: both are counted
#            whole, as above. Since #1424 the counter has a `reason` label (why
#            the pin was empty), so there are four series per agent (three
#            before #1482, which added tls_not_published). Its lines:
#              UNPINNED node=       per node: the sum, series, born, resets
#              UNPINNED reason=     per reason that moved: count, nodes, and
#                                   class=gap|no_tls|unknown|unlabelled
#              UNPINNED moved:      each series that moved, first and last sample
#              UNPINNED steps:      WHEN it moved: the first six samples that
#                                   showed an increase, to hold against the
#                                   agent rolls in churn.log. A step happened
#                                   in the export interval before its sample.
#              UNPINNED published:  what the agent's gauge
#                                   aether_agent_snapshot_tls_clusters
#                                   {pin="unpinned"} (#1425) held: per node,
#                                   job and reason with a non-zero sample, how
#                                   many samples, the longest run of them, the
#                                   most entries in one sample, the value at
#                                   the window's end, and over how many series
#                                   (an agent that was replaced is two).
#                                   `absent:` and `not graded:` are its
#                                   coverage (see the verdict).
#              UNPINNED expected:   only with --expect-tls-not-published: a
#                                   tls_not_published state that stood, with
#                                   its seconds, entries and count; not a
#                                   failure
#              UNPINNED silent: / missing: / ended:   the counter's coverage
#            The acknowledged gauge (aether_agent_xds_acked_tls_clusters) is
#            not read.
#   LOGS     with --logs-file / --logs-url: AETHER_PROBE_FAIL lines in the
#            window (one per failed probe, plus the `suppressed` counts of the
#            capped minutes) against the counters, per tier and result:
#            match, MISMATCH, or UNPROVEN (see "A suppressed summary" below)
#   VERDICT  prober=, unpinned=, logs=
#
# PROBER verdict:
#   PASS      no non-success in the window, and the success counters moved
#   FAIL      at least one non-success. Not by itself a failed soak: it is the
#             list of what has to be attributed, per tier, result and node. The
#             bars are in README.md, "Grading" (liveness 0; the dns_* classes 0).
#   UNPROVEN  no prober series in the window, or no success counted: the SLI
#             saw nothing, and its zeros mean nothing.
# UNPINNED verdict (#1491). The reasons are a closed set and two kinds of fact,
# so each is graded for what it is:
#   no_namespace_metadata, pin_not_rendered   (class gap)
#       TLS was published and checks no server identity: the mTLS validation
#       gap. FAIL on any movement of the counter, and on any non-zero sample
#       of the gauge (a gap that stands through a window with no new snapshot
#       does not move the counter). The text names reason, count and nodes.
#   trust_domain_unknown, tls_not_published   (class no_tls)
#       the agent published NO TLS for those entries. Every agent start does
#       that for a moment, and a soak rolls the agents twice. Reported, with
#       count and nodes; not a failure by itself. FAIL only when the state
#       STOOD: a run of non-zero gauge samples that spans PENDING_S (300 s) or
#       more, anywhere in the window -- an agent that did not get its trust
#       domain or its SVID, or endpoints with no namespace metadata whose TLS
#       is not published (which is the gap, waiting). The counter cannot say
#       this: it adds the entry count on every snapshot, so its size is entries
#       times snapshots, not time. A run is of the node, job and reason, over
#       every series of them, as the alert rule's `sum by (job, node, reason)`
#       is: where the collector keeps a per-pod label, a state that outlives
#       the pod that first showed it is one run, not two.
#       tls_not_published also stands BY DESIGN on some clusters: a mesh run
#       without SPIRE never leaves it, and a healthy agent keeps a TCP floor
#       entry in it for as long as the floor is not published (not in the
#       capture set). The default is strict, it fails like the other; with
#       --expect-tls-not-published the operator declares it expected, and a
#       standing tls_not_published is then an `UNPINNED expected:` line, with
#       its seconds, entries and count, and not a failure. The reason does not
#       tell the two apart: with the switch, an agent that cannot get its SVID
#       reads the same and is not failed HERE (it reports NotReady after two
#       minutes; trust_domain_unknown is timed whatever the switch).
#   any other value                           (class unknown)
#       an agent newer than this script. FAIL on any movement of the counter
#       and on any non-zero sample of the gauge, like a gap reason: a reason
#       this script does not know is not taken for a harmless one. A series of
#       it that rests at zero in both is not a verdict.
#   no `reason` label                         (class unlabelled)
#       an agent from before #1424. Its one series holds every reason, and
#       whether TLS was published under what it counted is not in it: FAIL on
#       any movement, as before #1491 and with the same text. Such an agent
#       has no gauge either (`UNPINNED published: not graded:`).
# PASS = none of those. UNPROVEN = no series at all (the counter is seeded at
# zero, so absent means it does not reach this Prometheus); a node whose agent
# has no sample in the window (`UNPINNED silent:`, `UNPINNED missing:`): a sum
# over the nodes that did report is not the fleet's; or an exporter whose
# counter has the `reason` label and whose gauge has no sample in the window
# (`UNPINNED published: absent:`): the gauge is written on every snapshot,
# zeros included, so an absent one is not a zero, and how long a state stood
# there was not seen. An exporter is every label of its counter series but
# `reason`, and a gauge series is its own when the two agree on every label
# both carry (each exporter by its own labels: a job without a per-pod label
# does not change the key of one that has it). With a per-pod label, then, a
# replaced agent that exports its seeded counter and never sets a snapshot is
# not covered by the gauge of the pod before it. Where restarts fall into one
# series (no per-pod label), the counter says when the process started again
# -- a series born in the window, or a reset -- and the gauge must have a
# sample from then on. (A reset is only seen when the new value is below the
# old one: a counter at rest at zero across a restart shows none.)
#
# The gauge at the window's start. The value at T0 is the last sample at or
# before T0, as for the counters. `gauge[<window>s]` at the window's end does
# not return it: a range selector is left-open since Prometheus 3.0, (T0, end],
# so even a sample exactly at T0 is left out (before 3.0 it was [T0, end]), and
# a sample from before T0 is in neither. A state already standing at T0 would
# be read a sample short. So the range reaches FRESH_S (120 s, two export
# intervals) and one second before the window -- the second so that a sample
# exactly FRESH_S old is returned by a left-open range too. The last sample at
# or before T0, at most FRESH_S old, is the state AT T0 and is counted as a
# sample at T0, and anything older is dropped. A run
# therefore starts at T0 at the earliest: how long a state stood before the
# window is not graded. The same on either side of 3.0.
# What the gauge cannot show: a state shorter than one export interval (a
# minute) may be in no sample, so a run is a LOWER bound of how long it stood;
# a run is not carried across more than STALE_S (300 s) without a sample; and
# a state still non-zero at the window's end for less than PENDING_S is an
# agent start that the window cut (said on its `published:` line, not graded).
#
# A series with no sample in the window (#1469). The window query returns only
# series that have a sample in it, so a series the query at T0 returned and the
# window query did not went silent -- but when? The query at T0 answers with a
# series' last sample, up to five minutes old (the lookback), so there are two
# cases, told apart by the AGE of that sample:
#   at most FRESH_S (120 s: two export intervals of one minute) old at T0
#       the exporter was alive at T0 and then exported NOTHING for the window.
#       A prober pod is in PODS at_start and is GONE. An agent: when no other
#       series of the same node and job has a sample in the window (a
#       replacement pod), that node's counter was not seen at all and the gate
#       is UNPROVEN, `UNPINNED silent: node=... job=...`.
#   older than that
#       it had stopped before T0 (a pod replaced a few minutes before the run)
#       and is not part of the window: an ENDED line (`UNPINNED ended:` for an
#       agent), never a verdict.
# One more check needs no T0 sample: a node whose PROBER has samples in the
# window while no series of the unpinned counter has any, under any job, is
# `UNPINNED missing:`, UNPROVEN -- an agent that was already silent five
# minutes before T0.
# What this cannot see, from Prometheus alone: an agent silent since more than
# FRESH_S before T0 on a node with no prober, or on a node where another job
# exports the same counter (the edge does); an exporter that was alive at T0
# with a last export more than FRESH_S old (a stalled exporter: it reads as
# ended before the window); a pod that died in the FRESH_S before T0 reads as
# alive at T0 (a GONE line for a prober; for an agent only when nothing replaced
# it); and a node that reports for part of the window only is covered for that
# part (its per-node line says how many series, not for how long).
#
# Options:
#   --dir DIR              the run directory of run.sh. T0 is the churn driver's
#                          start line in DIR/churn.log (window 8h); without a
#                          churn log (an e2e run), T_LOAD and DURATION_S of
#                          DIR/run.env. Also gives --context (CTX in run.env).
#   --start T              the window's start: epoch seconds, or anything
#                          `date -d` reads (2026-10-08T00:10:00Z). Overrides DIR.
#   --window D             Ns, Nm or Nh (default 8h with a churn log)
#   --end T                instead of --window
#   --prometheus URL       the Prometheus HTTP API's base URL. REQUIRED, this or
#   --prometheus-service NS/SVC:PORT
#                          the same API through the Kubernetes API server
#                          (`kubectl get --raw .../services/SVC:PORT/proxy/...`),
#                          with --context NAME. There is no default: this script
#                          does not know where any cluster keeps its metrics.
#   --context NAME         kubeconfig context for --prometheus-service
#   --match 'k="v",...'    extra label matchers for both metrics (a Prometheus
#                          that holds more than one cluster)
#   --node-label NAME      the label that holds the node (default `node`)
#   --job-label NAME       the label that tells one exporter of the unpinned
#                          counter on a node from another (default `job`)
#   --expect-tls-not-published
#                          declares that a standing `tls_not_published` is
#                          expected on this cluster (a mesh without SPIRE, or
#                          TCP floors that are not published): reported, not a
#                          failure. Off by default. See "UNPINNED verdict".
#   --logs-file FILE       prober log lines (`kubectl logs`, or an export): every
#                          line holding `AETHER_PROBE_FAIL {...}` is read
#   --logs-url URL         a VictoriaLogs base URL; /select/logsql/query is asked
#                          for --logs-query (default `"AETHER_PROBE_FAIL"`) over
#                          the window
#   --logs-query Q         narrow it to this cluster's prober (README, "Grading")
#
# The values at the window's two ends are the last ones Prometheus has at or
# before them: a failure in the seconds before T0 that was exported after it is
# counted in the window. The prober exports once a minute.
#
# A suppressed summary. A detail line's `t` is the failure's own time. A
# `suppressed` summary's `t` is when its window was CLOSED: the failures it
# counts happened before that, since the window opened -- at a failure -- and
# it is closed by the first once-a-minute flush that finds it a minute old,
# less than two `window_s` (2 x 60 s) later. So a summary whose window opened
# before an end of the run's window and was closed after it may count failures
# on both sides of that end, and nothing on the line says how many on each.
# Such a summary is printed (`LOGS    boundary:`) and kept out of the sum; when
# the counters lie between the sum without it and the sum with it, the row is
# UNPROVEN, not a match and not a MISMATCH.
# Where the window opened: a prober since #1463 says so, `window_start`, and
# that is the span. An older prober does not, and the span is taken as the two
# `window_s` before `t`, the most it can be. That prober also stamps the
# summary it writes when it STOPS one `window_s` in the future (#1463): a
# summary dated up to a minute after its pod's last line is that, not a clock
# error. Both stay handled for as long as a log can hold an older prober's
# lines -- across a roll from one to the other, the two are in one log. (For
# that one shutdown summary the two-window span is a minute short at its early
# side; it matters only for a prober stopped within three minutes after T0.)
# Whatever the prober, --logs-url asks the store for two minutes past the
# window's end: a window opened by a failure in the run's last second is closed,
# and its summary dated, up to two minutes after it.
#
# Exit: 0 every verdict PASS (and the logs match, when asked); 1 a FAIL with
# everything else proven; 2 anything UNPROVEN, a log count that does not match
# or cannot be told, a query that failed, or unusable arguments. 2 comes first:
# a FAIL beside a count that cannot be trusted is not yet a list of failures
# to attribute. Needs bash, jq, date and curl (or kubectl). PROBER_GRADE_CURL and
# PROBER_GRADE_KUBECTL replace them (the test hooks); //e2e/soak:harness_test
# runs it on canned query responses.
set -uo pipefail

PROBE_METRIC="aether_probe_requests_total"
UNPINNED_METRIC="aether_agent_identity_cluster_unpinned_total"
# The agent's gauge of what its last snapshot published (#1425): how many
# cluster entries have no pin, per `reason`, now. The counter says THAT a state
# was reported, summed over snapshots; only this says for how long it stood.
PUBLISHED_METRIC="aether_agent_snapshot_tls_clusters"
# The counter's `reason` label is a closed set
# (agent/internal/xds/cache/cachemetrics/cachemetrics.go, UnpinnedCause), and
# its two halves are different facts. GAP: TLS was published and checks no
# server identity -- the mTLS validation gap. NO_TLS: the agent published no
# TLS for the entry at all -- expected for a moment at an agent start. A value
# in neither list is a reason this script does not know: it fails the gate.
GAP_REASONS="no_namespace_metadata,pin_not_rendered"
NO_TLS_REASONS="trust_domain_unknown,tls_not_published"
# How long a NO_TLS state may stand, in the gauge's samples, before it is no
# longer an agent start. The same number, for the same reasons, as `for: 5m` of
# AetherMeshClusterPinPending (docs/observability/agent-pin-alerts.yml): an
# agent without its SVID after 2 minutes reports NotReady, plus two export
# intervals to see the state twice past that, plus one for jitter.
PENDING_S=300
# A series whose last sample is older than this at the window's end has ended
# (Prometheus's own lookback for an instant query).
STALE_S=300
# A series whose sample at the window's start is older than this had stopped
# exporting before the window: two export intervals (the prober and the agent
# export once a minute; on a test cluster the samples were 0 to 55 s old).
FRESH_S=120
# How long after a failure its `suppressed` summary can be written: two of the
# prober's fail-log windows (prober/internal/prober/faillog.go, failLogWindow).
# The log store is asked for this much past the window's end. True of every
# prober, for the summaries its once-a-minute flush writes. Not covered, and
# only for a prober from before #1463: the summary it writes when it stops is
# dated a window late, so one stopped in the two minutes after the window's
# end can date it up to three minutes after, and its failures can lie up to
# three windows before its `t`, not two.
LOG_EDGE_S=120

DIR="" START="" END="" WINDOW="" PROM_URL="" PROM_SVC="" CTX="" MATCH="" NODE_LABEL="node" JOB_LABEL="job"
LOGS_FILE="" LOGS_URL="" LOGS_QUERY='"AETHER_PROBE_FAIL"'
# The NO_TLS reasons the operator declared expected to stand on this cluster
# (--expect-tls-not-published). Empty: the strict default, each of them is timed.
EXPECTED_STANDING=""
CURL="${PROBER_GRADE_CURL:-curl}"
KUBECTL="${PROBER_GRADE_KUBECTL:-kubectl}"
die() {
	echo "prober-grade.sh: $*" >&2
	exit 2
}
while [ $# -gt 0 ]; do
	case "$1" in
	--dir | --start | --end | --window | --prometheus | --prometheus-service | --context | --match | --node-label | --job-label | --logs-file | --logs-url | --logs-query)
		[ $# -ge 2 ] || die "$1 needs a value"
		case "$1" in
		--dir) DIR="$2" ;;
		--start) START="$2" ;;
		--end) END="$2" ;;
		--window) WINDOW="$2" ;;
		--prometheus) PROM_URL="$2" ;;
		--prometheus-service) PROM_SVC="$2" ;;
		--context) CTX="$2" ;;
		--match) MATCH="$2" ;;
		--node-label) NODE_LABEL="$2" ;;
		--job-label) JOB_LABEL="$2" ;;
		--logs-file) LOGS_FILE="$2" ;;
		--logs-url) LOGS_URL="$2" ;;
		--logs-query) LOGS_QUERY="$2" ;;
		esac
		shift 2
		;;
	--expect-tls-not-published)
		EXPECTED_STANDING="tls_not_published"
		shift
		;;
	-h | --help)
		sed -n '2,/^set -uo pipefail$/p' "$0" | sed '$d'
		exit 0
		;;
	*) die "unknown argument '$1' (see the header of $0)" ;;
	esac
done
command -v jq >/dev/null 2>&1 || die "jq not found on PATH"

to_epoch() { # epoch seconds as they are; anything else through date -d
	case "$1" in
	'' | *[!0-9]*) date -u -d "$1" +%s 2>/dev/null ;;
	*) echo "$1" ;;
	esac
}
iso() { date -u -d "@$1" +%FT%TZ; }
seconds() { # Ns, Nm, Nh or a bare number of seconds
	[[ "$1" =~ ^([0-9]+)([hms]?)$ ]] || return 1
	case "${BASH_REMATCH[2]}" in
	h) echo $((10#${BASH_REMATCH[1]} * 3600)) ;;
	m) echo $((10#${BASH_REMATCH[1]} * 60)) ;;
	*) echo $((10#${BASH_REMATCH[1]})) ;;
	esac
}

# --- the window ------------------------------------------------------------------
T0_FROM="--start"
if [ -n "$DIR" ]; then
	[ -d "$DIR" ] || die "--dir $DIR is not a directory"
	if [ -r "$DIR/run.env" ] && [ -z "$CTX" ]; then
		CTX="$(sed -n 's/^CTX=//p' "$DIR/run.env" | tail -n 1)"
	fi
	if [ -z "$START" ]; then
		if [ -r "$DIR/churn.log" ]; then
			START="$(sed -n '1s/^.* churn driver start T0=\([^ ]*\) .*$/\1/p' "$DIR/churn.log")"
			[ -n "$START" ] || die "the first line of $DIR/churn.log is not a churn driver start line (no T0=): pass --start"
			T0_FROM="churn.log"
			[ -n "$WINDOW$END" ] || WINDOW=8h
		elif [ -r "$DIR/run.env" ]; then
			START="$(sed -n 's/^T_LOAD=//p' "$DIR/run.env" | tail -n 1)"
			[ -n "$START" ] || die "$DIR has no churn.log and its run.env has no T_LOAD: pass --start"
			T0_FROM="run.env T_LOAD"
			[ -n "$WINDOW$END" ] || WINDOW="$(sed -n 's/^DURATION_S=//p' "$DIR/run.env" | tail -n 1)"
		else
			die "$DIR has neither churn.log nor run.env: pass --start"
		fi
	fi
fi
[ -n "$START" ] || die "no window: pass --dir RUN_DIR, or --start with --window or --end"
START_S="$(to_epoch "$START")"
[ -n "$START_S" ] || die "cannot read --start '$START' as a time"
if [ -n "$END" ]; then
	END_S="$(to_epoch "$END")"
	[ -n "$END_S" ] || die "cannot read --end '$END' as a time"
else
	[ -n "$WINDOW" ] || die "no window length: pass --window (Ns, Nm, Nh) or --end"
	W="$(seconds "$WINDOW")" || die "--window takes Ns, Nm or Nh (got '$WINDOW')"
	END_S=$((START_S + W))
fi
W=$((END_S - START_S))
[ "$W" -gt 0 ] || die "the window ends ($(iso "$END_S")) before it starts ($(iso "$START_S"))"
NOW_S="$(date +%s)"
if [ "$END_S" -gt "$NOW_S" ]; then
	die "the window ends at $(iso "$END_S"), $((END_S - NOW_S)) s from now: a grade of a window that is not over is a grade of part of it"
fi

# --- the endpoint ----------------------------------------------------------------
if [ -n "$PROM_URL" ] && [ -n "$PROM_SVC" ]; then die "pass --prometheus or --prometheus-service, not both"; fi
if [ -z "$PROM_URL" ] && [ -z "$PROM_SVC" ]; then
	die "no Prometheus: pass --prometheus URL, or --prometheus-service NS/SVC:PORT with --context (there is no default endpoint)"
fi
if [ -n "$PROM_SVC" ]; then
	[[ "$PROM_SVC" =~ ^[^/]+/[^/:]+:[^/:]+$ ]] || die "--prometheus-service takes NS/SVC:PORT (got '$PROM_SVC')"
	[ -n "$CTX" ] || die "--prometheus-service needs --context NAME (or a --dir whose run.env has CTX): never the current-context (#951)"
fi

TMPD="$(mktemp -d "${TMPDIR:-/tmp}/prober-grade.XXXXXX")" || die "mktemp failed"
trap 'rm -rf "$TMPD"' EXIT

# pq <name> <promql> <time> <out> <vector|matrix>: one instant query. A query that
# fails is not a zero: the grade stops here, UNPROVEN.
pq() {
	local name="$1" q="$2" t="$3" out="$4" want="$5" enc rc
	echo "QUERY   $name  time=$(iso "$t")  $q"
	if [ -n "$PROM_URL" ]; then
		"$CURL" -fsS --max-time 120 --get "${PROM_URL%/}/api/v1/query" \
			--data-urlencode "query=$q" --data-urlencode "time=$t" >"$out" 2>"$TMPD/err"
		rc=$?
	else
		enc="$(jq -rn --arg q "$q" '$q | @uri')"
		"$KUBECTL" --context "$CTX" get --raw \
			"/api/v1/namespaces/${PROM_SVC%%/*}/services/${PROM_SVC#*/}/proxy/api/v1/query?query=$enc&time=$t" >"$out" 2>"$TMPD/err"
		rc=$?
	fi
	if [ "$rc" -ne 0 ]; then
		echo "VERDICT UNPROVEN the query '$name' failed (exit $rc): $(tr '\n' ' ' <"$TMPD/err" | cut -c1-300)"
		exit 2
	fi
	if ! jq -e --arg want "$want" '.status == "success" and .data.resultType == $want' "$out" >/dev/null 2>&1; then
		echo "VERDICT UNPROVEN the query '$name' did not return a $want: $(head -c 300 "$out" | tr '\n' ' ')"
		exit 2
	fi
}

# One record per series. $start: the instant query at the window's start; $at:
# the time of each of its samples (timestamp()); $win: the samples in the
# window. A series with a sample in the window is counted, as before. A series
# of $start with NO sample in the window is a record too, `silent`, counting
# nothing: `fresh` when its sample at the start is at most $fresh seconds old
# (alive at T0), not fresh when it is older or its time is not known (it had
# ended before the window). See "A series with no sample in the window".
# $keep: also keep WHEN the counter stepped (`steps`: the sample that showed
# each increase, and by how much). Asked for the unpinned counter only: a
# prober's success counter steps at every sample.
# shellcheck disable=SC2016 # jq variables, not the shell's
JQ_SERIES='
def key: del(.__name__) | [to_entries[] | "\(.key)=\(.value)"] | sort | join(",");
($start[0].data.result | map({key: (.metric | key), value: (.value[1] | tonumber)}) | from_entries) as $base
| ($at[0].data.result | map({key: (.metric | key), value: (.value[1] | tonumber)}) | from_entries) as $seen
| ($win[0].data.result | map({key: (.metric | key), value: true}) | from_entries) as $inwin
| [ $win[0].data.result[]
    | . as $s
    | $base[$s.metric | key] as $b
    | (reduce $s.values[] as $p ({prev: ($b // 0), inc: 0, resets: [], steps: []};
        ($p[1] | tonumber) as $v
        | (if $v < .prev then $v else $v - .prev end) as $d
        | if $v < .prev then .resets += [{at: $p[0], before: .prev, after: $v}] else . end
        | .inc += $d
        | if $keep and $d > 0 then .steps += [{t: $p[0], d: $d}] else . end
        | .prev = $v)) as $w
    | { labels: ($s.metric | del(.__name__)), born: ($b == null), silent: false, fresh: true,
        first: {t: $s.values[0][0], v: ($s.values[0][1] | tonumber)},
        last: {t: $s.values[-1][0], v: ($s.values[-1][1] | tonumber)},
        inc: $w.inc, resets: $w.resets, steps: $w.steps } ]
+ [ $start[0].data.result[]
    | (.metric | key) as $k
    | select($inwin[$k] | not)
    | $seen[$k] as $t
    | { labels: (.metric | del(.__name__)), born: false, silent: true,
        fresh: ($t != null and $t0 - $t <= $fresh),
        first: {t: $t, v: (.value[1] | tonumber)}, last: {t: $t, v: (.value[1] | tonumber)},
        inc: 0, resets: [], steps: [] } ]'

SEL=""
[ -z "$MATCH" ] || SEL="{$MATCH}"
echo "WINDOW  start=$(iso "$START_S") end=$(iso "$END_S") seconds=$W  (T0 from $T0_FROM)"
if [ -n "$PROM_URL" ]; then
	echo "SOURCE  ${PROM_URL%/}/api/v1/query"
else
	echo "SOURCE  kubectl --context $CTX get --raw /api/v1/namespaces/${PROM_SVC%%/*}/services/${PROM_SVC#*/}/proxy/api/v1/query"
fi
if [ -n "$EXPECTED_STANDING" ]; then
	echo "EXPECT  reason=$EXPECTED_STANDING may stand on this cluster (declared with --expect-tls-not-published): reported with how long it stood, not a failure"
fi
pq probe/start "$PROBE_METRIC$SEL" "$START_S" "$TMPD/p0.json" vector
pq probe/start-time "timestamp($PROBE_METRIC$SEL)" "$START_S" "$TMPD/pt.json" vector
pq probe/window "$PROBE_METRIC${SEL}[${W}s]" "$END_S" "$TMPD/pw.json" matrix
pq unpinned/start "$UNPINNED_METRIC$SEL" "$START_S" "$TMPD/u0.json" vector
pq unpinned/start-time "timestamp($UNPINNED_METRIC$SEL)" "$START_S" "$TMPD/ut.json" vector
pq unpinned/window "$UNPINNED_METRIC${SEL}[${W}s]" "$END_S" "$TMPD/uw.json" matrix
# FRESH_S more than the window, and a second: the state AT the window's start
# is a sample from before it, and a range selector leaves out a sample exactly
# at its start since Prometheus 3.0 -- here, one exactly FRESH_S old. See "The
# gauge at the window's start" in the header.
pq unpinned/published "$PUBLISHED_METRIC{pin=\"unpinned\"${MATCH:+,$MATCH}}[$((W + FRESH_S + 1))s]" "$END_S" "$TMPD/gw.json" matrix
jq -n --slurpfile start "$TMPD/p0.json" --slurpfile at "$TMPD/pt.json" --slurpfile win "$TMPD/pw.json" \
	--argjson t0 "$START_S" --argjson fresh "$FRESH_S" --argjson keep false "$JQ_SERIES" >"$TMPD/p.json" || die "could not read the prober samples"
jq -n --slurpfile start "$TMPD/u0.json" --slurpfile at "$TMPD/ut.json" --slurpfile win "$TMPD/uw.json" \
	--argjson t0 "$START_S" --argjson fresh "$FRESH_S" --argjson keep true "$JQ_SERIES" >"$TMPD/u.json" || die "could not read the unpinned-cluster samples"

# --- the prober ------------------------------------------------------------------
# shellcheck disable=SC2016
jq -r --arg node "$NODE_LABEL" --argjson w "$W" --argjson start "$START_S" --argjson end "$END_S" --argjson stale "$STALE_S" --argjson fresh "$FRESH_S" '
def l($k): .labels[$k] // "-";
def iso: if . == null then "unknown" else floor | todate end;
def n: if . == floor then floor else . end;
def age: if . == null then "of unknown age" else "\($start - . | floor) s old" end;
map(select(.silent | not)) as $all
| map(select(.silent and .fresh)) as $quiet
| (map(select(.silent and (.fresh | not))) | group_by(l("pod")) | map({pod: .[0].labels.pod, node: (.[0] | l($node)), last: (map(.last.t) | max)})
    | map(select(.pod as $p | ($all + $quiet) | any(.[]; .labels.pod == $p) | not))) as $before
| ($all | map(select(l("result") != "success"))) as $bad
| ($bad | map(.inc) | add // 0) as $nbad
| ($all | map(select(l("result") == "success") | .inc) | add // 0) as $ok
| ($all + $quiet | group_by(l("pod")) | map({pod: .[0].labels.pod, node: (.[0] | l($node)), counted: any(.[]; .silent | not),
    start: any(.[]; .born | not), end: any(.[]; (.silent | not) and .last.t >= $end - $stale), last: (map(.last.t) | max)})) as $pods
| ( $all | group_by([l("tier"), l("result")])[]
    | "TOTAL   tier=\(.[0] | l("tier")) result=\(.[0] | l("result")) count=\(map(.inc) | add | n) rate=\((map(.inc) | add) / $w * 100 | round / 100)/s series=\(length)" ),
  ( $bad | map(select(.inc > 0)) | group_by([l("tier"), l("result"), l($node)])[]
    | "FAILED  tier=\(.[0] | l("tier")) result=\(.[0] | l("result")) node=\(.[0] | l($node)) count=\(map(.inc) | add | n) pods=\(map(l("pod")) | unique | join(",")) targets=\(map(l("target")) | unique | join(","))" ),
  ( $bad[] | select(.born)
    | "BORN    tier=\(l("tier")) result=\(l("result")) node=\(l($node)) pod=\(l("pod")) target=\(l("target")) first=\(.first.v | n)@\(.first.t | iso) count=\(.inc | n)  (absent at the start: counted from 0; increase() cannot count the \(.first.v | n) that created it)" ),
  ( $all[] | . as $s | .resets[]
    | "RESET   tier=\($s | l("tier")) result=\($s | l("result")) node=\($s | l($node)) pod=\($s | l("pod")) at=\(.at | iso) before=\(.before | n) after=\(.after | n)  (counted: everything up to \(.before | n), and the \(.after | n) after it)" ),
  ( $pods[] | select(.end | not)
    | "GONE    pod=\(.pod) node=\(.node) last_sample=\(.last | iso)  "
      + (if .counted then "(its counts up to then are in the totals)"
         else "(alive at the start, its sample there \(.last | age), and no sample in the window: nothing of it is in the totals)" end) ),
  ( $before[]
    | "ENDED   pod=\(.pod) node=\(.node) last_sample=\(.last | iso)  (the query at the start still returned it, with a sample \(.last | age): more than \($fresh) s, so it had stopped before the window and is not in it)" ),
  "PODS    at_start=\($pods | map(select(.start)) | length) at_end=\($pods | map(select(.end)) | length) gone=\($pods | map(select(.end | not)) | length) new=\($pods | map(select(.start | not)) | length) nodes=\($pods | map(.node) | unique | length)",
  "PROBER  verdict=\(if ($all | length) == 0 or $ok == 0 then "UNPROVEN" elif $nbad > 0 then "FAIL" else "PASS" end) non_success=\($nbad | n) liveness_non_success=\($bad | map(select(l("tier") == "liveness") | .inc) | add // 0 | n) dns_class_non_success=\($bad | map(select(l("result") | startswith("dns_")) | .inc) | add // 0 | n) success=\($ok | n) series=\($all | length) born_in_window=\($bad | map(select(.born)) | length) resets=\($all | map(.resets | length) | add // 0)"
  + (if ($all | length) == 0 then "  (no prober series in the window: is --match right, and is this the Prometheus the prober reports to?)"
     elif $ok == 0 then "  (no success was counted: the SLI saw nothing, and its zeros mean nothing)" else "" end)
' "$TMPD/p.json" >"$TMPD/p.out" || die "could not grade the prober samples"
cat "$TMPD/p.out"
P_VERDICT="$(sed -n 's/^PROBER  verdict=\([A-Z]*\) .*/\1/p' "$TMPD/p.out")"

# --- the unpinned-cluster counter (#1423) ----------------------------------------
# One series per node up to #1424; one per node and `reason` since (four
# reasons, each seeded at zero; three before #1482). Every series is counted,
# whatever its labels; what its count MEANS depends on its reason (#1491), and
# that is its class:
#   gap         a reason of GAP_REASONS: TLS published without its pin
#   no_tls      a reason of NO_TLS_REASONS: no TLS published at all
#   unknown     a `reason` this script does not know
#   unlabelled  no `reason` label: an agent from before #1424, printed reason=-
# See "UNPINNED verdict" in the header for what each does to the verdict.
#
# How long a state stood is not in the counter: it adds the number of unpinned
# entries on EVERY snapshot, so 200 is two entries on a hundred snapshots or a
# hundred entries on two. The gauge's samples say it: a run of consecutive
# non-zero samples (no two more than STALE_S apart, the continuity Prometheus
# itself gives an alert's `for:`), from its first sample to its last. That is
# a LOWER bound, at the export interval's resolution: a state shorter than one
# interval may be in no sample at all, which is why the counter stays the
# measure of whether it happened. The runs are of a node, job and reason, over
# every series of them (a replaced pod is a new series, not a new state).
#
# Coverage (#1469): the sum is the fleet's only when every node's agent was
# seen. `silent`: an exporter (node and job) that was alive at the window's
# start and has no sample in the window, under any pod or reason. `missing`: a
# node whose prober reported in the window and whose counter did not. Either
# makes the gate UNPROVEN. `ended`: one that had stopped before the window; said,
# not graded. And the gauge's coverage (#1491): an exporter whose counter has
# the `reason` label and whose gauge has no sample in the window (or none since
# its counter started again) is UNPROVEN too -- both arrived in the same agent
# (#1424, #1425), the gauge is written on every snapshot, zeros included, so an
# absent one is not a zero.
# shellcheck disable=SC2016
jq -r --arg node "$NODE_LABEL" --arg job "$JOB_LABEL" --argjson start "$START_S" --argjson end "$END_S" \
	--argjson fresh "$FRESH_S" --argjson stale "$STALE_S" --argjson pending "$PENDING_S" \
	--arg gap "$GAP_REASONS" --arg notls "$NO_TLS_REASONS" --arg gauge "$PUBLISHED_METRIC" --arg expected "$EXPECTED_STANDING" \
	--slurpfile probe "$TMPD/p.json" --slurpfile pub "$TMPD/gw.json" '
def l($k): .labels[$k] // "-";
def iso: if . == null then "unknown" else floor | todate end;
def n: if . == floor then floor else . end;
def age: if . == null then "of unknown age" else "\($start - . | floor) s old" end;
def unit: [l($node), l($job)];
def nodes: map(l($node)) | unique | join(",");
def cls: .labels.reason as $r
  | if $r == null then "unlabelled"
    elif ($gap | split(",") | index($r)) != null then "gap"
    elif ($notls | split(",") | index($r)) != null then "no_tls"
    else "unknown" end;
# The runs of consecutive non-zero samples in a list of [time, value].
def runs: reduce .[] as $p ({done: [], cur: null};
    if $p[1] > 0 and .cur != null and $p[0] - .cur.to <= $stale
    then .cur.to = $p[0] | .cur.n += 1 | .cur.max = ([.cur.max, $p[1]] | max)
    else (if .cur != null then .done += [.cur] | .cur = null else . end)
      | (if $p[1] > 0 then .cur = {from: $p[0], to: $p[0], n: 1, max: $p[1]} else . end)
    end)
  | .done + (if .cur != null then [.cur] else [] end);
map(select(.silent | not)) as $all
| ($all | map(unit) | unique) as $covered
| (map(select(.silent)) | group_by(unit) | map({node: (.[0] | l($node)), job: (.[0] | l($job)), unit: (.[0] | unit),
      fresh: any(.[]; .fresh), series: length, last: (map(.last.t) | max)})
    | map(select(.unit as $u | $covered | index([$u]) | not))) as $lost
| ($lost | map(select(.fresh))) as $silent
| ($lost | map(select(.fresh | not))) as $ended
| ([$probe[0][] | select(.silent | not) | l($node)] | unique
    | map(select(. as $n | ($all | any(.[]; l($node) == $n) | not) and ($silent | any(.[]; .node == $n) | not)))) as $missing
# One record per gauge series: its samples in the window, the first of them the
# state AT the start -- the last sample at or before T0 and at most FRESH_S
# old (the query reaches a second further), placed at T0. Header, "The gauge at the start".
| [ $pub[0].data.result[]
    | (.values | map([.[0], (.[1] | tonumber)])) as $v
    | ($v | map(select(.[0] <= $start and .[0] >= $start - $fresh)) | last) as $at0
    | ((if $at0 != null then [[$start, $at0[1]]] else [] end) + ($v | map(select(.[0] > $start)))) as $vals
    | select(($vals | length) > 0)
    | {labels: (.metric | del(.__name__)), values: $vals, own: ($vals | runs), last: {t: $vals[-1][0], v: $vals[-1][1]}} ] as $g
| ($g | map(unit) | unique) as $gunits
# Per node, job and reason, over EVERY series of it: an agent that is replaced
# is a new series where the collector keeps a per-pod label, and the state does
# not end with the pod. The time line is every sample of every such series.
# The state at a sample is non-zero when that sample is, or when the sample
# lies inside a run of another series of the group: a zero from a pod that runs
# beside one that says non-zero before and after it is not the end of the
# state. The runs are the runs of that time line.
| ($g | group_by([l($node), l($job), l("reason")])
    | map(. as $ss
        | [$ss[].own[]] as $iv
        | ([$ss[].values[]] | sort_by(.[0])
            | map(. as $p | if $p[1] > 0 then $p
                else ([$iv[] | select(.from <= $p[0] and $p[0] <= .to) | .max] | max) as $m
                  | if $m == null then $p else [$p[0], $m] end end)
            | runs) as $r
        | {node: ($ss[0] | l($node)), job: ($ss[0] | l($job)), reason: ($ss[0] | l("reason")), class: ($ss[0] | cls),
           series: ($ss | length), nonzero: ($ss | map(.own[].n) | add // 0), longest: ($r | map(.to - .from) | max // 0),
           max: ($ss | map(.own[].max) | max // 0), at_end: ($ss | map(select(.last.t >= $end - $stale) | .last.v) | add // 0)})
    | map(select(.nonzero > 0))) as $held
| ($held | map(select(.class != "no_tls"))) as $gheld
| ($held | map(select(.class == "no_tls" and .longest >= $pending))) as $stood
| ($expected | split(",")) as $exp
| ($stood | map(select(.reason as $r | $exp | index($r) | not))) as $standing
| ($stood | map(select(.reason as $r | $exp | index($r)))) as $declared
# The coverage of the gauge, per EXPORTER: every label of a counter series but
# `reason`. A gauge series is that exporter when it agrees with it on every
# label the two series both carry (`pin` and `reason` aside): each exporter is
# matched on its OWN labels, so one job without a per-pod label does not take
# the pod out of the key of another. With a per-pod label a replaced agent is
# therefore not covered by the gauge of the one before it. Where restarts fall
# into one series, the counter says when the process started again (born in
# the window, or a reset), and the gauge must have a sample from then on.
| ($all | map(select(.labels | has("reason")))) as $lab
| def ident: .labels | del(.reason, .pin);
  ($lab | group_by(ident)
    | map(. as $ss | ($ss[0] | ident) as $id
        | ([$ss[] | (if .born then .first.t else empty end), .resets[].at] | max) as $since
        | ($g | map(select(ident as $c | all($id | to_entries[]; ($c[.key] // .value) == .value)))) as $mine
        | select($mine | any(.[]; .values | any(.[]; .[0] >= ($since // $start))) | not)
        | {node: ($ss[0] | l($node)), job: ($ss[0] | l($job)), unit: ($ss[0] | unit),
           more: ($id | to_entries | map(select(.key != $node and .key != $job) | " \(.key)=\(.value)") | join("")),
           since: (if ($mine | length) > 0 then $since else null end)})) as $nogauge
| ($all | map(unit) | unique | map(select(. as $u | ($gunits | index([$u]) | not) and ($nogauge | any(.[]; .unit == $u) | not)))) as $ungraded
| $all
| (map(.inc) | add // 0) as $inc
| (map(select(.inc > 0))) as $moved
| def inc($c): $moved | map(select(cls == $c) | .inc) | add // 0;
  def by_reason($c): $moved | map(select(cls == $c)) | group_by(l("reason"))
    | map("reason=\(.[0] | l("reason")) count=\(map(.inc) | add | n) nodes=\(nodes)") | join(", ");
  def gauge_of($c): $gheld | map(select(.class == $c) | "reason=\(.reason) node=\(.node) published=\(.max | n)") | join(", ");
  def said: "reason=\(.reason) node=\(.node) seconds=\(.longest | floor) published=\(.max | n)";
  def counted: . as $h | $moved | map(select(l($node) == $h.node and l($job) == $h.job and l("reason") == $h.reason) | .inc) | add // 0;
  (inc("gap") > 0 or inc("unknown") > 0 or inc("unlabelled") > 0 or ($gheld | length) > 0 or ($standing | length) > 0) as $failed
| ( group_by(l($node))[]
    | "UNPINNED node=\(.[0] | l($node)) count=\(map(.inc) | add | n) series=\(length) born_in_window=\(map(select(.born)) | length) resets=\(map(.resets | length) | add)" ),
  ( $moved | group_by(l("reason"))[]
    | "UNPINNED reason=\(.[0] | l("reason")) count=\(map(.inc) | add | n) nodes=\(nodes) class=\(.[0] | cls)" ),
  ( $moved[] | . as $s
    | "UNPINNED moved: node=\(l($node)) \(.labels | del(.[$node]) | to_entries | map("\(.key)=\(.value)") | join(" ")) count=\(.inc | n) first=\(.first.v | n)@\(.first.t | iso) last=\(.last.v | n)@\(.last.t | iso)\(if .born then " (absent at the start: counted from 0)" else "" end)\(if (.resets | length) > 0 then " (reset at \(.resets | map(.at | iso) | join(",")))" else "" end)" ),
  ( $moved[]
    | "UNPINNED steps: node=\(l($node)) job=\(l($job)) reason=\(l("reason")) at=\(.steps[:6] | map("\(.t | iso)(+\(.d | n))") | join(","))\(if (.steps | length) > 6 then " and \((.steps | length) - 6) more" else "" end)  (the sample that showed each increase: it happened in the export interval before it)" ),
  ( $held[]
    | "UNPINNED published: node=\(.node) job=\(.job) reason=\(.reason) class=\(.class) nonzero_samples=\(.nonzero) longest=\(.longest | floor)s max=\(.max | n) at_end=\(.at_end | n) series=\(.series)  "
      + (if .class != "no_tls" then "(the gauge held it: fails the gate)"
         elif .longest >= $pending and (.reason as $r | $exp | index($r)) then "(no TLS published for \($pending) s or more: declared expected on this cluster, --expect-tls-not-published; reported, not a failure)"
         elif .longest >= $pending then "(no TLS published for \($pending) s or more: not an agent start; fails the gate)"
         else "(less than \($pending) s: reported, not a failure)"
           + (if .at_end > 0 then " -- and still non-zero at the end of the window: whether it recovered is after it" else "" end) end) ),
  ( $declared[]
    | "UNPINNED expected: node=\(.node) job=\(.job) reason=\(.reason) seconds=\(.longest | floor) published=\(.max | n) count=\(counted | n)  (it stood for \($pending) s or more and --expect-tls-not-published declares that expected on this cluster: a mesh run without SPIRE, or a TCP floor that is not published. Not a failure; an agent that cannot get its SVID reads the same under this reason)" ),
  ( $silent[]
    | "UNPINNED silent: node=\(.node) job=\(.job) series=\(.series) last_sample=\(.last | iso)  (alive at the start, its sample there \(.last | age), and no sample in the window from this node and job: its counter was not seen)" ),
  ( $missing[]
    | "UNPINNED missing: node=\(.)  (a prober on this node has samples in the window and the counter has none from it: its agent was not seen)" ),
  ( $ended[]
    | "UNPINNED ended: node=\(.node) job=\(.job) series=\(.series) last_sample=\(.last | iso)  (the query at the start still returned it, with a sample \(.last | age): more than \($fresh) s, so it had stopped before the window and is not in it)" ),
  ( $nogauge[]
    | "UNPINNED published: absent: node=\(.node) job=\(.job)\(.more)  (its counter has the `reason` label and \($gauge){pin=\"unpinned\"} has no sample "
      + (if .since == null then "in the window from it" else "from it since its counter started again at \(.since | iso): the samples it has are from the process before" end)
      + ": how long a state stood there was not seen, and an absent gauge is not a zero)" ),
  ( if ($ungraded | length) > 0
    then "UNPINNED published: not graded: nodes=\($ungraded | map(.[0]) | unique | join(","))  (no `reason` label and no gauge: agents from before #1424. How long a state stood is not graded there; any movement of their counter fails)"
    else empty end ),
  "UNPINNED verdict=\(if length == 0 or ($silent | length) > 0 or ($missing | length) > 0 or ($nogauge | length) > 0 then "UNPROVEN" elif $failed then "FAIL" else "PASS" end) increase=\($inc | n) series=\(length) nodes=\(map(l($node)) | unique | length) resets=\(map(.resets | length) | add // 0)"
  + (if length == 0 then "  (no series: the counter is seeded at zero, so an absent one is not a zero -- it does not reach this Prometheus, or --match is wrong)"
     elif ($silent | length) > 0 or ($missing | length) > 0 then "  (no sample in the window from \([$silent[].node, $missing[]] | unique | join(",")): the sum is over the nodes that reported, not the fleet"
       + (if $inc > 0 then "; and the counter moved on those" else "" end) + ")"
     elif ($nogauge | length) > 0 then "  (the gauge \($gauge) has no sample in the window from \($nogauge | map(.node) | unique | join(",")): how long a state stood there was not seen"
       + (if $inc > 0 then "; and the counter moved" else "" end) + ")"
     elif $failed then "  (" + ([
         (if inc("unlabelled") > 0 then "a TLS cluster was published without its server-identity pin: the agent logged which and why, `mesh clusters published with no server-identity SAN pin`; what each reason means is in docs/runbook.md, \"The unpinned-cluster signal\" -- \(by_reason("unlabelled")), from agents with no `reason` label (before #1424): whether TLS was published under them is not in the counter, so any movement fails" else empty end),
         (if inc("gap") > 0 or (gauge_of("gap") | length) > 0 then "TLS was published without its server-identity pin, the mTLS validation gap: "
           + ([by_reason("gap"), gauge_of("gap")] | map(select(length > 0)) | join("; in the gauge: "))
           + " -- the agent logged which clusters, `mesh clusters published with no server-identity SAN pin`; docs/runbook.md, \"The unpinned-cluster signal\"" else empty end),
         (if inc("unknown") > 0 or (gauge_of("unknown") | length) > 0 or (gauge_of("unlabelled") | length) > 0 then "this script does not know "
           + ([by_reason("unknown"), gauge_of("unknown"), gauge_of("unlabelled")] | map(select(length > 0)) | join("; in the gauge: "))
           + ": an agent newer than this script reports a reason that is in neither GAP_REASONS (\($gap)) nor NO_TLS_REASONS (\($notls)), and it is not taken for a harmless one" else empty end),
         (if ($standing | length) > 0 then "no TLS was published for \($pending) s or more: "
           + ($standing | map(said) | join(", "))
           + " -- longer than an agent start: an agent without its trust domain or its SVID, or endpoints with no namespace metadata whose TLS is not published" else empty end)
       ] | join(" | ")) + ")"
     elif $inc > 0 or ($declared | length) > 0 then "  (" + ([
         (if $inc > 0 then "no TLS cluster was published without its pin: \(by_reason("no_tls")) -- under these the agent published no TLS at all, which is expected at an agent start, and no such state stood for \($pending) s in the samples of the gauge"
           + (if ($declared | length) > 0 then " but the one declared expected" else "" end) + ". Compare `UNPINNED steps:` with the agent rolls in churn.log" else empty end),
         (if ($declared | length) > 0 then "expected by declaration (--expect-tls-not-published), not a failure: "
           + ($declared | map("\(said) count=\(counted | n)") | join(", ")) else empty end)
       ] | join(" | ")) + ")"
     else "" end)
' "$TMPD/u.json" >"$TMPD/u.out" || die "could not grade the unpinned-cluster samples"
cat "$TMPD/u.out"
U_VERDICT="$(sed -n 's/^UNPINNED verdict=\([A-Z]*\) .*/\1/p' "$TMPD/u.out")"

# --- the cross-check against the prober's own failure lines ----------------------
L_VERDICT="not-checked"
if [ -n "$LOGS_FILE" ] || [ -n "$LOGS_URL" ]; then
	if [ -n "$LOGS_FILE" ]; then
		[ -r "$LOGS_FILE" ] || die "cannot read --logs-file $LOGS_FILE"
		cp "$LOGS_FILE" "$TMPD/logs.raw"
		LOGS_FROM="$LOGS_FILE"
	else
		LOGS_FROM="${LOGS_URL%/}/select/logsql/query  $LOGS_QUERY"
		if ! "$CURL" -fsS --max-time 300 --get "${LOGS_URL%/}/select/logsql/query" --data-urlencode "query=$LOGS_QUERY" \
			--data-urlencode "start=$START_S" --data-urlencode "end=$((END_S + LOG_EDGE_S))" >"$TMPD/logs.raw" 2>"$TMPD/err"; then
			echo "LOGS    verdict=UNPROVEN the log query failed: $(tr '\n' ' ' <"$TMPD/err" | cut -c1-300)"
			L_VERDICT="UNPROVEN"
		fi
	fi
	if [ "$L_VERDICT" = "not-checked" ]; then
		# A line is the prober's own (`AETHER_PROBE_FAIL {...}`), or a JSON
		# record whose _msg is that. A detail line counts when its `t` is in the
		# window. A summary counts when the whole span it can cover is: from its
		# `window_start` when the line has one (a prober since #1463), else from
		# two of its own window_s before `t`. One whose span crosses an end of
		# the window is a boundary summary: see the header.
		# shellcheck disable=SC2016
		jq -R -r -n --slurpfile series "$TMPD/p.json" --argjson start "$START_S" --argjson end "$END_S" --arg from "$LOGS_FROM" '
		def n: if . == floor then floor else . end;
		def iso: floor | todate;
		def at: (. // "") | sub("\\.[0-9]+Z$"; "Z") | (fromdateiso8601? // null);
		[ inputs | ((fromjson? | objects | ._msg) // .) | select(type == "string")
		  | (index("AETHER_PROBE_FAIL ")) as $i | select($i != null) | .[$i + 18:] | fromjson? | objects
		  | (.t | at) as $t
		  | select($t != null) | . + {T: $t} ] as $all
		| ($all | map(select((has("suppressed") | not) and .T >= $start and .T <= $end))) as $lines
		| ($all | map(select(has("suppressed")) | . + {from: ((.window_start | at) // (.T - 2 * (.window_s // 60)))})) as $sums
		| ($sums | map(select(.from >= $start and .T <= $end))) as $in
		| ($sums | map(select((.from < $start and .T > $start) or (.from < $end and .T > $end)))) as $edge
		| def bykey(f): group_by([.tier, .result]) | map({key: "\(.[0].tier) \(.[0].result)", value: f}) | from_entries;
		  ($lines | bykey(length)) as $nl
		| ($in | bykey(map(.suppressed) | add)) as $ns
		| ($edge | bykey(map(.suppressed) | add)) as $ne
		| ($series[0] | map(select((.silent | not) and (.labels.result // "-") != "success")) | group_by([.labels.tier, .labels.result])
		    | map({key: "\(.[0].labels.tier) \(.[0].labels.result)", value: (map(.inc) | add)}) | from_entries) as $ctr
		| ([($nl | keys[]), ($ns | keys[]), ($ne | keys[]), ($ctr | to_entries[] | select(.value > 0) | .key)] | unique) as $keys
		| ($keys | map(. as $k | {k: $k, lines: ($nl[$k] // 0), suppressed: ($ns[$k] // 0), boundary: ($ne[$k] // 0), counters: ($ctr[$k] // 0)}
		    | (.lines + .suppressed) as $lo
		    | .is = (if .boundary == 0 then (if $lo == .counters then "match" else "MISMATCH" end)
		             elif .counters >= $lo and .counters <= $lo + .boundary then "UNPROVEN"
		             else "MISMATCH" end))) as $rows
		| ( $edge[] | "LOGS    boundary: tier=\(.tier) result=\(.result) pod=\(.pod // "-") suppressed=\(.suppressed) closed=\(.T | iso) covers=\(.from | iso)..\(.T | iso)  (it may count failures on both sides of the \(if .from < $start and .T > $start then "start" else "end" end) of the window: not in the sum)" ),
		  ( $rows[] | "LOGS    tier=\(.k | split(" ")[0]) result=\(.k | split(" ")[1]) lines=\(.lines) suppressed=\(.suppressed)\(if .boundary > 0 then " boundary=\(.boundary)" else "" end) counters=\(.counters | n) \(.is)"
		    + (if .is == "UNPROVEN" then "  (the logs say between \(.lines + .suppressed) and \(.lines + .suppressed + .boundary))" else "" end) ),
		  "LOGS    verdict=\(if any($rows[]; .is == "MISMATCH") then "MISMATCH" elif any($rows[]; .is == "UNPROVEN") then "UNPROVEN" else "MATCH" end) lines=\($rows | map(.lines) | add // 0) suppressed=\($rows | map(.suppressed) | add // 0) boundary=\($rows | map(.boundary) | add // 0) counters=\($rows | map(.counters) | add // 0 | n)  (\($from))"
		' "$TMPD/logs.raw" >"$TMPD/l.out" || die "could not read the log lines"
		cat "$TMPD/l.out"
		L_VERDICT="$(sed -n 's/^LOGS    verdict=\([A-Z]*\) .*/\1/p' "$TMPD/l.out")"
	fi
fi

echo "VERDICT prober=${P_VERDICT:-UNPROVEN} unpinned=${U_VERDICT:-UNPROVEN} logs=$L_VERDICT"
# Unproven before failed: see "Exit" in the header.
case "${P_VERDICT:-UNPROVEN} ${U_VERDICT:-UNPROVEN} $L_VERDICT" in
*UNPROVEN* | *MISMATCH*) exit 2 ;;
*FAIL*) exit 1 ;;
esac
exit 0
