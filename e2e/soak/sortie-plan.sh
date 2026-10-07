#!/usr/bin/env bash
# Renders the soak's sortie plan (proposal 042) from the targets file, and the
# per-target share table sortie-gate.sh grades against. One template, two
# profiles: `e2e` (15 minutes) and `soak` (8h30m, the k6 run's own length).
#
#   sortie-plan.sh render --profile soak --backends 5 --dns HOST:8443 >plan.yaml
#   sortie-plan.sh shares                          # name<TAB>rps per node<TAB>url
#
# It refuses a plan sortie would silently change. sortie rounds a target's share
# (`rate x weight / sum(weights)`) to the nearest whole request per second, so
# 100 split 1:1:1 runs at 99: here a share that is not whole is an error, the
# shares must add up to --rate, and each must divide by --concurrency (sortie's
# own rule: Nighthawk's rate is per worker thread).
#
# Options (render and shares):
#   --targets FILE      default: sortie-targets.txt beside this script
#   --rate N            requests per second PER NODE across all targets (60)
#   --concurrency N     worker threads per target per engine (1)
# Options (render only):
#   --profile e2e|soak  the duration: 900s or 30600s (8h30m)
#   --duration Ns       overrides the profile's duration (seconds, `900s`)
#   --backends N        engines the pool must resolve to: the 2xx rate floor is
#                       written for exactly N, so a node missing from the pool
#                       fails every target instead of shrinking the run
#   --dns HOST:PORT     the engines' headless Service
#   --statsd IP:PORT    optional live metrics; an IP literal (Envoy's statsd sink
#                       does not resolve names). Omitted: no stats block.
#   --floor-pct P       2xx rate floor, percent of the planned rate (99)
#   --scenario NAME     scenario name = report label prefix = statsd level (mesh)
#   --idle-strategy S   Nighthawk's sequencer idle strategy: SLEEP (default),
#                       POLL or SPIN. See proposal 042, "CPU and memory", before
#                       changing it.
#   --max-pending N     Nighthawk's --max-pending-requests per worker: how many
#                       due requests may wait for a connection before the next
#                       is refused as pool_overflow (16). It is what lets the
#                       engine run under its CPU limit: a throttled worker
#                       releases the requests that came due while it was off
#                       CPU together, and with the engine's default queue those
#                       were refused (proposal 042, "CPU and memory").
#
# Needs bash and awk only: //e2e/soak:harness_test runs it offline.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
TARGETS="$HERE/sortie-targets.txt"
RATE=60
CONCURRENCY=1
PROFILE=""
DURATION=""
BACKENDS=""
DNS=""
STATSD=""
FLOOR_PCT=99
SCENARIO=mesh
IDLE=SLEEP
MAX_PENDING=16
# One execution per target runs at once on every engine; the chart's
# engine.maxConcurrentExecutions (sortie-values.yaml) must cover the list.
MAX_TARGETS=16

die() {
	echo "sortie-plan.sh: $*" >&2
	exit 2
}

CMD="${1:-}"
[ $# -gt 0 ] && shift
case "$CMD" in
render | shares) ;;
*) die "usage: sortie-plan.sh {render|shares} [options] (see the header)" ;;
esac
while [ $# -gt 0 ]; do
	case "$1" in
	--targets | --rate | --concurrency | --profile | --duration | --backends | --dns | --statsd | --floor-pct | --scenario | --idle-strategy | --max-pending)
		[ $# -ge 2 ] || die "$1 needs a value"
		case "$1" in
		--targets) TARGETS="$2" ;;
		--rate) RATE="$2" ;;
		--concurrency) CONCURRENCY="$2" ;;
		--profile) PROFILE="$2" ;;
		--duration) DURATION="$2" ;;
		--backends) BACKENDS="$2" ;;
		--dns) DNS="$2" ;;
		--statsd) STATSD="$2" ;;
		--floor-pct) FLOOR_PCT="$2" ;;
		--scenario) SCENARIO="$2" ;;
		--idle-strategy) IDLE="$2" ;;
		--max-pending) MAX_PENDING="$2" ;;
		esac
		shift 2
		;;
	*) die "unknown option '$1'" ;;
	esac
done

