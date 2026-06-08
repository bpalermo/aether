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
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	commonsetfsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/common/set_filter_state/v3"
	httpsetfsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/set_filter_state/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/durationpb"
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

	// peerURIFilterStateKey carries the verified tunnel peer (the originating
	// client pod) SPIFFE ID from the CONNECT-terminating listener across the
	// internal-listener boundary to the per-pod inner HCM, which rebuilds XFCC
	// from it (the inner HCM cannot see the mTLS peer through CONNECT-termination).
	peerURIFilterStateKey = "aether.peer.uri"
	// xfccHeader is the standard forwarded-client-cert header the inner HCM sets.
	xfccHeader = "x-forwarded-client-cert"

	innerListenerPrefix = "inner_"
	innerDemuxPrefix    = "inner_demux_"
)

// NodeConnectResources are the resources implementing the node-level tunnel
// ingress: the CONNECT-terminating listener, one inner HCM listener per local pod
// (rebuilds XFCC and forwards to the pod's app on loopback), and the
// internal_upstream clusters wiring the former to the latter.
type NodeConnectResources struct {
	Listeners []*listenerv3.Listener
	Clusters  []*clusterv3.Cluster
}

// innerListenerName / innerDemuxClusterName name the per-pod inner resources.
func innerListenerName(pod *cniv1.CNIPod) string     { return innerListenerPrefix + pod.GetName() }
func innerDemuxClusterName(pod *cniv1.CNIPod) string { return innerDemuxPrefix + pod.GetName() }

// GenerateNodeConnectResources builds the node tunnel ingress. Peer node proxies
// open an mTLS HTTP/2 CONNECT tunnel to the CONNECT listener; it captures the
// verified caller identity into filter state, then routes each tunnel by its
// :authority (the destination pod's IP) to that pod's inner HCM listener, which
// rebuilds XFCC from the captured identity and forwards the decrypted stream to
// the pod's application on loopback (app_<pod>). A non-tunneled GET on
// NodeLivePath is answered locally with 200 for reachability health checks.
//
// Downstream mTLS presents the node identity (nodeSpiffeID) and requires and
// validates the caller's workload SVID against the trust domain. Returns nil
// before the node SVID is served (it is the listener's server certificate).
func GenerateNodeConnectResources(localPods []*cniv1.CNIPod, nodeSpiffeID, validationContextName string) *NodeConnectResources {
	if nodeSpiffeID == "" {
		return nil
	}

	res := &NodeConnectResources{
		Listeners: []*listenerv3.Listener{buildNodeConnectListener(localPods, nodeSpiffeID, validationContextName)},
	}
	for _, pod := range localPods {
		if len(pod.GetIps()) == 0 {
			continue
		}
		res.Listeners = append(res.Listeners, buildInnerListener(pod, nodeSpiffeID))
		res.Clusters = append(res.Clusters, buildInnerDemuxCluster(pod))
	}
	return res
}

func buildNodeConnectListener(localPods []*cniv1.CNIPod, nodeSpiffeID, validationContextName string) *listenerv3.Listener {
	hcm := &http_connection_managerv3.HttpConnectionManager{
		StatPrefix: NodeConnectListenerName,
		CodecType:  http_connection_managerv3.HttpConnectionManager_HTTP2,
		Http2ProtocolOptions: &corev3.Http2ProtocolOptions{
			AllowConnect: true,
		},
		HttpFilters: []*http_connection_managerv3.HttpFilter{
			// Capture the verified peer (client pod) SPIFFE ID for the inner HCM.
			buildPeerURICaptureFilter(),
			routerHttpFilter(),
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

// buildPeerURICaptureFilter captures the verified downstream peer URI SAN (the
// originating client pod's SPIFFE ID) into filter state, shared with the upstream
// internal connection so the inner HCM can read it.
func buildPeerURICaptureFilter() *http_connection_managerv3.HttpFilter {
	cfg := &httpsetfsv3.Config{
		OnRequestHeaders: []*commonsetfsv3.FilterStateValue{
			{
				Key:        &commonsetfsv3.FilterStateValue_ObjectKey{ObjectKey: peerURIFilterStateKey},
				FactoryKey: "envoy.string",
				Value: &commonsetfsv3.FilterStateValue_FormatString{
					FormatString: &corev3.SubstitutionFormatString{
						Format: &corev3.SubstitutionFormatString_TextFormatSource{
							TextFormatSource: &corev3.DataSource{
								Specifier: &corev3.DataSource_InlineString{InlineString: "%DOWNSTREAM_PEER_URI_SAN%"},
							},
						},
					},
				},
				SharedWithUpstream: commonsetfsv3.FilterStateValue_TRANSITIVE,
			},
		},
	}
	return &http_connection_managerv3.HttpFilter{
		Name: "envoy.filters.http.set_filter_state",
		ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{
			TypedConfig: config.TypedConfig(cfg),
		},
	}
}

// buildNodeConnectRouteConfiguration builds the route table for the node CONNECT
// listener: a local-only readiness route, then one CONNECT route per local pod
// keyed on the destination pod's IP (:authority) that terminates the tunnel and
// forwards to that pod's inner HCM listener.
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
					ClusterSpecifier: &routev3.RouteAction_Cluster{Cluster: innerDemuxClusterName(pod)},
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

// buildInnerListener builds the per-pod inner HCM internal listener. It processes
// the tunneled HTTP request, rebuilds XFCC from the captured peer identity (the
// inner HCM cannot read the mTLS peer through CONNECT-termination), and forwards
// to the pod's app on loopback (app_<pod>).
func buildInnerListener(pod *cniv1.CNIPod, nodeSpiffeID string) *listenerv3.Listener {
	routeConfig := buildInboundRouteConfiguration(AppClusterName(pod))
	routeConfig.VirtualHosts[0].RequestHeadersToAdd = []*corev3.HeaderValueOption{
		{
			Header: &corev3.HeaderValue{
				Key:   xfccHeader,
				Value: fmt.Sprintf("By=%s;URI=%%FILTER_STATE(%s:PLAIN)%%", nodeSpiffeID, peerURIFilterStateKey),
			},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		},
	}

	hcm := buildHTTPConnectionManager(innerListenerName(pod), routeConfig)

	return &listenerv3.Listener{
		Name:              innerListenerName(pod),
		ListenerSpecifier: &listenerv3.Listener_InternalListener{InternalListener: &listenerv3.Listener_InternalListenerConfig{}},
		FilterChains: []*listenerv3.FilterChain{
			{
				Filters: []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
			},
		},
	}
}

// buildInnerDemuxCluster builds the STATIC cluster the CONNECT listener forwards a
// pod's terminated tunnel to. The internal_upstream transport socket carries the
// captured peer-identity filter state across to the inner HCM listener.
func buildInnerDemuxCluster(pod *cniv1.CNIPod) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:           innerDemuxClusterName(pod),
		ConnectTimeout: durationpb.New(2 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_STATIC,
		},
		TransportSocket: InternalUpstreamTransportSocket(),
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: innerDemuxClusterName(pod),
			Endpoints: []*endpointv3.LocalityLbEndpoints{
				{
					LbEndpoints: []*endpointv3.LbEndpoint{
						{
							HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
								Endpoint: &endpointv3.Endpoint{
									Address: &corev3.Address{
										Address: &corev3.Address_EnvoyInternalAddress{
											EnvoyInternalAddress: &corev3.EnvoyInternalAddress{
												AddressNameSpecifier: &corev3.EnvoyInternalAddress_ServerListenerName{
													ServerListenerName: innerListenerName(pod),
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
