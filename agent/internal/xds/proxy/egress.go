package proxy

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/serviceref"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// ServiceClusterName returns the data-plane name of a service's outbound
// cluster: <svc>.<ns>.<meshDomain> (proposal 020 Part 1). The control-plane key
// (registry, watch filter, dependency set, EDS, stats) is the namespace-qualified
// "<ns>/<svc>" serviceref key; this renders it as the FQDN authority, so the warm
// path (service vhost) and the cold path (catch-all cluster_header: ":authority"
// + ODCDS) resolve the same resource deterministically.
//
// The cutover is complete (GAMMA #397, l4route + edge L4): every caller passes a
// namespace-qualified key. A bare (non-namespaced) name therefore can't occur in
// production; rather than render a flat <name>.<meshDomain> that would silently
// mis-route, it returns "" — an empty cluster name matches no cluster, so a stray
// bare key degrades to a clean no-route instead of a wrong route.
func ServiceClusterName(serviceName, meshDomain string) string {
	ref, ok := serviceref.ParseKey(serviceName)
	if !ok {
		return ""
	}
	return ref.FQDN(meshDomain)
}

// PortClusterName returns the data-plane name of a service's per-port cluster:
// <svc>.<ns>.<meshDomain>:<port>. Equals the FQDN:port authority a client
// addresses, so the catch-all cluster_header resolves it directly. Returns ""
// when the service key is not namespace-qualified (see ServiceClusterName).
func PortClusterName(serviceName, meshDomain string, port uint32) string {
	base := ServiceClusterName(serviceName, meshDomain)
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", base, port)
}

// TCPClusterName returns the data-plane name of a service's TCP-floor cluster:
// "tcp:<svc>.<ns>.<meshDomain>". This cluster is identical to the HTTP cluster
// (<svc>.<ns>.<meshDomain>) except it uses ALPN "aether-tcp" on its upstream
// transport socket so the destination inbound demuxes to the TCP floor chain.
// The capture TCP floor filter chains reference this cluster by this name.
// Returns "" when the service key is not namespace-qualified.
func TCPClusterName(serviceName, meshDomain string) string {
	base := ServiceClusterName(serviceName, meshDomain)
	if base == "" {
		return ""
	}
	return "tcp:" + base
}

// UDPClusterName returns the data-plane name of a service's UDP-floor cluster:
// "udp:<svc>.<ns>.<meshDomain>". This cluster carries the same backend endpoints
// as the HTTP cluster but is a plain EDS cluster with no transport socket (UDP
// datagrams are not wrapped in mTLS — mesh mTLS is a TCP/TLS construct and
// DTLS is not supported). The udp_proxy capture listener references this cluster.
// Returns "" when the service key is not namespace-qualified.
func UDPClusterName(serviceName, meshDomain string) string {
	base := ServiceClusterName(serviceName, meshDomain)
	if base == "" {
		return ""
	}
	return "udp:" + base
}

// ServiceFromClusterName maps a data-plane cluster name (a mesh authority,
// <svc>.<ns>.<meshDomain>) back to the namespace-qualified "<ns>/<svc>" service
// key (proposal 020 Part 1). ok is false when the name is not under the mesh
// domain or the remainder is not exactly two DNS labels (<svc>.<ns> — service
// names are ServiceAccount names and namespaces are both single lowercase
// labels), so nested or foreign authorities are rejected deterministically.
func ServiceFromClusterName(clusterName, meshDomain string) (string, bool) {
	// Strip an optional :port (multi-port authority <svc>.<ns>.<domain>:<port>).
	name := clusterName
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		if _, err := strconv.Atoi(name[i+1:]); err == nil {
			name = name[:i]
		}
	}
	suffix := "." + meshDomain
	if !strings.HasSuffix(name, suffix) {
		return "", false
	}
	// "<svc>.<ns>" — exactly two labels (service name, then namespace).
	svc, ns, found := strings.Cut(strings.TrimSuffix(name, suffix), ".")
	if !found || svc == "" || ns == "" || strings.Contains(ns, ".") {
		return "", false
	}
	return serviceref.New(ns, svc).Key(), true
}

