package envoy_validate

import (
	"fmt"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	otelaccesslogv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/open_telemetry/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

// AccessLogFormat is one OTel access logger found in a generated bootstrap.
type AccessLogFormat struct {
	// Where names the logger: "<listener>/<chain> (<filter kind>)".
	Where string
	// LogName is the logger's log_name: proxy's aether_access_logs for an
	// HTTP connection manager, proxy.L4AccessLogName for a tcp_proxy.
	LogName string
	// Attributes is the attribute key -> format string, as serialised.
	Attributes map[string]string
}

// AccessLogFormats returns every OTel access logger on every static listener
// of a generated bootstrap: those of the HTTP connection managers and those of
// the tcp_proxy filters, on filter chains and default chains alike.
//
// It reads the bytes handed to `envoy --mode validate`, so the format strings
// are the ones Envoy parsed. Parsing is all that proves: Envoy accepts a
// %COMMON_DURATION% over a time point it has never heard of and renders "-"
// (aether#1333). What the strings must BE is asserted on this function's
// output; that the pinned Envoy renders them as numbers is
// //agent/test/mtlspool TestAccessLogConnectionTiming.
func AccessLogFormats(bootstrapJSON []byte) ([]AccessLogFormat, error) {
	var bs bootstrapv3.Bootstrap
	if err := protojson.Unmarshal(bootstrapJSON, &bs); err != nil {
		return nil, fmt.Errorf("unmarshal bootstrap: %w", err)
	}
	var out []AccessLogFormat
	for _, l := range bs.GetStaticResources().GetListeners() {
		chains := append([]*listenerv3.FilterChain{}, l.GetFilterChains()...)
		if fc := l.GetDefaultFilterChain(); fc != nil {
			chains = append(chains, fc)
		}
		for _, fc := range chains {
			found, err := chainAccessLogFormats(l.GetName(), fc)
			if err != nil {
				return nil, err
			}
			out = append(out, found...)
		}
	}
	return out, nil
}

// chainAccessLogFormats is AccessLogFormats for one filter chain.
func chainAccessLogFormats(listener string, fc *listenerv3.FilterChain) ([]AccessLogFormat, error) {
	var out []AccessLogFormat
	for _, f := range fc.GetFilters() {
		logs, kind, err := filterAccessLogs(f)
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", listener, fc.GetName(), err)
		}
		for _, al := range logs {
			e, ok, err := otelAccessLogFormat(al)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", listener, fc.GetName(), err)
			}
			if !ok {
				continue
			}
			e.Where = fmt.Sprintf("%s/%s (%s)", listener, fc.GetName(), kind)
			out = append(out, e)
		}
	}
	return out, nil
}

// filterAccessLogs returns the access loggers of a network filter that is an
// HTTP connection manager or a tcp_proxy, and a label for it; nothing for any
// other filter.
func filterAccessLogs(f *listenerv3.Filter) ([]*accesslogv3.AccessLog, string, error) {
	tc := f.GetTypedConfig()
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	tcp := &tcp_proxyv3.TcpProxy{}
	switch {
	case tc == nil:
		return nil, "", nil
	case tc.MessageIs(hcm):
		if err := tc.UnmarshalTo(hcm); err != nil {
			return nil, "", fmt.Errorf("unmarshal HCM: %w", err)
		}
		return hcm.GetAccessLog(), "http_connection_manager " + hcm.GetStatPrefix(), nil
	case tc.MessageIs(tcp):
		if err := tc.UnmarshalTo(tcp); err != nil {
			return nil, "", fmt.Errorf("unmarshal tcp_proxy: %w", err)
		}
		return tcp.GetAccessLog(), "tcp_proxy " + tcp.GetStatPrefix(), nil
	}
	return nil, "", nil
}

// otelAccessLogFormat decodes one access logger; ok is false when it is not
// an OTel logger.
func otelAccessLogFormat(al *accesslogv3.AccessLog) (AccessLogFormat, bool, error) {
	cfg := &otelaccesslogv3.OpenTelemetryAccessLogConfig{}
	if al.GetTypedConfig() == nil || !al.GetTypedConfig().MessageIs(cfg) {
		return AccessLogFormat{}, false, nil
	}
	if err := al.GetTypedConfig().UnmarshalTo(cfg); err != nil {
		return AccessLogFormat{}, false, fmt.Errorf("unmarshal access logger: %w", err)
	}
	e := AccessLogFormat{LogName: cfg.GetLogName(), Attributes: map[string]string{}}
	for _, kv := range cfg.GetAttributes().GetValues() {
		e.Attributes[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	return e, true, nil
}
