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

func GenerateListenersFromEntries(entries []*registryv1.RegistryEntry) []types.Resource {
	var listeners []types.Resource
	for _, entry := range entries {
		listeners = append(listeners, generateInboundHTTPListener(entry))
		listeners = append(listeners, generateOutboundHTTPListener(entry))
	}

	return listeners
}

func generateInboundHTTPListener(entry *registryv1.RegistryEntry) *listenerv3.Listener {
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
					NetworkNamespaceFilepath: entry.NetworkNs,
				},
			},
		},
		TrafficDirection: corev3.TrafficDirection_INBOUND,
		ListenerFilters:  buildInboundListenerFilters(),
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultInboundHTTPFilterChain(entry.PodName),
		},
	}
}

func generateOutboundHTTPListener(entry *registryv1.RegistryEntry) *listenerv3.Listener {
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
					NetworkNamespaceFilepath: entry.NetworkNs,
				},
			},
		},
		TrafficDirection: corev3.TrafficDirection_OUTBOUND,
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultOutboundHTTPFilterChain(entry.PodName),
		},
	}
}
