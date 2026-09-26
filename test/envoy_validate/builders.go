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

	configprotov1 "aethermesh.dev/api/aether/config/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	meshconst "aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/common/extensionfilter"
	"aethermesh.dev/common/udspath"

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
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
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

	// nodeSpiffeIDFmt is the agent's OWN SVID, the client certificate every
	// node-originated connection presents: the cluster matcher's on_no_match
	// socket and the inbound-readiness probe.
	//
	// It is a /ns/ WORKLOAD id, not the spiffe://<td>/node/<node> shape this
	// harness used to spell. The agent is an ordinary pod, so SPIRE's fallback
	// ClusterSPIFFEID issues it the same /ns/<ns>/sa/<sa> template every other
	// pod gets (#825, and networkfilter.go says so explicitly). The difference
	// became load-bearing with #843: the inbound listener now requires a client
	// URI SAN under "spiffe://<td>/ns/", so a fixture spelling the node identity
	// as /node/<node> models a certificate the mesh would REJECT — it would make
	// this harness agree with a config that demotes every endpoint on the node.
	nodeSpiffeIDFmt = "spiffe://%s/ns/aether-system/sa/aether-agent"

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

	// Access logging ON for this bootstrap, so the OTel access logger's ~30
	// %COMMAND% operators are parsed by a real Envoy. Until #824 nothing in this
	// harness enabled it, so the entire access-log config — every substitution
	// operator, the DYNAMIC_METADATA and FILTER_STATE specs, the filter tree —
	// reached production without ever having been through the config loader.
	// Envoy resolves format strings at load, so a typo or an operator that does
	// not exist in the pinned build fails validation here instead of rendering
	// "-" on every line in production. The collector cluster the logger names
	// must exist in the bootstrap (accessLogCluster below).
	proxy.SetAccessLogConfig(proxy.AccessLogConfig{Enabled: true, SuccessSampleRate: 100})
	defer proxy.SetAccessLogConfig(proxy.AccessLogConfig{})

	authzEntry := proxy.AuthzSidecarHTTPFilter(200*time.Millisecond, false)
	inbound, outbound, appClusters, healthCluster, err := proxy.GenerateListenersFromRegistryPod(pod, trustDomain, meshDomain, false, false, []*http_connection_managerv3.HttpFilter{authzEntry}, nil, "")
	if err != nil {
		return nil, fmt.Errorf("GenerateListenersFromRegistryPod: %w", err)
	}

	passthrough := proxy.NewPassthroughOriginalDstCluster()
	svcCluster := newServiceCluster("echo."+meshDomain, trustDomain, "default", "echo")
	// The per-source mTLS shape every mesh service cluster actually carries:
	// since #842, one socket with an on-demand certificate selector driven by
	// the source-identity filter state.
	perSourceCluster := newPerSourceServiceCluster("per-source-echo."+meshDomain, trustDomain, "default", "echo")
	// Exercises the proposal 019 two-level transport-socket matcher (endpoint
	// waypoint metadata -> source identity) + the endpoint_metadata matcher
	// input, so `envoy --mode validate` proves the config is accepted by a real
	// Envoy.
	waypointCluster := newWaypointServiceCluster("waypoint-echo."+meshDomain, trustDomain, "default", "echo")
	// Proposal 019 Phase 3b dest side: the host-netns tunnel listener (wildcard
	// SNI -> tcp_proxy passthrough) and its STATIC ew_ingress cluster.
	ewIngress := proxy.BuildWaypointIngressCluster("echo."+meshDomain, []string{"10.244.1.5"})
	tunnel := proxy.BuildWaypointTunnelListener(proxy.DefaultEastWestTunnelPort,
		[]*listenerv3.FilterChain{proxy.BuildWaypointTunnelChain("echo." + meshDomain)})

	// The per-pod inbound-readiness probe (issue #815): a TCP (connect-only)
	// active health check over an mTLS UpstreamTlsContext, SAN-pinned to the
	// pod's own SPIFFE ID, dialing the pod's mesh inbound inside its netns.
	// Validated here so stock-proxy acceptance of "TCP health check + TLS
	// transport socket + combined validation context" is a build-time gate.
	inboundReady := proxy.NewInboundReadyProbeCluster(
		proxy.InboundReadyClusterName(pod),
		pod.GetNetworkNamespace(),
		fmt.Sprintf(nodeSpiffeIDFmt, trustDomain),
		"spiffe://"+trustDomain,
		proxy.SpiffeIDFromPod(pod, trustDomain),
	)
	rewriteSDSToExplicitSource(inboundReady)

	staticClusters := []*clusterv3.Cluster{xdsCluster(), agentXDSCluster(), passthrough, svcCluster, perSourceCluster, waypointCluster, ewIngress, authzSidecarCluster()}
	staticClusters = append(staticClusters, appClusters...)
	staticClusters = append(staticClusters, healthCluster, inboundReady, accessLogCluster())

	// The agent-facing health gateway, whose per-pod health_check filter now
	// gates on BOTH probe clusters (issue #815). Validating it proves the
	// two-entry cluster_min_healthy_percentages is accepted and that both
	// referenced clusters resolve.
	healthGateway := proxy.BuildHealthGatewayListener("/run/aether/health.sock", []proxy.HealthGatewayProbe{
		proxy.NewHealthGatewayProbe(healthCluster.GetName(), inboundReady.GetName()),
	})

	return newBootstrap(staticClusters, []*listenerv3.Listener{inbound, outbound, tunnel, healthGateway}), nil
}

