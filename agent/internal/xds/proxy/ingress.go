package proxy

import (
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	healthcheckv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// NodeInboundListenerName is the node-level inbound listener that terminates
	// mTLS from peer node proxies and demuxes each request to the destination pod
	// by its rewritten :authority (the dest pod IP).
	NodeInboundListenerName = "node_inbound"
	// defaultInboundAddress is the bind address for the node inbound listener
	// (all interfaces); peer node proxies dial it across the node network.
	defaultInboundAddress = "0.0.0.0"
	// defaultNodeInboundPort is the host port the node inbound listener binds. It is
	// node-level (host network namespace): all inbound mesh traffic arrives here and
	// is routed to the local pod named by the request authority.
	defaultNodeInboundPort = 15008
	// NodeLivePath is the readiness path a peer proxy's reachability health check
	// hits on the node inbound listener; it is answered locally, never routed to a pod.
	NodeLivePath = "/-/-/node/live"
	// nodeLiveHealthCheckFilterName is the HTTP health-check filter that answers
	// NodeLivePath locally (non-pass-through), short-circuiting it before the router.
	nodeLiveHealthCheckFilterName = "envoy.filters.http.health_check"
)

// NewNodeInboundListener builds the single node-level inbound listener. Peer node
// proxies open an mTLS HTTP/2 connection to it presenting the originating pod's
// SVID; the listener terminates mTLS, sets XFCC natively from the verified peer
// (SANITIZE_SET), and routes each request by its :authority (the destination pod
// IP, set by the source's auto_host_rewrite) to that pod's application cluster
// (app_<pod>) on loopback. A GET on NodeLivePath is answered locally for
// reachability health checks. Returns nil before the node SVID is served (it is the
// listener's server certificate).
func NewNodeInboundListener(localPods []*cniv1.CNIPod, nodeSpiffeID, validationContextName string) *listenerv3.Listener {
	if nodeSpiffeID == "" {
		return nil
	}

	hcm := &http_connection_managerv3.HttpConnectionManager{
		StatPrefix: NodeInboundListenerName,
		CodecType:  http_connection_managerv3.HttpConnectionManager_HTTP2,
		// XFCC is set natively from the verified peer certificate (the source pod);
		// the connection is per-source, so the peer identity is the caller's SVID.
		ForwardClientCertDetails: http_connection_managerv3.HttpConnectionManager_SANITIZE_SET,
		SetCurrentClientCertDetails: &http_connection_managerv3.HttpConnectionManager_SetCurrentClientCertDetails{
			Uri: true,
		},
		HttpFilters: []*http_connection_managerv3.HttpFilter{
			// Answer the peer reachability probe (NodeLivePath) locally before routing.
			buildNodeLiveHealthCheckFilter(),
			routerHttpFilter(),
		},
		RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{
			RouteConfig: buildNodeInboundRouteConfiguration(localPods),
		},
	}

	return &listenerv3.Listener{
		Name: NodeInboundListenerName,
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultNodeInboundPort,
					},
				},
			},
		},
		StatPrefix:       NodeInboundListenerName,
		TrafficDirection: corev3.TrafficDirection_INBOUND,
		ListenerFilters:  buildInboundListenerFilters(),
		FilterChains: []*listenerv3.FilterChain{
			{
				Name:            NodeInboundListenerName,
				Filters:         []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
				TransportSocket: DownstreamTransportSocket(nodeSpiffeID, validationContextName),
			},
		},
	}
}

// buildNodeLiveHealthCheckFilter answers the peer reachability probe (a GET on
// NodeLivePath) locally with 200, short-circuiting it before the router so it
// needs no route. Non-pass-through mode also wires up the proxy admin
// /healthcheck/fail|ok toggles, letting a node be drained gracefully (peers' HCs
// then see 503 and shift traffic off this node). Other requests pass through.
func buildNodeLiveHealthCheckFilter() *http_connection_managerv3.HttpFilter {
	return httpFilter(nodeLiveHealthCheckFilterName, &healthcheckv3.HealthCheck{
		PassThroughMode: wrapperspb.Bool(false),
		Headers: []*routev3.HeaderMatcher{
			{
				Name: ":path",
				HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
					StringMatch: &matcherv3.StringMatcher{
						MatchPattern: &matcherv3.StringMatcher_Exact{Exact: NodeLivePath},
					},
				},
			},
		},
	})
}

// buildNodeInboundRouteConfiguration builds the route table for the node inbound
// listener: one virtual host per local pod, matched by the pod's IP as the request
// :authority (set by the source proxy's auto_host_rewrite), routing to that pod's
// application cluster. The reachability probe (NodeLivePath) is handled by the
// health_check HTTP filter, not a route here.
func buildNodeInboundRouteConfiguration(localPods []*cniv1.CNIPod) *routev3.RouteConfiguration {
	vhosts := make([]*routev3.VirtualHost, 0, len(localPods))
	for _, pod := range localPods {
		ips := pod.GetIps()
		if len(ips) == 0 {
			continue
		}
		vhosts = append(vhosts, &routev3.VirtualHost{
			Name:    pod.GetName(),
			Domains: []string{ips[0]},
			Routes: []*routev3.Route{
				{
					Match: &routev3.RouteMatch{
						PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"},
					},
					Action: &routev3.Route_Route{
						Route: &routev3.RouteAction{
							ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: AppClusterName(pod)},
						},
					},
				},
			},
		})
	}

	return &routev3.RouteConfiguration{
		Name: NodeInboundListenerName,
		// The per-pod app_<pod> clusters churn on pod restart; don't let an inline
		// route's cluster reference wedge the listener during the delta-xDS
		// make-before-break window if the cluster is momentarily unknown.
		ValidateClusters: wrapperspb.Bool(false),
		VirtualHosts:     vhosts,
	}
}
