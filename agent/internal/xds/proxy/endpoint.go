package proxy

import (
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	envoyFilterMetadataSubsetNamespace = "envoy.lb"

	subsetClusterKey      = "cluster"
	subsetIPKey           = "ip"
	subsetPodNamespaceKey = "namespace"
	subsetPodNameKey      = "pod"
)

func NewClusterLoadAssignment(serviceName string) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: serviceName,
		Endpoints:   []*endpointv3.LocalityLbEndpoints{},
	}
}

func LocalityLbEndpointFromRegistryEndpoint(endpoint *registryv1.ServiceEndpoint) *endpointv3.LocalityLbEndpoints {
	var locality *core.Locality
	if endpoint.GetLocality() != nil && endpoint.GetLocality().GetRegion() != "" && endpoint.GetLocality().GetZone() != "" {
		locality = &core.Locality{
			Region: endpoint.GetLocality().GetRegion(),
			Zone:   endpoint.GetLocality().GetZone(),
		}
	}

	// add required metadata
	metadata := &core.Metadata{
		FilterMetadata: map[string]*structpb.Struct{
			envoyFilterMetadataSubsetNamespace: {
				Fields: map[string]*structpb.Value{
					subsetClusterKey: structpb.NewStringValue(endpoint.GetClusterName()),
					subsetIPKey:      structpb.NewStringValue(endpoint.GetIp()),
				},
			},
		},
	}

	if endpoint.GetKubernetesMetadata() != nil {
		metadata.FilterMetadata[envoyFilterMetadataSubsetNamespace].Fields[subsetPodNamespaceKey] = structpb.NewStringValue(endpoint.GetKubernetesMetadata().GetNamespace())
		metadata.FilterMetadata[envoyFilterMetadataSubsetNamespace].Fields[subsetPodNameKey] = structpb.NewStringValue(endpoint.GetKubernetesMetadata().GetPodName())
	}

	// add user defined metadata
	if endpoint.GetMetadata() != nil {
		for key, value := range endpoint.GetMetadata() {
			metadata.FilterMetadata[envoyFilterMetadataSubsetNamespace].Fields[key] = structpb.NewStringValue(value)
		}
	}

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
		Locality: locality,
		Metadata: metadata,
	}
}
