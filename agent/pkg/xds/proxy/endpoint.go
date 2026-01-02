package proxy

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	corev1 "k8s.io/api/core/v1"
)

func NewClusterLoadAssignment(name string) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: name,
		Endpoints: []*endpointv3.LocalityLbEndpoints{
			{},
		},
	}
}

func LocalityLbEndpointFromPod(pod *corev1.Pod) *endpointv3.LocalityLbEndpoints {
	return &endpointv3.LocalityLbEndpoints{
		LbEndpoints: []*endpointv3.LbEndpoint{
			{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{
						Address: &corev3.Address{
							Address: &corev3.Address_SocketAddress{
								SocketAddress: &corev3.SocketAddress{
									Protocol: corev3.SocketAddress_TCP,
									Address:  pod.Status.PodIP,
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