// NewServiceCluster builds the outbound service cluster, named
// <service>.<meshDomain> (see ServiceClusterName). Endpoints (delivered via
// EDS under the bare service name) are the destination pods' mesh inbound
// addresses (pod_ip:defaultInboundPort), each reached by the pod's own
// netns-bound inbound listener. The cluster speaks HTTP/2 mTLS to the
// destination pod, presenting the originating pod's identity. The per-source
// mTLS transport socket is injected at snapshot time (InjectUpstreamMTLS)
// from the current local workloads; connection_pool_per_downstream_connection
// keeps a source pod's connection (and therefore its certificate) from being
// reused for another source.
// maxSubsetComboKeys caps how many provider-defined keys participate in
// multi-key combinations: the selector count is 2 + 2^k - 1, and a service
// with more than 4 routing dimensions is an architecture smell, not a config
// to silently honor. Beyond the cap, the lexicographically-first keys keep
// full combinations and the rest are emitted as singletons only.
const maxSubsetComboKeys = 4

// subsetSelectors builds the cluster's subset index:
//
//   - The always-on ip/pod pinning selectors. These identify ONE endpoint by
//     construction and are therefore never combined with other keys (pin
//     headers are exclusive — mixing a pin with subset headers matches no
//     selector and falls back to normal balancing). single_host_per_subset
//     tells Envoy each subset holds exactly one host, replacing the
//     per-value subset maps with a direct host index.
//   - The power set of the provider-defined keys (derived from the service's
//     endpoint metadata, pre-sorted): a request carrying N subset headers
//     routes to endpoints matching ALL N dimensions. Combos are emitted in
//     deterministic order (by length, then lexicographically) — selector
//     order is part of the CDS resource bytes (delta-xDS hashing).
//
// Every selector is NO_FALLBACK — a request asking for a subset that doesn't
// exist fails (pin-or-fail; spilling a version=v2 request onto v1 is the
// canary-poisoning failure mode; deterministic exclusion over spraying, same
// doctrine as panic-0). Criteria-LESS traffic never consults selectors and
// rides the cluster-level ANY_ENDPOINT fallback.
func subsetSelectors(subsetKeys []string) []*clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector {
	selectors := []*clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector{
		{
			Keys:                []string{subsetIPKey},
			FallbackPolicy:      clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector_NO_FALLBACK,
			SingleHostPerSubset: true,
		},
		{
			Keys:                []string{subsetPodNameKey},
			FallbackPolicy:      clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector_NO_FALLBACK,
			SingleHostPerSubset: true,
		},
	}
	for _, combo := range subsetKeyCombos(subsetKeys) {
		selectors = append(selectors, &clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector{
			Keys:           combo,
			FallbackPolicy: clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector_NO_FALLBACK,
		})
	}
	return selectors
}

// subsetKeyCombos returns every non-empty combination of the (sorted) keys,
// ordered by size then lexicographically. Keys beyond maxSubsetComboKeys
// participate as singletons only.
func subsetKeyCombos(keys []string) [][]string {
	comboKeys := keys
	var singletonOnly []string
	if len(keys) > maxSubsetComboKeys {
		comboKeys = keys[:maxSubsetComboKeys]
		singletonOnly = keys[maxSubsetComboKeys:]
	}

	var combos [][]string
	n := len(comboKeys)
	for size := 1; size <= n; size++ {
		var build func(start int, cur []string)
		build = func(start int, cur []string) {
			if len(cur) == size {
				combos = append(combos, append([]string(nil), cur...))
				return
			}
			for i := start; i < n; i++ {
				build(i+1, append(cur, comboKeys[i]))
			}
		}
		build(0, nil)
	}
	for _, k := range singletonOnly {
		combos = append(combos, []string{k})
	}
	return combos
}

