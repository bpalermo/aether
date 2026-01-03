package proxy

import (
	"fmt"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

const (
	defaultInboundAddress   = "0.0.0.0"
	defaultHTTPInboundPort  = 18080
	defaultOutboundAddress  = "127.0.0.1"
	defaultHTTPOutboundPort = 18081
)

func GenerateListenersFromEvent(netNs *registryv1.Event_NetworkNamespace) []types.Resource {
	var listeners []types.Resource
	listeners = append(listeners, generateInboundHTTPListener(netNs))
	listeners = append(listeners, generateOutboundHTTPListener(netNs))

	return listeners
}

func generateInboundHTTPListener(netNs *registryv1.Event_NetworkNamespace) *listenerv3.Listener {
	return &listenerv3.Listener{
		Name: fmt.Sprintf("inbound_http"),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultHTTPInboundPort,
					},
					NetworkNamespaceFilepath: netNs.Path,
				},
			},
		},
		TrafficDirection: corev3.TrafficDirection_INBOUND,
		ListenerFilters:  buildInboundListenerFilters(),
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultInboundHTTPFilterChain(netNs.Pod.Name),
		},
	}
}

func generateOutboundHTTPListener(netNs *registryv1.Event_NetworkNamespace) *listenerv3.Listener {
	return &listenerv3.Listener{
		Name: fmt.Sprintf("outbound_http"),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultOutboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultHTTPOutboundPort,
					},
					NetworkNamespaceFilepath: netNs.Path,
				},
			},
		},
		TrafficDirection: corev3.TrafficDirection_OUTBOUND,
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultOutboundHTTPFilterChain(netNs.Pod.Name),
		},
	}
}
