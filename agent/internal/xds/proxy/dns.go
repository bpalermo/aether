package proxy

import (
	"fmt"
	"net"
	"strconv"
	"time"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	dnsv3 "github.com/envoyproxy/go-control-plane/envoy/data/dns/v3"
	dns_filterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/dns_filter/v3"
	"google.golang.org/protobuf/types/known/durationpb"
)

// listenerFilterDNSName is the Envoy UDP DNS filter name.
const listenerFilterDNSName = "envoy.filters.udp.dns_filter"

// dnsAnswerTTL is the TTL on synthesized mesh A records. Short, so a client picks up
// a sentinel/VIP change quickly; the real endpoint set lives in EDS, not DNS.
const dnsAnswerTTL = 30 * time.Second

// DNSVirtualDomain is one answerable mesh name: Domain (may be a wildcard like
// "*.aether.internal") resolved to the given A-record Addresses (the sentinel VIP).
type DNSVirtualDomain struct {
	Domain    string
	Addresses []string
}

// DNSCaptureListenerName returns the per-pod mesh-DNS listener name.
func DNSCaptureListenerName(cniPod *cniv1.CNIPod) string {
	return fmt.Sprintf("dns_%s", cniPod.GetName())
}

// GenerateDNSListener builds the per-pod mesh-DNS listener (proposal 018, mesh-global
// FQDN): a UDP listener bound to dnsPort inside the pod netns, running Envoy's
// dns_filter. The CNI redirects the pod's outbound :53 here; the filter answers the
// mesh virtual domains from its inline table and forwards everything else to the
// upstream resolvers (the pod's original kube-dns). Default off (no listener is
// generated unless mesh-DNS capture is enabled).
func GenerateDNSListener(cniPod *cniv1.CNIPod, dnsPort uint32, virtualDomains []DNSVirtualDomain, upstreamResolvers []string) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}
	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	return &listenerv3.Listener{
		Name: DNSCaptureListenerName(cniPod),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_UDP,
					Address:  defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: dnsPort,
					},
					NetworkNamespaceFilepath: cniPod.GetNetworkNamespace(),
				},
			},
		},
		ListenerFilters: []*listenerv3.ListenerFilter{
			listenerFilter(listenerFilterDNSName, BuildDNSFilterConfig(cniPod.GetName(), virtualDomains, upstreamResolvers)),
		},
		UdpListenerConfig: &listenerv3.UdpListenerConfig{},
	}, nil
}

// BuildDNSFilterConfig builds the dns_filter config: an inline DNS table answering the
// mesh virtual domains (each name -> its A-record addresses), plus a client config
// that forwards unmatched queries to the upstream resolvers (host:port; bare host
// defaults to :53).
func BuildDNSFilterConfig(statSuffix string, virtualDomains []DNSVirtualDomain, upstreamResolvers []string) *dns_filterv3.DnsFilterConfig {
	domains := make([]*dnsv3.DnsTable_DnsVirtualDomain, 0, len(virtualDomains))
	for _, vd := range virtualDomains {
		domains = append(domains, &dnsv3.DnsTable_DnsVirtualDomain{
			Name: vd.Domain,
			Endpoint: &dnsv3.DnsTable_DnsEndpoint{
				EndpointConfig: &dnsv3.DnsTable_DnsEndpoint_AddressList{
					AddressList: &dnsv3.DnsTable_AddressList{Address: vd.Addresses},
				},
			},
			AnswerTtl: durationpb.New(dnsAnswerTTL),
		})
	}

	resolvers := make([]*corev3.Address, 0, len(upstreamResolvers))
	for _, r := range upstreamResolvers {
		resolvers = append(resolvers, dnsResolverAddress(r))
	}

	return &dns_filterv3.DnsFilterConfig{
		StatPrefix: fmt.Sprintf("dns_%s", statSuffix),
		ServerConfig: &dns_filterv3.DnsFilterConfig_ServerContextConfig{
			ConfigSource: &dns_filterv3.DnsFilterConfig_ServerContextConfig_InlineDnsTable{
				InlineDnsTable: &dnsv3.DnsTable{VirtualDomains: domains},
			},
		},
		ClientConfig: &dns_filterv3.DnsFilterConfig_ClientContextConfig{
			ResolverTimeout:   durationpb.New(5 * time.Second),
			UpstreamResolvers: resolvers,
			MaxPendingLookups: 256,
		},
	}
}

// splitHostDNSPort splits host:port, defaulting a bare host to the DNS port 53.
func splitHostDNSPort(hostPort string) (string, uint32) {
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort, 53
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return host, 53
	}
	return host, uint32(p)
}

// dnsResolverAddress builds an upstream resolver UDP address; a bare host gets :53.
func dnsResolverAddress(hostPort string) *corev3.Address {
	host, port := splitHostDNSPort(hostPort)
	return &corev3.Address{
		Address: &corev3.Address_SocketAddress{
			SocketAddress: &corev3.SocketAddress{
				Protocol:      corev3.SocketAddress_UDP,
				Address:       host,
				PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
			},
		},
	}
}
