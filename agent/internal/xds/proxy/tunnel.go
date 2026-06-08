package proxy

import (
	"fmt"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	internalupstreamv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/internal_upstream/v3"
	rawbufferv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/raw_buffer/v3"
	metadatav3 "github.com/envoyproxy/go-control-plane/envoy/type/metadata/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	// TunnelInternalListenerName is the internal listener that encapsulates an
	// outbound stream into an HBONE-style HTTP/2 CONNECT tunnel to the
	// destination node.
	TunnelInternalListenerName = "tunnel_internal"
	// TunnelOriginateClusterName is the ORIGINAL_DST cluster the tunnel's
	// tcp_proxy dials; the destination node address is read per-endpoint from the
	// passed-through tunnel metadata.
	TunnelOriginateClusterName = "tunnel_originate"

	// tunnelMetadataNamespace carries the per-endpoint tunnel coordinates passed
	// across the internal-listener boundary by the internal_upstream transport
	// socket.
	tunnelMetadataNamespace = "tunnel"
	// tunnelAuthorityKey is the destination pod address used as the CONNECT
	// :authority (so the dest node routes the tunnel to that exact pod).
	tunnelAuthorityKey = "authority"
	// tunnelNodeKey is the destination node address (node_ip:port) the tunnel is
	// dialed to via the ORIGINAL_DST metadata_key.
	tunnelNodeKey = "node"

	// rawBufferTransportSocketName is the inner transport socket wrapped by
	// internal_upstream (the tunnel carries its own mTLS at the originate cluster).
	rawBufferTransportSocketName = "envoy.transport_sockets.raw_buffer"
	// internalUpstreamTransportSocketName passes the selected endpoint's metadata
	// (and the source filter state) across the internal-listener boundary.
	internalUpstreamTransportSocketName = "envoy.transport_sockets.internal_upstream"
)

// TunnelEndpointMetadata builds the Envoy metadata for a tunneled endpoint: the
// envoy.lb subset keys used for affinity selection, plus the tunnel namespace
// carrying the destination pod address (CONNECT authority) and node address
// (tunnel target). internal_upstream passes the tunnel namespace across the
// internal-listener boundary to the originate cluster.
func TunnelEndpointMetadata(authority, nodeAddr string, subsetKeys map[string]string) *corev3.Metadata {
	tunnel := &structpb.Struct{Fields: map[string]*structpb.Value{
		tunnelAuthorityKey: structpb.NewStringValue(authority),
		tunnelNodeKey:      structpb.NewStringValue(nodeAddr),
	}}

	lb := &structpb.Struct{Fields: map[string]*structpb.Value{}}
	for k, v := range subsetKeys {
		lb.Fields[k] = structpb.NewStringValue(v)
	}

	return &corev3.Metadata{
		FilterMetadata: map[string]*structpb.Struct{
			tunnelMetadataNamespace:            tunnel,
			envoyFilterMetadataSubsetNamespace: lb,
		},
	}
}

// NewTunnelServiceCluster builds the outbound service cluster for the tunnel data
// path. Its endpoints (delivered via EDS) are internal-listener addresses carrying
// per-endpoint tunnel metadata; the internal_upstream transport socket passes that
// metadata across the internal-listener boundary. lb_subset_config enables per-pod
// affinity (selection by endpoint IP) while defaulting to load balancing across all
// endpoints. The cluster itself is not mTLS — the tunnel's mTLS is at the originate
// cluster, presenting the originating pod's identity.
func NewTunnelServiceCluster(serviceName string) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:           serviceName,
		ConnectTimeout: durationpb.New(2 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		TransportSocket: InternalUpstreamTransportSocket(),
		LbSubsetConfig: &clusterv3.Cluster_LbSubsetConfig{
			FallbackPolicy: clusterv3.Cluster_LbSubsetConfig_ANY_ENDPOINT,
			SubsetSelectors: []*clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector{
				{Keys: []string{subsetIPKey}},
			},
		},
	}
}

