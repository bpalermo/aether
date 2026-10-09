package proxy

import (
	"aethermesh.dev/agent/internal/xds/config"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	otelaccesslogv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/open_telemetry/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	otlpcommonv1 "go.opentelemetry.io/proto/otlp/common/v1"
)

const (
	accessLogName        = "aether_access_logs"
	accessLogStatusKey   = "aether.access_log.min_status"
	accessLogSampleKey   = "aether.access_log.sample"
	accessLogMinStatus   = 500
	defaultCollectorName = "otel_collector"

	// meshProbePathPrefix is the reserved path prefix for the in-mesh
	// liveness/readiness probes (MeshLivePath, MeshReadyPath). Both the inbound
	// health_check filters and the egress local-reply liveness route answer
	// these; the access-log filter drops them so probe traffic never reaches
	// VictoriaLogs. No real service path uses this prefix.
	meshProbePathPrefix = "/-/-/"

	// ReporterSource/ReporterDestination tag an access log entry with the hop that
	// emitted it (egress client side vs per-pod inbound server side), mirroring the
	// aether_stats `reporter` label.
	ReporterSource      = "source"
	ReporterDestination = "destination"

	// restartEpochEnv is the environment variable the proxy supervisor starts
	// every Envoy child with: that child's --restart-epoch
	// (agent/internal/proxy/hotrestart, issue #1333). The name is spelled here
	// too because the agent and the supervisor are separate binaries in
	// separate images, and the agent must not link the supervisor.
	restartEpochEnv = "AETHER_RESTART_EPOCH"
)

// AccessLogConfig is the global access-log configuration. It is set ONCE at agent
// startup (SetAccessLogConfig) before the snapshot cache builds any listener, then
// read without locking — the same set-once pattern as SnapshotCache.emitStatsPod.
type AccessLogConfig struct {
	// Enabled turns the OTel access logger on for every HCM.
	Enabled bool
	// CollectorCluster is the OTLP gRPC cluster the logs are pushed to (the proxy
	// bootstrap's "otel_collector"). Empty defaults to defaultCollectorName.
	CollectorCluster string
	// SuccessSampleRate is the percent (0-100) of *successful* requests logged.
	// Failures (any response flag, or status >= 500) are always logged regardless.
	SuccessSampleRate uint32
}

var accessLogConfig AccessLogConfig

// SetAccessLogConfig sets the global access-log configuration. Call once before
// the manager starts.
func SetAccessLogConfig(c AccessLogConfig) { accessLogConfig = c }

