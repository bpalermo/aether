#!/usr/bin/env bash
# Kind leg for aether#1009: the `DC` access-log lines a SOURCE-proxy hot restart
# leaves on HTTP/1.1 requests that ride an east-west QUIC twin.
#
# The mechanism (issue #1009, read from the pinned Envoy): the draining parent
# stamps `Connection: close` on the 200; the HTTP/1.1 client reads the full
# Content-Length body and closes; the h3 upstream FIN is decoded a moment later,
# so ConnectionManagerImpl::onEvent(RemoteClose) -> resetAllStreams flags the
# still-open stream `DC` with response_code_details=downstream_remote_disconnect.
# The client got every byte and counted a success. nghttp2 delivers DATA +
# END_STREAM from one TCP read, so the h2 path has no such window.
#
# What this leg asserts, on the eastwest-quic bring-up plus an OTLP collector
# stand-in and access logs at 100 % (so every request is a log line):
#
#   H0  preflight  the proxy's listeners carry the aether_access_logs logger and
#                  the #1009 timing attributes; the twin for (client-a, quic-a)
#                  exists and carries requests (the loop below rides HTTP/3)
#   H1  client     HTTP/1.1 keep-alive loops from client-a to quic-a, quic-b
#                  (h3 twins) and gamma-a behind the weighted gamma-a/gamma-b
#                  HTTPRoute (the h2 control: east-west QUIC is unconditional
#                  since #979, so a weighted split -- which never selects a
#                  twin, #961 -- is the one h2-by-design path) through HR_RESTARTS
#                  supervisor hot restarts (SIGHUP, the supervisor's own
#                  trigger): every transfer answers 200 with the full body, and
#                  the loops RECONNECTED (the drain closed connections, so the
#                  leg exercised what it claims to)
#   H2  logs       every source-reporter DC line on the h3 destinations is
#                  benign: 200 + downstream_remote_disconnect + bytes_sent equal
#                  to that destination's clean-line size (the soak grader rule,
#                  e2e/soak/README.md "Benign DC"); the h2 control logged no DC
#
# A run that saw zero DC lines passes H2 vacuously and says so: the race is
# timing-dependent. HR_REQUIRE_DC=1 turns that into a failure.
#
# HR_MODE=sparse is a different leg, for aether#1054: a SOURCE h3 connection
# that outlives the DESTINATION proxy's hot-restart parent. When the parent
# exits, the connection's next packet reaches the child, which answers with a
# QUIC stateless reset the source accepts (the token is derived from the
# connection ID alone), and the request in flight fails `503 UC ...
# Received_stateless_reset`. Busy loops do not reproduce it: every connection
# keeps drawing responses and so, sooner or later in the drain, a GOAWAY. The
# sparse shape leaves a connection that is open but quiet when the parent
# exits:
#
#   S0  the chart as rendered: the live Envoy's --drain-strategy (destination
#       node) and the idle_timeout on the source's quic: twins (config_dump)
#   S1  per restart (HR_RESTARTS, default 10): HR_PRE s of light loops from
#       both sources to the h3 destinations, which END at the SIGHUP (so every
#       pair has a live h3 connection to the parent); SIGHUP the DESTINATION
#       node's supervisor; ONE request per (source, h3 destination) at T+2;
#       wait for the parent's `shutting down due to child request` log line;
#       then a burst of HR_BURST requests per source at concurrency
#       HR_BURST_C, spanning the parent's exit
#   S2  per restart, count the SOURCE-reporter access-log lines of that
#       restart whose response_code_details carry Received_stateless_reset,
#       and those carrying PEER_GOING_AWAY (so a fix that only changes the
#       error's flavour cannot pass), plus client-side non-200s
#
# Unforced, the window is the few milliseconds between the child unpausing its
# inherited UDP listeners (just before it sends Terminate) and the parent's
# workers stopping, and after drainTime a gradual drain GOAWAYs every response,
# so only the FIRST request on a connection left idle since T+2 can land in it.
# HR_FREEZE_PARENT_S=N (a forced-fault probe, like hotrestart-wedge.sh's
# WEDGE_FREEZE_S) holds that window open: SIGSTOP the parent at T+HR_FREEZE_AT
# (default 13, before the child's parent-shutdown timer), burst into the
# freeze, SIGCONT after N s. The burst's packets on connections the parent
# still owns are then read by the child, exactly the production race.
#
# It needs EWQ_WORKER=1 at `up` (destinations on a worker node, sources on the
# control plane): on one node the source and destination are the same Envoy,
# whose client connections die with the parent, so the stateless reset cannot
# happen there. Green is 0 of each on every restart; HR_REQUIRE_RESET=1 makes a
# run that saw no stateless reset fail instead (the red arm's assertion).
#
# Red/green (#1054, e2e/soak/README.md):
#   red    HR_DRAIN_STRATEGY=gradual HR_QUIC_IDLE=30s EWQ_WORKER=1 ... up
#          HR_MODE=sparse HR_REQUIRE_RESET=1 HR_FREEZE_PARENT_S=4 HR_FREEZE_AT=14 ... verify
#   green  EWQ_WORKER=1 ... up          (chart defaults: gradual, 8s; patched proxy)
#          HR_MODE=sparse ... verify    (and again with the freeze)
# On kind (2026-09-28) red saw 0 resets in 10 restarts unforced and 1 in 10
# forced; green saw 0 in 10 both ways. The race is real but rare on two
# kind nodes; the soak gate is the evidence that counts.
#
# Usage: e2e/eastwest-quic-hotrestart.sh {up|verify|down}   (bare = up + verify)
# Env: HR_MODE (dc: the #1009 leg above, the default; sparse: #1054),
#      HR_RESTARTS (default 6; 10 in sparse mode), HR_SECONDS (loop length,
#      default covers the restarts), HR_RATE (per-loop requests/s, default 5),
#      HR_LOOPS (loops per destination, default 16), HR_PRE (sparse pre-roll
#      seconds, default 6), HR_BURST (sparse burst requests per source, default
#      200), HR_BURST_C (sparse burst concurrency, default 20),
#      HR_FREEZE_PARENT_S / HR_FREEZE_AT (sparse forced fault, default off / 13);
#      HR_TRACE=1 (set -x); set at `up`
#      only: HR_SAMPLE (success sample %, default 2), HR_SKIP_PARENT_STATS
#      (proxy.hotRestart.skipParentStats, default false: the chart default
#      since #1060), HR_DRAIN_STRATEGY (proxy.hotRestart.drainStrategy; unset =
#      the chart default), HR_QUIC_IDLE (the agent's
#      --east-west-quic-idle-timeout, patched onto the DaemonSet AFTER the
#      install, so it bypasses the chart's idle + 5s < parentShutdownTime
#      check: the red arm's pre-#1054 30s only), plus everything
#      e2e/eastwest-quic.sh reads.
set -euo pipefail
if [ -n "${HR_TRACE:-}" ]; then set -x; fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export EWQ_CLUSTER="${EWQ_CLUSTER:-eastwest-quic-hr}"