// NewServiceCluster builds an EDS service cluster. perDownstreamConnectionPool
// keys upstream connection pools by the downstream connection so a source pod's
// connection (and its client certificate) is never reused for another source —
// required on the node proxy, which multiplexes many workload identities. The
// edge proxy carries a single identity and passes false for full upstream
// multiplexing (immune to the per-downstream-pool leak class).
func NewServiceCluster(name, edsServiceName, altStatName string, subsetKeys []string, perDownstreamConnectionPool bool) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name: name,
		// Stats stay keyed by the bare service name (cardinality rounds 1-2
		// shapes unchanged by the FQDN/port cluster naming).
		AltStatName:                           altStatName,
		ConnectTimeout:                        durationpb.New(2 * time.Second),
		PerConnectionBufferLimitBytes:         wrapperspb.UInt32(perConnectionBufferLimitBytes),
		ConnectionPoolPerDownstreamConnection: perDownstreamConnectionPool,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
			// EDS resource name: the default cluster shares the bare-service EDS
			// (all endpoints); a per-port cluster uses its own name so its EDS
			// membership is filtered to pods advertising that port (safe new-port
			// rollout). The cache keys load assignments by this name.
			ServiceName: edsServiceName,
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(config.Http2ProtocolOptions()),
		},
		// Cluster-level ANY_ENDPOINT routes criteria-less traffic (normal
		// requests with no subset headers) across all healthy endpoints; the
		// selectors themselves are NO_FALLBACK (see subsetSelectors).
		LbSubsetConfig: &clusterv3.Cluster_LbSubsetConfig{
			FallbackPolicy:  clusterv3.Cluster_LbSubsetConfig_ANY_ENDPOINT,
			SubsetSelectors: subsetSelectors(subsetKeys),
		},
		// Per-endpoint health comes from delegated liveness via EDS (proposal
		// 004 Phase 4): endpoints register UNHEALTHY, the destination-side
		// health gateway vets them, and the registrar propagates promotion in
		// ~1s — so new endpoints enter every client pre-warmed and the
		// O(clients x endpoints) active-HC term is retired, not just reduced.
		// Fast local failure detection is outlier detection below, which is
		// O(actual traffic).
		//
		// Panic routing is disabled: with draining endpoints reported
		// UNHEALTHY (see endpointHealthStatus), a heavy simultaneous roll
		// could cross Envoy's default 50% panic threshold and spray traffic
		// at known-unhealthy hosts; deterministic exclusion + per-request
		// retries + the registry's own health pipeline are the mesh's
		// answer, not panic spraying.
		CommonLbConfig: &clusterv3.Cluster_CommonLbConfig{
			HealthyPanicThreshold: &typev3.Percent{Value: 0},
		},
		// Outlier detection ejects endpoints that fail in practice — consecutive
		// locally-originated failures (connect refused/reset/timeout: 3 in a row,
		// the SIGKILL'd-pod case EDS hasn't caught up with yet) or consecutive
		// 5xx from the app (5 in a row). The ejection percent cap keeps a local
		// ejection wave from emptying a cluster EDS still believes healthy
		// (panic-0 above would then fail ALL requests): at most half the
		// endpoints can be ejected at once.
		OutlierDetection: &clusterv3.OutlierDetection{
			SplitExternalLocalOriginErrors: true,
			ConsecutiveLocalOriginFailure:  wrapperspb.UInt32(3),
			Consecutive_5Xx:                wrapperspb.UInt32(5),
			BaseEjectionTime:               durationpb.New(30 * time.Second),
			MaxEjectionPercent:             wrapperspb.UInt32(50),
			Interval:                       durationpb.New(10 * time.Second),
		},
		// Close pool connections the moment an endpoint goes unhealthy (incl.
		// the EDS drain mark above): the pools are idle then, and closing them
		// pre-empts the app-exit GOAWAY race that strands claimed-but-unanswered
		// streams. See endpointHealthStatus.
		CloseConnectionsOnHostHealthFailure: true,
		// Retry headroom: the default cluster max_retries circuit breaker (3
		// concurrent) rejected retries during roll bursts (retry_overflow,
		// instrumented 2026-06-11); pool-close at drain-mark briefly retries a
		// burst of pre-request resets, which must never be sacrificed to the
		// breaker.
		CircuitBreakers: &clusterv3.CircuitBreakers{
			Thresholds: []*clusterv3.CircuitBreakers_Thresholds{
				{MaxRetries: wrapperspb.UInt32(16)},
			},
		},
		// Envoy by default keeps an EDS-removed host in rotation while it is
		// marked unhealthy (now: outlier-ejected) — which would defeat the
		// early termination drain (the agent deregisters an endpoint the
		// moment its pod's deletion is requested, precisely so clients stop
		// picking it before the app dies). Honor EDS removals immediately.
		IgnoreHealthOnHostRemoval: true,
	}
}