// buildAccessLog returns the OTel access logger for an HCM, tagged with reporter
// and the local pod this listener serves (the source pod on egress, the
// destination pod on inbound — disambiguate via reporter), or nil when access
// logging is disabled. The logger pushes OTLP logs to the collector cluster; the
// filter logs all failures plus a SuccessSampleRate sample of successes.
func buildAccessLog(reporter, podName, podNamespace string) []*accesslogv3.AccessLog {
	if !accessLogConfig.Enabled {
		return nil
	}
	cluster := accessLogConfig.CollectorCluster
	if cluster == "" {
		cluster = defaultCollectorName
	}

	otelCfg := &otelaccesslogv3.OpenTelemetryAccessLogConfig{
		// GrpcService + LogName are the current fields; the older common_config
		// (CommonGrpcAccessLogConfig) wrapper is deprecated. Transport is V3 only.
		LogName: accessLogName,
		GrpcService: &corev3.GrpcService{
			TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
				EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: cluster},
			},
		},
		// Concise human-readable _msg for eyeballing; the full queryable field set
		// (Istio's defaults + more) lives in attributes, not duplicated in the body.
		Body: stringValue("%RESPONSE_CODE% %RESPONSE_FLAGS% %REQ(:METHOD)% %REQ(:AUTHORITY)%%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)% %DURATION%ms -> %UPSTREAM_HOST%"),
		// Full Istio default field set as structured attributes, plus aether's
		// reporter/pod identity/source_netns and the W3C traceparent (populates once
		// the mesh propagates trace context; "-" until then).
		Attributes: &otlpcommonv1.KeyValueList{Values: []*otlpcommonv1.KeyValue{
			kv("reporter", reporter),
			// Literal pod identity baked in per-pod listener: the local pod this hop
			// serves (source pod on egress, destination pod on inbound).
			kv("pod_name", podName),
			kv("pod_namespace", podNamespace),
			kv("start_time", "%START_TIME%"),
			kv("method", "%REQ(:METHOD)%"),
			kv("path", "%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)%"),
			kv("protocol", "%PROTOCOL%"),
			kv("response_code", "%RESPONSE_CODE%"),
			kv("response_flags", "%RESPONSE_FLAGS%"),
			kv("response_code_details", "%RESPONSE_CODE_DETAILS%"),
			kv("connection_termination_details", "%CONNECTION_TERMINATION_DETAILS%"),
			kv("upstream_transport_failure_reason", "%UPSTREAM_TRANSPORT_FAILURE_REASON%"),
			kv("bytes_received", "%BYTES_RECEIVED%"),
			kv("bytes_sent", "%BYTES_SENT%"),
			kv("duration_ms", "%DURATION%"),
			// Upstream-response timing, both anchored at the first upstream
			// response byte (US_RX_BEG), so a DC line says WHERE the stream was
			// when the downstream closed (#1009):
			//
			//   - upstream_rx_ms: first -> last upstream response byte. Set only
			//     once the router decoded the upstream end_stream
			//     (maybeEndDecode); "-" = the upstream FIN never landed.
			//   - downstream_tx_end_ms: first upstream byte -> the downstream
			//     codec finished encoding the response (onCodecEncodeComplete);
			//     "-" = the response was never completed downstream.
			//
			// Envoy's CommonDurationFormatter renders "-" whenever either time
			// point is unset, never a bogus 0. The benign QUIC-twin hot-restart
			// race (the HTTP/1.1 client read the full Content-Length body under
			// the draining parent's `Connection: close` and closed before the h3
			// FIN was decoded) is a DC line with a full bytes_sent and
			// upstream_rx_ms "-": resetAllStreams destroyed and logged the stream
			// before the end_stream arrived. A clean line carries both numbers.
			// docs/runbook.md (#1009) has the grading rule.
			kv("upstream_rx_ms", "%COMMON_DURATION(US_RX_BEG:US_RX_END:ms)%"),
			kv("downstream_tx_end_ms", "%COMMON_DURATION(US_RX_BEG:DS_TX_END:ms)%"),
			// Which proxy generation wrote the line (#1333); what the connection
			// the request rode had cost is appended below
			// (connectionTimingFields).
			generationField(),
			kv("upstream_service_time", "%RESP(X-ENVOY-UPSTREAM-SERVICE-TIME)%"),
			kv("x_forwarded_for", "%REQ(X-FORWARDED-FOR)%"),
			kv("user_agent", "%REQ(USER-AGENT)%"),
			kv("x_request_id", "%REQ(X-REQUEST-ID)%"),
			kv("authority", "%REQ(:AUTHORITY)%"),
			kv("upstream_host", "%UPSTREAM_HOST%"),
			kv("upstream_cluster", "%UPSTREAM_CLUSTER%"),
			kv("upstream_local_address", "%UPSTREAM_LOCAL_ADDRESS%"),
			kv("downstream_local_address", "%DOWNSTREAM_LOCAL_ADDRESS%"),
			kv("downstream_remote_address", "%DOWNSTREAM_REMOTE_ADDRESS%"),
			kv("requested_server_name", "%REQUESTED_SERVER_NAME%"),
			kv("route_name", "%ROUTE_NAME%"),
			kv("traceparent", "%REQ(TRACEPARENT)%"),
			// The ONLY remaining reader of the aether netns filter-state copy
			// (buildNetworkNamespaceFilterState) since #822 moved the cluster
			// matcher off it. Source-side only: the copy is stamped by the five
			// mesh-ORIGINATING chains, of which just two (outbound_http,
			// capture_http) carry an access log, so every destination-reporter
			// line renders "-". Verified peer identity on the inbound side is
			// #824, and is a different mechanism (%DOWNSTREAM_PEER_URI_SAN%).
			kv("source_netns", "%FILTER_STATE(aether.network.network_namespace:PLAIN)%"),
			// The source identity the control plane CLAIMED for this hop: the
			// literal SPIFFE ID buildCertMapperIdentityFilterState stamped on
			// this filter chain, which is also the SDS secret name the
			// cluster's filter_state_override certificate mapper resolves to
			// pick the client certificate (#842). Read from that key since
			// #1165 retired the aether.source.spiffe_id copy; the attribute
			// name is unchanged. Source-side only — the key is stamped by the
			// mesh-ORIGINATING chains, so a destination-reporter line renders "-".
			//
			// Its value is in pairing it with the verified fields, because the
			// two answer different questions and a mismatch is a defect:
			//
			//   - source line: source_spiffe_id is what this proxy MEANT to
			//     present. Its counterpart is the destination line's
			//     downstream_peer_uri_san (#824), which is what the destination
			//     actually VERIFIED. Join the two on x_request_id; they must be
			//     equal. They diverge if the certificate mapper fell back to its
			//     default_value (the node SVID appears at the destination,
			//     #686/#825) or if an upstream connection carrying another
			//     source's certificate were reused for this stream (issue #831 —
			//     prevented since #842 by this same key's hash partitioning the
			//     upstream pool, and demonstrated in //agent/test/mtlspool).
			//   - "-" here on a source-reporter line means the chain stamped no
			//     identity at all, i.e. the trust domain was still unknown when
			//     the listener was generated (#819).
			//
			// :PLAIN is MANDATORY. %FILTER_STATE(key)% defaults to TYPED, and
			// HashableString (a Router::StringAccessorImpl) does not implement
			// serializeAsProto, so the TYPED form renders "-" on every line —
			// silently, since a never-populated field is indistinguishable from
			// an absent one. The
			// same trap is documented at buildNetworkNamespaceFilterState.
			kv("source_spiffe_id", "%FILTER_STATE("+SourceIdentityCertMapperFilterStateKey+":PLAIN)%"),
			// RBAC AUDIT shadow decision — populated when the INBOUND listener carries an
			// AUDIT-mode RBAC filter (scope INBOUND, proposal 026 M4). Envoy prepends the
			// shadow_rules_stat_prefix ("aether_audit_") to both the stat counter names AND
			// the dynamic-metadata field names (rbac_filter.cc evaluateShadowEngine →
			// shadowEngineResultField / shadowEffectivePolicyIdField). The DYNAMIC_METADATA
			// substitution must therefore use the same prefix; bare "shadow_engine_result"
			// always returns "-". Returns "-" on requests where RBAC shadow is not active.
			kv("rbac_shadow_result", "%DYNAMIC_METADATA(envoy.filters.http.rbac:aether_audit_shadow_engine_result)%"),
			kv("rbac_shadow_policy", "%DYNAMIC_METADATA(envoy.filters.http.rbac:aether_audit_shadow_effective_policy_id)%"),
		}},
	}
	otelCfg.Attributes.Values = append(otelCfg.Attributes.Values, connectionTimingFields()...)
	// Appended rather than inlined above: the field is reporter-dependent, and
	// keeping the common set one flat literal keeps that list readable.
	otelCfg.Attributes.Values = append(otelCfg.Attributes.Values, peerIdentityFields(reporter)...)

	return []*accesslogv3.AccessLog{{
		Name:       "envoy.access_loggers.open_telemetry",
		Filter:     accessLogFilter(),
		ConfigType: &accesslogv3.AccessLog_TypedConfig{TypedConfig: config.TypedConfig(otelCfg)},
	}}
}

