package proxy

import (
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
)

const (
	// NodeConnectListenerName is the name of the node-level CONNECT-terminating
	// listener that receives HBONE-style tunnels from peer node proxies.
	NodeConnectListenerName = "node_connect"
	// defaultNodeConnectPort is the host port the node CONNECT listener binds.
	// It is node-level (host network namespace), distinct from the per-pod
	// inbound listeners on 18080, so the R2 tunnel data path can be introduced
	// without disturbing the existing R1 inbound path.
	defaultNodeConnectPort = 15008
	// NodeLivePath is the readiness path a peer proxy's reachability health check
	// hits on the node CONNECT listener; it is answered locally, never tunneled.
	NodeLivePath = "/-/-/node/live"
)

// GenerateNodeConnectListener builds the node-level CONNECT-terminating listener.
// Peer node proxies open an mTLS HTTP/2 CONNECT tunnel to it; the listener routes
// each tunnel by its :authority (the destination pod's IP) to that pod's app
// cluster, delivering the decrypted stream to the pod's application on loopback.
// A non-tunneled GET on NodeLivePath is answered locally with 200 so peers can
// health-check node reachability without hitting any application.
//
// Downstream mTLS presents the node identity (nodeSpiffeID) and requires and
// validates the caller's workload SVID against the trust domain
// (validationContextName); the verified peer identity — the originating client
// pod — is exposed to the application via XFCC. nodeSpiffeID may be empty before
// the node SVID is served, in which case no listener is produced.
func GenerateNodeConnectListener(localPods []*cniv1.CNIPod, nodeSpiffeID, validationContextName string) *listenerv3.Listener {
	if nodeSpiffeID == "" {
		return nil
	}

	hcm := &http_connection_managerv3.HttpConnectionManager{
		StatPrefix: NodeConnectListenerName,
		CodecType:  http_connection_managerv3.HttpConnectionManager_HTTP2,
		Http2ProtocolOptions: &corev3.Http2ProtocolOptions{
			AllowConnect: true,
		},
		HttpFilters: []*http_connection_managerv3.HttpFilter{
			routerHttpFilter(),
		},
		// Expose the tunnel-verified caller (the client pod) to the application.
		ForwardClientCertDetails: http_connection_managerv3.HttpConnectionManager_SANITIZE_SET,
		SetCurrentClientCertDetails: &http_connection_managerv3.HttpConnectionManager_SetCurrentClientCertDetails{
			Uri: true,
		},
		RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{
			RouteConfig: buildNodeConnectRouteConfiguration(localPods),
		},
	}

	return &listenerv3.Listener{
		Name: NodeConnectListenerName,
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  defaultInboundAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultNodeConnectPort,
					},
				},
			},
		},
		StatPrefix:       NodeConnectListenerName,
		TrafficDirection: corev3.TrafficDirection_INBOUND,
		ListenerFilters:  buildInboundListenerFilters(),
		FilterChains: []*listenerv3.FilterChain{
			{
				Name:            NodeConnectListenerName,
				Filters:         []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
				TransportSocket: DownstreamTransportSocket(nodeSpiffeID, validationContextName),
			},
		},
	}
}

// buildNodeConnectRouteConfiguration builds the route table for the node CONNECT
// listener: a local-only readiness route, then one CONNECT route per local pod
// keyed on the destination pod's IP (:authority) that terminates the tunnel and
// forwards to that pod's app cluster.
func buildNodeConnectRouteConfiguration(localPods []*cniv1.CNIPod) *routev3.RouteConfiguration {
	routes := make([]*routev3.Route, 0, len(localPods)+1)

	// Reachability health check, answered locally (never tunneled to an app).
	routes = append(routes, &routev3.Route{
		Match: &routev3.RouteMatch{
			PathSpecifier: &routev3.RouteMatch_Path{Path: NodeLivePath},
		},
		Action: &routev3.Route_DirectResponse{
			DirectResponse: &routev3.DirectResponseAction{
				Status: 200,
				Body: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineString{InlineString: "node-alive\n"},
				},
			},
		},
	})

	for _, pod := range localPods {
		ips := pod.GetIps()
		if len(ips) == 0 {
			continue
		}
		routes = append(routes, &routev3.Route{
			Match: &routev3.RouteMatch{
				PathSpecifier: &routev3.RouteMatch_ConnectMatcher_{
					ConnectMatcher: &routev3.RouteMatch_ConnectMatcher{},
				},
				Headers: []*routev3.HeaderMatcher{
					{
						Name: ":authority",
						HeaderMatchSpecifier: &routev3.HeaderMatcher_StringMatch{
							StringMatch: &matcherv3.StringMatcher{
								MatchPattern: &matcherv3.StringMatcher_Exact{Exact: ips[0]},
							},
						},
					},
				},
			},
			Action: &routev3.Route_Route{
				Route: &routev3.RouteAction{
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: AppClusterName(pod)},
					UpgradeConfigs: []*routev3.RouteAction_UpgradeConfig{
						{
							UpgradeType:   "CONNECT",
							ConnectConfig: &routev3.RouteAction_UpgradeConfig_ConnectConfig{},
						},
					},
				},
			},
		})
	}

	return &routev3.RouteConfiguration{
		Name:         NodeConnectListenerName,
		VirtualHosts: []*routev3.VirtualHost{{Name: "tunnel", Domains: []string{"*"}, Routes: routes}},
	}
}