is_pos_int() { case "$1" in '' | *[!0-9]* | 0*) return 1 ;; *) return 0 ;; esac }
is_pos_int "$RATE" || die "--rate must be a positive integer, got '$RATE'"
is_pos_int "$CONCURRENCY" || die "--concurrency must be a positive integer, got '$CONCURRENCY'"
[ -r "$TARGETS" ] || die "cannot read the targets file '$TARGETS'"

# The share table: `name<TAB>rps<TAB>url`, or one `ERROR ...` line. All the
# arithmetic is integer: a share is whole only if rate*weight divides by the sum.
SHARES="$(awk -v rate="$RATE" -v conc="$CONCURRENCY" -v max="$MAX_TARGETS" '
	/^[ \t]*(#|$)/ { next }
	{
		if (NF != 3) { printf "ERROR line %d: want `name url weight`, got %d fields\n", NR, NF; bad = 1; exit }
		if ($1 !~ /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/) { printf "ERROR line %d: target name \"%s\" must be [a-z0-9-]\n", NR, $1; bad = 1; exit }
		if ($1 in seen) { printf "ERROR line %d: target \"%s\" is listed twice\n", NR, $1; bad = 1; exit }
		if ($2 !~ /^http:\/\/[^\/ ]+(\/.*)?$/) { printf "ERROR line %d: target \"%s\": url must be http://host[:port]/path (the soak drives plain HTTP/1.1), got \"%s\"\n", NR, $1, $2; bad = 1; exit }
		if ($3 !~ /^[1-9][0-9]*$/) { printf "ERROR line %d: target \"%s\": weight must be a positive integer, got \"%s\"\n", NR, $1, $3; bad = 1; exit }
		seen[$1] = 1; n++; name[n] = $1; url[n] = $2; w[n] = $3; sum += $3
	}
	END {
		if (bad) exit
		if (n == 0) { print "ERROR no targets"; exit }
		if (n > max) { printf "ERROR %d targets, but an engine runs at most %d executions at once (engine.maxConcurrentExecutions)\n", n, max; exit }
		for (i = 1; i <= n; i++) {
			if ((rate * w[i]) % sum != 0) {
				printf "ERROR target \"%s\": %d rps x %d/%d is %.3f rps, not a whole number (sortie would round it and the shares would no longer add up to the rate)\n", name[i], rate, w[i], sum, rate * w[i] / sum
				exit
			}
			share[i] = rate * w[i] / sum
			if (share[i] % conc != 0) {
				printf "ERROR target \"%s\": its share, %d rps, is not a multiple of concurrency %d\n", name[i], share[i], conc
				exit
			}
			total += share[i]
		}
		if (total != rate) { printf "ERROR the shares add up to %d rps, not the rate %d\n", total, rate; exit }
		for (i = 1; i <= n; i++) printf "%s\t%d\t%s\n", name[i], share[i], url[i]
	}' "$TARGETS")"
case "$SHARES" in
ERROR*) die "$TARGETS: ${SHARES#ERROR }" ;;
'') die "$TARGETS: no targets" ;;
esac

if [ "$CMD" = shares ]; then
	printf '%s\n' "$SHARES"
	exit 0
fi

case "$PROFILE" in
e2e) : "${DURATION:=900s}" ;;
soak) : "${DURATION:=30600s}" ;;
*) die "--profile must be e2e or soak, got '$PROFILE'" ;;
esac
case "$DURATION" in
*[!0-9]*s | s | '' | 0*) die "--duration must be whole seconds written like 900s (protobuf's duration form), got '$DURATION'" ;;
*s) ;;
*) die "--duration must be whole seconds written like 900s (protobuf's duration form), got '$DURATION'" ;;
esac
is_pos_int "$BACKENDS" || die "--backends must be a positive integer (the engines the pool must resolve to), got '$BACKENDS'"
is_pos_int "$FLOOR_PCT" && [ "$FLOOR_PCT" -le 100 ] || die "--floor-pct must be 1..100, got '$FLOOR_PCT'"
case "$DNS" in
*:[0-9]*) ;;
*) die "--dns must be host:port (the engines' headless Service), got '$DNS'" ;;
esac
case "$SCENARIO" in
'' | *[!a-z0-9_]*) die "--scenario must be [a-z0-9_], got '$SCENARIO'" ;;
esac
is_pos_int "$MAX_PENDING" || die "--max-pending must be a positive integer, got '$MAX_PENDING'"
case "$IDLE" in
SLEEP | POLL | SPIN) ;;
*) die "--idle-strategy must be SLEEP, POLL or SPIN, got '$IDLE'" ;;
esac
if [ -n "$STATSD" ]; then
	printf '%s\n' "$STATSD" | awk -F'[.:]' '
		NF != 5 { exit 1 }
		{ for (i = 1; i <= 4; i++) if ($i !~ /^[0-9]+$/ || $i > 255) exit 1
		  if ($5 !~ /^[0-9]+$/ || $5 < 1 || $5 > 65535) exit 1 }' ||
		die "--statsd must be an IPv4 literal and a port, like 10.96.14.7:8125 (the sink does not resolve names), got '$STATSD'"
