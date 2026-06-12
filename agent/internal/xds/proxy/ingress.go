package proxy

import (
	"fmt"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	healthcheckv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// defaultInboundAddress is the bind address for the per-pod inbound listener
	// (all interfaces within the pod's network namespace).
	defaultInboundAddress = "0.0.0.0"
	// defaultInboundPort is the mesh inbound port. The per-pod inbound listener binds
	// it inside the pod's network namespace, and source proxies dial the destination
	// pod at <pod_ip>:defaultInboundPort.
	defaultInboundPort = 15008

	// MeshLivePath is the liveness path answered locally by the inbound listener: a
	// 200 proves the proxy config is loaded and the listener is serving, and (since
	// the request arrived over the listener's mTLS) that mTLS termination is up.
	MeshLivePath = "/-/-/live"
	// MeshReadyPath is the readiness path: it returns 200 only when, in addition to
	// the liveness conditions, the pod's application health-probe cluster is healthy.
	MeshReadyPath = "/-/-/ready"

	livenessHealthCheckFilterName  = "envoy.filters.http.health_check.live"
	readinessHealthCheckFilterName = "envoy.filters.http.health_check.ready"
)

// InboundListenerName returns the name of a pod's inbound listener.
func InboundListenerName(cniPod *cniv1.CNIPod) string {
	return fmt.Sprintf("inbound_%s", cniPod.GetName())
}

// NewInboundListener builds a pod's inbound listener. It is bound into the pod's
// network namespace at :defaultInboundPort so the pod is reachable at
// <pod_ip>:defaultInboundPort, terminates mTLS presenting the pod's own SVID (so
// callers cryptographically verify they reached this pod, not just the node),
// requires and validates the caller's workload SVID, sets XFCC natively from the
// verified peer (SANITIZE_SET), and forwards the request to the pod's application
// on loopback (app_<pod>). Because the listener lives in the pod's netns, it follows
// the pod's lifecycle (drains on removal) and pod-scoped network policy applies to it.
func NewInboundListener(cniPod *cniv1.CNIPod, trustDomain string) (*listenerv3.Listener, error) {
	if cniPod == nil {
		return nil, fmt.Errorf("pod is required")
	}
	if cniPod.GetNetworkNamespace() == "" {
		return nil, fmt.Errorf("network namespace is required")
	}

	tlsCertificateSecretName := SpiffeIDFromPod(cniPod, trustDomain)
	validationContextName := fmt.Sprintf("spiffe://%s", trustDomain)

	return &listenerv3.Listener{
		Name:                          InboundListenerName(cniPod),
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultInboundPort,
					},
					NetworkNamespaceFilepath: cniPod.GetNetworkNamespace(),
				},
			},
		},
		// Shared across all pods (cardinality round 2): per-pod inbound traffic
		// visibility lives in client-side service-cluster stats and the per-pod
		// health_ membership gauges; the listener/HCM stat families aggregate
		// node-wide. Listener NAME stays per-pod — only stats collapse.
		StatPrefix:       "inbound",
		TrafficDirection: corev3.TrafficDirection_INBOUND,
		ListenerFilters:  buildInboundListenerFilters(),
		FilterChains: []*listenerv3.FilterChain{
			buildInboundFilterChain(cniPod, tlsCertificateSecretName, validationContextName),
		},
	}, nil
}