COLLECTOR_NS="o11y"
COLLECTOR_SVC="otel-collector"
COLLECTOR_ENDPOINT="${COLLECTOR_SVC}.${COLLECTOR_NS}.svc.cluster.local:4317"
COLLECTOR_IMAGE="ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector:0.159.0"

# Access logs ON: the MeshConfig CR the controller seeds on first install
# (meshConfig.proxy), and the collector the proxy bootstrap's otel_collector
# cluster points at. Consumed by install_aether. Every line with a response flag
# is logged regardless of the sample rate (accessLogFilter), so the DC lines are
# all there; successes are sampled at HR_SAMPLE % because the debug exporter
# prints ~2.5 KB per record and kubelet rotates the collector's log at 10 MiB --
# at 100 % the run's records rotate away before they are read.
EWQ_EXTRA_HELM_ARGS=(
	--set "otel.endpoint=$COLLECTOR_ENDPOINT"
	--set "cniInstall.otlpEndpoint=$COLLECTOR_ENDPOINT"
	--set meshConfig.proxy.accessLogsEnabled=true
	--set "meshConfig.proxy.accessLogSuccessSampleRate=${HR_SAMPLE:-2}"
	# The #1050 workaround (Envoy --skip-hot-restart-parent-stats). Chart default
	# is false since the carried Envoy patch (#1060) fixed the deadlock.
	--set "proxy.hotRestart.skipParentStats=${HR_SKIP_PARENT_STATS:-false}"
)
# #1054's red arm: the pre-fix drain strategy.
if [ -n "${HR_DRAIN_STRATEGY:-}" ]; then
	EWQ_EXTRA_HELM_ARGS+=(--set "proxy.hotRestart.drainStrategy=$HR_DRAIN_STRATEGY")
fi

# shellcheck source=e2e/eastwest-quic.sh
. "$HERE/eastwest-quic.sh"

HR_MODE="${HR_MODE:-dc}"
if [ "$HR_MODE" = sparse ]; then HR_RESTARTS="${HR_RESTARTS:-10}"; fi
HR_RESTARTS="${HR_RESTARTS:-6}"
HR_PRE="${HR_PRE:-6}"
HR_BURST="${HR_BURST:-200}"
HR_BURST_C="${HR_BURST_C:-20}"
HR_FREEZE_PARENT_S="${HR_FREEZE_PARENT_S:-0}"
HR_FREEZE_AT="${HR_FREEZE_AT:-13}"
# Long enough for every restart: 10 s lead-in, 24 s per restart, 10 s tail.
HR_SECONDS="${HR_SECONDS:-$((20 + HR_RESTARTS * 24))}"
HR_RATE="${HR_RATE:-5}"
# Each loop is one keep-alive connection, and each drain closes it once: the
# race gets HR_LOOPS chances per destination per restart.
HR_LOOPS="${HR_LOOPS:-16}"
HR_SRC="client-a"
# The h2 control: the parent of eastwest-quic.sh's weighted canary. Both
# backends are agnhost netexec, so the echo body (and bytes_sent) is the same
# whichever one answers.
HR_H2_CTL="${GAMMA_DSTS[0]}"
HR_DSTS=(quic-a quic-b "$HR_H2_CTL")
# A fixed 700-byte echo body, so bytes_sent is one known number per destination.
HR_MSG="$(printf 'x%.0s' $(seq 1 700))"
HR_PATH="/echo?msg=$HR_MSG"

