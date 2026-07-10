// Package envoy_validate provides functions to build representative Envoy
// bootstrap JSON configurations derived from the aether node-agent's actual
// xDS proxy builders.  The resulting configs are validated with
// "envoy --mode validate" in the test to catch structural regressions before
// production.
//
// Three scenarios are modelled:
//
//  1. Node proxy  – per-pod inbound (mTLS) + outbound HTTP listener, ORIGINAL_DST
//     passthrough cluster, per-pod app cluster, EDS service cluster with SPIRE mTLS.
//  2. Capture     – transparent-capture listener, HTTP + TCP service clusters,
//     passthrough chain.
//  3. Edge        – service EDS cluster with direct-to-SPIRE SDS, spire_agent
//     static cluster.
//
// Custom extensions (aether_stats) that require the custom Envoy binary are
// stripped before serialisation so that stock Envoy can validate the structural
// correctness of the config.
package envoy_validate

import (
	"fmt"
	"time"

	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	"github.com/bpalermo/aether/common/extensionfilter"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
)

const (
	// trustDomain is the SPIFFE trust domain used in generated resource names.
	trustDomain = "aether.internal"
	// meshDomain is the DNS-style suffix for mesh service cluster names.
	meshDomain = "mesh.local"
	// fakeNetns is a placeholder netns path.  Envoy --mode validate checks
	// field presence, not whether the path exists on disk.
	fakeNetns = "/proc/1/ns/net"
	// xdsSockPath is the agent's UDS; the static xds_cluster points here so all
	// ADS config-source references resolve when Envoy validates the bootstrap.
	xdsSockPath = "/var/run/aether/xds.sock"

	// aetherStatsFilterName mirrors the unexported statsFilterName constant in
	// agent/internal/xds/proxy/stats_filter.go.  This filter requires the custom
	// C++ extension compiled into the proxy workspace Envoy (proposal 012); it is
	// stripped before stock-Envoy validation.  Keep in sync if the constant changes.
	aetherStatsFilterName = "aether.filters.http.aether_stats"
)

// NodeBootstrapJSON builds the core node-proxy bootstrap config and returns
// its JSON representation (after stripping custom extensions).
func NodeBootstrapJSON() ([]byte, error) {
	bs, err := buildNodeBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(bs)
}

// NodeCleartextBootstrapJSON builds the SPIRE-off (cleartext) node-proxy bootstrap
// and returns its JSON. It exercises the cleartext inbound listener — a single
// default HCM chain with no downstream mTLS transport socket — so the offline
// `envoy --mode validate` gate covers the MESH-HTTP-on-kind path, not just mTLS.
func NodeCleartextBootstrapJSON() ([]byte, error) {
	bs, err := buildNodeCleartextBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(bs)
}

// CaptureBootstrapJSON builds the transparent-capture bootstrap config and
// returns its JSON representation.
func CaptureBootstrapJSON() ([]byte, error) {
	bs, err := buildCaptureBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(bs)
}

// EdgeBootstrapJSON builds the edge proxy bootstrap config and returns its
// JSON representation.
func EdgeBootstrapJSON() ([]byte, error) {
	bs, err := buildEdgeBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(bs)
}

// buildNodeBootstrap builds the core node-proxy bootstrap config. The outbound
// listener carries the node-local authz-sidecar ext_authz entry (proposal 027,
// disabled — the real transport config, validated by stock Envoy) alongside a
// static authz_sidecar UDS cluster mirroring the chart's bootstrap cluster.
func buildNodeBootstrap() (*bootstrapv3.Bootstrap, error) {
	pod := testPod()

	authzEntry := proxy.AuthzSidecarHTTPFilter(200*time.Millisecond, false)
	inbound, outbound, appClusters, healthCluster, err := proxy.GenerateListenersFromRegistryPod(pod, trustDomain, meshDomain, false, false, []*http_connection_managerv3.HttpFilter{authzEntry}, nil)
	if err != nil {
		return nil, fmt.Errorf("GenerateListenersFromRegistryPod: %w", err)
	}

	passthrough := proxy.NewPassthroughOriginalDstCluster()
	svcCluster := newServiceCluster("echo."+meshDomain, trustDomain, "default", "echo")

	staticClusters := []*clusterv3.Cluster{xdsCluster(), passthrough, svcCluster, authzSidecarCluster()}
	staticClusters = append(staticClusters, appClusters...)
	staticClusters = append(staticClusters, healthCluster)

	return newBootstrap(staticClusters, []*listenerv3.Listener{inbound, outbound}), nil
}

