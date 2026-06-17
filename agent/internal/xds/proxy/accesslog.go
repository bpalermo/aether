package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	grpcaccesslogv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/grpc/v3"
	otelaccesslogv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/open_telemetry/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	otlpcommonv1 "go.opentelemetry.io/proto/otlp/common/v1"
)

const (
	accessLogName        = "aether_access"
	accessLogStatusKey   = "aether.access_log.min_status"
	accessLogSampleKey   = "aether.access_log.sample"
	accessLogMinStatus   = 500
	defaultCollectorName = "otel_collector"

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

// buildAccessLog returns the OTel access logger for an HCM, tagged with reporter,
// or nil when access logging is disabled. The logger pushes OTLP logs to the
// collector cluster; the filter logs all failures plus a SuccessSampleRate sample
// of successes.
func buildAccessLog(reporter string) []*accesslogv3.AccessLog {
	if !accessLogConfig.Enabled {
		return nil
	}
	cluster := accessLogConfig.CollectorCluster
	if cluster == "" {
		cluster = defaultCollectorName
	}

	otelCfg := &otelaccesslogv3.OpenTelemetryAccessLogConfig{
		CommonConfig: &grpcaccesslogv3.CommonGrpcAccessLogConfig{
			LogName: accessLogName,
			GrpcService: &corev3.GrpcService{
				TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
					EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: cluster},
				},
			},
			TransportApiVersion: corev3.ApiVersion_V3,
		},
		// Human-readable line; structured fields go in attributes for querying.
		Body: stringValue("%RESPONSE_CODE% %RESPONSE_FLAGS% %REQ(:METHOD)% %REQ(:AUTHORITY)%%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)% %DURATION%ms -> %UPSTREAM_HOST%"),
		Attributes: &otlpcommonv1.KeyValueList{Values: []*otlpcommonv1.KeyValue{
			kv("reporter", reporter),
			kv("response_code", "%RESPONSE_CODE%"),
			kv("response_flags", "%RESPONSE_FLAGS%"),
			kv("method", "%REQ(:METHOD)%"),
			kv("authority", "%REQ(:AUTHORITY)%"),
			kv("path", "%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)%"),
			kv("protocol", "%PROTOCOL%"),
			kv("duration_ms", "%DURATION%"),
			kv("upstream_service_time", "%RESP(X-ENVOY-UPSTREAM-SERVICE-TIME)%"),
			kv("bytes_sent", "%BYTES_SENT%"),
			kv("bytes_received", "%BYTES_RECEIVED%"),
			kv("upstream_host", "%UPSTREAM_HOST%"),
			kv("upstream_cluster", "%UPSTREAM_CLUSTER%"),
			kv("x_request_id", "%REQ(X-REQUEST-ID)%"),
			kv("source_netns", "%FILTER_STATE(aether.network.network_namespace:PLAIN)%"),
		}},
	}

	return []*accesslogv3.AccessLog{{
		Name:       "envoy.access_loggers.open_telemetry",
		Filter:     accessLogFilter(),
		ConfigType: &accesslogv3.AccessLog_TypedConfig{TypedConfig: config.TypedConfig(otelCfg)},
	}}
}

// accessLogFilter logs every failure (any response flag OR status >= 500) plus a
// SuccessSampleRate% sample of everything else.
func accessLogFilter() *accesslogv3.AccessLogFilter {
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