// L4AccessLogName is the log_name of the connection-level L4 access log
// (aether#1023): its own VictoriaLogs stream, `log_name=aether_l4_access_logs`,
// beside the HTTP stream `aether_access_logs`.
//
// A separate stream because the field shapes differ: an L4 record has no
// method, path, authority, response code or request id, and every one of those
// would render "-" in the HTTP stream. And it deliberately carries NO `reporter`
// attribute: the collector's identity connectors (k8s-talos-main
// otel-collector values, aether#863/#842) key on `reporter` -- log_name is a
// RESOURCE attribute the transform cannot see -- so an L4 record carrying it
// would enter the HTTP request counters and their denominator. The L4 record
// is always the SOURCE side (the capture listener), which is what
// filter_chain_name and pod_name already say.
const L4AccessLogName = "aether_l4_access_logs"

// buildL4AccessLog returns the OTel access logger for a capture-listener
// tcp_proxy (aether#1023), or nil when access logging is disabled -- the same
// switch as the HTTP log (MeshConfig access_logs_enabled), the same sink
// (the proxy bootstrap's otel_collector cluster) and the same success sample.
//
// Connection-level, by construction: tcp_proxy with no access_log_options
// writes one record when the downstream connection CLOSES, never per read or
// per flush, and the UDP capture path carries no logger at all (a per-datagram
// log would be unbounded). The filter keeps every connection that carries a
// response flag (UF/UH/UO/NR/DC..., which is where a SAN rejection or a
// blackholed flow lands) plus SuccessSampleRate% of the clean ones.
//
// podName/podNamespace are the SOURCE pod: the capture listener is per pod,
// so the literal is baked in the way the HTTP log's pod_name is. The node is
// the OTel resource attribute node_name Envoy's logger adds itself.
func buildL4AccessLog(podName, podNamespace string) []*accesslogv3.AccessLog {
	if !accessLogConfig.Enabled {
		return nil
	}
	cluster := accessLogConfig.CollectorCluster
	if cluster == "" {
		cluster = defaultCollectorName
	}

	otelCfg := &otelaccesslogv3.OpenTelemetryAccessLogConfig{
		LogName: L4AccessLogName,
		GrpcService: &corev3.GrpcService{
			TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
				EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: cluster},
			},
		},
		Body: stringValue("%RESPONSE_FLAGS% %FILTER_CHAIN_NAME% %DOWNSTREAM_LOCAL_ADDRESS% -> %UPSTREAM_CLUSTER% %UPSTREAM_HOST% %DURATION%ms"),
		Attributes: &otlpcommonv1.KeyValueList{Values: []*otlpcommonv1.KeyValue{
			// Source: the capturing pod, its netns and the identity the chain
			// stamped (the certificate the upstream mTLS presents, #842).
			kv("pod_name", podName),
			kv("pod_namespace", podNamespace),
			kv("source_netns", "%FILTER_STATE(aether.network.network_namespace:PLAIN)%"),
			kv("source_spiffe_id", "%FILTER_STATE("+SourceIdentityCertMapperFilterStateKey+":PLAIN)%"),
			// Which capture chain took the connection: cap_tcp_* (floor, per-port,
			// TCPRoute-weighted, the any-port shim), cap_tls_* (a TLSRoute SNI
			// chain) or cap_tcp_blackhole. The chain kind the stat key cannot
			// carry, since TLS chains report under their backends' tcp: keys.
			kv("filter_chain_name", "%FILTER_CHAIN_NAME%"),
			// Destination as dialled (the restored original destination: VIP:port)
			// and as reached (the endpoint, <pod IP>:18008).
			kv("downstream_local_address", "%DOWNSTREAM_LOCAL_ADDRESS%"),
			kv("downstream_remote_address", "%DOWNSTREAM_REMOTE_ADDRESS%"),
			kv("upstream_cluster", "%UPSTREAM_CLUSTER%"),
			kv("upstream_host", "%UPSTREAM_HOST%"),
			kv("upstream_local_address", "%UPSTREAM_LOCAL_ADDRESS%"),
			kv("requested_server_name", "%REQUESTED_SERVER_NAME%"),
			// The verified server identity on success; on a SAN rejection the
			// handshake never completes, this renders "-", and the presented SANs
			// are in upstream_transport_failure_reason instead.
			kv("upstream_peer_uri_san", "%UPSTREAM_PEER_URI_SAN%"),
			kv("response_flags", "%RESPONSE_FLAGS%"),
			kv("upstream_transport_failure_reason", "%UPSTREAM_TRANSPORT_FAILURE_REASON%"),
			kv("connection_termination_details", "%CONNECTION_TERMINATION_DETAILS%"),
			kv("start_time", "%START_TIME%"),
			kv("duration_ms", "%DURATION%"),
			kv("bytes_received", "%BYTES_RECEIVED%"),
			kv("bytes_sent", "%BYTES_SENT%"),
			// The proxy generation that wrote the record (#1333). An L4 record
			// is written when the connection closes, so across a hot restart
			// this names the generation that HELD the connection: the draining
			// parent for every connection accepted before the roll.
			generationField(),
		}},
	}

	return []*accesslogv3.AccessLog{{
		Name:       "envoy.access_loggers.open_telemetry",
		Filter:     l4LogWorthyFilter(),
		ConfigType: &accesslogv3.AccessLog_TypedConfig{TypedConfig: config.TypedConfig(otelCfg)},
	}}
}