deploy_collector() {
	log "deploying the OTLP collector stand-in at $COLLECTOR_ENDPOINT"
	kc create ns "$COLLECTOR_NS" >/dev/null 2>&1 || true
	kc apply -f - >/dev/null <<YAML || die "collector apply failed"
apiVersion: v1
kind: ConfigMap
metadata: {name: $COLLECTOR_SVC, namespace: $COLLECTOR_NS}
data:
  config.yaml: |
    receivers:
      otlp:
        protocols:
          grpc: {endpoint: 0.0.0.0:4317}
    exporters:
      debug: {verbosity: detailed}
      debug/quiet: {verbosity: basic}
    service:
      pipelines:
        logs: {receivers: [otlp], exporters: [debug]}
        metrics: {receivers: [otlp], exporters: [debug/quiet]}
        traces: {receivers: [otlp], exporters: [debug/quiet]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: $COLLECTOR_SVC, namespace: $COLLECTOR_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: $COLLECTOR_SVC}}
  template:
    metadata:
      labels: {app: $COLLECTOR_SVC}
    spec:
      containers:
        - name: collector
          image: $COLLECTOR_IMAGE
          args: ["--config=/conf/config.yaml"]
          ports: [{containerPort: 4317, name: otlp-grpc}]
          volumeMounts: [{name: conf, mountPath: /conf}]
      volumes:
        - name: conf
          configMap: {name: $COLLECTOR_SVC}
---
apiVersion: v1
kind: Service
metadata: {name: $COLLECTOR_SVC, namespace: $COLLECTOR_NS}
spec:
  selector: {app: $COLLECTOR_SVC}
  ports: [{name: otlp-grpc, port: 4317, targetPort: 4317}]
YAML
	kc -n "$COLLECTOR_NS" rollout status "deploy/$COLLECTOR_SVC" --timeout=180s >/dev/null ||
		die "the OTLP collector never became Ready"
	ok "collector up"
}

# supervisor_pid [NODE] — the aether-proxy supervisor's PID as the kind node
# sees it (default $NODE). Matched on argv[0] EXACTLY, so the scanning shell
# (argv[0] "sh") never matches itself.
supervisor_pid() {
	# shellcheck disable=SC2016  # evaluated by the node's shell
	docker exec "${1:-$NODE}" sh -c '
		for p in /proc/[0-9]*; do
			a0=$({ tr "\0" "\n" <"$p/cmdline"; } 2>/dev/null | head -n 1)
			if [ "$a0" = /opt/aether/supervisor ]; then echo "${p#/proc/}"; fi
		done; exit 0' | head -n 1
}

# admin_on NODE PATH — admin() against a given kind node's proxy.
admin_on() { docker exec "$1" curl -s --max-time 5 "http://127.0.0.1:9901$2"; }

restart_epoch() {
	admin_on "${1:-$NODE}" /server_info 2>/dev/null | tr -d ' \n' | { grep -o '"restart_epoch":[0-9]*' || true; } | cut -d: -f2
}

# hot_restart [NODE] — ask NODE's supervisor (default $NODE) for a hot restart
# the way it is asked in production (SIGHUP == a watched-config change) and
# wait for the new epoch.
hot_restart() {
	local node="${1:-$NODE}" pid before after i
	pid="$(supervisor_pid "$node")"
	[ -n "$pid" ] || die "no /opt/aether/supervisor process on $node"
	before="$(restart_epoch "$node")"
	docker exec "$node" kill -HUP "$pid" || die "could not SIGHUP the supervisor (pid $pid)"
	for i in $(seq 1 60); do
		after="$(restart_epoch "$node")"
		if [ -n "$after" ] && [ -n "$before" ] && [ "$after" -gt "$before" ]; then
			ok "hot restart: epoch $before -> $after ($(date -u +%H:%M:%SZ))"
			return 0
		fi
		sleep 1
	done
	die "the supervisor never reached a new epoch after SIGHUP (still ${after:-?})"
}

# loop_on DST — HR_LOOPS keep-alive HTTP/1.1 loops from client-a's pod to DST's
# mesh name, one curl process each (one connection, reused until the proxy
# closes it). Prints "<http_code> <size_download> <num_connects>" per transfer.
loop_on() {
	local dst="$1" pod n
	pod="$(pod_of "$HR_SRC")"
	n=$((HR_SECONDS * HR_RATE))
	# shellcheck disable=SC2016  # evaluated by the POD's shell
	kc -n "$TEST_NS" exec "$pod" -c curl -- sh -c '
		url="$1"; n="$2"; rate="$3"; loops="$4"; ua="$5"
		cfg=/tmp/hr-$$.cfg; : >"$cfg"; i=0
		while [ "$i" -lt "$n" ]; do printf "url = \"%s\"\noutput = \"/dev/null\"\n" "$url" >>"$cfg"; i=$((i + 1)); done
		j=0
		while [ "$j" -lt "$loops" ]; do
			curl -s --max-time 5 --rate "$rate/s" -A "$ua-$j" -K "$cfg" \
				-w "%{http_code} %{size_download} %{num_connects}\n" >"$cfg.$j" &
			j=$((j + 1))
		done
		# One file per curl: parallel block-buffered writers on one pipe split lines.
		wait; cat "$cfg".*; rm -f "$cfg" "$cfg".*
	' sh "http://$(fqdn "$dst"):$OUTBOUND_PORT$HR_PATH" "$n" "$HR_RATE" "$HR_LOOPS" "aether-hr-$dst" 2>/dev/null
}

