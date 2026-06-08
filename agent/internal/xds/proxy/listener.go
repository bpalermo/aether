package proxy

import (
	"fmt"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
)

const (
	// defaultInboundAddress is the address for inbound listeners (all interfaces)
	defaultInboundAddress = "0.0.0.0"
	// defaultHTTPInboundPort is the port for inbound HTTP listeners
	defaultHTTPInboundPort = 18080
	// defaultOutboundAddress is the address for outbound listeners (localhost only)
	defaultOutboundAddress = "127.0.0.1"
	// defaultHTTPOutboundPort is the port for outbound HTTP listeners
	defaultHTTPOutboundPort = 18081
)

// GenerateListenersFromRegistryPod generates inbound and outbound HTTP listeners
// and the per-pod application cluster for a pod.
// Inbound listeners accept traffic from any interface on the pod's network namespace
// and forward decrypted traffic to the application cluster (the pod's own app on
// loopback). Outbound listeners route traffic destined for other services.
// Both listeners use HTTP protocol and include appropriate filter chains.
// The trustDomain is the SPIFFE trust domain URI used for SDS validation context.
func GenerateListenersFromRegistryPod(cniPod *cniv1.CNIPod, trustDomain string) (inbound *listenerv3.Listener, outbound *listenerv3.Listener, appCluster *clusterv3.Cluster, healthCluster *clusterv3.Cluster, err error) {
	inbound, err = generateInboundHTTPListener(cniPod, trustDomain)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	outbound, err = generateOutboundHTTPListener(cniPod)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	netns := cniPod.GetNetworkNamespace()
	port := AppPortFromPod(cniPod)
	appCluster = NewAppCluster(AppClusterName(cniPod), netns, port)
	// Separate, unrouted cluster carrying the active app health check (delegated
	// liveness); keeping the HC off app_<pod> avoids gating the delivery path.
	healthCluster = NewAppHealthProbeCluster(HealthProbeClusterName(cniPod), netns, port, AppHealthPathFromPod(cniPod))

	return inbound, outbound, appCluster, healthCluster, nil
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

func generateInboundHTTPListener(cniPod *cniv1.CNIPod, trustDomain string) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}

	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	tlsCertificateSecretName := SpiffeIDFromPod(cniPod, trustDomain)
	validationContextName := fmt.Sprintf("spiffe://%s", trustDomain)

	return &listenerv3.Listener{
		Name: fmt.Sprintf("inbound_http_%s", cniPod.GetName()),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultHTTPInboundPort,
					},
					NetworkNamespaceFilepath: cniPod.GetNetworkNamespace(),
				},
			},
		},
		StatPrefix:       fmt.Sprintf("in_http_%s", cniPod.GetName()),
		TrafficDirection: corev3.TrafficDirection_INBOUND,
		ListenerFilters:  buildInboundListenerFilters(),
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultInboundHTTPFilterChain(cniPod.GetName(), AppClusterName(cniPod), tlsCertificateSecretName, validationContextName),
		},
	}, nil
}

func generateOutboundHTTPListener(cniPod *cniv1.CNIPod) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}

	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	return &listenerv3.Listener{
		Name: fmt.Sprintf("outbound_http_%s", cniPod.GetName()),
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
		StatPrefix:       fmt.Sprintf("out_http_%s", cniPod.GetName()),
		TrafficDirection: corev3.TrafficDirection_OUTBOUND,
		FilterChains: []*listenerv3.FilterChain{
			buildDefaultOutboundHTTPFilterChain(cniPod.GetName()),
		},
	}, nil
}