// newPerSourceServiceCluster is newServiceCluster with the per-source upstream
// mTLS every mesh cluster carries: since issue #842 that is ONE transport
// socket whose custom_tls_certificate_selector
// (envoy.tls.certificate_selectors.on_demand_secret, with the
// envoy.tls.upstream_certificate_mappers.filter_state_override mapper) resolves
// the client certificate per connection.
//
// What this gate catches is the structural half, and it is worth being precise
// about it because the runtime half fails OPEN. Both extensions are resolved by
// NAME at config load, so a typo'd or absent extension is an
// `envoy --mode validate` FAILURE here — which is the point, because a wrong
// name would otherwise surface in production only as every connection quietly
// presenting the mapper's default_value. What validation cannot see is whether
// the filter-state object actually arrives on the upstream connection; that is
// //test/mtlspool's job.
func newPerSourceServiceCluster(clusterName, td, namespace, svcName string) *clusterv3.Cluster {
	nodeID := fmt.Sprintf(nodeSpiffeIDFmt, td)
	validationCtxName := fmt.Sprintf("spiffe://%s", td)
	sanURI := fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", td, namespace, svcName)

	c := &clusterv3.Cluster{
		Name:           clusterName,
		ConnectTimeout: durationpb.New(5e9),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(32 * 1024),
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(
				config.Http2ProtocolOptions(),
			),
		},
	}
	proxy.InjectUpstreamMTLS(c, nodeID, validationCtxName, []string{sanURI}, "8080", "")
	return c
}

// newWaypointServiceCluster is newServiceCluster with the proposal 019 waypoint
// wiring: the two-level transport-socket matcher (waypoint metadata -> source
// identity) and both the local (port-SNI) and waypoint (structured-SNI) socket
// sets, injected via the same InjectUpstreamMTLS the agent uses.
func newWaypointServiceCluster(clusterName, td, namespace, svcName string) *clusterv3.Cluster {
	nodeID := fmt.Sprintf(nodeSpiffeIDFmt, td)
	validationCtxName := fmt.Sprintf("spiffe://%s", td)
	sanURI := fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", td, namespace, svcName)

	c := &clusterv3.Cluster{
		Name:           clusterName,
		ConnectTimeout: durationpb.New(5e9),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(32 * 1024),
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(
				config.Http2ProtocolOptions(),
			),
		},
	}
	proxy.InjectUpstreamMTLS(c, nodeID, validationCtxName, []string{sanURI}, "8080", "8080."+clusterName)
	return c
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

	inbound, outbound, appClusters, healthCluster, err := proxy.GenerateListenersFromRegistryPod(pod, trustDomain, meshDomain, false, true, nil, nil, "")
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

// NodeUDSBootstrapJSON builds the UDS-delivery (proposal 034 Phase 1) node-proxy
// bootstrap and returns its JSON. It exercises the pipe app clusters (one per
// declared port, all dialing the same socket, no upstream bind config) and the
// HTTP/1.1 active health check over a pipe upstream, so stock Envoy proves the
// pipe delivery config is accepted.
func NodeUDSBootstrapJSON() ([]byte, error) {
	bs, err := buildNodeUDSBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(bs)
}