// authzSidecarCluster mirrors the chart's static UDS cluster for the authz sidecar.
func authzSidecarCluster() *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                 proxy.AuthzSidecarClusterName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		ConnectTimeout:       durationpb.New(time.Second),
		LoadAssignment:       pipeEndpoint(proxy.AuthzSidecarClusterName, "/run/aether/authz/authz.sock"),
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(
				config.Http2ProtocolOptions(),
			),
		},
	}
}

// buildNodeCleartextBootstrap builds the node-proxy bootstrap with SPIRE off: the
// inbound listener is generated cleartext (last arg true), so the validated config
// has no SDS-backed downstream transport socket on the inbound chain.
func buildNodeCleartextBootstrap() (*bootstrapv3.Bootstrap, error) {
	pod := testPod()

	inbound, outbound, appClusters, healthCluster, err := proxy.GenerateListenersFromRegistryPod(pod, trustDomain, meshDomain, false, true, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("GenerateListenersFromRegistryPod (cleartext): %w", err)
	}

	passthrough := proxy.NewPassthroughOriginalDstCluster()
	svcCluster := newServiceCluster("echo."+meshDomain, trustDomain, "default", "echo")

	staticClusters := []*clusterv3.Cluster{xdsCluster(), passthrough, svcCluster}
	staticClusters = append(staticClusters, appClusters...)
	staticClusters = append(staticClusters, healthCluster)

	return newBootstrap(staticClusters, []*listenerv3.Listener{inbound, outbound}), nil
}

// buildCaptureBootstrap builds the transparent-capture bootstrap config.
func buildCaptureBootstrap() (*bootstrapv3.Bootstrap, error) {
	pod := testPod()

	tcpSvc := proxy.CaptureTCPService{
		ClusterName: "redis." + meshDomain,
		ClusterIP:   "10.96.1.10",
	}
	captureListener, err := proxy.GenerateCaptureListener(
		pod,
		15006,
		meshDomain,
		false, // emitStatsPod
		[]proxy.CaptureTCPService{tcpSvc},
		true, // withPassthrough
		nil,  // extensionFilters (escape hatch exercised in the route-target bootstrap)
	)
	if err != nil {
		return nil, fmt.Errorf("GenerateCaptureListener: %w", err)
	}

	passthrough := proxy.NewPassthroughOriginalDstCluster()
	httpSvc := newServiceCluster("echo."+meshDomain, trustDomain, "default", "echo")
	tcpSvc2 := newServiceCluster("redis."+meshDomain, trustDomain, "default", "redis")

	return newBootstrap(
		[]*clusterv3.Cluster{xdsCluster(), passthrough, httpSvc, tcpSvc2},
		[]*listenerv3.Listener{captureListener},
	), nil
}

// CaptureRouteTargetBootstrapJSON builds a bootstrap whose listener inlines the
// cap_http RouteConfiguration for a GAMMA route TARGET addressed on its REAL
// Service port (proposal 023 M2): the vhost carries
// "<svc>.<ns>.svc.cluster.local:<realPort>" domains and the GAMMA rules route to
// SA-backed backend clusters. Validating it through "envoy --mode validate" proves
// the M2 route table (real-port domains + path-based GAMMA action) is structurally
// accepted, complementing the Go-level captureVhosts unit test.
func CaptureRouteTargetBootstrapJSON() ([]byte, error) {
	bs, err := buildCaptureRouteTargetBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(bs)
}