# records — the collector's access-log records as TSV, source reporter and this
# leg's loops only (user agent aether-hr-<dst>-<n>, or aether-hr-sparse-<i>-...):
# authority, response_code, response_flags, response_code_details, bytes_sent,
# upstream_rx_ms, downstream_tx_end_ms, protocol, start_time, user_agent.
records() {
	kc -n "$COLLECTOR_NS" logs "deploy/$COLLECTOR_SVC" --tail=-1 2>/dev/null | awk '
		function flush() {
			if (a["reporter"] == "source" && index(a["user_agent"], "aether-hr-") == 1)
				printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a["authority"], a["response_code"],
					a["response_flags"], a["response_code_details"], a["bytes_sent"],
					a["upstream_rx_ms"], a["downstream_tx_end_ms"], a["protocol"], a["start_time"], a["user_agent"]
			delete a
		}
		/^LogRecord #/ { flush(); next }
		/^ *-> [a-z_]+: Str\(/ {
			k = $2; sub(/:$/, "", k)
			v = $0; sub(/^[^(]*\(/, "", v); sub(/\)$/, "", v)
			a[k] = v
		}
		END { flush() }'
}

verify_hotrestart() {
	log "H0 preflight: access logs on the proxy listeners, with the #1009 timing attributes"
	local i dump
	for i in $(seq 1 30); do
		dump="$(admin '/config_dump?resource=dynamic_listeners' 2>/dev/null || true)"
		grep -q 'aether_access_logs' <<<"$dump" && break
		if [ "$i" = 15 ]; then
			echo "  (no access logger yet; rolling the agent once so it reads the seeded MeshConfig)"
			kc -n "$NS" rollout restart ds/aether-agent >/dev/null
			kc -n "$NS" rollout status ds/aether-agent --timeout=180s >/dev/null || true
		fi
		sleep 4
	done
	grep -q 'aether_access_logs' <<<"$dump" || die "H0: no listener carries the aether_access_logs logger"
	grep -q 'COMMON_DURATION(US_RX_BEG:US_RX_END:ms)' <<<"$dump" ||
		die "H0: the access logger lacks upstream_rx_ms — is this image built from this branch?"
	ok "access logger present with upstream_rx_ms / downstream_tx_end_ms"

	# The h2 control is h2 only while the weighted split is live (#979).
	gamma_split_up H0

	# Warm every pair (the first request fetches the twin over ODCDS) and prove
	# the h3 destinations ride their twin.
	local d dumpc before after
	for d in "${HR_DSTS[@]}"; do req_batch "$HR_SRC" "$d" "$HR_PATH" 3 >/dev/null; done
	dumpc="$(admin /clusters)"
	before="$(host_rq "$dumpc" "$(twin quic-a "$HR_SRC")")"
	req_batch "$HR_SRC" quic-a "$HR_PATH" 5 >/dev/null
	after="$(host_rq "$(admin /clusters)" "$(twin quic-a "$HR_SRC")")"
	[ "$((after - before))" -ge 5 ] || die "H0: the ($HR_SRC, quic-a) twin did not carry the warm-up batch ($before -> $after)"
	ok "($HR_SRC -> quic-a) rides its HTTP/3 twin ($before -> $after)"
	local ctl_twins
	ctl_twins="$(quic_twins "$(admin /clusters)")"
	case $'\n'"$ctl_twins"$'\n' in
	*$'\n'"$(twin "$HR_H2_CTL" "$HR_SRC")"$'\n'*)
		die "H0: the h2 control $HR_H2_CTL has a quic: twin for $HR_SRC — the weighted split selected one (#961)"
		;;
	esac
	ok "the h2 control $HR_H2_CTL has no quic: twin for $HR_SRC (weighted split)"

	# A fresh collector, so the records read in H2 are this run's only.
	kc -n "$COLLECTOR_NS" rollout restart "deploy/$COLLECTOR_SVC" >/dev/null
	kc -n "$COLLECTOR_NS" rollout status "deploy/$COLLECTOR_SVC" --timeout=120s >/dev/null ||
		die "the collector did not come back"
	sleep 5

	log "H1 client: $HR_LOOPS keep-alive HTTP/1.1 loops per destination x ${HR_SECONDS}s at ${HR_RATE}/s, through $HR_RESTARTS hot restarts"
	local out
	out="$(mktemp -d)"
	for d in "${HR_DSTS[@]}"; do loop_on "$d" >"$out/$d.txt" & done
	sleep 10
	for i in $(seq 1 "$HR_RESTARTS"); do
		hot_restart
		# The next SIGHUP must find no draining parent: parentShutdownTime is 15s.
		sleep 22
	done
	wait
	local total bad conns
	for d in "${HR_DSTS[@]}"; do
		total="$(wc -l <"$out/$d.txt")"
		bad="$(awk '$1 != "200" || $2 != 700' "$out/$d.txt" | sort | uniq -c | tr '\n' ' ')"
		conns="$(awk '{ s += $3 } END { print s + 0 }' "$out/$d.txt")"
		[ "$total" -gt 0 ] || die "H1: no transfers recorded for $d"
		[ -z "$bad" ] || die "H1: $d had client-side failures: $bad (of $total)"
		[ "$conns" -gt "$HR_LOOPS" ] || die "H1: $d never reconnected ($conns connections for $HR_LOOPS loops) — the drain closed nothing, so this leg measured nothing"
		ok "$d: $total/$total answered 200 with the full 700-byte body; $conns connections across $HR_LOOPS loops"
	done
	rm -rf "$out"

	log "H2 logs: every DC on an h3 destination is benign; the h2 control has none"
	sleep 10 # the OTLP logger batches; let the last flush land
	local recs
	recs="$(records)"
	[ -n "$recs" ] || die "H2: the collector holds no source-reporter access-log records"
	local auth clean ndc nbenign
	local seen_dc=0
	for d in "${HR_DSTS[@]}"; do
		auth="$(fqdn "$d"):$OUTBOUND_PORT"
		clean="$(awk -F'\t' -v a="$auth" '$1 == a && $2 == "200" && $3 == "-" { print $5 }' <<<"$recs" | sort | uniq -c | sort -rn)"
		[ "$(wc -l <<<"$clean")" -eq 1 ] && [ -n "$clean" ] || die "H2: $d has no single clean-line bytes_sent: [$clean]"
		clean="$(awk '{ print $2 }' <<<"$clean")"
		ndc="$(awk -F'\t' -v a="$auth" '$1 == a && $3 ~ /DC/' <<<"$recs" | wc -l)"
		nbenign="$(awk -F'\t' -v a="$auth" -v c="$clean" \
			'$1 == a && $3 == "DC" && $2 == "200" && $4 == "downstream_remote_disconnect" && $5 == c' <<<"$recs" | wc -l)"
		echo "  $d: records=$(awk -F'\t' -v a="$auth" '$1 == a' <<<"$recs" | wc -l) clean_bytes=$clean DC=$ndc benign=$nbenign"
		if [ "$ndc" -gt 0 ]; then
			echo "  $d DC lines (code flags details bytes_sent upstream_rx_ms downstream_tx_end_ms protocol start_time):"
			awk -F'\t' -v a="$auth" '$1 == a && $3 ~ /DC/ { $1 = ""; print "     " $0 }' <<<"$recs" | head -20
		fi
		echo "  $d clean-line timing (upstream_rx_ms, downstream_tx_end_ms) top 3:"
		awk -F'\t' -v a="$auth" '$1 == a && $3 == "-" { print "     " $6 " " $7 }' <<<"$recs" | sort | uniq -c | sort -rn | head -3
		if [ "$d" = "$HR_H2_CTL" ]; then
			[ "$ndc" -eq 0 ] || die "H2: the h2 control $d logged $ndc DC lines"
			ok "h2 control $d: no DC"
		else
			[ "$ndc" -eq "$nbenign" ] || die "H2: $d has $((ndc - nbenign)) NON-benign DC lines"
			seen_dc=$((seen_dc + ndc))
			ok "$d: $nbenign/$ndc DC lines benign"
		fi
	done
	if [ "$seen_dc" -eq 0 ]; then
		[ "${HR_REQUIRE_DC:-0}" = 1 ] && die "H2: the race did not reproduce (0 DC lines) and HR_REQUIRE_DC=1"
		echo "  NOTE: 0 DC lines on the h3 destinations — the race did not reproduce this run; H2 passed vacuously"
	fi
	log "hot-restart DC attribution leg passed"
}

# --- HR_MODE=sparse (aether#1054) -------------------------------------------

# proxy_pod_on NODE — the aether-proxy pod scheduled on NODE.
proxy_pod_on() {
	kc -n "$NS" get pod -l app.kubernetes.io/component=proxy --field-selector "spec.nodeName=$1" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# drain_strategy_on NODE — the --drain-strategy a live Envoy on NODE was started
# with ("gradual" when absent: Envoy's default), read from its argv rather than
# from the chart setting. Anchored on argv[0], so the scanning shell never
# matches itself.
drain_strategy_on() {
	# shellcheck disable=SC2016  # evaluated by the node's shell
	docker exec "$1" sh -c '
		for p in /proc/[0-9]*; do
			c=$({ tr "\0" " " <"$p/cmdline"; } 2>/dev/null) || continue
			case "$c" in
			"/usr/local/bin/envoy "*--restart-epoch*)
				s=$(printf "%s\n" "$c" | sed -n "s/.*--drain-strategy \([a-z]*\).*/\1/p")
				echo "${s:-gradual}"; exit 0 ;;
			esac
		done; echo none'
}

# envoy_pid_of_epoch NODE EPOCH — the PID of NODE's Envoy started with
# --restart-epoch EPOCH. Anchored on argv[0], so the scanning shell never
# matches itself.
envoy_pid_of_epoch() {
	# shellcheck disable=SC2016  # evaluated by the node's shell
	docker exec "$1" sh -c '
		for p in /proc/[0-9]*; do
			c=$({ tr "\0" " " <"$p/cmdline"; } 2>/dev/null) || continue
			case "$c" in
			"/usr/local/bin/envoy "*"--restart-epoch $1 "*) echo "${p#/proc/}"; exit 0 ;;
			esac
		done; exit 0' sh "$2"
}

