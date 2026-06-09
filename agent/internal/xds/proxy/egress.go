package proxy

import (
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
)

// NewServiceCluster builds the outbound service cluster. Endpoints (delivered via
// EDS) are the destination pods' mesh inbound addresses (pod_ip:defaultInboundPort),
// each reached by the pod's own netns-bound inbound listener. The cluster speaks
// HTTP/2 mTLS to the destination pod, presenting the originating pod's identity. The
// per-source mTLS transport socket is injected at snapshot time (InjectUpstreamMTLS)
// from the current local workloads; connection_pool_per_downstream_connection keeps a
// source pod's connection (and therefore its certificate) from being reused for
// another source.
func NewServiceCluster(serviceName string) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                                  serviceName,
		ConnectTimeout:                        durationpb.New(2 * time.Second),
		ConnectionPoolPerDownstreamConnection: true,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(config.Http2ProtocolOptions()),
		},
		LbSubsetConfig: &clusterv3.Cluster_LbSubsetConfig{
			FallbackPolicy: clusterv3.Cluster_LbSubsetConfig_ANY_ENDPOINT,
			SubsetSelectors: []*clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector{
				{Keys: []string{subsetIPKey}},
			},
		},
	}
}

// InjectUpstreamMTLS sets the per-source mTLS transport socket on a service cluster:
// a transport-socket matcher keyed on the source pod's network namespace selects
// that pod's client certificate (on-no-match presents the node identity). The
// matches reference each local workload's SVID over SDS. This is applied at snapshot
// time because it depends on the current set of local workloads.
func InjectUpstreamMTLS(cluster *clusterv3.Cluster, netnsToSpiffeID map[string]string, spiffeIDs []string, nodeSpiffeID, validationContextName string) {
	matcher := UpstreamTransportSocketMatcher(netnsToSpiffeID)
	matcher.OnNoMatch = transportSocketNameOnMatch(nodeSpiffeID)

	cluster.TransportSocketMatcher = matcher
	cluster.TransportSocketMatches = UpstreamTransportSocketMatches(append(spiffeIDs, nodeSpiffeID), validationContextName)
}

// ServiceLocalityLbEndpointFromRegistryEndpoint builds an endpoint for the plain
// transport: its socket address is the destination pod's mesh inbound
// (<pod_ip>:defaultInboundPort), reached by the pod's own netns-bound inbound
// listener, and its host metadata carries the envoy.lb subset keys for affinity.
// The endpoint health reflects the registry's delegated-liveness status.
func ServiceLocalityLbEndpointFromRegistryEndpoint(endpoint *registryv1.ServiceEndpoint) *endpointv3.LocalityLbEndpoints {
	subsetKeys := map[string]string{
		subsetClusterKey: endpoint.GetClusterName(),
		subsetIPKey:      endpoint.GetIp(),
	}
	if endpoint.GetKubernetesMetadata() != nil {
		subsetKeys[subsetPodNamespaceKey] = endpoint.GetKubernetesMetadata().GetNamespace()
		subsetKeys[subsetPodNameKey] = endpoint.GetKubernetesMetadata().GetPodName()
	}
	for k, v := range endpoint.GetMetadata() {
		subsetKeys[k] = v
	}

	lb := &structpb.Struct{Fields: map[string]*structpb.Value{}}
	for k, v := range subsetKeys {
		lb.Fields[k] = structpb.NewStringValue(v)
	}
	md := &corev3.Metadata{
		FilterMetadata: map[string]*structpb.Struct{
			envoyFilterMetadataSubsetNamespace: lb,
		},
	}

	var locality *corev3.Locality
	if loc := endpoint.GetLocality(); loc != nil && loc.GetRegion() != "" && loc.GetZone() != "" {
		locality = &corev3.Locality{Region: loc.GetRegion(), Zone: loc.GetZone()}
	}

	return &endpointv3.LocalityLbEndpoints{
		LbEndpoints: []*endpointv3.LbEndpoint{
			{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{
						Address: &corev3.Address{
							Address: &corev3.Address_SocketAddress{
								SocketAddress: &corev3.SocketAddress{
									Protocol: corev3.SocketAddress_TCP,
									// The destination pod's mesh inbound; reached by the
									// pod's own netns-bound inbound listener.
									Address: endpoint.GetIp(),
									PortSpecifier: &corev3.SocketAddress_PortValue{
										PortValue: defaultInboundPort,
									},
								},
							},
						},
					},
				},
				Metadata:     md,
				HealthStatus: endpointHealthStatus(endpoint),
			},
		},
		Locality: locality,
	}
}

// endpointHealthStatus maps the registry endpoint's delegated-liveness health to
// the Envoy EDS health status. Unknown/unspecified defaults to HEALTHY so older
// agents and freshly registered endpoints route until proven unhealthy.
func endpointHealthStatus(endpoint *registryv1.ServiceEndpoint) corev3.HealthStatus {
	if endpoint.GetHealth() == registryv1.ServiceEndpoint_HEALTH_UNHEALTHY {
		return corev3.HealthStatus_UNHEALTHY
	}
	return corev3.HealthStatus_HEALTHY
}