// buildNodeUDSBootstrap builds the node-proxy bootstrap for a multi-port pod
// whose application is delivered over a Unix socket.
func buildNodeUDSBootstrap() (*bootstrapv3.Bootstrap, error) {
	pod := testPod()
	pod.Annotations = map[string]string{
		aetherannotations.AnnotationEndpointPort:      "8080",
		aetherannotations.AnnotationEndpointPorts:     "8080,9090",
		aetherannotations.AnnotationEndpointUDSSocket: "uds/app.sock",
	}

	socketPath, err := udspath.Resolve(udspath.DefaultKubeletPodsDir, "11111111-2222-3333-4444-555555555555", "uds/app.sock")
	if err != nil {
		return nil, fmt.Errorf("udspath.Resolve: %w", err)
	}

	inbound, outbound, appClusters, healthCluster, err := proxy.GenerateListenersFromRegistryPod(pod, trustDomain, meshDomain, false, false, nil, nil, socketPath)
	if err != nil {
		return nil, fmt.Errorf("GenerateListenersFromRegistryPod (uds): %w", err)
	}

	staticClusters := []*clusterv3.Cluster{xdsCluster(), proxy.NewPassthroughOriginalDstCluster(), newServiceCluster("echo."+meshDomain, trustDomain, "default", "echo")}
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
		// TCP-primary: these fixtures exercise the TCP floor, which since
		// proposal 037 design (d) is emitted only for a TCP-primary service.
		PrimaryIsTCP: true,
	}
	captureListener, err := proxy.GenerateCaptureListener(
		pod,
		proxy.SourceIdentityForPod(pod, trustDomain),
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

// ---------------------------------------------------------------------------
// L4 routes: TCPRoute / TLSRoute / UDPRoute (proposal 018 Phase 3b, issue #868)
// ---------------------------------------------------------------------------
//
// These three bootstraps are the per-PR half of #868's coverage. The nightly
// kind harness exercises the L4 data path; what a real Envoy accepts is checked
// here, because it is cheap, needs no cluster, and gates every PR
// (scripts/ci-impacted-targets.sh classifies //test/envoy_validate as a unit
// target).
//
// The three shapes and why each is worth an Envoy's opinion:
//
//   - TCPRoute: the per-ClusterIP floor chain carries a tcp_proxy with
//     weighted_clusters instead of a single cluster.
//   - TLSRoute: per-SNI chains match prefix_ranges + server_names and sit on
//     the SAME listener as the floor chain, which matches the same /32 with NO
//     server_names. Envoy rejects a listener whose filter chains are ambiguous,
//     so it is Envoy — not a Go assertion — that certifies the coexistence.
//     That coexistence IS the fall-through the harness then drives: a
//     non-matching SNI reaching the floor is designed behaviour, not a bug.
//   - UDPRoute: a connection-less UDP listener must carry NO filter_chains
//     ("N filter chain(s) specified for connection-less UDP listener"), so the
//     udp_proxy config rides listener_filters. That rule is recorded in a
//     comment in l4route.go and nothing checked it until now.
//
// Service keys are namespace-qualified "<ns>/<svc>" because that is what the
// cluster namers parse (proxy.ServiceClusterName -> serviceref.ParseKey).

const (
	l4TCPParent   = "default/l4-front"
	l4TCPBackendA = "default/l4-a"
	l4TCPBackendB = "default/l4-b"

	l4TLSParent   = "default/l4-tls-front"
	l4TLSBackendA = "default/l4-tls-a"
	l4TLSBackendB = "default/l4-tls-b"

	// The UDPRoute fixture has TWO parents on the same pod, each with its own
	// backend, because the thing the transparent listener adds over the old
	// single-cluster one is exactly that (proposal 038): the udp_proxy matcher
	// keys on the dialled VIP.
	l4UDPParentA  = "default/l4-udp-front-a"
	l4UDPParentB  = "default/l4-udp-front-b"
	l4UDPBackendA = "default/l4-udp-a"
	l4UDPBackendB = "default/l4-udp-b"
	// L4UDPParentClusterIPA/B are the UDPRoute parents' VIPs: the matcher keys.
	L4UDPParentClusterIPA = "10.96.2.40"
	L4UDPParentClusterIPB = "10.96.2.41"

	// L4TCPParentClusterIP and L4TLSParentClusterIP are the parent Services'
	// ClusterIPs: the /32 every chain of that leg matches on.
	L4TCPParentClusterIP = "10.96.2.20"
	L4TLSParentClusterIP = "10.96.2.30"

	// L4TCPWeightA and L4TCPWeightB are the TCPRoute fixture's backendRef
	// weights. Deliberately unequal and not co-prime with each other's sum, so
	// a builder that normalised or swapped them would be visible.
	L4TCPWeightA = 75
	L4TCPWeightB = 25

	// L4SNIAlpha and L4SNIBravo are the TLSRoute fixture's two hostnames — one
	// TLSRoute object each, because TLSRoute.Spec.Hostnames is route-level.
	L4SNIAlpha = "a.l4.test"
	L4SNIBravo = "b.l4.test"

	// L4UDPBackendPort is the backend's APPLICATION UDP port. The UDP floor has
	// no inbound mTLS hop, so udp_proxy dials this port directly.
	L4UDPBackendPort = 9001
	// l4UDPMeshInboundPort is the mesh inbound TCP port the shared bare-name EDS
	// load assignment carries, and therefore the port proxy.UDPLoadAssignment
	// has to rewrite AWAY from. The exact number is not the assertion; that it
	// differs from L4UDPBackendPort is.
	l4UDPMeshInboundPort = 18008
	// l4UDPEndpointIP is the backend pod IP in the inline UDP load assignment.
	l4UDPEndpointIP = "10.244.3.7"
)

// L4TCPBackendClusterA and friends are the data-plane cluster names the L4
// fixtures reference, exported so the test asserts on the names the builders
// actually emitted rather than re-deriving them from a second copy of the
// naming rule.
func L4TCPBackendClusterA() string { return proxy.TCPClusterName(l4TCPBackendA, meshDomain) }

// L4TCPBackendClusterB is the second TCPRoute backend's TCP floor cluster.
func L4TCPBackendClusterB() string { return proxy.TCPClusterName(l4TCPBackendB, meshDomain) }

// L4TLSBackendClusterA is the a.l4.test SNI chain's backend cluster.
func L4TLSBackendClusterA() string { return proxy.TCPClusterName(l4TLSBackendA, meshDomain) }

// L4TLSBackendClusterB is the b.l4.test SNI chain's backend cluster.
func L4TLSBackendClusterB() string { return proxy.TCPClusterName(l4TLSBackendB, meshDomain) }

// L4TLSParentFloorCluster is the TLS parent's own TCP floor cluster: where a
// connection whose SNI matches no TLSRoute deliberately lands.
func L4TLSParentFloorCluster() string { return proxy.TCPClusterName(l4TLSParent, meshDomain) }

// L4UDPBackendClusterA is parent A's backend's plaintext UDP cluster.
func L4UDPBackendClusterA() string { return proxy.UDPClusterName(l4UDPBackendA, meshDomain) }

// L4UDPBackendClusterB is parent B's backend's plaintext UDP cluster.
func L4UDPBackendClusterB() string { return proxy.UDPClusterName(l4UDPBackendB, meshDomain) }

// CaptureTCPRouteBootstrapJSON builds a capture bootstrap whose per-ClusterIP
// floor chain is a TCPRoute-weighted tcp_proxy (proposal 018 Phase 3b) over two
// backends, 75/25.
//
// The weighted form is what a TCPRoute produces and what the floor chain never
// carries without one, so this is the only place in the harness where Envoy
// parses a TcpProxy_WeightedClusters at all.
func CaptureTCPRouteBootstrapJSON() ([]byte, error) {
	bs, err := buildCaptureTCPRouteBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(bs)
}

func buildCaptureTCPRouteBootstrap() (*bootstrapv3.Bootstrap, error) {
	pod := testPod()

	svc := proxy.CaptureTCPService{
		// The production ClusterName is the "tcp:"-prefixed floor cluster, not
		// the bare mesh authority (cache/capture.go builds it with
		// proxy.TCPClusterName). Spelling it any other way here would model a
		// listener the agent never emits.
		ClusterName: proxy.TCPClusterName(l4TCPParent, meshDomain),
		ClusterIP:   L4TCPParentClusterIP,
		// TCP-primary: these fixtures exercise the TCP floor, which since
		// proposal 037 design (d) is emitted only for a TCP-primary service.
		PrimaryIsTCP: true,
		TCPRouteRules: []proxy.L4ServiceRoute{{
			Backends: []proxy.L4Backend{
				{Service: l4TCPBackendA, Cluster: L4TCPBackendClusterA(), Weight: L4TCPWeightA},
				{Service: l4TCPBackendB, Cluster: L4TCPBackendClusterB(), Weight: L4TCPWeightB},
			},
		}},
	}

	listener, err := proxy.GenerateCaptureListener(
		pod,
		proxy.SourceIdentityForPod(pod, trustDomain),
		meshconst.ProxyCapturePort,
		meshDomain,
		false, // emitStatsPod
		[]proxy.CaptureTCPService{svc},
		true, // withPassthrough (redirect-all, the shipped default)
		nil,  // extensionFilters
	)
	if err != nil {
		return nil, fmt.Errorf("GenerateCaptureListener: %w", err)
	}

	return newBootstrap(
		[]*clusterv3.Cluster{
			xdsCluster(),
			// The floor clusters' per-connection certificate selector fetches
			// over its own api_config_source naming agent_xds (#842), so the
			// bootstrap has to define it or the reference dangles.
			agentXDSCluster(),
			proxy.NewPassthroughOriginalDstCluster(),
			newTCPFloorCluster(l4TCPBackendA, "l4-a"),
			newTCPFloorCluster(l4TCPBackendB, "l4-b"),
		},
		[]*listenerv3.Listener{listener},
	), nil
}

// CaptureTLSRouteBootstrapJSON builds a capture bootstrap carrying two TLSRoute
// SNI chains ALONGSIDE the parent's TCP floor chain on one listener.
//
// The three chains all match prefix_ranges <ClusterIP>/32; two of them add
// server_names and one does not. Envoy resolves that by specificity — SNI match
// wins, no SNI match falls through to the floor — and rejects filter chains it
// cannot disambiguate, so `--mode validate` accepting this listener is the
// structural half of the fall-through behaviour #868 asks for.
func CaptureTLSRouteBootstrapJSON() ([]byte, error) {
	bs, err := buildCaptureTLSRouteBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(bs)
}

func buildCaptureTLSRouteBootstrap() (*bootstrapv3.Bootstrap, error) {
	pod := testPod()

	svc := proxy.CaptureTCPService{
		ClusterName: proxy.TCPClusterName(l4TLSParent, meshDomain),
		ClusterIP:   L4TLSParentClusterIP,
		// TCP-primary: these fixtures exercise the TCP floor, which since
		// proposal 037 design (d) is emitted only for a TCP-primary service.
		PrimaryIsTCP: true,
		// No TCPRouteRules on purpose: the parent keeps its plain passthrough
		// floor chain, which is the chain a non-matching SNI is DESIGNED to
		// reach. Adding a TCPRoute here would hide that half of the shape.
		TLSRouteRules: []proxy.L4ServiceRoute{
			{
				SNIHostnames: []string{L4SNIAlpha},
				Backends: []proxy.L4Backend{
					{Service: l4TLSBackendA, Cluster: L4TLSBackendClusterA(), Weight: 1},
				},
			},
			{
				SNIHostnames: []string{L4SNIBravo},
				Backends: []proxy.L4Backend{
					{Service: l4TLSBackendB, Cluster: L4TLSBackendClusterB(), Weight: 1},
				},
			},
		},
	}

	listener, err := proxy.GenerateCaptureListener(
		pod,
		proxy.SourceIdentityForPod(pod, trustDomain),
		meshconst.ProxyCapturePort,
		meshDomain,
		false, // emitStatsPod
		[]proxy.CaptureTCPService{svc},
		true, // withPassthrough
		nil,  // extensionFilters
	)
	if err != nil {
		return nil, fmt.Errorf("GenerateCaptureListener: %w", err)
	}

	return newBootstrap(
		[]*clusterv3.Cluster{
			xdsCluster(),
			agentXDSCluster(),
			proxy.NewPassthroughOriginalDstCluster(),
			// The floor chain routes to the PARENT's own TCP cluster; the SNI
			// chains route to the backends'.
			newTCPFloorCluster(l4TLSParent, "l4-tls-front"),
			newTCPFloorCluster(l4TLSBackendA, "l4-tls-a"),
			newTCPFloorCluster(l4TLSBackendB, "l4-tls-b"),
		},
		[]*listenerv3.Listener{listener},
	), nil
}

// CaptureUDPBootstrapJSON builds the transparent, connection-less UDP capture
// listener on the L4 mesh port plus the plaintext "udp:" clusters it routes to
// (proposal 018 Phase 3b, transparent capture per proposal 038).
//
// SCOPE: delivery AND selection. Two UDPRoute parents, two VIPs, two backends:
// the listener must carry one matcher arm per parent, keyed on the dialled VIP
// (DestinationIPInput reads the ORIGINAL destination because the CNI divert
// leaves the header intact). What the generator chooses AMONG a parent's
// backends (drained ones skipped, then the heaviest) is covered by unit tests
// in agent/internal/xds/proxy; a traffic SPLIT is not expressible (#873).
func CaptureUDPBootstrapJSON() ([]byte, error) {
	bs, err := buildCaptureUDPBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(bs)
}

func buildCaptureUDPBootstrap() (*bootstrapv3.Bootstrap, error) {
	pod := testPod()

	clusterA, clusterB := L4UDPBackendClusterA(), L4UDPBackendClusterB()
	listener, err := proxy.GenerateUDPCaptureListener(
		pod.GetName(),
		pod.GetNetworkNamespace(),
		meshconst.ProxyL4OutboundPort,
		map[string][]proxy.L4Backend{
			l4UDPParentA: {{Service: l4UDPBackendA, Cluster: clusterA, Weight: 1}},
			l4UDPParentB: {{Service: l4UDPBackendB, Cluster: clusterB, Weight: 1}},
		},
		map[string]string{
			l4UDPParentA: L4UDPParentClusterIPA,
			l4UDPParentB: L4UDPParentClusterIPB,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("GenerateUDPCaptureListener: %w", err)
	}
	if listener == nil {
		return nil, fmt.Errorf("GenerateUDPCaptureListener returned no listener for a non-empty route set")
	}

	// The production cluster is built from the service's EXISTING (TCP inbound,
	// :18008) load assignment, rewritten by proxy.UDPLoadAssignment onto the
	// backend's application UDP port. Model it the same way round so the
	// rewrite is what produces the fixture, not a hand-written UDP endpoint.
	laA := proxy.UDPLoadAssignment(meshInboundLoadAssignment(l4UDPBackendA), clusterA, L4UDPBackendPort)
	laB := proxy.UDPLoadAssignment(meshInboundLoadAssignment(l4UDPBackendB), clusterB, L4UDPBackendPort)
	if laA == nil || laB == nil {
		return nil, fmt.Errorf("UDPLoadAssignment returned nil")
	}

	return newBootstrap(
		[]*clusterv3.Cluster{
			xdsCluster(),
			proxy.NewUDPServiceCluster(clusterA, l4UDPBackendA, laA),
			proxy.NewUDPServiceCluster(clusterB, l4UDPBackendB, laB),
		},
		[]*listenerv3.Listener{listener},
	), nil
}

// newTCPFloorCluster builds a service's "tcp:" floor cluster exactly as
// SnapshotCache.captureTCPClusters does: NewTCPServiceCluster plus the
// per-connection mesh mTLS socket, SAN-pinned to the backend's workload
// identity. saName is the bare service name the SPIFFE ID's sa/ segment
// carries (refreshEntryMTLSLocked uses the bare name, not the key).
func newTCPFloorCluster(serviceKey, saName string) *clusterv3.Cluster {
	c := proxy.NewTCPServiceCluster(proxy.TCPClusterName(serviceKey, meshDomain), serviceKey, serviceKey)
	proxy.InjectUpstreamTCPMTLS(
		c,
		fmt.Sprintf(nodeSpiffeIDFmt, trustDomain),
		fmt.Sprintf("spiffe://%s", trustDomain),
		[]string{fmt.Sprintf("spiffe://%s/ns/default/sa/%s", trustDomain, saName)},
		"", // no SNI on the floor: the peer must demux to its inbound default chain
	)
	return c
}

// meshInboundLoadAssignment is the shared bare-name EDS load assignment a mesh
// service carries: the destination pod's mesh INBOUND TCP port, which is
// exactly what the UDP path must not use.
func meshInboundLoadAssignment(clusterName string) *endpointv3.ClusterLoadAssignment {
	return &endpointv3.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []*endpointv3.LocalityLbEndpoints{{
			LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
					Endpoint: &endpointv3.Endpoint{
						Address: &corev3.Address{
							Address: &corev3.Address_SocketAddress{
								SocketAddress: &corev3.SocketAddress{
									Protocol: corev3.SocketAddress_TCP,
									Address:  l4UDPEndpointIP,
									PortSpecifier: &corev3.SocketAddress_PortValue{
										PortValue: l4UDPMeshInboundPort,
									},
								},
							},
						},
					},
				},
			}},
		}},
	}
}

