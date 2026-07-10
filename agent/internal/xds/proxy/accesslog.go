package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
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
			kv("source_netns", "%FILTER_STATE(aether.network.network_namespace:PLAIN)%"),
			// RBAC AUDIT-mode visibility (proposal 025): the local-authz filter writes
			// its shadow decision to dynamic metadata on every request it evaluates in
			// AUDIT mode. Surfacing it per-request in the access log is the reliable way
			// to observe "what ENFORCE would block" — the per-route rbac.shadow_* stats
			// are fleet-collapsed and hard to attribute. "-" for non-audited requests.
			kv("rbac_shadow_result", "%DYNAMIC_METADATA(envoy.filters.http.rbac:shadow_engine_result)%"),
			kv("rbac_shadow_policy", "%DYNAMIC_METADATA(envoy.filters.http.rbac:shadow_effective_policy_id)%"),
		}},
	}

	return []*accesslogv3.AccessLog{{
		Name:       "envoy.access_loggers.open_telemetry",
		Filter:     accessLogFilter(),
		ConfigType: &accesslogv3.AccessLog_TypedConfig{TypedConfig: config.TypedConfig(otelCfg)},
	}}
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