// buildCaptureRouteTargetBootstrap assembles the M2 route-target capture config.
func buildCaptureRouteTargetBootstrap() (*bootstrapv3.Bootstrap, error) {
	const (
		targetMesh = "echo.team-a." + meshDomain
		v1Cluster  = "echo-v1.team-a." + meshDomain
		v2Cluster  = "echo-v2.team-a." + meshDomain
		fqdn       = "echo.team-a.svc.cluster.local"
		realPort   = 8080
	)

	// The route target's GAMMA rules mirror the exact MESH-HTTP conformance feature
	// shapes (MeshHTTPRouteWeight / RequestHeaderModifier / RedirectHostAndStatus) so
	// the offline `envoy --mode validate` gate proves Envoy ACCEPTS the capture-path
	// route action for each — a NACK here would break the whole cap_http table at
	// runtime (every request then falls to the redirect-all passthrough, i.e. the
	// kube-proxy bypass that manifested as the conformance timeouts / wrong split):
	//   - /v2 -> echo-v2 (single backend, segment-prefix match)
	//   - /redirect -> RequestRedirect (host + 301, NO backend cluster)
	//   - /headers -> echo-v1 with a request header set+add+remove + response mutation
	//   - / (default) -> 70/30 WEIGHTED split across echo-v1/echo-v2
	// Escape-hatch (proposal 025): a header_mutation ExtensionFilter on /v2. Its
	// per-route config is a HeaderMutationPerRoute (note: header_mutation's per-route
	// type differs from its HCM filter config type) appending a response header.
	// Proving Envoy ACCEPTS both the default-disabled HCM entry (below) AND this
	// per-route override is exactly what the M2 webhook approximates in-process.
	headerMutationPerRoute := mustAny(&header_mutationv3.HeaderMutationPerRoute{
		Mutations: &header_mutationv3.Mutations{
			ResponseMutations: []*mutation_rulesv3.HeaderMutation{{
				Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
					Header:       &corev3.HeaderValue{Key: "x-aether-escape-hatch", Value: "applied"},
					AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
				}},
			}},
		},
	})
	rules := []proxy.GammaRoute{
		{
			Matches:  []proxy.GammaMatch{{Prefix: "/v2"}},
			Backends: []proxy.GammaBackend{{Service: "team-a/echo-v2", Cluster: v2Cluster, Weight: 1}},
			ExtensionFilters: []proxy.ExtensionFilter{{
				Name:   "envoy.filters.http.header_mutation",
				Config: headerMutationPerRoute,
			}, {
				// rbac (local authz): the real renderer's RBACPerRoute — audit-mode
				// shadow rules with the namespace-sugar regex principal — accepted
				// by stock Envoy alongside its empty default-disabled chain entry.
				Name:   extensionfilter.RBACFilterName,
				Config: rbacPerRoute(),
			}},
		},
		{
			// RequestRedirect: replaces the route action with a RedirectAction and
			// carries NO backend cluster (Gateway API redirect-takes-precedence shape).
			Matches:  []proxy.GammaMatch{{Prefix: "/redirect"}},
			Redirect: &proxy.GammaRedirect{Hostname: "example.org", StatusCode: 301},
		},
		{
			// Full header mutation: set/add/remove on the request, set/remove on the
			// response — exactly the RequestHeaderModifier conformance vocabulary.
			Matches:  []proxy.GammaMatch{{Prefix: "/headers"}},
			Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: v1Cluster, Weight: 1}},
			HeaderMutation: &proxy.GammaHeaderMutation{
				SetRequest:     []proxy.GammaHeaderKV{{Name: "X-Header-Set", Value: "set-overwrites-values"}},
				AddRequest:     []proxy.GammaHeaderKV{{Name: "X-Header-Add", Value: "add-appends-values"}},
				RemoveRequest:  []string{"X-Header-Remove"},
				SetResponse:    []proxy.GammaHeaderKV{{Name: "X-Resp-Set", Value: "resp"}},
				RemoveResponse: []string{"X-Resp-Remove"},
			},
		},
		{
			// 70/30 WEIGHTED split (two backends) on the default "/" — the
			// MeshHTTPRouteWeight shape (no match → default prefix).
			Matches: []proxy.GammaMatch{{Prefix: "/"}},
			Backends: []proxy.GammaBackend{
				{Service: "team-a/echo-v1", Cluster: v1Cluster, Weight: 70},
				{Service: "team-a/echo-v2", Cluster: v2Cluster, Weight: 30},
			},
		},
	}
	// Real-port domains (M2) + portless + mesh-name spellings, the exact set
	// captureVhosts emits for a route target with port 8080.
	domains := []string{
		fqdn, fmt.Sprintf("%s:18081", fqdn),
		targetMesh, fmt.Sprintf("%s:18081", targetMesh),
		fmt.Sprintf("%s:%d", fqdn, realPort),
		fmt.Sprintf("%s:%d", targetMesh, realPort),
	}
	vhost := proxy.BuildOutboundServiceVirtualHost(targetMesh, domains, rules)
	// Known-target safety net (the RDS-reload-race fix): the redirect-all catch-all
	// pins the route target's non-mesh dial spellings (any port) to its mesh cluster
	// so a captured request never leaks to the passthrough while this vhost rebuilds.
	// Validating it here proves Envoy ACCEPTS the extra :authority safe_regex route.
	knownTargets := []proxy.KnownTargetRoute{{
		AuthorityRegex: `^(echo|echo\.team-a|echo\.team-a\.svc|echo\.team-a\.svc\.cluster\.local)(:[0-9]+)?$`,
		Cluster:        targetMesh,
	}}
	routeCfg := proxy.BuildCaptureRouteConfiguration([]*routev3.VirtualHost{vhost}, meshDomain, true, knownTargets...)

	hcm := &http_connection_managerv3.HttpConnectionManager{
		StatPrefix: "cap_http_validate",
		RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{
			RouteConfig: routeCfg,
		},
		HttpFilters: append(
			// Escape-hatch (025): the union of allow-listed filters the routes reference,
			// default-disabled — header_mutation here. typed_per_filter_config (the per-
			// route override above) can only enable a filter already in the chain.
			proxy.CollectExtensionFilters(rules),
			&http_connection_managerv3.HttpFilter{
				Name: "envoy.filters.http.router",
				ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{
					TypedConfig: mustAny(&routerv3.Router{}),
				},
			},
		),
	}
	listener := &listenerv3.Listener{
		Name: "cap_http_validate",
		Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address:       "127.0.0.1",
			PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 15011},
		}}},
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(hcm)},
			}},
		}},
	}

	// The backend clusters the GAMMA rules reference (SA-backed, mTLS) plus the
	// route target's own mesh cluster (trailing default route falls back to it).
	target := newServiceCluster(targetMesh, trustDomain, "team-a", "echo")
	v1 := newServiceCluster(v1Cluster, trustDomain, "team-a", "echo-v1")
	v2 := newServiceCluster(v2Cluster, trustDomain, "team-a", "echo-v2")
	// The redirect-all capture catch-all routes its final fallthrough to the
	// ORIGINAL_DST passthrough cluster, so include it for the validate.
	passthrough := proxy.NewPassthroughOriginalDstCluster()

	return newBootstrap(
		[]*clusterv3.Cluster{xdsCluster(), target, v1, v2, passthrough},
		[]*listenerv3.Listener{listener},
	), nil
}

