package proxy

import (
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	// envoyFilterMetadataSubsetNamespace is the Envoy metadata namespace for load balancing subsets
	envoyFilterMetadataSubsetNamespace = "envoy.lb"

	// subsetClusterKey is the metadata key for the cluster name
	subsetClusterKey = "cluster"
	// subsetIPKey is the metadata key for the endpoint IP address
	subsetIPKey = "ip"
	// subsetPodNamespaceKey is the metadata key for the pod namespace
	subsetPodNamespaceKey = "namespace"
	// subsetPodNameKey is the metadata key for the pod name
	subsetPodNameKey = "pod"

	// defaultLocalEndpointBindAddress is the default bind address for local endpoints (localhost)
	defaultLocalEndpointBindAddress = "127.0.0.1"
)

// NewClusterLoadAssignment creates an empty cluster load assignment for a service.
// Endpoints should be added to the Endpoints field.
func NewClusterLoadAssignment(serviceName string) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: serviceName,
		Endpoints:   []*endpointv3.LocalityLbEndpoints{},
	}
}

// LocalLocalityLbEndpointFromRegistryEndpoint creates a locality lb endpoint for a local service endpoint.
// It binds to 127.0.0.1 (localhost) for local-only access within the pod's network namespace.
func LocalLocalityLbEndpointFromRegistryEndpoint(endpoint *registryv1.ServiceEndpoint) *endpointv3.LocalityLbEndpoints {
	return &endpointv3.LocalityLbEndpoints{
		LbEndpoints: []*endpointv3.LbEndpoint{
			{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{
						Address: &core.Address{
							Address: &core.Address_SocketAddress{
								SocketAddress: &core.SocketAddress{
									Protocol: core.SocketAddress_TCP,
									Address:  defaultLocalEndpointBindAddress,
									PortSpecifier: &core.SocketAddress_PortValue{
										PortValue: endpoint.GetPort(),
									},
								},
							},
						},
						HealthCheckConfig: &endpointv3.Endpoint_HealthCheckConfig{
							Hostname:  "localhost",
							PortValue: endpoint.GetPort(),
						},
					},
				},
			},
		},
	}
}

// LocalityLbEndpointFromRegistryEndpoint creates a locality lb endpoint from a service endpoint in the registry.
// It includes the endpoint's IP address and port, along with metadata for load balancing subsets.
// Metadata includes cluster name, IP, pod name, and pod namespace for topology-aware routing.
// If the endpoint includes locality information (region/zone), it is included in the endpoint.
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