// OutboundZeroVhostRouteBootstrapJSON builds a bootstrap whose listener inlines
// the out_http RouteConfiguration the agent publishes when it has NOTHING to
// publish — zero service virtual hosts, i.e. the local-only start of issue #817.
//
// That table used not to be emitted at all, which under delta ADS meant the
// egress listener's RDS subscription was answered with silence, warmed out, and
// went active with no routes (404 NR route_not_found on everything). It is now
// unconditional, so what a real Envoy has to accept is a RouteConfiguration
// consisting of the on-demand catch-all and nothing else: the liveness
// direct-response, the mesh-authority safe_regex → cluster_header ODCDS route,
// and the hard 404 fallthrough. Inlining it as a static route_config is the only
// way this offline gate can see an RDS-delivered table at all.
func OutboundZeroVhostRouteBootstrapJSON() ([]byte, error) {
	bs, err := buildOutboundZeroVhostRouteBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(bs)
}

// buildOutboundZeroVhostRouteBootstrap assembles the zero-vhost out_http config.
func buildOutboundZeroVhostRouteBootstrap() (*bootstrapv3.Bootstrap, error) {
	routeCfg := proxy.BuildOutboundRouteConfiguration(nil, meshDomain)
	if len(routeCfg.GetVirtualHosts()) != 1 {
		return nil, fmt.Errorf("zero-vhost out_http must carry exactly the catch-all, got %d virtual hosts",
			len(routeCfg.GetVirtualHosts()))
	}

	hcm := &http_connection_managerv3.HttpConnectionManager{
		StatPrefix: "out_http_validate",
		RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{
			RouteConfig: routeCfg,
		},
		HttpFilters: []*http_connection_managerv3.HttpFilter{{
			Name: "envoy.filters.http.router",
			ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{
				TypedConfig: mustAny(&routerv3.Router{}),
			},
		}},
	}
	listener := &listenerv3.Listener{
		Name: "out_http_validate",
		Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
			Address:       "127.0.0.1",
			PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 15012},
		}}},
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(hcm)},
			}},
		}},
	}

	// The catch-all's non-mesh fallthrough is a direct 404 here (the outbound
	// listener never passes through), so no passthrough cluster is needed; the
	// mesh-authority route resolves its cluster from :authority via ODCDS, which
	// names no cluster at config load either.
	return newBootstrap(
		[]*clusterv3.Cluster{xdsCluster()},
		[]*listenerv3.Listener{listener},
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
	)

	edgeHTTP := proxy.BuildEdgeGatewayHTTPListener(edgeGWNamespace, edgeGWName, edgeHTTPPort, false, geo, edgeCfg)
	edgeHTTPS := proxy.BuildEdgeGatewayHTTPSListener(edgeGWNamespace, edgeGWName, edgeHTTPSPort, []string{edgeTLSCertName}, geo, edgeCfg)
	edgeH3 := proxy.BuildEdgeGatewayHTTP3Listener(edgeGWNamespace, edgeGWName, edgeHTTPSPort, []string{edgeTLSCertName}, geo, edgeCfg)

	return newBootstrap(
		[]*clusterv3.Cluster{xdsCluster(), spire, edgeSvc},
		[]*listenerv3.Listener{edgeHTTP, edgeHTTPS, edgeH3},
	), nil
}