fi

# The floor is one number for the whole scenario (a scenario's thresholds apply
# to every target alike), so it is written for the SMALLEST share; rate
# thresholds are judged against the pool total, hence x backends.
# sortie-gate.sh applies each target's own floor from the share table.
FLOOR="$(printf '%s\n' "$SHARES" | awk -F'\t' -v n="$BACKENDS" -v pct="$FLOOR_PCT" '
	NR == 1 || $2 < min { min = $2 }
	END { printf "%.2f", min * n * pct / 100 }')"

cat <<EOF
# Rendered by e2e/soak/sortie-plan.sh -- do not edit; edit sortie-targets.txt.
# profile=$PROFILE rate=$RATE rps per node, $BACKENDS nodes, concurrency $CONCURRENCY per target.
version: v1
pools:
  - name: nodes
    # The engines' headless Service: one address per ready engine pod, resolved
    # ONCE when the run starts.
    dns: $DNS
EOF
if [ -n "$STATSD" ]; then
	cat <<EOF
stats:
  flush_interval: 5s
  prefix: sortie
  statsd:
    address: "$STATSD"
EOF
fi
cat <<EOF
scenarios:
  - name: $SCENARIO
    pool: nodes
    protocol: http1
    concurrency: "$CONCURRENCY"
    max_pending_requests: $MAX_PENDING
    headers:
      - "User-Agent: aether-soak-sortie"
    executor:
      type: constant-rate
      rate: $RATE
      duration: $DURATION
      per_backend: true
      open_loop: true
    # Nighthawk's sequencer SPINS between requests by default: one core per
    # worker thread, eight per node for this plan. SLEEP hands the core back.
    nighthawk_template:
      sequencer_idle_strategy:
        value: $IDLE
      # Nighthawk ENDS an execution at the first http_4xx, http_5xx,
      # pool_connection_failure or stream_reset (its default failure
      # predicates), and the report then carries an error and no counters: one
      # 503 during a roll would stop that target for the rest of the soak and
      # hide what it was. A soak counts failures; the thresholds below judge
      # them. These limits are never reached.
      failure_predicates:
        benchmark.http_4xx: 1000000000000
        benchmark.http_5xx: 1000000000000
        benchmark.pool_connection_failure: 1000000000000
        benchmark.stream_resets: 1000000000000
    targets:
EOF
printf '%s\n' "$SHARES" | awk -F'\t' '{ printf "      - {name: %s, url: \"%s\", weight: %d}  # %d rps per node\n", $1, $3, $2, $2 }'
cat <<EOF
    thresholds:
      # The zero-failure set. The counters overlap (do not add them up); a
      # counter that never incremented reads zero.
      - "counter:benchmark.http_4xx == 0"
      - "counter:benchmark.http_5xx == 0"
      - "counter:benchmark.stream_resets == 0"
      - "counter:benchmark.pool_connection_failure == 0"
      - "counter:benchmark.pool_failure_timeout == 0"
      - "counter:benchmark.pool_overflow == 0"
      # $FLOOR_PCT% of the smallest share x $BACKENDS nodes.
      - "rate:benchmark.http_2xx >= $FLOOR"
      # Not a gate: it puts the pool's 2xx total into the JSON report, which
      # otherwise carries no request count.
      - "counter:benchmark.http_2xx > 0"
EOF