// NewTCPServiceCluster builds an EDS service cluster for a non-HTTP service's TCP
// floor path (proposal 018, Phase 3a). It is identical to NewServiceCluster except:
//   - It omits TypedExtensionProtocolOptions (no HTTP/2 upstream protocol — the
//     tcp_proxy sends raw bytes after mTLS termination, so h2 negotiation is wrong).
//   - The cluster's transport socket (injected at snapshot time by InjectUpstreamTCPMTLS)
//     uses ALPN "aether-tcp" instead of "h2", so the destination inbound's tls_inspector
//     routes to the TCP floor filter chain, not the HCM.
//   - subset_selectors and retry circuit-breakers are omitted — TCP proxy doesn't use
//     subset routing or the HTTP retry mechanism.
func NewTCPServiceCluster(name, edsServiceName, altStatName string, perDownstreamConnectionPool bool) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                                  name,
		AltStatName:                           altStatName,
		ConnectTimeout:                        durationpb.New(2 * time.Second),
		PerConnectionBufferLimitBytes:         wrapperspb.UInt32(perConnectionBufferLimitBytes),
		ConnectionPoolPerDownstreamConnection: perDownstreamConnectionPool,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig:   config.XDSConfigSourceADS(),
			ServiceName: edsServiceName,
		},
		// No TypedExtensionProtocolOptions: tcp_proxy does not speak h2 upstream.
		CommonLbConfig: &clusterv3.Cluster_CommonLbConfig{
			HealthyPanicThreshold: &typev3.Percent{Value: 0},
		},
		OutlierDetection: &clusterv3.OutlierDetection{
			SplitExternalLocalOriginErrors: true,
			ConsecutiveLocalOriginFailure:  wrapperspb.UInt32(3),
			BaseEjectionTime:               durationpb.New(30 * time.Second),
			MaxEjectionPercent:             wrapperspb.UInt32(50),
			Interval:                       durationpb.New(10 * time.Second),
		},
		CloseConnectionsOnHostHealthFailure: true,
		IgnoreHealthOnHostRemoval:           true,
	}
}

