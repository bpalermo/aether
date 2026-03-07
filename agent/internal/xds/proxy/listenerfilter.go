package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tls_inspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	"google.golang.org/protobuf/proto"
)

const (
	// listenerFilterTLSInspectorName is the Envoy TLS inspector listener filter name
	listenerFilterTLSInspectorName = "envoy.filters.listener.tls_inspector"
)

// buildInboundListenerFilters creates listener filters for inbound connections.
// It includes a TLS inspector to detect TLS protocol on incoming connections.
func buildInboundListenerFilters() []*listenerv3.ListenerFilter {
	var filters []*listenerv3.ListenerFilter

	filters = append(filters, tlsInspector())

	return filters
}

// tlsInspector creates a TLS inspector listener filter.
func tlsInspector() *listenerv3.ListenerFilter {
	filter := &tls_inspectorv3.TlsInspector{}
	return listenerFilter(listenerFilterTLSInspectorName, filter)
}

// listenerFilter creates a listener filter with the given name and configuration.
func listenerFilter(name string, msg proto.Message) *listenerv3.ListenerFilter {
	return &listenerv3.ListenerFilter{
		Name: name,
		ConfigType: &listenerv3.ListenerFilter_TypedConfig{
			TypedConfig: config.TypedConfig(msg),
		},
	}
}
