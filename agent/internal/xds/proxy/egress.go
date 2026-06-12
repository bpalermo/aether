package proxy

import (
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

// NewServiceCluster builds the outbound service cluster. Endpoints (delivered via
// EDS) are the destination pods' mesh inbound addresses (pod_ip:defaultInboundPort),
// each reached by the pod's own netns-bound inbound listener. The cluster speaks
// HTTP/2 mTLS to the destination pod, presenting the originating pod's identity. The
// per-source mTLS transport socket is injected at snapshot time (InjectUpstreamMTLS)
// from the current local workloads; connection_pool_per_downstream_connection keeps a
// source pod's connection (and therefore its certificate) from being reused for
// another source.
func NewServiceCluster(serviceName string) *clusterv3.Cluster {
	return &clusterv3.Cluster{
		Name:                                  serviceName,
		ConnectTimeout:                        durationpb.New(2 * time.Second),
		PerConnectionBufferLimitBytes:         wrapperspb.UInt32(perConnectionBufferLimitBytes),
		ConnectionPoolPerDownstreamConnection: true,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterv3.Cluster_EDS,
		},
		EdsClusterConfig: &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: config.XDSConfigSourceADS(),
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			config.UpstreamHTTPProtocolOptionsKey: config.TypedConfig(config.Http2ProtocolOptions()),
		},
		LbSubsetConfig: &clusterv3.Cluster_LbSubsetConfig{
			FallbackPolicy: clusterv3.Cluster_LbSubsetConfig_ANY_ENDPOINT,
			SubsetSelectors: []*clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector{
				{Keys: []string{subsetIPKey}},
			},
		},
		// Every client proxy actively health-checks each endpoint's mesh readiness
		// path over the cluster's mTLS (the HC connection has no source filter state,
		// so the transport-socket matcher's on-no-match presents the node identity).
		// A 200 means the destination pod is ready (config + mTLS up and the app
		// passes its readiness probe); a 503 or failure drops it from load balancing.
		HealthChecks: []*corev3.HealthCheck{
			{
				Timeout:            durationpb.New(2 * time.Second),
				Interval:           durationpb.New(5 * time.Second),
				HealthyThreshold:   wrapperspb.UInt32(1),
				UnhealthyThreshold: wrapperspb.UInt32(2),
				HealthChecker: &corev3.HealthCheck_HttpHealthCheck_{
					HttpHealthCheck: &corev3.HealthCheck_HttpHealthCheck{
						Path:            MeshReadyPath,
						CodecClientType: typev3.CodecClientType_HTTP2,
					},
				},
			},
		},
		// Don't route to a newly discovered endpoint until its first readiness check
		// passes, so traffic never lands on a not-yet-ready pod. Panic routing is
		// disabled: with draining endpoints now reported UNHEALTHY (see
		// endpointHealthStatus), a heavy simultaneous roll could cross Envoy's
		// default 50% panic threshold and spray traffic at known-unhealthy hosts;
		// deterministic exclusion + per-request retries + the registry's own
		// health pipeline are the mesh's answer, not panic spraying.
		CommonLbConfig: &clusterv3.Cluster_CommonLbConfig{
			IgnoreNewHostsUntilFirstHc: true,
			HealthyPanicThreshold:      &typev3.Percent{Value: 0},
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
		// With active health checks configured, Envoy by default keeps an
		// EDS-removed host in rotation until its health check fails — which would
		// defeat the early termination drain (the agent deregisters an endpoint
		// the moment its pod's deletion is requested, precisely so clients stop
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
func InjectUpstreamMTLS(cluster *clusterv3.Cluster, netnsToSpiffeID map[string]string, spiffeIDs []string, nodeSpiffeID, validationContextName string) {
	matcher := UpstreamTransportSocketMatcher(netnsToSpiffeID)
	if matcher == nil {
		cluster.TransportSocket = UpstreamTransportSocket(nodeSpiffeID, validationContextName)
		return
	}
	matcher.OnNoMatch = transportSocketNameOnMatch(nodeSpiffeID)

	cluster.TransportSocketMatcher = matcher
	cluster.TransportSocketMatches = UpstreamTransportSocketMatches(append(spiffeIDs, nodeSpiffeID), validationContextName)
}

// ServiceLocalityLbEndpointFromRegistryEndpoint builds an endpoint for the plain
// transport: its socket address is the destination pod's mesh inbound
// (<pod_ip>:defaultInboundPort), reached by the pod's own netns-bound inbound
// listener, and its host metadata carries the envoy.lb subset keys for affinity.
// The endpoint health reflects the registry's delegated-liveness status.
func ServiceLocalityLbEndpointFromRegistryEndpoint(endpoint *registryv1.ServiceEndpoint) *endpointv3.LocalityLbEndpoints {
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
