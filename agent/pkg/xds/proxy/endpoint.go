package proxy

import (
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
)

func NewClusterLoadAssignment(name string) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: name,
		Endpoints:   []*endpointv3.LocalityLbEndpoints{},
	}
}

func LocalityLbEndpointFromPod(pod *registryv1.Event_KubernetesPod) *endpointv3.LocalityLbEndpoints {
	return &endpointv3.LocalityLbEndpoints{
		LbEndpoints: []*endpointv3.LbEndpoint{
			{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{
						Address: &corev3.Address{
							Address: &corev3.Address_SocketAddress{
								SocketAddress: &corev3.SocketAddress{
									Protocol: corev3.SocketAddress_TCP,
									Address:  pod.Ip,
									PortSpecifier: &corev3.SocketAddress_PortValue{
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
