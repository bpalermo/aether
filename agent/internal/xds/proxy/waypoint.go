package proxy

import (
	"fmt"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// WaypointTunnelListenerName is the node's host-netns east/west tunnel listener
// (proposal 019). One per node; it accepts cross-cluster mTLS dialed at
// node_ip:<tunnel-port> and SNI-forwards it to the destination local pod.
const WaypointTunnelListenerName = "ew_tunnel"

// WaypointIngressClusterName is the STATIC cluster of a service's LOCAL pods (at
// the mesh inbound port) that the node tunnel forwards a cross-cluster connection
// to. Keyed by the service FQDN, so it is unique per service on the node.
func WaypointIngressClusterName(fqdn string) string { return "ew_ingress_" + fqdn }

// BuildWaypointTunnelListener builds the host-netns east/west tunnel listener:
// tls_inspector reads the ClientHello SNI and each chain (built by
// BuildWaypointTunnelChain) routes by the SNI suffix to a local service's pods.
// It binds 0.0.0.0:<tunnel-port> WITHOUT a network-namespace filepath, so it
// lives in the host netns (the proxy DaemonSet is host-network and can bind the
// node address). No TLS termination — the source's end-to-end mTLS passes
// through to the destination pod. Returns nil when there are no chains.
func BuildWaypointTunnelListener(tunnelPort uint32, chains []*listenerv3.FilterChain) *listenerv3.Listener {
	if len(chains) == 0 {
		return nil
	}
	return &listenerv3.Listener{
		Name: WaypointTunnelListenerName,
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  "0.0.0.0",
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: tunnelPort,
					},
				},
			},
		},
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		StatPrefix:                    WaypointTunnelListenerName,
		TrafficDirection:              corev3.TrafficDirection_INBOUND,
		ListenerFilters:               []*listenerv3.ListenerFilter{tlsInspector()},
		FilterChains:                  chains,
	}
}

// BuildWaypointTunnelChain builds one local-service chain for the tunnel: it
// matches the structured waypoint SNI by suffix ("*.<fqdn>" — Envoy's leftmost
// wildcard is a suffix match, which is safe because aether owns the SNI and each
// service has a unique .<fqdn> suffix) and tcp_proxies the raw (still-mTLS)
// stream to that service's local pods at :18008.
func BuildWaypointTunnelChain(fqdn string) *listenerv3.FilterChain {
	name := "ew_" + fqdn
	return &listenerv3.FilterChain{
		Name: name,
		FilterChainMatch: &listenerv3.FilterChainMatch{
			ServerNames:       []string{"*." + fqdn},
			TransportProtocol: "tls",
		},
		Filters: []*listenerv3.Filter{
			buildTCPProxyNetworkFilter(name, WaypointIngressClusterName(fqdn)),
		},
	}
}

// BuildWaypointIngressCluster builds the STATIC cluster of a service's local pods
// (podIP:defaultInboundPort) that the node tunnel forwards to. No transport
// socket: the bytes are already the source's end-to-end mTLS, forwarded raw; the
// destination pod's inbound listener terminates it. podIPs must be non-empty and
// pre-sorted by the caller for snapshot stability.
func BuildWaypointIngressCluster(fqdn string, podIPs []string) *clusterv3.Cluster {
	name := WaypointIngressClusterName(fqdn)
	lbEndpoints := make([]*endpointv3.LbEndpoint, 0, len(podIPs))
	for _, ip := range podIPs {
		lbEndpoints = append(lbEndpoints, &endpointv3.LbEndpoint{
			HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
				Endpoint: &endpointv3.Endpoint{
					Address: &corev3.Address{
						Address: &corev3.Address_SocketAddress{
							SocketAddress: &corev3.SocketAddress{
								Protocol: corev3.SocketAddress_TCP,
								Address:  ip,
								PortSpecifier: &corev3.SocketAddress_PortValue{
									PortValue: defaultInboundPort,
								},
							},
						},
					},
				},
			},
		})
	}
	return &clusterv3.Cluster{
		Name:                 name,
		ConnectTimeout:       durationpb.New(5 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: lbEndpoints,
			}},
		},
	}
}

// waypointTunnelChainName is exported indirectly via BuildWaypointTunnelChain; a
// tiny helper kept for symmetry / tests.
func waypointTunnelChainName(fqdn string) string { return fmt.Sprintf("ew_%s", fqdn) }
