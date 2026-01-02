package proxy

import (
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tls_inspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	"google.golang.org/protobuf/proto"
)

func buildInboundListenerFilters() []*listenerv3.ListenerFilter {
	var filters []*listenerv3.ListenerFilter

	filters = append(filters, tlsInspector())

	return filters
}

func tlsInspector() *listenerv3.ListenerFilter {
	config := &tls_inspectorv3.TlsInspector{}
	return listenerFilter("envoy.filters.listener.tls_inspector", config)
}

func listenerFilter(name string, config proto.Message) *listenerv3.ListenerFilter {
	return &listenerv3.ListenerFilter{
		Name: name,
		ConfigType: &listenerv3.ListenerFilter_TypedConfig{
			TypedConfig: typedConfig(config),
		},
	}
}