// l4LogWorthyFilter is logWorthyFilter without the HTTP status arm: every
// connection with a response flag, plus the success sample (same runtime key,
// so one override moves both logs).
func l4LogWorthyFilter() *accesslogv3.AccessLogFilter {
	return &accesslogv3.AccessLogFilter{
		FilterSpecifier: &accesslogv3.AccessLogFilter_OrFilter{
			OrFilter: &accesslogv3.OrFilter{
				Filters: []*accesslogv3.AccessLogFilter{
					{FilterSpecifier: &accesslogv3.AccessLogFilter_ResponseFlagFilter{
						ResponseFlagFilter: &accesslogv3.ResponseFlagFilter{},
					}},
					{FilterSpecifier: &accesslogv3.AccessLogFilter_RuntimeFilter{
						RuntimeFilter: &accesslogv3.RuntimeFilter{
							RuntimeKey: accessLogSampleKey,
							PercentSampled: &typev3.FractionalPercent{
								Numerator:   accessLogConfig.SuccessSampleRate,
								Denominator: typev3.FractionalPercent_HUNDRED,
							},
							UseIndependentRandomness: true,
						},
					}},
				},
			},
		},
	}
}

// generationField is proxy_epoch: the hot-restart epoch of the Envoy process
// that wrote the line, the same number as envoy_server_hot_restart_epoch.
//
// It exists because the stats cannot say it. A hot-restart parent stops
// flushing to stat sinks the moment its child starts, and the child's exported
// values already contain the parent's, so no metric label separates the two
// generations during the overlap (#1333). An access-log line is written by one
// process, so "which generation served, or reset, this request" is a lookup.
//
// %ENVIRONMENT(...)% is read ONCE, when the formatter is built (getenv in
// EnvironmentFormatter's constructor), so it costs nothing per line and cannot
// change under a running process. It renders "-" when the variable is unset:
// an agent that ships this format to a proxy whose supervisor predates the
// variable logs "-" and nothing else changes.
func generationField() *otlpcommonv1.KeyValue {
	return kv("proxy_epoch", "%ENVIRONMENT("+restartEpochEnv+")%")
}