# twin_idle — the idle_timeout(s) the source node's Envoy holds, split into
# quic: twins and every other cluster ("<count> <kind> <value>" per distinct
# pair). A cluster's name is the first "name" after its Cluster @type line.
twin_idle() {
	admin '/config_dump?resource=dynamic_active_clusters' 2>/dev/null | awk '
		/"@type": "type.googleapis.com\/envoy.config.cluster.v3.Cluster"/ { want = 1; next }
		want && /"name": "/ { n = $0; sub(/.*"name": "/, "", n); sub(/".*/, "", n); cur = n; want = 0 }
		/"idle_timeout": / {
			v = $0; sub(/.*"idle_timeout": "/, "", v); sub(/".*/, "", v)
			print (index(cur, "quic:") == 1 ? "quic" : "other"), v
		}' | sort | uniq -c | tr -s ' \n' ' ' || true
}

# patch_quic_idle D — replace the agent's --east-west-quic-idle-timeout with D
# on the live DaemonSet (HR_QUIC_IDLE; the red arm's pre-#1054 30s, which the
# chart refuses to render) and wait for the roll.
patch_quic_idle() {
	local d="$1" names args idx=-1 cidx=-1 i=0 n
	names="$(kc -n "$NS" get ds aether-agent -o jsonpath='{.spec.template.spec.containers[*].name}')"
	for n in $names; do
		if [ "$n" = agent ]; then cidx=$i; fi
		i=$((i + 1))
	done
	[ "$cidx" -ge 0 ] || die "no agent container in ds/aether-agent"
	args="$(kc -n "$NS" get ds aether-agent -o jsonpath="{range .spec.template.spec.containers[$cidx].args[*]}{@}{\"\\n\"}{end}")"
	idx="$(awk '/^--east-west-quic-idle-timeout=/ { print NR - 1; exit }' <<<"$args")"
	[ -n "$idx" ] || die "the agent has no --east-west-quic-idle-timeout arg to patch"
	kc -n "$NS" patch ds aether-agent --type=json \
		-p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/$cidx/args/$idx\",\"value\":\"--east-west-quic-idle-timeout=$d\"}]" >/dev/null ||
		die "could not patch the agent's idle timeout"
	kc -n "$NS" rollout status ds/aether-agent --timeout=300s >/dev/null || die "the agent never rolled"
	ok "agent --east-west-quic-idle-timeout=$d (patched past the chart's check: red arm only)"
}