// NewUDPServiceCluster builds a plain EDS cluster for a UDPRoute's backend
// (proposal 018, Phase 3b). It is intentionally transport-socket-free — UDP
// datagrams are not wrapped in mTLS (mTLS is a TCP/TLS construct; DTLS is not
// implemented in this mesh). Traffic forwarded via this cluster is PLAINTEXT.
//
// SECURITY NOTE: mesh mTLS does NOT cover the UDP floor. All datagrams
// forwarded to backends via this cluster travel without encryption or mutual
// authentication. This is a known limitation; see proposal 018 Phase 3b.
//
// The cluster is STATIC with an inline load assignment rather than EDS: the UDP
// floor has no inbound mTLS hop, so udp_proxy must reach the backend's application
// UDP port directly — not the mesh inbound :18008 that the shared bare-name EDS
// carries. Callers pass a load assignment from UDPLoadAssignment (app-port endpoints).
func NewUDPServiceCluster(name, altStatName string, loadAssignment *endpointv3.ClusterLoadAssignment) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                          name,
		AltStatName:                   altStatName,
		ConnectTimeout:                durationpb.New(2 * time.Second),
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(perConnectionBufferLimitBytes),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_STATIC,
		},
		LoadAssignment: loadAssignment,
		// No TypedExtensionProtocolOptions: udp_proxy speaks raw UDP upstream.
		// No LbSubsetConfig: subset routing is not meaningful for UDP.
		// No TransportSocket: UDP is plaintext — see security note above.
		CommonLbConfig: &clusterv3.Cluster_CommonLbConfig{
			HealthyPanicThreshold: &typev3.Percent{Value: 0},
		},
		OutlierDetection: &clusterv3.OutlierDetection{
			SplitExternalLocalOriginErrors: true,
			ConsecutiveLocalOriginFailure:  wrapperspb.UInt32(3),
			BaseEjectionTime:               durationpb.New(30 * time.Second),
			MaxEjectionPercent:             wrapperspb.UInt32(50),
			Interval:                       durationpb.New(10 * time.Second),
		},
		IgnoreHealthOnHostRemoval: true,
	}
}

// UDPLoadAssignment derives an inline UDP load assignment from a service's existing
// (TCP-inbound, :18008) load assignment: it clones it and rewrites every endpoint to
// the backend's application UDP port and UDP protocol. The UDP floor has no inbound
// mTLS proxy, so udp_proxy reaches the application port directly. Returns nil if src
// is nil.
func UDPLoadAssignment(src *endpointv3.ClusterLoadAssignment, clusterName string, port uint32) *endpointv3.ClusterLoadAssignment {
	if src == nil {
		return nil
	}
	la, _ := proto.Clone(src).(*endpointv3.ClusterLoadAssignment)
	la.ClusterName = clusterName
	for _, lle := range la.GetEndpoints() {
		for _, lb := range lle.GetLbEndpoints() {
			sa := lb.GetEndpoint().GetAddress().GetSocketAddress()
			if sa == nil {
				continue
			}
			sa.Protocol = corev3.SocketAddress_UDP
			sa.PortSpecifier = &corev3.SocketAddress_PortValue{PortValue: port}
		}
	}
	return la
}

// InjectUpstreamTCPMTLS is InjectUpstreamMTLS for TCP floor clusters: it injects
// a per-source transport socket that uses ALPN "aether-tcp" (UpstreamTCPTransportSocket)
// instead of "h2", so the destination inbound filter-chain match on
// application_protocols:["aether-tcp"] routes to the TCP floor tcp_proxy chain.
func InjectUpstreamTCPMTLS(cluster *clusterv3.Cluster, netnsToSpiffeID map[string]string, spiffeIDs []string, nodeSpiffeID, validationContextName string, sanURIs []string, sni string) {
	matcher := UpstreamTransportSocketMatcher(netnsToSpiffeID)
	if matcher == nil {
		cluster.TransportSocket = UpstreamTCPTransportSocket(nodeSpiffeID, validationContextName, sanURIs, sni)
		return
	}
	matcher.OnNoMatch = transportSocketNameOnMatch(nodeSpiffeID)

	cluster.TransportSocketMatcher = matcher
	cluster.TransportSocketMatches = UpstreamTCPTransportSocketMatches(append(spiffeIDs, nodeSpiffeID), validationContextName, sanURIs, sni)
}