// connectionTimingFields says what the downstream connection had cost before
// this request, per request, on both reporters (#1333):
//
//   - connection_id: Envoy's id for the downstream connection, unique within
//     one Envoy process (so pair it with proxy_epoch and the node). Lines
//     sharing it rode one connection, which is what turns "N requests failed"
//     into "one connection failed".
//   - ds_cx_age_ms: how old that connection was when this request started
//     (DS_CX_BEG, when Envoy accepted it, to DS_RX_BEG, the request's first
//     byte). Small = the request paid for a new connection; large = reuse.
//   - ds_hs_ms: accept to the end of the downstream TLS handshake. A property
//     of the connection, so every line on one connection_id repeats it. "-"
//     where the downstream is not TLS: every source-reporter line (the local
//     application speaks cleartext to its proxy) and a cleartext inbound.
//   - us_tx_beg_ms: the request's first byte to the first byte sent upstream,
//     i.e. routing plus waiting for an upstream connection (a new upstream
//     handshake lands here). "-" when nothing was sent upstream (local reply,
//     no healthy upstream, connect failure).
//
// The time-point names are checked against the pinned Envoy by
// //agent/test/mtlspool (TestAccessLogConnectionTiming), not by
// `envoy --mode validate`: CommonDurationFormatter resolves a name it does not
// know as a dynamic time point that nothing ever sets, so a typo is accepted
// and renders "-" on every line forever. Two more things it does at the pin:
// "-" whenever either end is unset or the end precedes the start, never a
// bogus 0; and DS_CX_BEG falls back to the request start when the connection
// start is unknown, so ds_cx_age_ms would read 0 (not "-") there.
//
// What none of these can show: time a SYN or a datagram waited in the kernel
// before Envoy read it. Envoy takes no kernel receive timestamp, so DS_CX_BEG
// is when Envoy's worker accepted the connection (for QUIC, when it created
// the session from the first packet it read), not when the peer sent.
func connectionTimingFields() []*otlpcommonv1.KeyValue {
	return []*otlpcommonv1.KeyValue{
		kv("connection_id", "%CONNECTION_ID%"),
		kv("ds_cx_age_ms", "%COMMON_DURATION(DS_CX_BEG:DS_RX_BEG:ms)%"),
		kv("ds_hs_ms", "%COMMON_DURATION(DS_CX_BEG:DS_HS_END:ms)%"),
		kv("us_tx_beg_ms", "%COMMON_DURATION(DS_RX_BEG:US_TX_BEG:ms)%"),
	}
}