# sparse_loops SRC TAG SECONDS — light keep-alive loops from SRC to each h3
# destination for SECONDS, then return: they end at the SIGHUP.
sparse_loops() {
	local src="$1" tag="$2" secs="$3" pod d
	pod="$(pod_of "$src")"
	for d in "${QUIC_DSTS[@]}"; do
		# shellcheck disable=SC2016  # evaluated by the POD's shell
		kc -n "$TEST_NS" exec "$pod" -c curl -- sh -c '
			url="$1"; n="$2"; ua="$3"; cfg=/tmp/sp-$$.cfg; : >"$cfg"; i=0
			while [ "$i" -lt "$n" ]; do printf "url = \"%s\"\noutput = \"/dev/null\"\n" "$url" >>"$cfg"; i=$((i + 1)); done
			curl -s --max-time 5 --rate 5/s -A "$ua" -K "$cfg" -w "%{http_code}\n"; rm -f "$cfg"
		' sh "http://$(fqdn "$d"):$OUTBOUND_PORT$HR_PATH" "$((secs * 5))" "$tag-pre-$src-$d" 2>/dev/null &
	done
	wait
}

# sparse_one SRC TAG — ONE request per h3 destination.
sparse_one() {
	local src="$1" tag="$2" pod d
	pod="$(pod_of "$src")"
	for d in "${QUIC_DSTS[@]}"; do
		kc -n "$TEST_NS" exec "$pod" -c curl -- curl -s -o /dev/null --max-time 5 -A "$tag-t2-$src-$d" \
			-w "%{http_code}\n" "http://$(fqdn "$d"):$OUTBOUND_PORT$HR_PATH" 2>/dev/null || echo 000
	done
}

# sparse_burst SRC TAG — HR_BURST requests at concurrency HR_BURST_C,
# alternating over the h3 destinations; one status code per line.
sparse_burst() {
	local src="$1" tag="$2" pod d
	local urls=()
	pod="$(pod_of "$src")"
	for d in "${QUIC_DSTS[@]}"; do urls+=("http://$(fqdn "$d"):$OUTBOUND_PORT$HR_PATH"); done
	# shellcheck disable=SC2016  # evaluated by the POD's shell
	kc -n "$TEST_NS" exec "$pod" -c curl -- sh -c '
		n="$1"; c="$2"; ua="$3"; shift 3
		cfg=/tmp/sb-$$.cfg; : >"$cfg"; i=0
		while [ "$i" -lt "$n" ]; do
			for u in "$@"; do
				[ "$i" -lt "$n" ] || break
				printf "url = \"%s\"\noutput = \"/dev/null\"\n" "$u" >>"$cfg"; i=$((i + 1))
			done
		done
		curl -s --max-time 20 --parallel --parallel-max "$c" -A "$ua" -K "$cfg" -w "%{http_code}\n"; rm -f "$cfg"
	' sh "$HR_BURST" "$HR_BURST_C" "$tag-burst-$src" "${urls[@]}" 2>/dev/null || true
}