// buildEdgeBootstrap builds the edge (north-south ingress) proxy bootstrap. The
// edge HTTP listener carries the geo pipeline (proposal 028): the reserved x-geo-*
// strip + the geoip filter with the MaxMind provider over the REAL MaxMind test
// database (testdata/GeoIP2-City-Test.mmdb) — the provider opens the file at config
// load, so stock Envoy validates the whole shape end-to-end.
//
// Three listeners are exercised:
//
//  1. HTTP (plain): geo pipeline + RDS (ADS).
//  2. HTTPS (TLS): SDS-backed cert + geo + RDS. Validates the DownstreamTlsContext shape.
//  3. H3 (QUIC/UDP, proposal 029 M3): envoy.transport_sockets.quic + HTTP3 codec + RDS.
//     Stock Envoy 1.38.0 compiles in QUIC, so this validates the full QUIC listener shape.
func buildEdgeBootstrap() (*bootstrapv3.Bootstrap, error) {
	edgeSvc := newEdgeServiceCluster("echo."+meshDomain, trustDomain, "default", "echo")
	spire := newSpireAgentCluster()

	geo := []*http_connection_managerv3.HttpFilter{
		proxy.GeoStripHTTPFilter(),
		proxy.GeoipHTTPFilter(proxy.GeoipConfig{
			CityDBPath:        "testdata/GeoIP2-City-Test.mmdb",
			Headers:           []string{"country", "city"},
			XffNumTrustedHops: 1,
		}),
		// Route-cache-clear (lua) so geo header-routing works — validate its lua
		// compiles in stock Envoy alongside geoip (proposal 028).
		proxy.GeoRouteCacheClearHTTPFilter(),
	}
	// Full edge hardening (proposal 029): validate use_remote_address, header-underscore
	// rejection, downstream h2 caps and timeouts are accepted by stock Envoy.
	// http3.enabled is set so the route config gets the alt-svc header, exercising M3.
	edgeCfg := configprotov1.EdgeConfigSpec_builder{
		UseRemoteAddress:             wrapperspb.Bool(true),
		XffNumTrustedHops:            wrapperspb.UInt32(1),
		HeadersWithUnderscoresAction: configprotov1.EdgeConfigSpec_HEADERS_WITH_UNDERSCORES_ACTION_REJECT_REQUEST.Enum(),
		RequestTimeout:               durationpb.New(300 * time.Second),
		Http3:                        configprotov1.Http3Options_builder{Enabled: wrapperspb.Bool(true)}.Build(),
	}.Build()

	const (
		edgeGWNamespace = "edge-ns"
		edgeGWName      = "edge-gw"
		edgeHTTPPort    = uint32(18150)
		edgeHTTPSPort   = uint32(18443)
		// SDS cert name — ADS-served; validate mode accepts the reference without a live SDS server.
		edgeTLSCertName = "spiffe://aether.internal/edge-test-cert"
		// External HTTPS port for alt-svc advertisement.
		externalHTTPSPort = uint32(443)
	)

	edgeHTTP := proxy.BuildEdgeGatewayHTTPListener(edgeGWNamespace, edgeGWName, edgeHTTPPort, false, geo, edgeCfg)
	edgeHTTPS := proxy.BuildEdgeGatewayHTTPSListener(edgeGWNamespace, edgeGWName, edgeHTTPSPort, []string{edgeTLSCertName}, geo, edgeCfg)
	edgeH3 := proxy.BuildEdgeGatewayHTTP3Listener(edgeGWNamespace, edgeGWName, edgeHTTPSPort, []string{edgeTLSCertName}, geo, edgeCfg)

	return newBootstrap(
		[]*clusterv3.Cluster{xdsCluster(), spire, edgeSvc},
		[]*listenerv3.Listener{edgeHTTP, edgeHTTPS, edgeH3},
	), nil
}