// UnpinnedMeshClusters returns the names of every upstream TLS context in a
// generated bootstrap that carries NO match_typed_subject_alt_names — i.e.
// every cluster whose handshake would prove trust-domain membership and nothing
// else, so any mesh workload satisfies it (issue #832).
//
// It is the config-shape half of that issue's gate, and it runs over the exact
// bytes handed to `envoy --mode validate`: Envoy ACCEPTS an unpinned validation
// context, so this is a shape a passing validate can never catch. Nothing here
// is allow-listed by name — a cluster is checked precisely when it has an
// UpstreamTlsContext, so the passthrough, app, health, xds, spire_agent and
// waypoint-ingress clusters (no TLS at all) are out of scope automatically, and
// a NEW mesh cluster added to a builder is in scope the moment it grows one.
//
// Each returned name is "<cluster>" for a plain transport_socket or
// "<cluster>/<match>" for a transport_socket_matches entry (the per-source mTLS
// shape, where the pin lives on every match INCLUDING the node-identity
// on_no_match one).
func UnpinnedMeshClusters(bootstrapJSON []byte) ([]string, error) {
	var bs bootstrapv3.Bootstrap
	if err := protojson.Unmarshal(bootstrapJSON, &bs); err != nil {
		return nil, fmt.Errorf("unmarshal bootstrap: %w", err)
	}

	var unpinned []string
	for _, c := range bs.GetStaticResources().GetClusters() {
		pinned, err := upstreamTLSPinned(c.GetTransportSocket())
		if err != nil {
			return nil, fmt.Errorf("cluster %s: %w", c.GetName(), err)
		}
		if !pinned {
			unpinned = append(unpinned, c.GetName())
		}
		for _, m := range c.GetTransportSocketMatches() {
			pinned, err := upstreamTLSPinned(m.GetTransportSocket())
			if err != nil {
				return nil, fmt.Errorf("cluster %s match %s: %w", c.GetName(), m.GetName(), err)
			}
			if !pinned {
				unpinned = append(unpinned, c.GetName()+"/"+m.GetName())
			}
		}
	}
	return unpinned, nil
}