// InjectUpstreamMTLS sets the per-source mTLS transport socket on a service cluster:
// a transport-socket matcher keyed on the source pod's network namespace selects
// that pod's client certificate (on-no-match presents the node identity). The
// matches reference each local workload's SVID over SDS. This is applied at snapshot
// time because it depends on the current set of local workloads.
//
// With no local workload mappings the matcher is omitted entirely — an empty
// exact_match_map NACKs the whole CDS push — and the node identity is presented
// directly (what on_no_match would have done for every connection). No
// transport_socket_matches are set in that branch: without the matcher Envoy
// falls back to legacy metadata matching, where an empty match criteria set
// would select the first entry for every endpoint.
// sanURIs (the service's expected server SPIFFE IDs) pin the upstream peer
// identity on every emitted socket; empty disables pinning (bundle-only).
// InjectUpstreamMTLS wires the cluster's per-source upstream mTLS. sni is the
// local (intra-cluster) SNI — the destination port. When waypointSNI is
// non-empty (proposal 019 waypoint enabled), the cluster additionally carries a
// waypoint socket per source identity presenting waypointSNI (the structured
// <port>.<svc>.<ns>.<meshDomain>), selected by the two-level matcher for
// endpoints tagged waypoint=true; every other endpoint uses the local socket.
func InjectUpstreamMTLS(cluster *clusterv3.Cluster, netnsToSpiffeID map[string]string, spiffeIDs []string, nodeSpiffeID, validationContextName string, sanURIs []string, sni, waypointSNI string) {
	allIDs := append(spiffeIDs, nodeSpiffeID)

	if waypointSNI == "" {
		matcher := UpstreamTransportSocketMatcher(netnsToSpiffeID)
		if matcher == nil {
			cluster.TransportSocket = UpstreamTransportSocket(nodeSpiffeID, validationContextName, sanURIs, sni)
			return
		}
		matcher.OnNoMatch = transportSocketNameOnMatch(nodeSpiffeID)
		cluster.TransportSocketMatcher = matcher
		cluster.TransportSocketMatches = UpstreamTransportSocketMatches(allIDs, validationContextName, sanURIs, sni)
		return
	}

	matcher := WaypointTransportSocketMatcher(netnsToSpiffeID, nodeSpiffeID)
	if matcher == nil {
		// No local workloads: nothing originates traffic, so a single plain
		// socket suffices (mirrors the no-workload path above).
		cluster.TransportSocket = UpstreamTransportSocket(nodeSpiffeID, validationContextName, sanURIs, sni)
		return
	}
	cluster.TransportSocketMatcher = matcher
	local := upstreamTransportSocketMatchesNamed(allIDs, validationContextName, sanURIs, sni, identitySocketName)
	waypoint := upstreamTransportSocketMatchesNamed(allIDs, validationContextName, sanURIs, waypointSNI, waypointSocketName)
	cluster.TransportSocketMatches = append(local, waypoint...)
}

// remoteClusterPriorityBand is added to a remote-cluster endpoint's locality
// priority when the east/west waypoint is enabled, so the local cluster's
// endpoints (band 0-2) are always preferred and a remote cluster (band 3-5) is
// pure failover. Sparse priority levels are fine — Envoy treats missing ones as
// empty and spills to the next healthy band (same as the locality tiers).
const remoteClusterPriorityBand = 3

// WaypointRewrite controls the split-horizon EDS rewrite (proposal 019). When
// Enabled, an endpoint belonging to a cluster other than LocalCluster is dialed
// at its node's routable IP + TunnelPort (the per-node east/west waypoint)
// instead of its (cross-cluster, possibly non-routable) pod IP, and is tagged
// with waypoint metadata so the cluster's transport-socket matcher can present
// the structured SNI. When disabled (the default) every endpoint is dialed at
// pod_ip:18008 — the flat pod-to-pod path (proposal 018 flat-network mode),
// unchanged. LocalCluster is this agent's own cluster name.
type WaypointRewrite struct {
	Enabled      bool
	TunnelPort   uint32
	LocalCluster string
}

