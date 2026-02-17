package proxy

import (
	"fmt"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
)

const (
	defaultInboundAddress   = "0.0.0.0"
	defaultHTTPInboundPort  = 18080
	defaultOutboundAddress  = "127.0.0.1"
	defaultHTTPOutboundPort = 18081
)

func GenerateListenersFromRegistryPod(cniPod *cniv1.CNIPod) (inbound *listenerv3.Listener, outbound *listenerv3.Listener, err error) {
	inbound, err = generateInboundHTTPListener(cniPod)
	if err != nil {
		return nil, nil, err
	}

	outbound, err = generateOutboundHTTPListener(cniPod)
	if err != nil {
		return nil, nil, err
	}

	return inbound, outbound, nil
}

func generateInboundHTTPListener(cniPod *cniv1.CNIPod) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}

	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	return &listenerv3.Listener{
		Name: "inbound_http",
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultHTTPInboundPort,
					},
					NetworkNamespaceFilepath: cniPod.GetNetworkNamespace(),
				},
			},
		},
		StatPrefix:       fmt.Sprintf("in_http_%s", cniPod.GetName()),
		TrafficDirection: corev3.TrafficDirection_INBOUND,
		ListenerFilters:  buildInboundListenerFilters(),
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultInboundHTTPFilterChain(cniPod.GetName()),
		},
	}, nil
}

func generateOutboundHTTPListener(cniPod *cniv1.CNIPod) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}

	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	return &listenerv3.Listener{
		Name: "outbound_http",
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultOutboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultHTTPOutboundPort,
					},
					NetworkNamespaceFilepath: cniPod.GetNetworkNamespace(),
				},
			},
		},
		StatPrefix:       fmt.Sprintf("out_http_%s", cniPod.GetName()),
		TrafficDirection: corev3.TrafficDirection_OUTBOUND,
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultOutboundHTTPFilterChain(cniPod.GetName()),
		},
	}, nil
}