// UnpinnedInboundChains returns "<listener>/<chain>" for every filter chain in
// a generated bootstrap whose DownstreamTlsContext requires a client
// certificate but carries NO match_typed_subject_alt_names (issue #843).
//
// require_client_certificate on its own proves only that SOME certificate this
// trust bundle signs was presented — any mesh workload, and anything else the
// bundle happens to sign. The inbound HCM then stamps that certificate's URI
// SAN into XFCC with SANITIZE_SET, and RBAC/ext_authz decisions key on it, so
// an unpinned inbound socket silently weakens every XFCC-derived rule.
//
// `envoy --mode validate` ACCEPTS an unpinned context (the same fail-open
// direction #832 found on the upstream side), so this has to be a config-shape
// assertion over the generated bytes rather than something the validate below
// can catch. It runs on the exact bytes handed to Envoy.
//
// Scope is structural, never an allow-list: a chain is checked precisely when
// it requires a client certificate. The edge's downstream contexts do NOT —
// external callers have no mesh identity — so they are out of scope
// automatically, and a NEW mTLS-terminating chain is in scope the moment it
// sets the field.
func UnpinnedInboundChains(bootstrapJSON []byte) ([]string, error) {
	var bs bootstrapv3.Bootstrap
	if err := protojson.Unmarshal(bootstrapJSON, &bs); err != nil {
		return nil, fmt.Errorf("unmarshal bootstrap: %w", err)
	}

	var unpinned []string
	for _, l := range bs.GetStaticResources().GetListeners() {
		chains := l.GetFilterChains()
		if def := l.GetDefaultFilterChain(); def != nil {
			chains = append(chains, def)
		}
		for _, fc := range chains {
			pinned, err := downstreamTLSPinned(fc.GetTransportSocket())
			if err != nil {
				return nil, fmt.Errorf("listener %s chain %s: %w", l.GetName(), fc.GetName(), err)
			}
			if !pinned {
				unpinned = append(unpinned, l.GetName()+"/"+fc.GetName())
			}
		}
	}
	return unpinned, nil
}

