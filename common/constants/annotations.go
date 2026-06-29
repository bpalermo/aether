// Package constants defines shared constants used throughout the Aether codebase.
// It includes Kubernetes labels, annotations, and configuration constants for the service mesh.
package constants

const (
	// Aether annotations
	annotationAetherPrefix         = "aether.io/"
	annotationAetherEndpointPrefix = "endpoint." + annotationAetherPrefix
	// config.aether.io/* annotations describe what the pod CONSUMES (mesh
	// client configuration), distinct from endpoint.aether.io/* which states
	// endpoint registration facts (what the pod serves).
	annotationAetherConfigPrefix = "config." + annotationAetherPrefix
	// capture.aether.io/* annotations control the pod's transparent-capture
	// behaviour (what the CNI intercepts for the pod).
	annotationAetherCapturePrefix = "capture." + annotationAetherPrefix

	// AnnotationCaptureRedirectAll controls per-pod redirect-all transparent-capture
	// (proposal 022): the CNI installs the broad nft rule that sends ALL outbound
	// non-local TCP into the pod-local capture listener, where non-mesh egress is
	// forwarded in plaintext via the Envoy passthrough_original_dst cluster. The
	// annotation overrides the node default both ways:
	//   - "true"  → force ON (opt-in; test redirect-all on one pod while the node
	//     default is off).
	//   - "false" → force OFF (opt-out; carve an infra/hostNetwork/prober pod out of
	//     the managed-pod default, agent.captureRedirectAllDefault).
	// Absent (or any other value) falls through to the node default. REQUIRES the
	// agent --capture-redirect-all flag so the capture listener carries the
	// passthrough fallback filter chain.
	AnnotationCaptureRedirectAll = annotationAetherCapturePrefix + "redirect-all"

	// AnnotationCaptureExcludeOutboundPorts carves specific outbound TCP
	// destination ports OUT of transparent capture (proposal 022, M2-default,
	// Istio parity with traffic.sidecar.istio.io/excludeOutboundPorts). The value
	// is a comma-separated list of ports (e.g. "5432,9000"); the CNI emits an
	// nft RETURN for each, ahead of the redirect rule, so connections to those
	// ports bypass the mesh entirely (a DB, a scrape target, an external
	// dependency). Applies to both the scoped and redirect-all capture paths.
	// Invalid/empty entries are ignored. Independent of the redirect mode — it is
	// the operator's escape hatch once redirect-all is the managed-pod default.
	AnnotationCaptureExcludeOutboundPorts = annotationAetherCapturePrefix + "exclude-outbound-ports"

	// AnnotationCaptureExcludeOutboundIPRanges carves outbound traffic to specific
	// destination IP ranges OUT of transparent capture (proposal 022, M2-default,
	// Istio parity with traffic.sidecar.istio.io/excludeOutboundIPRanges). The value
	// is a comma-separated list of IPv4 CIDRs (e.g. "10.0.0.0/8,192.168.1.5/32"; a
	// bare address is treated as /32); the CNI emits an nft RETURN matching the
	// destination range, ahead of the redirect rule, so connections to those ranges
	// bypass the mesh entirely (an external dependency, a metadata endpoint, a peer
	// CIDR). The match is protocol-agnostic (destination-based), so it carves out
	// both the TCP and UDP capture rules. Applies to both the scoped and redirect-all
	// capture paths. Invalid/empty/non-IPv4 entries are ignored. Independent of the
	// redirect mode — the operator's escape hatch once redirect-all is the
	// managed-pod default.
	AnnotationCaptureExcludeOutboundIPRanges = annotationAetherCapturePrefix + "exclude-outbound-ip-ranges"

	// AnnotationConfigUpstreams is the pod annotation declaring the upstream
	// services the pod calls, as a comma-separated list of service names
	// (e.g. "svc-payments,svc-ledger"). The agent unions the annotations of
	// its local pods into the node dependency set and distributes
	// clusters/endpoints/routes only for that set (demand-scoped
	// distribution, proposal 004). Declared upstreams are warm before first
	// use; undeclared upstreams are loaded on demand (ODCDS) with one xDS
	// round-trip of first-request latency.
	AnnotationConfigUpstreams = annotationAetherConfigPrefix + "upstreams"

	// AnnotationSpiffeID is the pod annotation key for specifying the workload's SPIFFE ID.
	// When set, this is used as the SDS secret name for the workload's TLS certificate.
	AnnotationSpiffeID = annotationAetherPrefix + "spiffe-id"

	// AnnotationEndpointPort is the pod annotation key for specifying the service port
	AnnotationEndpointPort = annotationAetherEndpointPrefix + "port"
	// AnnotationEndpointPorts is the pod annotation listing ALL application ports
	// the pod serves, comma-separated (e.g. "8080,9090"), for multi-port routing
	// (proposal 005). AnnotationEndpointPort remains the default/primary port;
	// when AnnotationEndpointPorts is unset the served set is just {port}.
	AnnotationEndpointPorts = annotationAetherEndpointPrefix + "ports"
	// AnnotationEndpointWeight is the pod annotation key for specifying endpoint weight in load balancing
	AnnotationEndpointWeight = annotationAetherEndpointPrefix + "weight"
	// AnnotationEndpointHealthPath is the pod annotation key for the HTTP path the
	// agent active-health-checks on the pod's application (delegated liveness).
	AnnotationEndpointHealthPath = annotationAetherEndpointPrefix + "health-path"

	// AnnotationEndpointHealthCheckMode is the pod annotation key selecting how
	// client proxies determine this endpoint's health. Unset or "eds" (default)
	// makes clients rely on the EDS health status pushed by the node-local agent
	// (delegated liveness): the endpoint registers UNHEALTHY, is promoted once
	// the local proxy sees the app pass its health check, and enters every
	// client pre-warmed (no per-client first-HC round). "active" opts the pod
	// back into per-client active health checking of its mesh readiness path.
	AnnotationEndpointHealthCheckMode = annotationAetherEndpointPrefix + "health-check-mode"
	// HealthCheckModeActive / HealthCheckModeEDS are the accepted annotation values.
	HealthCheckModeActive = "active"
	HealthCheckModeEDS    = "eds"

	// AnnotationEndpointProtocol is the pod annotation selecting the mesh
	// service's wire protocol (a registration fact: what the pod serves). Unset
	// or "http" (default) registers the workload as a PROTOCOL_HTTP service
	// reached via the HCM path; "tcp" registers it as a PROTOCOL_TCP service
	// reached as a raw mTLS passthrough through the transparent-capture TCP
	// floor (proposal 018, Phase 3a). Distinct from aether.io/app-protocol
	// (AnnotationMeshAppProtocol), which the registrar STAMPS on the generated
	// mesh Service for the agent's capture reconciler to read.
	AnnotationEndpointProtocol = annotationAetherEndpointPrefix + "protocol"
	// ProtocolHTTP / ProtocolTCP are the accepted AnnotationEndpointProtocol
	// values.
	ProtocolHTTP = "http"
	ProtocolTCP  = "tcp"

	// AnnotationAetherEndpointMetadataPrefix is the prefix for endpoint metadata annotations
	AnnotationAetherEndpointMetadataPrefix = "metadata." + annotationAetherEndpointPrefix

	// AnnotationGatewayHTTPRedirect is a Gateway annotation that opts a plain-HTTP
	// listener into HTTP→HTTPS redirect behaviour. When set to "true" on a Gateway
	// of the aether GatewayClass, the edge emits a 301-redirect listener on the
	// Gateway's HTTP port instead of serving routes directly.
	//
	// Default absent / "false" → the HTTP listener serves its attached HTTPRoutes
	// (no redirect). Operators MUST set this annotation on any Gateway whose HTTP
	// listener should redirect to HTTPS (e.g. the production api.palermo.dev edge
	// Gateway). The aether chart sets it automatically when edge.tls.enabled is true.
	AnnotationGatewayHTTPRedirect = "gateway.aether.io/http-redirect"

	// Kubernetes topology annotations
	// AnnotationKubernetesNodeTopologyRegion is the Kubernetes node label for the region
	AnnotationKubernetesNodeTopologyRegion = "topology.kubernetes.io/region"
	// AnnotationKubernetesNodeTopologyZone is the Kubernetes node label for the zone
	AnnotationKubernetesNodeTopologyZone = "topology.kubernetes.io/zone"
)