verify_sparse() {
	[ "$DST_NODE" != "$NODE" ] ||
		die "HR_MODE=sparse needs EWQ_WORKER=1 at \`up\` (and now): on one node the source and destination are the same Envoy, so the stateless reset cannot happen"
	local ppod pdrain idle s d
	log "S0 the rendered fix: drain strategy on $DST_NODE, idle timeout on the source's quic: twins"
	for s in "${SOURCES[@]}"; do for d in "${QUIC_DSTS[@]}"; do req_batch "$s" "$d" "$HR_PATH" 3 >/dev/null; done; done
	pdrain="$(drain_strategy_on "$DST_NODE")"
	idle="$(twin_idle)"
	grep -q ' quic ' <<<" $idle" || die "S0: no quic: twin on the source node ($NODE) carries an idle_timeout — are the pairs warm? ($idle)"
	echo "  destination envoy --drain-strategy: $pdrain"
	echo "  source cluster idle_timeout (count kind value):$idle"
	echo "  agent: $(kc -n "$NS" get ds aether-agent -o jsonpath='{.spec.template.spec.containers[*].args}' | tr ',' '\n' | grep -o 'east-west-quic-idle-timeout=[^"]*' || echo '(flag absent)')"
	# A fresh collector, so the records read in S2 are this run's only.
	kc -n "$COLLECTOR_NS" rollout restart "deploy/$COLLECTOR_SVC" >/dev/null
	kc -n "$COLLECTOR_NS" rollout status "deploy/$COLLECTOR_SVC" --timeout=120s >/dev/null ||
		die "the collector did not come back"
	sleep 5

	log "S1 $HR_RESTARTS destination hot restarts on $DST_NODE: ${HR_PRE}s loops ending at SIGHUP, 1 request at T+2, a ${HR_BURST}-request burst (c=$HR_BURST_C) per source at the parent's exit"
	local out i tag follow fpid t0 found t_exit spid before after ppid
	local jobs=()
	out="$(mktemp -d)"
	for i in $(seq 1 "$HR_RESTARTS"); do
		tag="aether-hr-sparse-$i"
		ppod="$(proxy_pod_on "$DST_NODE")"
		[ -n "$ppod" ] || die "no proxy pod on $DST_NODE"
		spid="$(supervisor_pid "$DST_NODE")"
		[ -n "$spid" ] || die "no /opt/aether/supervisor process on $DST_NODE"
		before="$(restart_epoch "$DST_NODE")"
		follow="$out/proxy-$i.log"
		kc -n "$NS" logs -f "$ppod" -c proxy --since=1s >"$follow" 2>&1 &
		fpid=$!
		jobs=()
		for s in "${SOURCES[@]}"; do
			sparse_loops "$s" "$tag" "$HR_PRE" >>"$out/pre-$i.txt" &
			jobs+=("$!")
		done
		wait "${jobs[@]}"
		# The loops have ended: SIGHUP now, one request at T+2.
		t0=$SECONDS
		docker exec "$DST_NODE" kill -HUP "$spid" || die "could not SIGHUP the supervisor (pid $spid)"
		sleep 2
		for s in "${SOURCES[@]}"; do sparse_one "$s" "$tag" >>"$out/t2-$i.txt"; done
		jobs=()
		if [ "$HR_FREEZE_PARENT_S" -gt 0 ]; then
			# Forced fault: stop the parent's workers reading BEFORE the child
			# unpauses its inherited UDP listeners, and burst into the freeze.
			# The burst's packets on parent-owned connections queue on the shared
			# sockets; the child reads them once it completes the drains and sends
			# Terminate, i.e. the production window held open.
			sleep "$((t0 + HR_FREEZE_AT > SECONDS ? t0 + HR_FREEZE_AT - SECONDS : 0))"
			ppid="$(envoy_pid_of_epoch "$DST_NODE" "${before:-0}")"
			[ -n "$ppid" ] || die "S1 restart $i: no parent envoy (epoch ${before:-0}) on $DST_NODE to freeze"
			docker exec "$DST_NODE" kill -STOP "$ppid"
			for s in "${SOURCES[@]}"; do
				sparse_burst "$s" "$tag" >"$out/burst-$i-$s.txt" &
				jobs+=("$!")
			done
			sleep "$HR_FREEZE_PARENT_S"
			docker exec "$DST_NODE" kill -CONT "$ppid" || true
		fi
		found=0
		while [ $((SECONDS - t0)) -lt 60 ]; do
			if grep -q 'shutting down due to child request' "$follow"; then
				found=1
				break
			fi
			sleep 0.1
		done
		[ "$found" = 1 ] || die "S1 restart $i: the parent never logged 'shutting down due to child request' within 60s (log: $follow)"
		t_exit=$((SECONDS - t0))
		if [ "$HR_FREEZE_PARENT_S" -eq 0 ]; then
			for s in "${SOURCES[@]}"; do
				sparse_burst "$s" "$tag" >"$out/burst-$i-$s.txt" &
				jobs+=("$!")
			done
		fi
		wait "${jobs[@]}"
		kill "$fpid" 2>/dev/null || true
		wait "$fpid" 2>/dev/null || true
		after="$(restart_epoch "$DST_NODE")"
		echo "  restart $i: epoch ${before:-?} -> ${after:-?}; parent exit at ~T+${t_exit}s; burst codes: $(cat "$out"/burst-"$i"-*.txt | sort | uniq -c | tr '\n' ' ')"
		# Let the parent finish exiting before the next pre-roll.
		sleep 5
	done

	log "S2 per-restart source-side stateless resets and PEER_GOING_AWAY"
	sleep 10 # the OTLP logger batches; let the last flush land
	local recs total_reset=0 total_goaway=0 total_bad=0 total_hs=0 total_hung=0 r g b hs hung nrec
	recs="$(records)"
	[ -n "$recs" ] || die "S2: the collector holds no source-reporter access-log records"
	for i in $(seq 1 "$HR_RESTARTS"); do
		tag="aether-hr-sparse-$i-"
		nrec="$(awk -F'\t' -v t="$tag" 'index($10, t) == 1' <<<"$recs" | wc -l)"
		r="$(awk -F'\t' -v t="$tag" 'index($10, t) == 1 && $4 ~ /Received_stateless_reset/' <<<"$recs" | wc -l)"
		g="$(awk -F'\t' -v t="$tag" 'index($10, t) == 1 && $4 ~ /PEER_GOING_AWAY/' <<<"$recs" | wc -l)"
		# A NEW connection opened during a forced freeze cannot finish its
		# handshake (parent stopped, child still paused): a freeze artifact.
		hs="$(awk -F'\t' -v t="$tag" 'index($10, t) == 1 && $4 ~ /local_connection_failure\|QUIC_NETWORK_IDLE_TIMEOUT/' <<<"$recs" | wc -l)"
		b="$(cat "$out/pre-$i.txt" "$out/t2-$i.txt" "$out"/burst-"$i"-*.txt | awk '$1 != "200"' | wc -l)"
		hung="$(cat "$out"/burst-"$i"-*.txt | awk '$1 == "000"' | wc -l)"
		echo "  restart $i: stateless_reset=$r peer_going_away=$g client_non200=$b (hung=$hung handshake_timeout=$hs; records=$nrec)"
		total_reset=$((total_reset + r))
		total_goaway=$((total_goaway + g))
		total_bad=$((total_bad + b))
		total_hs=$((total_hs + hs))
		total_hung=$((total_hung + hung))
	done
	local details
	details="$(awk -F'\t' '$3 != "-" { print $2 " " $3 " " $4 }' <<<"$recs" | sort | uniq -c | sort -rn | head -10 | awk '{ print "     " $0 }')"
	[ -z "$details" ] || printf '  failure details (count code flags details):\n%s\n' "$details"
	rm -rf "$out"
	echo "SPARSE RESTARTS=$HR_RESTARTS FREEZE_S=$HR_FREEZE_PARENT_S STATELESS_RESET=$total_reset PEER_GOING_AWAY=$total_goaway CLIENT_NON200=$total_bad HUNG=$total_hung HANDSHAKE_TIMEOUT=$total_hs DRAIN_STRATEGY=$pdrain TWIN_IDLE=[$idle]"
	if [ "${HR_REQUIRE_RESET:-0}" = 1 ]; then
		[ "$total_reset" -gt 0 ] || die "S2: HR_REQUIRE_RESET=1 but no source saw a stateless reset: the red arm did not reproduce"
		ok "red arm reproduced: $total_reset stateless resets"
		return 0
	fi
	[ "$total_reset" -eq 0 ] || die "S2: $total_reset source requests died on a stateless reset"
	[ "$total_goaway" -eq 0 ] || die "S2: $total_goaway source requests died on PEER_GOING_AWAY"
	[ "$total_hung" -eq 0 ] || die "S2: $total_hung burst requests hung for 20s (a stale connection the source still uses)"
	if [ "$HR_FREEZE_PARENT_S" -gt 0 ]; then
		# The freeze itself fails new handshakes; only those may be non-200.
		[ "$((total_bad - total_hs))" -le 0 ] ||
			die "S2: $total_bad client-side non-200s, of which only $total_hs are the freeze's handshake timeouts"
		ok "no source request died on a stale connection across ${HR_RESTARTS} frozen restarts ($total_hs freeze handshake timeouts, not gated)"
		return 0
	fi
	[ "$total_bad" -eq 0 ] || die "S2: $total_bad client-side non-200s"
	ok "no source request died on a destination hot restart (${HR_RESTARTS} restarts)"
}

hr_up() {
	raise_inotify
	build_images
	create_cluster
	load_images
	deploy_collector
	install_gwapi_crds
	install_spire
	install_aether
	if [ -n "${HR_QUIC_IDLE:-}" ]; then patch_quic_idle "$HR_QUIC_IDLE"; fi
	deploy_workloads
}

hr_verify() {
	case "$HR_MODE" in
	dc) verify_hotrestart ;;
	sparse) verify_sparse ;;
	*) die "HR_MODE must be dc or sparse, got '$HR_MODE'" ;;
	esac
}

case "${1:-}" in
up) hr_up ;;
verify) hr_verify ;;
down) down ;;
"") hr_up && hr_verify ;;
*) die "usage: $0 {up|verify|down}" ;;
esac