// peerIdentityFields returns the VERIFIED mTLS peer identity for this hop — the
// URI SAN of the certificate the other end actually presented and this proxy
// validated, which is the only identity in an access-log line that a workload
// cannot assert about itself.
//
// Split by reporter because each side has exactly one interesting peer and the
// other operator would render "-" on every line:
//
//   - destination (the per-pod INBOUND listener): DOWNSTREAM_PEER_URI_SAN is the
//     CALLER's identity, verified against the mesh trust bundle. This is what
//     answers "which workload actually called me", and what makes a wrong client
//     certificate visible in the logs — issue #824, and the check the release-two
//     validation of #815 could not run: a workload's traffic arriving as the node
//     agent's own SVID is the signature of the cluster matcher falling to
//     on_no_match.
//   - source (outbound_http / capture_http): UPSTREAM_PEER_URI_SAN is the SERVER
//     certificate the destination proxy presented. That is the client-side view of
//     the #638 identity cross-wiring, where envoy_cluster_ssl_fail_verify_san today
//     says only that SOME peer failed SAN validation.
//
// Both render "-" when the hop is not mTLS (SPIRE disabled, cleartext inbound,
// the loopback hop to the local application), so this costs one field per line
// and no configuration branch beyond the reporter. Cardinality is bounded by the
// number of ServiceAccounts, the same bound aether_stats already carries.
func peerIdentityFields(reporter string) []*otlpcommonv1.KeyValue {
	if reporter == ReporterDestination {
		return []*otlpcommonv1.KeyValue{
			kv("downstream_peer_uri_san", "%DOWNSTREAM_PEER_URI_SAN%"),
		}
	}
	return []*otlpcommonv1.KeyValue{
		kv("upstream_peer_uri_san", "%UPSTREAM_PEER_URI_SAN%"),
	}
}