// TunnelLocalityLbEndpointFromRegistryEndpoint builds a tunnel endpoint: its
// address is the internal tunnel listener, and its host metadata carries both the
// envoy.lb subset keys (for affinity selection) and the tunnel coordinates — the
// destination pod IP as the CONNECT authority and node_ip:15008 as the tunnel
// target. The internal_upstream transport socket on the service cluster passes
// this host metadata through to the tunnel.
func TunnelLocalityLbEndpointFromRegistryEndpoint(endpoint *registryv1.ServiceEndpoint) *endpointv3.LocalityLbEndpoints {
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

	nodeAddr := fmt.Sprintf("%s:%d", endpoint.GetKubernetesMetadata().GetNodeIp(), defaultNodeConnectPort)
	md := TunnelEndpointMetadata(endpoint.GetIp(), nodeAddr, subsetKeys)

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
							Address: &corev3.Address_EnvoyInternalAddress{
								EnvoyInternalAddress: &corev3.EnvoyInternalAddress{
									AddressNameSpecifier: &corev3.EnvoyInternalAddress_ServerListenerName{
										ServerListenerName: TunnelInternalListenerName,
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

// NewTunnelInternalListener builds the internal listener that encapsulates a
// stream into a CONNECT tunnel. Its tcp_proxy dials the originate cluster and
// sets the CONNECT :authority from the destination pod address carried in the
// passed-through tunnel metadata.
func NewTunnelInternalListener() *listenerv3.Listener {
	tcpProxy := &tcpproxyv3.TcpProxy{
		StatPrefix: TunnelInternalListenerName,
		ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{
			Cluster: TunnelOriginateClusterName,
		},
		TunnelingConfig: &tcpproxyv3.TcpProxy_TunnelingConfig{
			Hostname: "%DYNAMIC_METADATA(" + tunnelMetadataNamespace + ":" + tunnelAuthorityKey + ")%",
		},
	}

	return &listenerv3.Listener{
		Name:              TunnelInternalListenerName,
		ListenerSpecifier: &listenerv3.Listener_InternalListener{InternalListener: &listenerv3.Listener_InternalListenerConfig{}},
		FilterChains: []*listenerv3.FilterChain{
			{
				Filters: []*listenerv3.Filter{
					networkFilter("envoy.filters.network.tcp_proxy", tcpProxy),
				},
			},
		},
	}
}

// NewTunnelOriginateCluster builds the ORIGINAL_DST cluster the tunnel dials. The
// destination node address is taken per-request from the tunnel.node metadata
// (metadata_key). connection_pool_per_downstream_connection guarantees a source
// pod's tunnel connection is never reused for another source — so the per-source
// client certificate (selected by the transport-socket matcher on the source
// netns filter state) cannot leak across identities. on-no-match presents the
// node identity, used for reachability health-check tunnels.
func NewTunnelOriginateCluster(netnsToSpiffeID map[string]string, spiffeIDs []string, nodeSpiffeID, validationContextName string) *clusterv3.Cluster {
	matcher := UpstreamTransportSocketMatcher(netnsToSpiffeID)
	matcher.OnNoMatch = transportSocketNameOnMatch(nodeSpiffeID)

	matches := UpstreamTransportSocketMatches(append(spiffeIDs, nodeSpiffeID), validationContextName)

	return &clusterv3.Cluster{
		Name:                                  TunnelOriginateClusterName,
		ConnectTimeout:                        durationpb.New(2 * time.Second),
		ConnectionPoolPerDownstreamConnection: true,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_ORIGINAL_DST,
		},
		LbPolicy: clusterv3.Cluster_CLUSTER_PROVIDED,
		LbConfig: &clusterv3.Cluster_OriginalDstLbConfig_{
			OriginalDstLbConfig: &clusterv3.Cluster_OriginalDstLbConfig{
				MetadataKey: &metadatav3.MetadataKey{
					Key: tunnelMetadataNamespace,
					Path: []*metadatav3.MetadataKey_PathSegment{
						{Segment: &metadatav3.MetadataKey_PathSegment_Key{Key: tunnelNodeKey}},
					},
				},
			},
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(config.Http2ProtocolOptions()),
		},
		TransportSocketMatcher: matcher,
		TransportSocketMatches: matches,
	}
}

// InternalUpstreamTransportSocket wraps raw_buffer with the internal_upstream
// transport socket, passing the selected endpoint's tunnel metadata across the
// internal-listener boundary so the tcp_proxy can read the CONNECT authority and
// the originate cluster can read the node address.
func InternalUpstreamTransportSocket() *corev3.TransportSocket {
	cfg := &internalupstreamv3.InternalUpstreamTransport{
		PassthroughMetadata: []*internalupstreamv3.InternalUpstreamTransport_MetadataValueSource{
			{
				Name: tunnelMetadataNamespace,
				Kind: &metadatav3.MetadataKind{
					Kind: &metadatav3.MetadataKind_Host_{Host: &metadatav3.MetadataKind_Host{}},
				},
			},
		},
		TransportSocket: &corev3.TransportSocket{
			Name: rawBufferTransportSocketName,
			ConfigType: &corev3.TransportSocket_TypedConfig{
				TypedConfig: config.TypedConfig(&rawbufferv3.RawBuffer{}),
			},
		},
	}
	return &corev3.TransportSocket{
		Name: internalUpstreamTransportSocketName,
		ConfigType: &corev3.TransportSocket_TypedConfig{
			TypedConfig: config.TypedConfig(cfg),
		},
	}
}
