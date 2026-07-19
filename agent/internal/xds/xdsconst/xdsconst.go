// Package xdsconst holds the Aether pod annotation keys consumed exclusively by
// the agent's xDS proxy-config generation (proxy/cache/server), keeping them out
// of the cross-tree common/constants fan-in.
package xdsconst

const (
	// AnnotationConfigUpstreams is the pod annotation declaring the upstream
	// services the pod calls, as a comma-separated list of service names
	// (e.g. "svc-payments,svc-ledger"). The agent unions the annotations of
	// its local pods into the node dependency set and distributes
	// clusters/endpoints/routes only for that set (demand-scoped
	// distribution, proposal 004). Declared upstreams are warm before first
	// use; undeclared upstreams are loaded on demand (ODCDS) with one xDS
	// round-trip of first-request latency.
	AnnotationConfigUpstreams = "config.aether.io/upstreams"

	// AnnotationSpiffeID is the pod annotation key for specifying the workload's SPIFFE ID.
	// When set, this is used as the SDS secret name for the workload's TLS certificate.
	AnnotationSpiffeID = "aether.io/spiffe-id"

	// AnnotationEndpointHealthPath is the pod annotation key for the HTTP path the
	// agent active-health-checks on the pod's application (delegated liveness).
	AnnotationEndpointHealthPath = "endpoint.aether.io/health-path"
)