// remoteCluster reports whether the endpoint belongs to a cluster other than the
// local one while waypoint mode is on (independent of whether it has a routable
// node IP — used for priority banding, which deprioritizes every remote endpoint).
func (w WaypointRewrite) remoteCluster(endpoint *registryv1.ServiceEndpoint) bool {
	return w.Enabled && endpoint.GetClusterName() != w.LocalCluster
}

// viaWaypoint reports whether the endpoint must be dialed through its node's
// waypoint: waypoint mode is on, the endpoint is in another cluster, AND it
// advertises a routable node IP. A remote endpoint without a node IP falls back
// to its pod IP (best effort — only works if the pod network happens to route).
func (w WaypointRewrite) viaWaypoint(endpoint *registryv1.ServiceEndpoint) bool {
	return w.remoteCluster(endpoint) && endpoint.GetKubernetesMetadata().GetNodeIp() != ""
}

// EndpointPriority maps an endpoint's locality distance from this node into
// an EDS priority (locality-aware failover): same zone 0, same region 1,
// elsewhere or unknown 2. Envoy keeps traffic at the lowest healthy priority
// and spills proportionally as it degrades (overprovisioning factor 1.4
// default), so a zonal roll or outage drains to the region automatically —
// including DRAINING/UNHEALTHY EDS marks. An agent without locality labels
// expresses no preference (everything 0). This is deliberately NOT Envoy's
// zone_aware_lb_config, which needs the local proxy fleet's per-zone
// membership on every node — O(all nodes) state that demand-scoped
// distribution (004) exists to eliminate. With the waypoint enabled, a
// remote-cluster endpoint is shifted into a higher band so the local cluster
// is always preferred (same-cluster-first).
func EndpointPriority(localRegion, localZone string, endpoint *registryv1.ServiceEndpoint, wp WaypointRewrite) uint32 {
	base := localityPriority(localRegion, localZone, endpoint)
	if wp.remoteCluster(endpoint) {
		return base + remoteClusterPriorityBand
	}
	return base
}

func localityPriority(localRegion, localZone string, endpoint *registryv1.ServiceEndpoint) uint32 {
	if localRegion == "" {
		return 0
	}
	loc := endpoint.GetLocality()
	if loc.GetRegion() != localRegion {
		return 2
	}
	if localZone == "" || loc.GetZone() == localZone {
		return 0
	}
	return 1
}