// marshalBootstrap serialises a Bootstrap proto to protojson, stripping
// custom extensions that require the proxy-workspace Envoy binary.
func marshalBootstrap(bs *bootstrapv3.Bootstrap) ([]byte, error) {
	stripCustomFilters(bs)
	opts := protojson.MarshalOptions{
		Multiline:     true,
		Indent:        "  ",
		UseProtoNames: true,
	}
	data, err := opts.Marshal(bs)
	if err != nil {
		return nil, fmt.Errorf("marshal bootstrap: %w", err)
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// testPod returns a minimal CNIPod for proxy builder calls.
func testPod() *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "test-pod",
		Namespace:        "default",
		NetworkNamespace: fakeNetns,
		ContainerId:      "abc123",
		Ips:              []string{"10.96.0.1"},
		ServiceAccount:   "default",
	}
}

// newBootstrap assembles a Bootstrap proto with static resources and ADS-backed
// dynamic_resources pointing at the static xds_cluster.
func newBootstrap(clusters []*clusterv3.Cluster, listeners []*listenerv3.Listener) *bootstrapv3.Bootstrap {
	return &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: "test-node", Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Clusters:  clusters,
			Listeners: listeners,
		},
		DynamicResources: &bootstrapv3.Bootstrap_DynamicResources{
			AdsConfig: &corev3.ApiConfigSource{
				ApiType:             corev3.ApiConfigSource_GRPC,
				TransportApiVersion: corev3.ApiVersion_V3,
				GrpcServices: []*corev3.GrpcService{{
					TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
						EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: "xds_cluster"},
					},
				}},
			},
			CdsConfig: &corev3.ConfigSource{ConfigSourceSpecifier: &corev3.ConfigSource_Ads{}},
			LdsConfig: &corev3.ConfigSource{ConfigSourceSpecifier: &corev3.ConfigSource_Ads{}},
		},
		Admin: &bootstrapv3.Admin{
			Address: &corev3.Address{
				Address: &corev3.Address_SocketAddress{
					SocketAddress: &corev3.SocketAddress{
						Address:       "127.0.0.1",
						PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 19000},
					},
				},
			},
		},
	}
}