// buildInboundFilterChain builds the mTLS-terminating HTTP filter chain for a pod's
// inbound listener: it routes all requests to the pod's application cluster and sets
// XFCC from the verified peer certificate's URI SAN (the caller's SVID).
func buildInboundFilterChain(cniPod *cniv1.CNIPod, tlsCertificateSecretName, validationContextName string) *listenerv3.FilterChain {
	hcm := buildHTTPConnectionManager("inbound", buildInboundRouteConfiguration(AppClusterName(cniPod)))
	// Liveness/readiness are answered locally before the router; everything else
	// passes through to the pod's application.
	hcm.HttpFilters = []*http_connection_managerv3.HttpFilter{
		buildLivenessHealthCheckFilter(),
		buildReadinessHealthCheckFilter(HealthProbeClusterName(cniPod)),
		routerHttpFilter(),
	}
	// SANITIZE_SET replaces any client-supplied XFCC with details derived from the
	// verified peer certificate, exposing the caller's SPIFFE ID (URI SAN) to the app.
	hcm.ForwardClientCertDetails = http_connection_managerv3.HttpConnectionManager_SANITIZE_SET
	hcm.SetCurrentClientCertDetails = &http_connection_managerv3.HttpConnectionManager_SetCurrentClientCertDetails{
		Subject: wrapperspb.Bool(true),
		Uri:     true,
	}

	return &listenerv3.FilterChain{
		Name:            fmt.Sprintf("in_%s", cniPod.GetName()),
		Filters:         []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
		TransportSocket: DownstreamTransportSocket(tlsCertificateSecretName, validationContextName),
	}
}

// buildLivenessHealthCheckFilter answers MeshLivePath locally with 200. Reaching it
// means the listener is serving with its config loaded; because the listener
// requires mTLS, a successful response also proves mTLS termination is up. It does
// not depend on the application, so it stays green while the app is (re)starting.
func buildLivenessHealthCheckFilter() *http_connection_managerv3.HttpFilter {
	return buildHealthCheckFilter(livenessHealthCheckFilterName, MeshLivePath, nil)
}

// buildReadinessHealthCheckFilter answers MeshReadyPath with 200 only when the pod's
// application health-probe cluster (health_<pod>, which actively health-checks the
// app's readiness path) is healthy; otherwise it returns 503. This composes the
// liveness conditions with actual application readiness.
func buildReadinessHealthCheckFilter(healthClusterName string) *http_connection_managerv3.HttpFilter {
	return buildHealthCheckFilter(readinessHealthCheckFilterName, MeshReadyPath, map[string]*typev3.Percent{
		healthClusterName: {Value: 100},
	})
}

// buildHealthCheckFilter builds a non-pass-through health_check HTTP filter that
// intercepts requests whose :path exactly matches path and answers them locally.
// When clusterMinHealthy is set, it returns 503 unless every named cluster meets its
// minimum healthy percentage. Requests on other paths pass through to the router.
func buildHealthCheckFilter(name, path string, clusterMinHealthy map[string]*typev3.Percent) *http_connection_managerv3.HttpFilter {
	hc := &healthcheckv3.HealthCheck{
		PassThroughMode: wrapperspb.Bool(false),
		Headers: []*routev3.HeaderMatcher{
			{
				Name: ":path",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
					StringMatch: &matcherv3.StringMatcher{
						MatchPattern: &matcherv3.StringMatcher_Exact{Exact: path},
					},
				},
			},
		},
		ClusterMinHealthyPercentages: clusterMinHealthy,
	}
	return httpFilter(name, hc)
}

// buildInboundRouteConfiguration routes all inbound requests to the per-pod
// application cluster, which forwards to the pod's own application on loopback.
func buildInboundRouteConfiguration(appClusterName string) *routev3.RouteConfiguration {
	return &routev3.RouteConfiguration{
		Name: "in_http",
		// The app_<pod> cluster churns on pod restart; don't let the inline route's
		// cluster reference wedge the listener during the delta-xDS make-before-break
		// window if it is momentarily unknown.
		ValidateClusters: wrapperspb.Bool(false),
		VirtualHosts: []*routev3.VirtualHost{
			{
				Name:    "catch_all",
				Domains: []string{"*"},
				Routes: []*routev3.Route{
					{
						Match: &routev3.RouteMatch{
							PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
						},
						Action: &routev3.Route_Route{
							Route: &routev3.RouteAction{
								ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: appClusterName},
							},
						},
					},
				},
			},
		},
	}
}
