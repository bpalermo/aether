package proxy

import (
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
)

func NewClusterLoadAssignment(serviceName string) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: serviceName,
		Endpoints:   []*endpointv3.LocalityLbEndpoints{},
	}
}

func LocalityLbEndpointFromRegistryEndpoint(endpoint *registryv1.ServiceEndpoint) *endpointv3.LocalityLbEndpoints {
	return &endpointv3.LocalityLbEndpoints{
		LbEndpoints: []*endpointv3.LbEndpoint{
			{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{
						Address: &core.Address{
							Address: &core.Address_SocketAddress{
								SocketAddress: &core.SocketAddress{
									Protocol: core.SocketAddress_TCP,
									Address:  endpoint.GetIp(),
									PortSpecifier: &core.SocketAddress_PortValue{
										PortValue: defaultHTTPInboundPort,
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