// downstreamTLSPinned reports whether a downstream transport socket pins the
// CLIENT identity. A socket that is absent, is not a DownstreamTlsContext, or
// does not require a client certificate is "pinned" vacuously: there is no
// verified peer identity to constrain in the first place.
func downstreamTLSPinned(ts *corev3.TransportSocket) (bool, error) {
	if ts.GetTypedConfig() == nil || !ts.GetTypedConfig().MessageIs(&tlsv3.DownstreamTlsContext{}) {
		return true, nil
	}
	var ctx tlsv3.DownstreamTlsContext
	if err := ts.GetTypedConfig().UnmarshalTo(&ctx); err != nil {
		return false, fmt.Errorf("unmarshal DownstreamTlsContext: %w", err)
	}
	if !ctx.GetRequireClientCertificate().GetValue() {
		return true, nil
	}
	return len(ctx.GetCommonTlsContext().GetCombinedValidationContext().
		GetDefaultValidationContext().GetMatchTypedSubjectAltNames()) > 0, nil
}

// upstreamTLSPinned reports whether a transport socket is SAN-pinned. A socket
// that is absent or is not an UpstreamTlsContext is "pinned" vacuously: it has
// no upstream peer identity to check in the first place.
func upstreamTLSPinned(ts *corev3.TransportSocket) (bool, error) {
	if ts.GetTypedConfig() == nil || !ts.GetTypedConfig().MessageIs(&tlsv3.UpstreamTlsContext{}) {
		return true, nil
	}
	var ctx tlsv3.UpstreamTlsContext
	if err := ts.GetTypedConfig().UnmarshalTo(&ctx); err != nil {
		return false, fmt.Errorf("unmarshal UpstreamTlsContext: %w", err)
	}
	// The pin lives in the COMBINED validation context; the plain
	// ValidationContextSdsSecretConfig form carries the trust bundle alone.
	return len(ctx.GetCommonTlsContext().GetCombinedValidationContext().
		GetDefaultValidationContext().GetMatchTypedSubjectAltNames()) > 0, nil
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
// accessLogCluster is the OTLP sink the access logger references by name
// ("otel_collector"). Envoy validates that an envoy_grpc access logger names a
// known cluster, so the bootstrap must carry it once access logging is on.
func accessLogCluster() *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:           "otel_collector",
		ConnectTimeout: durationpb.New(5e9), // 5 s
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_STATIC,
		},
		LoadAssignment: pipeEndpoint("otel_collector", xdsSockPath),
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustAny(
				config.Http2ProtocolOptions(),
			),
		},
	}
}