// xdsCluster is the static cluster the agent's xDS server listens on (UDS).
// All ADS config-source references inside generated resources resolve to it.
func xdsCluster() *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:           "xds_cluster",
		ConnectTimeout: durationpb.New(5e9), // 5 s
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_STATIC,
		},
		LoadAssignment: pipeEndpoint("xds_cluster", xdsSockPath),
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(
				config.Http2ProtocolOptions(),
			),
		},
	}
}

// newServiceCluster builds an EDS cluster with SPIRE mTLS upstream transport
// socket — the exact shape the agent distributes to node proxies.
func newServiceCluster(clusterName, td, namespace, svcName string) *clusterv3.Cluster {
	tlsCertName := fmt.Sprintf("spiffe://%s/node/test-node", td)
	validationCtxName := fmt.Sprintf("spiffe://%s", td)
	sanURI := fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", td, namespace, svcName)

	ts := proxy.UpstreamTransportSocket(tlsCertName, validationCtxName, []string{sanURI}, clusterName)

	return &clusterv3.Cluster{
		Name:           clusterName,
		ConnectTimeout: durationpb.New(5e9),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(32 * 1024),
		TransportSocket:               ts,
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(
				config.Http2ProtocolOptions(),
			),
		},
	}
}

// newEdgeServiceCluster is like newServiceCluster but fetches SDS certs from
// the static spire_agent cluster (edge proxy shape: direct-SPIRE, not ADS).
func newEdgeServiceCluster(clusterName, td, namespace, svcName string) *clusterv3.Cluster {
	tlsCertName := fmt.Sprintf("spiffe://%s/edge/aether-ingress/edge", td)
	validationCtxName := fmt.Sprintf("spiffe://%s", td)
	sanURI := fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", td, namespace, svcName)

	ts := proxy.EdgeUpstreamTransportSocket(tlsCertName, validationCtxName, []string{sanURI}, clusterName)

	return &clusterv3.Cluster{
		Name:           clusterName,
		ConnectTimeout: durationpb.New(5e9),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(32 * 1024),
		TransportSocket:               ts,
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(
				config.Http2ProtocolOptions(),
			),
		},
	}
}