// accessLogFilter keeps a log entry only when it is NOT a health/liveness probe
// AND it is log-worthy (a failure, or part of the success sample). Health checks
// are dropped unconditionally — including failing ones — so probe traffic
// (proposal 013 mesh prober + inbound live/ready) never reaches VictoriaLogs.
func accessLogFilter() *accesslogv3.AccessLogFilter {
	return &accesslogv3.AccessLogFilter{
		FilterSpecifier: &accesslogv3.AccessLogFilter_AndFilter{
			AndFilter: &accesslogv3.AndFilter{
				Filters: []*accesslogv3.AccessLogFilter{
					notHealthCheckFilter(),
					notProbePathFilter(),
					logWorthyFilter(),
				},
			},
		},
	}
}

// notHealthCheckFilter drops requests Envoy marked as health checks — the inbound
// live/ready HTTP health_check filters and the per-pod health gateway. The egress
// liveness route is a local-reply (not health_check-marked), so notProbePathFilter
// covers it by path.
func notHealthCheckFilter() *accesslogv3.AccessLogFilter {
	return &accesslogv3.AccessLogFilter{
		FilterSpecifier: &accesslogv3.AccessLogFilter_NotHealthCheckFilter{
			NotHealthCheckFilter: &accesslogv3.NotHealthCheckFilter{},
		},
	}
}

// notProbePathFilter drops any request whose :path is under the reserved mesh
// probe prefix (MeshLivePath/MeshReadyPath), regardless of how it was answered.
func notProbePathFilter() *accesslogv3.AccessLogFilter {
	return &accesslogv3.AccessLogFilter{
		FilterSpecifier: &accesslogv3.AccessLogFilter_HeaderFilter{
			HeaderFilter: &accesslogv3.HeaderFilter{
				Header: &routev3.HeaderMatcher{
					Name: ":path",
					HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
						StringMatch: &matcherv3.StringMatcher{
							MatchPattern: &matcherv3.StringMatcher_Prefix{Prefix: meshProbePathPrefix},
						},
					},
					InvertMatch: true,
				},
			},
		},
	}
}

// logWorthyFilter logs every failure (any response flag OR status >= 500) plus a
// SuccessSampleRate% sample of everything else.
func logWorthyFilter() *accesslogv3.AccessLogFilter {
	return &accesslogv3.AccessLogFilter{
		FilterSpecifier: &accesslogv3.AccessLogFilter_OrFilter{
			OrFilter: &accesslogv3.OrFilter{
				Filters: []*accesslogv3.AccessLogFilter{
					{FilterSpecifier: &accesslogv3.AccessLogFilter_ResponseFlagFilter{
						ResponseFlagFilter: &accesslogv3.ResponseFlagFilter{},
					}},
					{FilterSpecifier: &accesslogv3.AccessLogFilter_StatusCodeFilter{
						StatusCodeFilter: &accesslogv3.StatusCodeFilter{
							Comparison: &accesslogv3.ComparisonFilter{
								Op: accesslogv3.ComparisonFilter_GE,
								Value: &corev3.RuntimeUInt32{
									DefaultValue: accessLogMinStatus,
									RuntimeKey:   accessLogStatusKey,
								},
							},
						},
					}},
					{FilterSpecifier: &accesslogv3.AccessLogFilter_RuntimeFilter{
						RuntimeFilter: &accesslogv3.RuntimeFilter{
							RuntimeKey: accessLogSampleKey,
							PercentSampled: &typev3.FractionalPercent{
								Numerator:   accessLogConfig.SuccessSampleRate,
								Denominator: typev3.FractionalPercent_HUNDRED,
							},
							UseIndependentRandomness: true,
						},
					}},
				},
			},
		},
	}
}

func stringValue(s string) *otlpcommonv1.AnyValue {
	return &otlpcommonv1.AnyValue{Value: &otlpcommonv1.AnyValue_StringValue{StringValue: s}}
}

func kv(key, val string) *otlpcommonv1.KeyValue {
	return &otlpcommonv1.KeyValue{Key: key, Value: stringValue(val)}
}