// ServiceLocalityLbEndpointFromRegistryEndpoint builds an endpoint for the plain
// transport: its socket address is the destination pod's mesh inbound
// (<pod_ip>:defaultInboundPort), reached by the pod's own netns-bound inbound
// listener, and its host metadata carries the envoy.lb subset keys for affinity.
// The endpoint health reflects the registry's delegated-liveness status, and
// its priority the locality distance from this node (EndpointPriority).
func ServiceLocalityLbEndpointFromRegistryEndpoint(endpoint *registryv1.ServiceEndpoint, localRegion, localZone string, wp WaypointRewrite) *endpointv3.LocalityLbEndpoints {
	viaWaypoint := wp.viaWaypoint(endpoint)

	subsetKeys := map[string]string{
		subsetClusterKey: endpoint.GetClusterName(),
		subsetIPKey:      endpoint.GetIp(),
	}
	if endpoint.GetKubernetesMetadata() != nil {
		subsetKeys[subsetPodNamespaceKey] = endpoint.GetKubernetesMetadata().GetNamespace()
		subsetKeys[subsetPodNameKey] = endpoint.GetKubernetesMetadata().GetPodName()
	}
	for k, v := range endpoint.GetMetadata() {
		subsetKeys[k] = v
	}
	if viaWaypoint {
		// Tag the endpoint so the cluster's transport-socket matcher selects the
		// waypoint socket (structured SNI) for it (proposal 019 Phase 2b). Lives
		// in the envoy.lb metadata namespace the EndpointMetadataInput reads; it
		// is not a subset selector key, so it does not affect subset LB.
		subsetKeys[subsetWaypointKey] = subsetWaypointValue
	}

	lb := &structpb.Struct{Fields: map[string]*structpb.Value{}}
	for k, v := range subsetKeys {
		lb.Fields[k] = structpb.NewStringValue(v)
	}
	md := &corev3.Metadata{
		FilterMetadata: map[string]*structpb.Struct{
			envoyFilterMetadataSubsetNamespace: lb,
		},
	}

	var locality *corev3.Locality
	if loc := endpoint.GetLocality(); loc != nil && loc.GetRegion() != "" && loc.GetZone() != "" {
		locality = &corev3.Locality{Region: loc.GetRegion(), Zone: loc.GetZone()}
	}

	// Local: the destination pod's mesh inbound (pod_ip:18008), reached by the
	// pod's own netns-bound inbound listener. Remote (waypoint): the destination
	// node's routable IP + tunnel port, where that node's host-network proxy
	// SNI-forwards to the local pod — the source's mTLS passes through unchanged.
	dialAddr := endpoint.GetIp()
	dialPort := uint32(defaultInboundPort)
	if viaWaypoint {
		dialAddr = endpoint.GetKubernetesMetadata().GetNodeIp()
		dialPort = wp.TunnelPort
	}

	ep := &endpointv3.Endpoint{
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					Address:  dialAddr,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: dialPort,
					},
				},
			},
		},
	}
	// In EDS mode (set per pod by annotation) this endpoint opts out of the cluster's
	// active readiness health check and relies on the EDS health status pushed by the
	// node-local agent (delegated liveness) instead.
	if endpoint.GetHealthCheckMode() == registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS {
		ep.HealthCheckConfig = &endpointv3.Endpoint_HealthCheckConfig{
			DisableActiveHealthCheck: true,
		}
	}

	return &endpointv3.LocalityLbEndpoints{
		LbEndpoints: []*endpointv3.LbEndpoint{
			{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: ep},
				Metadata:       md,
				HealthStatus:   endpointHealthStatus(endpoint),
			},
		},
		Locality: locality,
		Priority: EndpointPriority(localRegion, localZone, endpoint, wp),
	}
}

// endpointHealthStatus maps the registry endpoint's delegated-liveness health to
// the Envoy EDS health status. Unknown/unspecified defaults to HEALTHY so older
// agents and freshly registered endpoints route until proven unhealthy.
func endpointHealthStatus(endpoint *registryv1.ServiceEndpoint) corev3.HealthStatus {
	switch endpoint.GetHealth() {
	case registryv1.ServiceEndpoint_HEALTH_UNHEALTHY:
		return corev3.HealthStatus_UNHEALTHY
	case registryv1.ServiceEndpoint_HEALTH_DRAINING:
		// Pod deletion requested: phase 1 of the two-phase drain. Envoy
		// DRAINING excludes the host from new selections but leaves
		// established connections untouched, so requests already in flight
		// complete (directive 2026-06-11: never hard-close active streams).
		//
		// History of the single-phase variants, both instrumented:
		//  - DRAINING alone left idle H2 pool connections open until the
		//    app's shutdown GOAWAY/RST, and streams claimed-but-unanswered at
		//    that instant failed non-retriably (~3-7 per pod delete).
		//  - UNHEALTHY + close_connections_on_host_health_failure (#143)
		//    closed pools at drain-mark (~t0+0.8s) but cut streams that were
		//    active at that moment (~6-7 per roll).
		// The two-phase drain composes them: DRAINING at deletion-requested
		// (in-flight finishes; no new claims after propagation ~1s), then the
		// termination watch re-registers UNHEALTHY after drainPoolCloseDelay,
		// closing the by-then-idle pools while the app is still alive — ahead
		// of the GOAWAY race. See handlePodTerminating.
		return corev3.HealthStatus_DRAINING
	default:
		return corev3.HealthStatus_HEALTHY
	}
}
