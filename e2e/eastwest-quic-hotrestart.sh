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
#                  (h3 twins) and h2only (the h2 control) through HR_RESTARTS
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
# Usage: e2e/eastwest-quic-hotrestart.sh {up|verify|down}   (bare = up + verify)
# Env: HR_RESTARTS (default 6), HR_SECONDS (loop length, default covers the
#      restarts), HR_RATE (per-loop requests/s, default 5), HR_LOOPS (loops per
#      destination, default 16), HR_SAMPLE (success sample %, default 2; set at
#      `up` only), plus everything e2e/eastwest-quic.sh reads.
set -euo pipefail

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
)

# shellcheck source=e2e/eastwest-quic.sh
. "$HERE/eastwest-quic.sh"

HR_RESTARTS="${HR_RESTARTS:-6}"
# Long enough for every restart: 10 s lead-in, 24 s per restart, 10 s tail.
HR_SECONDS="${HR_SECONDS:-$((20 + HR_RESTARTS * 24))}"
HR_RATE="${HR_RATE:-5}"
# Each loop is one keep-alive connection, and each drain closes it once: the
# race gets HR_LOOPS chances per destination per restart.
HR_LOOPS="${HR_LOOPS:-16}"
HR_SRC="client-a"
HR_DSTS=(quic-a quic-b "$H2_DST")
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

# supervisor_pid — the aether-proxy supervisor's PID as the kind node sees it.
# Matched on argv[0] EXACTLY, so the scanning shell (argv[0] "sh") never matches
# itself.
supervisor_pid() {
	# shellcheck disable=SC2016  # evaluated by the node's shell
	docker exec "$NODE" sh -c '
		for p in /proc/[0-9]*; do
			a0=$(tr "\0" "\n" <"$p/cmdline" 2>/dev/null | head -n 1)
			if [ "$a0" = /opt/aether/supervisor ]; then echo "${p#/proc/}"; fi
		done; exit 0' | head -n 1
}

restart_epoch() {
	admin /server_info 2>/dev/null | tr -d ' \n' | { grep -o '"restart_epoch":[0-9]*' || true; } | cut -d: -f2
}

# hot_restart — ask the supervisor for a hot restart the way it is asked in
# production (SIGHUP == a watched-config change) and wait for the new epoch.
hot_restart() {
	local pid before after i
	pid="$(supervisor_pid)"
	[ -n "$pid" ] || die "no /opt/aether/supervisor process on $NODE"
	before="$(restart_epoch)"
	docker exec "$NODE" kill -HUP "$pid" || die "could not SIGHUP the supervisor (pid $pid)"
	for i in $(seq 1 60); do
		after="$(restart_epoch)"
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
# leg's loops only (user agent aether-hr-<dst>-<n>):
# authority, response_code, response_flags, response_code_details, bytes_sent,
# upstream_rx_ms, downstream_tx_end_ms, protocol, start_time.
records() {
	kc -n "$COLLECTOR_NS" logs "deploy/$COLLECTOR_SVC" --tail=-1 2>/dev/null | awk '
		function flush() {
			if (a["reporter"] == "source" && index(a["user_agent"], "aether-hr-") == 1)
				printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a["authority"], a["response_code"],
					a["response_flags"], a["response_code_details"], a["bytes_sent"],
					a["upstream_rx_ms"], a["downstream_tx_end_ms"], a["protocol"], a["start_time"]
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
		if [ "$d" = "$H2_DST" ]; then
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

hr_up() {
	raise_inotify
	build_images
	create_cluster
	load_images
	deploy_collector
	install_gwapi_crds
	install_spire
	install_aether
	deploy_workloads
}

case "${1:-}" in
up) hr_up ;;
verify) verify_hotrestart ;;
down) down ;;
"") hr_up && verify_hotrestart ;;
*) die "usage: $0 {up|verify|down}" ;;
esac
