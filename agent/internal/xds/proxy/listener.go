package proxy

import (
	"fmt"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// defaultOutboundAddress is the address for outbound listeners (localhost only)
	defaultOutboundAddress = "127.0.0.1"
	// defaultHTTPOutboundPort is the port for outbound HTTP listeners. Shared
	// with the CNI plugin, which probes it in-netns for data-plane readiness.
	defaultHTTPOutboundPort = constants.ProxyOutboundPort
	// perConnectionBufferLimitBytes caps read/write buffering per connection on
	// every generated listener and cluster (Envoy edge-hardening guidance: 32
	// KiB). Envoy's default is 1 MiB per connection per direction — with the
	// node proxy's per-pod listeners and per-source upstream pools carrying
	// thousands of connections, that default turns connection-count incidents
	// into memory incidents. Flow control (watermarks) handles larger payloads;
	// this does not cap request/response sizes.
	perConnectionBufferLimitBytes = 32 * 1024
)

// OutboundListenerName returns the name of the per-pod outbound HTTP listener,
// used by the CNI server to await Envoy's delta-xDS ACK of the listener.
func OutboundListenerName(cniPod *cniv1.CNIPod) string {
	return fmt.Sprintf("outbound_http_%s", cniPod.GetName())
}

// GenerateListenersFromRegistryPod generates the per-pod inbound and outbound HTTP
// listeners and the per-pod application and health-probe clusters for a pod.
// The inbound listener (netns-bound, mTLS) accepts mesh traffic at <pod_ip>:15008
// and forwards it to the pod's application on loopback; the outbound listener routes
// the pod's traffic to other services. The trustDomain names the pod's SVID and the
// SDS validation context for the inbound listener's mTLS.
// appClusters is one per served port (the SNI-selected inbound chains forward
// to these); healthCluster is the single delegated-liveness probe on the
// primary port.
// cleartext (SPIRE off) builds the inbound listener without a downstream mTLS
// transport socket — symmetric with the cleartext outbound clusters — so the mesh
// data path is routable without SPIRE.
func GenerateListenersFromRegistryPod(cniPod *cniv1.CNIPod, trustDomain string, meshDomain string, emitStatsPod bool, cleartext bool, extensionFilters []*http_connection_managerv3.HttpFilter, inboundFilter *ExtensionFilter) (inbound *listenerv3.Listener, outbound *listenerv3.Listener, appClusters []*clusterv3.Cluster, healthCluster *clusterv3.Cluster, err error) {
	// Inbound never carries the egress source-metadata entry: the caller identity
	// on the inbound path is the verified XFCC, and stamping the destination pod
	// into aether.source would mislead authz policies.
	inbound, err = NewInboundListener(cniPod, trustDomain, emitStatsPod, cleartext, WithoutSourceMetadata(extensionFilters), inboundFilter)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	outbound, err = GenerateOutboundHTTPListener(cniPod, meshDomain, emitStatsPod, extensionFilters)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	netns := cniPod.GetNetworkNamespace()
	// One app cluster per served port, each bound into the pod's netns at
	// 127.0.0.1:<port>; the matching inbound SNI chain routes to it. The
	// per-port app protocol (h1 default, h2 via the "=h2" annotation suffix)
	// sets the loopback hop's codec — protocol heterogeneity across ports.
	h2Ports := AppPortProtocols(cniPod)
	for _, port := range AppPortsFromPod(cniPod) {
		appClusters = append(appClusters, NewAppCluster(AppClusterName(cniPod, port), netns, port, h2Ports[port]))
	}
	// Separate, unrouted cluster carrying the active app health check (delegated
	// liveness) on the primary port; keeping the HC off app_<pod> avoids gating
	// the delivery path. Liveness stays pod-level (primary port), not per-port.
	primary := AppPortFromPod(cniPod)
	// TCP-floor (non-HTTP) services have no HTTP readiness surface: the probe is a
	// raw TCP connect to the app port instead of an HTTP GET.
	isTCP := cniPod.GetAnnotations()[constants.AnnotationEndpointProtocol] == constants.ProtocolTCP
	healthCluster = NewAppHealthProbeCluster(HealthProbeClusterName(cniPod), netns, primary, AppHealthPathFromPod(cniPod), isTCP)

	return inbound, outbound, appClusters, healthCluster, nil
}

// SpiffeIDFromPod returns the SPIFFE ID for the pod. It first checks the
// aether.io/spiffe-id annotation. If not set, it constructs the SPIFFE ID
// from the trust domain, namespace, and service account using the standard
// SPIRE convention: spiffe://<trust-domain>/ns/<namespace>/sa/<service-account>.
func SpiffeIDFromPod(cniPod *cniv1.CNIPod, trustDomain string) string {
	if id, ok := cniPod.GetAnnotations()[constants.AnnotationSpiffeID]; ok && id != "" {
		return id
	}
	return fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, cniPod.GetNamespace(), cniPod.GetServiceAccount())
}

func GenerateOutboundHTTPListener(cniPod *cniv1.CNIPod, meshDomain string, emitStatsPod bool, extensionFilters []*http_connection_managerv3.HttpFilter) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}

	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	return &listenerv3.Listener{
		Name: OutboundListenerName(cniPod),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultOutboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultHTTPOutboundPort,
					},
					NetworkNamespaceFilepath: cniPod.GetNetworkNamespace(),
				},
			},
		},
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		// Per-pod listener stats kept (see ingress.go); "out_http_<pod>" is the
		// shape the aether.pod stats_tag extracts.
		StatPrefix:       fmt.Sprintf("out_http_%s", cniPod.GetName()),
		TrafficDirection: corev3.TrafficDirection_OUTBOUND,
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultOutboundHTTPFilterChain(cniPod, meshDomain, emitStatsPod, extensionFilters),
		},
	}, nil
}
