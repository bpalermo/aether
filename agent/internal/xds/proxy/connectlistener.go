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
	healthcheckv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/health_check/v3"
	httpsetfsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/set_filter_state/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// NodeConnectListenerName is the name of the node-level CONNECT-terminating
	// listener that receives HBONE-style tunnels from peer node proxies.
	NodeConnectListenerName = "node_connect"
	// defaultInboundAddress is the bind address for the node CONNECT listener
	// (all interfaces); peer node proxies dial it across the node network.
	defaultInboundAddress = "0.0.0.0"
	// defaultNodeConnectPort is the host port the node CONNECT listener binds.
	// It is node-level (host network namespace): all inbound mesh traffic arrives
	// here over the HBONE tunnel and is demuxed to the per-pod inner listeners.
	defaultNodeConnectPort = 15008
	// NodeLivePath is the readiness path a peer proxy's reachability health check
	// hits on the node CONNECT listener; it is answered locally, never tunneled.
	NodeLivePath = "/-/-/node/live"
	// nodeLiveHealthCheckFilterName is the HTTP health-check filter that answers
	// NodeLivePath locally (non-pass-through), short-circuiting it before the router.
	nodeLiveHealthCheckFilterName = "envoy.filters.http.health_check"

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
			// Answer the peer reachability probe (NodeLivePath) locally; CONNECT
			// tunnels carry no :path and pass straight through to the router.
			buildNodeLiveHealthCheckFilter(),
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

// buildNodeLiveHealthCheckFilter answers the peer reachability probe (a GET on
// NodeLivePath) locally with 200, short-circuiting it before the router so it
// needs no route. Non-pass-through mode also wires up the proxy admin
// /healthcheck/fail|ok toggles, letting a node be drained gracefully (peers' HCs
// then see 503 and shift traffic off this node). CONNECT requests carry no :path
// pseudo-header, so they never match and fall through to the router.
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

// buildNodeConnectRouteConfiguration builds the route table for the node CONNECT
// listener: one CONNECT route per local pod keyed on the destination pod's IP
// (:authority) that terminates the tunnel and forwards to that pod's inner HCM
// listener. The reachability probe (NodeLivePath) is handled by the health_check
// HTTP filter, not a route here.
func buildNodeConnectRouteConfiguration(localPods []*cniv1.CNIPod) *routev3.RouteConfiguration {
	routes := make([]*routev3.Route, 0, len(localPods))

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
		Name: NodeConnectListenerName,
		// Per-pod inner-demux clusters churn on every pod restart (their names are
		// pod-scoped). Without this, Envoy validates the inline route's cluster
		// references at listener-load time and rejects the whole node_connect
		// listener if a referenced cluster is momentarily not yet known (the
		// delta-xDS make-before-break window) — wedging the node's tunnel ingress
		// until a proxy restart. Disabling validation makes only the affected route
		// transiently return 503 until its cluster arrives, leaving the listener up.
		ValidateClusters: wrapperspb.Bool(false),
		VirtualHosts:     []*routev3.VirtualHost{{Name: "tunnel", Domains: []string{"*"}, Routes: routes}},
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