// newSpireAgentCluster is the static cluster the edge proxy uses to reach the
// SPIRE Agent's native SDS API (Workload API Unix socket).
func newSpireAgentCluster() *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:           "spire_agent",
		ConnectTimeout: durationpb.New(5e9),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_STATIC,
		},
		LoadAssignment: pipeEndpoint("spire_agent", "/run/spire/sockets/agent.sock"),
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(
				&httpv3.HttpProtocolOptions{
					UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
						ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
							ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
								Http2ProtocolOptions: &corev3.Http2ProtocolOptions{},
							},
						},
					},
				},
			),
		},
	}
}

// pipeEndpoint builds a ClusterLoadAssignment with a single Unix-socket endpoint.
func pipeEndpoint(clusterName, path string) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []*endpointv3.LocalityLbEndpoints{{
			LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{
						Address: &corev3.Address{
							Address: &corev3.Address_Pipe{
								Pipe: &corev3.Pipe{Path: path},
							},
						},
					},
				},
			}},
		}},
	}
}

// stripCustomFilters removes HTTP filters that require the custom aether_stats
// C++ extension compiled into the proxy workspace Envoy (proposal 012).
// Stock Envoy does not have this extension and would fail --mode validate.
// All other structural elements (TLS, addresses, cluster types, SAN matchers)
// remain and are validated; aether_stats is tested by the proxy workspace's
// envoy_cc_test targets.
func stripCustomFilters(bs *bootstrapv3.Bootstrap) {
	for _, lis := range bs.GetStaticResources().GetListeners() {
		for _, fc := range lis.GetFilterChains() {
			stripHCMCustomFilters(fc)
		}
		if fc := lis.GetDefaultFilterChain(); fc != nil {
			stripHCMCustomFilters(fc)
		}
	}
}

// stripHCMCustomFilters removes the aether_stats HTTP filter from a
// FilterChain's HttpConnectionManager.  Other network filter types are unchanged.
func stripHCMCustomFilters(fc *listenerv3.FilterChain) {
	for _, f := range fc.GetFilters() {
		// aether uses the legacy name "envoy.http_connection_manager".
		// Check both the legacy and canonical names for safety.
		name := f.GetName()
		if name != "envoy.http_connection_manager" &&
			name != "envoy.filters.network.http_connection_manager" {
			continue
		}
		hcmAny := f.GetTypedConfig()
		if hcmAny == nil {
			continue
		}
		hcm := &http_connection_managerv3.HttpConnectionManager{}
		if err := hcmAny.UnmarshalTo(hcm); err != nil {
			continue
		}
		filtered := make([]*http_connection_managerv3.HttpFilter, 0, len(hcm.GetHttpFilters()))
		for _, hf := range hcm.GetHttpFilters() {
			if hf.GetName() == aetherStatsFilterName {
				continue // strip: requires custom binary
			}
			filtered = append(filtered, hf)
		}
		hcm.HttpFilters = filtered
		packed, err := anypb.New(hcm)
		if err != nil {
			panic(fmt.Sprintf("re-pack HCM: %v", err))
		}
		f.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: packed}
	}
}

// mustAny packs a proto.Message into anypb.Any, panicking on error.
// All types used here are well-formed static configs.
func mustAny(m proto.Message) *anypb.Any {
	a, err := anypb.New(m)
	if err != nil {
		panic(fmt.Sprintf("mustAny: %v", err))
	}
	return a
}

// rbacPerRoute renders a representative typed rbac form through the REAL renderer.
func rbacPerRoute() *anypb.Any {
	sp := &configprotov1.HTTPFilterSpec{}
	sp.SetRbac(configprotov1.RBACRoute_builder{
		Mode: configprotov1.RBACRoute_MODE_AUDIT,
		Policies: []*configprotov1.RBACRoute_Policy{
			configprotov1.RBACRoute_Policy_builder{
				Name: "audit-callers",
				Principals: []*configprotov1.RBACRoute_Principal{
					configprotov1.RBACRoute_Principal_builder{Namespace: "team-a"}.Build(),
				},
			}.Build(),
		},
	}.Build())
	_, cfg, err := extensionfilter.Render(sp)
	if err != nil {
		panic(err)
	}
	return cfg
}
