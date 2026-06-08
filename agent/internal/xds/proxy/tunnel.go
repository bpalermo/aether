package proxy

import (
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
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
