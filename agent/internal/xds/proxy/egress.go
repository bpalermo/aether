package proxy

import (
	"strings"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// ServiceClusterName returns the data-plane name of a service's outbound
// cluster: <service>.<meshDomain>. Authorities are FQDN-only, and the cluster
// name equals the authority so the warm path (service vhost) and the cold
// path (catch-all cluster_header: ":authority" + ODCDS) resolve the same
// resource deterministically. The bare service name remains the
// control-plane key (registry, watch filter, dependency set, EDS, stats).
func ServiceClusterName(serviceName, meshDomain string) string {
	return serviceName + "." + meshDomain
}

// ServiceFromClusterName maps a data-plane cluster name (a mesh authority,
// <service>.<meshDomain>) back to the bare service name. ok is false when the
// name is not under the mesh domain or the remainder is not a single DNS
// label (service names are ServiceAccount names — single lowercase labels),
// so nested or foreign authorities are rejected deterministically.
func ServiceFromClusterName(clusterName, meshDomain string) (string, bool) {
	suffix := "." + meshDomain
	if !strings.HasSuffix(clusterName, suffix) {
		return "", false
	}
	service := strings.TrimSuffix(clusterName, suffix)
	if service == "" || strings.Contains(service, ".") {
		return "", false
	}
	return service, true
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

func NewServiceCluster(serviceName, meshDomain string, subsetKeys []string) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name: ServiceClusterName(serviceName, meshDomain),
		// Stats stay keyed by the bare service name (cardinality rounds 1-2
		// shapes unchanged by the FQDN cluster naming).
		AltStatName:                           serviceName,
		ConnectTimeout:                        durationpb.New(2 * time.Second),
		PerConnectionBufferLimitBytes:         wrapperspb.UInt32(perConnectionBufferLimitBytes),
		ConnectionPoolPerDownstreamConnection: true,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
			// EDS resources stay keyed by the bare service name: the cache's
			// endpoint bookkeeping, the registrar watch, and the dependency
			// set all speak bare names; only the cluster (and the vhost that
			// references it) carries the FQDN.
			ServiceName: serviceName,
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
func InjectUpstreamMTLS(cluster *clusterv3.Cluster, netnsToSpiffeID map[string]string, spiffeIDs []string, nodeSpiffeID, validationContextName string, sanURIs []string) {
	matcher := UpstreamTransportSocketMatcher(netnsToSpiffeID)
	if matcher == nil {
		cluster.TransportSocket = UpstreamTransportSocket(nodeSpiffeID, validationContextName, sanURIs)
		return
	}
	matcher.OnNoMatch = transportSocketNameOnMatch(nodeSpiffeID)

	cluster.TransportSocketMatcher = matcher
	cluster.TransportSocketMatches = UpstreamTransportSocketMatches(append(spiffeIDs, nodeSpiffeID), validationContextName, sanURIs)
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
// distribution (004) exists to eliminate.
func EndpointPriority(localRegion, localZone string, endpoint *registryv1.ServiceEndpoint) uint32 {
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
func ServiceLocalityLbEndpointFromRegistryEndpoint(endpoint *registryv1.ServiceEndpoint, localRegion, localZone string) *endpointv3.LocalityLbEndpoints {
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

	ep := &endpointv3.Endpoint{
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_TCP,
					// The destination pod's mesh inbound; reached by the pod's own
					// netns-bound inbound listener.
					Address: endpoint.GetIp(),
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: defaultInboundPort,
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
		Priority: EndpointPriority(localRegion, localZone, endpoint),
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