// agentXDSCluster is the chart's agent_xds cluster, present here because since
// issue #842's fix the per-connection certificate selector names it in its own
// api_config_source instead of riding `ads: {}`.
//
// It must exist for that reference to resolve: an api_config_source naming an
// unknown cluster is a dangling reference, and the only reason it does not fail
// `envoy --mode validate` today is that the on-demand SDS subscription is
// created lazily on the main dispatcher, which validate mode never runs. The
// bootstrap test asserts the reference resolves against these clusters, so the
// gate does not depend on that accident.
func agentXDSCluster() *clusterv3.Cluster {
	c := xdsCluster()
	c.Name = proxy.AgentXDSClusterName
	c.LoadAssignment = pipeEndpoint(proxy.AgentXDSClusterName, xdsSockPath)
	return c
}

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
	tlsCertName := fmt.Sprintf(nodeSpiffeIDFmt, td)
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
// rewriteSDSToExplicitSource repoints a cluster's upstream TLS SDS references
// from `ads: {}` to an explicit api_config_source naming the static xds_cluster.
//
// This is a WORKAROUND FOR `--mode validate` ONLY; the mesh ships `ads: {}`.
//
// `envoy --mode validate` SEGFAULTS on any STATIC cluster whose transport
// socket resolves a secret over `ads: {}`: a static cluster's startPreInit runs
// during MainImpl::initialize, which fires the SDS init target, and
// Secret::SdsApi::initialize() then dereferences an ADS mux that
// Server::ValidationInstance never builds. Reproduced standalone against the
// pinned binary (1.40.0-dev) with a two-cluster bootstrap and nothing aether in
// it; the same cluster with an explicit api_config_source validates OK. It is a
// property of the validation server, not of the config.
//
// It does not arise in production because these probe clusters are delivered
// over CDS as SECONDARY clusters, after the ADS stream is up — unlike a
// bootstrap static_resources cluster, which is what this harness has to model.
// Rewriting only the SDS config source keeps everything this gate exists to
// check — STATIC + netns bind config + TCP health check + UpstreamTlsContext +
// SAN-pinned combined validation context — under a real Envoy's parser.
func rewriteSDSToExplicitSource(c *clusterv3.Cluster) {
	ts := c.GetTransportSocket()
	if ts == nil {
		return
	}
	var ctx tlsv3.UpstreamTlsContext
	if err := ts.GetTypedConfig().UnmarshalTo(&ctx); err != nil {
		return
	}
	explicit := config.SDSConfigSourceFromCluster("xds_cluster")
	for _, sc := range ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs() {
		sc.SdsConfig = explicit
	}
	if combined := ctx.GetCommonTlsContext().GetCombinedValidationContext(); combined != nil {
		combined.ValidationContextSdsSecretConfig.SdsConfig = explicit
	}
	ts.ConfigType = &corev3.TransportSocket_TypedConfig{TypedConfig: mustAny(&ctx)}
}

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
