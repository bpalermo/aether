package proxy

import (
	"fmt"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	udp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// MeshDNSClusterName is the static cluster the per-pod DNS listeners forward to —
	// the node agent's in-process resolver, reached at HOST_IP:resolverPort.
	MeshDNSClusterName = "mesh_dns"

	listenerFilterUDPProxyName = "envoy.filters.udp.udp_proxy"
)

// DNSListenerNames returns the per-pod UDP and TCP DNS listener names.
func DNSListenerNames(cniPod *cniv1.CNIPod) (udp, tcp string) {
	return fmt.Sprintf("dns_udp_%s", cniPod.GetName()), fmt.Sprintf("dns_tcp_%s", cniPod.GetName())
}

// GenerateDNSListeners builds the per-pod UDP + TCP listeners bound to dnsPort inside
// the pod netns (proposal 018, mesh-global FQDN). The CNI redirects the pod's :53
// here; the listeners transparently relay the datagrams/streams to the agent's
// resolver via the mesh_dns cluster (udp_proxy / tcp_proxy — NOT DNS-aware, so
// c-ares is unaffected). Envoy binds these the same way it binds the pod's other
// listeners (NetworkNamespaceFilepath, NET_ADMIN) — no extra privilege.
func GenerateDNSListeners(cniPod *cniv1.CNIPod, dnsPort uint32) ([]*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}
	netns := cniPod.GetNetworkNamespace()
	if netns == "" {
		return nil, fmt.Errorf("network namespace is required")
	}
	udpName, tcpName := DNSListenerNames(cniPod)

	udp := &listenerv3.Listener{
		Name:              udpName,
		Address:           dnsSocketAddress(corev3.SocketAddress_UDP, dnsPort, netns),
		UdpListenerConfig: &listenerv3.UdpListenerConfig{},
		ListenerFilters: []*listenerv3.ListenerFilter{
			listenerFilter(listenerFilterUDPProxyName, &udp_proxyv3.UdpProxyConfig{
				StatPrefix:     fmt.Sprintf("dns_udp_%s", cniPod.GetName()),
				RouteSpecifier: &udp_proxyv3.UdpProxyConfig_Cluster{Cluster: MeshDNSClusterName},
			}),
		},
	}

	tcp := &listenerv3.Listener{
		Name:    tcpName,
		Address: dnsSocketAddress(corev3.SocketAddress_TCP, dnsPort, netns),
		FilterChains: []*listenerv3.FilterChain{{
			Name: tcpName,
			Filters: []*listenerv3.Filter{{
				Name: "envoy.filters.network.tcp_proxy",
				ConfigType: &listenerv3.Filter_TypedConfig{
					TypedConfig: config.TypedConfig(&tcp_proxyv3.TcpProxy{
						StatPrefix:       fmt.Sprintf("dns_tcp_%s", cniPod.GetName()),
						ClusterSpecifier: &tcp_proxyv3.TcpProxy_Cluster{Cluster: MeshDNSClusterName},
					}),
				},
			}},
		}},
	}

	return []*listenerv3.Listener{udp, tcp}, nil
}

func dnsSocketAddress(proto corev3.SocketAddress_Protocol, port uint32, netns string) *corev3.Address {
	return &corev3.Address{
		Address: &corev3.Address_SocketAddress{
			SocketAddress: &corev3.SocketAddress{
				Protocol:                 proto,
				Address:                  defaultInboundAddress,
				PortSpecifier:            &corev3.SocketAddress_PortValue{PortValue: port},
				NetworkNamespaceFilepath: netns,
			},
		},
	}
}

// NewMeshDNSCluster builds the static cluster pointing at the agent's in-process
// resolver on the node (HOST_IP:resolverPort).
func NewMeshDNSCluster(hostIP string, resolverPort uint32) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                          MeshDNSClusterName,
		ConnectTimeout:                durationpb.New(2 * time.Second),
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		ClusterDiscoveryType:          &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: MeshDNSClusterName,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
						Endpoint: &endpointv3.Endpoint{
							Address: &corev3.Address{
								Address: &corev3.Address_SocketAddress{
									SocketAddress: &corev3.SocketAddress{
										Protocol:      corev3.SocketAddress_TCP,
										Address:       hostIP,
										PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: resolverPort},
									},
								},
							},
						},
					},
				}},
			}},
		},
	}
}
