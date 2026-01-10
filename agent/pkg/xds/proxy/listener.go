package proxy

import (
	"fmt"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
)

const (
	defaultInboundAddress   = "0.0.0.0"
	defaultHTTPInboundPort  = 18080
	defaultOutboundAddress  = "127.0.0.1"
	defaultHTTPOutboundPort = 18081
)

func GenerateListenersFromEvent(cniPod *registryv1.CNIPod) (inbound *listenerv3.Listener, outbound *listenerv3.Listener) {
	inbound = generateInboundHTTPListener(cniPod)
	outbound = generateOutboundHTTPListener(cniPod)

	return inbound, outbound
}

func generateInboundHTTPListener(cniPod *registryv1.CNIPod) *listenerv3.Listener {
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
					NetworkNamespaceFilepath: cniPod.NetworkNamespace,
				},
			},
		},
		TrafficDirection: corev3.TrafficDirection_INBOUND,
		ListenerFilters:  buildInboundListenerFilters(),
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultInboundHTTPFilterChain(cniPod.Name),
		},
	}
}

func generateOutboundHTTPListener(cniPod *registryv1.CNIPod) *listenerv3.Listener {
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
					NetworkNamespaceFilepath: cniPod.NetworkNamespace,
				},
			},
		},
		TrafficDirection: corev3.TrafficDirection_OUTBOUND,
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultOutboundHTTPFilterChain(cniPod.Name),
		},
	}
}
