package cache

import (
	"context"
	"slices"
	"strconv"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"google.golang.org/protobuf/types/known/durationpb"
)

// routeSpecificity returns a composite sort key for a Route per Gateway API
// precedence rules. The dominant dimension is path type/length; secondary
// dimensions (header count, method presence, query count) break ties within the
// same path key. A higher return value means higher precedence.
//
// Sort key layout (descending importance):
//
//	bits[60..32] — path key: Exact=1<<30+1, Prefix=len(prefix) (max ~2^30)
//	bits[31..16] — header count (capped at 0xffff, more = more specific)
//	bits[15]     — method present (1 > 0)
//	bits[14..0]  — query param count (capped at 0x7fff)
//
// Using a packed int64 ensures a single integer comparison covers all dimensions
// in priority order.
func routeSpecificity(r Route) int64 {
	var pathKey int64
	if r.Exact != "" {
		pathKey = int64(1)<<30 + 1
	} else {
		p := r.Prefix
		if p == "" {
			p = "/"
		}
		pathKey = int64(len(p))
	}

	hdrCount := int64(len(r.Headers))
	if hdrCount > 0xffff {
		hdrCount = 0xffff
	}

	methodBit := int64(0)
	if r.Method != "" {
		methodBit = 1
	}

	qpCount := int64(len(r.QueryParams))
	if qpCount > 0x7fff {
		qpCount = 0x7fff
	}

	return (pathKey << 32) | (hdrCount << 16) | (methodBit << 15) | qpCount
}

// sortRoutesBySpecificity stable-sorts a Route slice in-place by Gateway API
// precedence (most-specific first):
//
//  1. Path: Exact > longer PathPrefix > shorter PathPrefix
//  2. Header count: more header matches > fewer
//  3. Method: method present > absent
//  4. Query params: more query matches > fewer
//
// It is stable so the caller's tie-break order (creationTimestamp / namespace /
// name / rule index, established by the reconciler's sort of HTTPRoute objects)
// is preserved for routes with equal specificity on all dimensions.
func sortRoutesBySpecificity(routes []Route) {
	slices.SortStableFunc(routes, func(a, b Route) int {
		sa, sb := routeSpecificity(a), routeSpecificity(b)
		if sa != sb {
			// Descending: higher specificity comes first.
			if sa > sb {
				return -1
			}
			return 1
		}
		return 0
	})
}

// EdgeGatewayListenerEntry describes one listener port within a per-Gateway entry.
// ExternalPort is the Gateway listener's declared port (e.g. 80 or 443).
// InternalPort is the container port allocated by the port allocator that the
// per-Gateway LoadBalancer Service maps ExternalPort → InternalPort.
type EdgeGatewayListenerEntry struct {
	// ExternalPort is the Gateway listener's declared external port (e.g. 80, 443).
	ExternalPort uint32
	// InternalPort is the unique container port allocated for this (Gateway, listener).
	InternalPort uint32
	// TLSSecretNames are the SDS cert names for downstream TLS (empty = plain HTTP).
	TLSSecretNames []string
	// HTTPRedirect when true means this listener emits an HTTP→HTTPS 301 redirect
	// instead of serving routes directly.
	HTTPRedirect bool
}

// EdgeGatewayEntry is the cache's per-Gateway routing data for proposal 021 Phase 2.
// Each entry maps to one per-Gateway LoadBalancer Service and a set of per-listener
// Envoy listeners bound on InternalPorts.
type EdgeGatewayEntry struct {
	// Namespace is the Gateway's Kubernetes namespace.
	Namespace string
	// Name is the Gateway's name.
	Name string
	// Listeners is the per-listener port allocations and TLS config.
	Listeners []EdgeGatewayListenerEntry
	// VirtualHosts are the HTTPRoute virtual hosts attached to this Gateway.
	VirtualHosts []VirtualHost
}

// VirtualHost is the cache's projection of an HTTPRoute (and the legacy
// VirtualHost CR): a set of external hosts carrying an ordered list of
// path-matched routes to backend services, optionally served under a downstream
// cert. Empty Routes makes it inert. Empty Hosts is NOT inert — per Gateway API a
// hostname-less route matches every host on its listener, so it is served via the
// edge's catch-all "*" vhost (see buildEdgeVhostsLocked).
type VirtualHost struct {
	Hosts  []string
	Routes []Route
	// TLSSecret is the provider-prefixed SDS name of the cert presented for this
	// virtual host's hosts (empty = no cert). Cert bytes are supplied separately
	// via SetEdgeTLSSecrets.
	TLSSecret string
	// Gateways are the "<ns>/<name>" keys of the Gateways this route attaches to
	// (its parentRefs). Under proposal 021 Phase 2 it scopes which per-Gateway route
	// tables get this vhost — assignment is by ATTACHMENT, not by cert. Empty means
	// unscoped (Phase 1 shared listener, or attach-to-all fallback).
	Gateways []string
}

// RouteBackend is one weighted backend within an HTTPRoute rule. It corresponds
// to one element of backendRefs with its resolved service name, namespace, port,
// and weight. Weight 0 means the backend receives no traffic but is still a
// valid entry (per Gateway API — a zero-weight backend is not an error).
type RouteBackend struct {
	Service          string
	BackendNamespace string
	Port             uint32
	// Weight is the Gateway API backendRef weight. Defaults to 1 when unset.
	// The Envoy weighted_clusters total_weight is the sum of all backend weights
	// within the rule. Weight 0 = backend receives no traffic.
	Weight uint32
	// DialPort is the TCP port a non-mesh (cleartext STRICT_DNS) cluster actually
	// connects to. It differs from Port for a HEADLESS Service: the Service FQDN
	// resolves directly to pod IPs (no kube-proxy in the path to remap the port), so
	// the edge must dial the Service's numeric targetPort, not its service port.
	// For a ClusterIP Service the FQDN resolves to the ClusterIP and kube-proxy
	// DNATs port->targetPort, so DialPort == Port. Zero means "same as Port" (the
	// reconciler leaves it 0 when no Service-type-specific remap is needed); the
	// cluster builder falls back to Port. The cluster NAME stays keyed by Port so
	// route->cluster names remain stable regardless of the resolved dial port.
	DialPort uint32
}

// Route is one path-match -> backend rule within a VirtualHost. Exactly one of
// Prefix/Exact is set.
// HeaderMutation carries the merged request/response header modifier filters for
// this rule (nil = no header mutations).
// When Redirect is non-nil the route returns a redirect response (no backend).
// When URLRewrite is non-nil the route rewrites the request URL before forwarding.
//
// Backends holds the weighted backend list for this rule. When populated,
// BuildEdgeRoute emits a weighted_clusters action (multiple backends) or a
// plain single-cluster action (one backend). Backends supersedes the legacy
// Service/Port/BackendNamespace fields; all three are populated for backward
// compatibility when there is exactly one backend so existing single-backend
// code paths continue to work.
//
// BackendNamespace is the namespace of the (first) backendRef (defaults to the
// route's own namespace when not set by the reconciler). Used to build the k8s-
// Service FQDN for non-mesh (cleartext) backends when Backends is not set.
//
// Headers, Method, QueryParams carry the additional match predicates from
// Gateway API HTTPRouteMatch: within one Match all predicates are ANDed.
// Headers maps to Envoy RouteMatch.headers; Method maps to a :method header
// matcher; QueryParams maps to RouteMatch.query_parameters.
//
// DirectResponseStatus, when non-zero, causes the route to emit a fixed
// direct_response with that HTTP status code (no backend). Used to implement
// Gateway API semantics for rules whose all backends are unresolvable
// (ResolvedRefs=False): the data plane must return 500.
type Route struct {
	Prefix           string
	Exact            string
	Service          string
	Port             uint32
	BackendNamespace string
	// DialPort is the cleartext STRICT_DNS dial port for the legacy single-backend
	// path (mirrors RouteBackend.DialPort). Zero = dial Port. See RouteBackend.DialPort.
	DialPort uint32
	// Backends is the weighted backend list for this rule. A single-element list
	// is equivalent to the legacy Service/Port/BackendNamespace fields. An empty
	// Backends with a non-empty Service falls back to the legacy single-backend
	// path so old callers remain compatible.
	Backends       []RouteBackend
	HeaderMutation *proxy.GammaHeaderMutation
	Redirect       *proxy.GammaRedirect
	URLRewrite     *proxy.GammaURLRewrite

	// Headers are the per-header exact/regex predicates for this match.
	// All entries must match (AND semantics within one HTTPRouteMatch).
	Headers []proxy.RouteHeaderMatch
	// Method is the HTTP method that must match (e.g. "GET"). Empty = any method.
	Method string
	// QueryParams are the per-parameter exact/regex predicates for this match.
	QueryParams []proxy.RouteQueryParamMatch

	// DirectResponseStatus, when non-zero, causes the route to emit a fixed
	// HTTP direct_response with this status (no backend routing). Set to 500
	// for rules whose backendRef(s) cannot be resolved (BackendNotFound /
	// InvalidKind / RefNotPermitted per Gateway API).
	DirectResponseStatus uint32

	// Timeout is the per-route request timeout from the HTTPRoute rule's
	// timeouts.request field (GEP-2257 duration). Nil means no timeout is applied
	// on this route (Envoy inherits the listener/cluster default). When set it is
	// applied as RouteAction.timeout on the forwarding route.
	Timeout *durationpb.Duration
}

// EdgeTLSCert is raw certificate material for an edge downstream TLS secret,
// keyed by its provider-prefixed SDS name.
type EdgeTLSCert struct {
	Cert []byte
	Key  []byte
}

// SetEdgeTLSSecrets replaces the edge's downstream TLS certs (keyed by SDS name)
// and serves them over the ADS SecretType channel for Envoy to select by SNI.
func (c *SnapshotCache) SetEdgeTLSSecrets(ctx context.Context, certs map[string]EdgeTLSCert) error {
	secrets := make([]*tlsv3.Secret, 0, len(certs))
	for name, cert := range certs {
		secrets = append(secrets, proxy.NewDownstreamTLSSecret(name, cert.Cert, cert.Key))
	}
	return c.SetSecrets(ctx, secrets)
}

// edgeTLSSecretNames returns the distinct SDS cert names referenced by the
// current virtual hosts, sorted for deterministic listener bytes.
func (c *SnapshotCache) edgeTLSSecretNames() []string {
	c.edgeMu.RLock()
	defer c.edgeMu.RUnlock()
	seen := make(map[string]struct{}, len(c.virtualHosts))
	names := make([]string, 0, len(c.virtualHosts))
	for _, v := range c.virtualHosts {
		if v.TLSSecret == "" {
			continue
		}
		if _, ok := seen[v.TLSSecret]; ok {
			continue
		}
		seen[v.TLSSecret] = struct{}{}
		names = append(names, v.TLSSecret)
	}
	slices.Sort(names)
	return names
}

// SetEdgeGateways replaces the per-Gateway routing set (proposal 021 Phase 2). When
// non-empty the snapshot generates per-Gateway listeners (bound on InternalPorts) and
// per-Gateway route tables instead of the shared edge_http/edge_https listeners.
// When empty, the cache falls back to Phase 1 (shared-listener) behavior.
// Thread-safe; triggers a dependency rebuild and snapshot push on change.
func (c *SnapshotCache) SetEdgeGateways(gateways []EdgeGatewayEntry) {
	c.edgeMu.Lock()
	changed := !equalEdgeGateways(c.edgeGateways, gateways)
	c.edgeGateways = gateways
	c.edgeMu.Unlock()

	c.rebuildEdgeDependencies()
	if changed {
		c.signalDependencyChange()
	}
}

// edgeGatewayListeners generates per-Gateway Envoy listeners from edgeGateways.
// Each Gateway gets one listener per listener entry, bound on InternalPort.
// All names are UNIQUE (edge_gw_<ns>_<gwname>_<internalPort>) — no two listeners
// can share a name regardless of how many Gateways are configured (regression guard
// for #332 where shared name "edge_http" dropped the :443 listener).
func (c *SnapshotCache) edgeGatewayListeners() []types.Resource {
	c.edgeMu.RLock()
	gws := slices.Clone(c.edgeGateways)
	c.edgeMu.RUnlock()

	var out []types.Resource
	for _, gw := range gws {
		for _, ln := range gw.Listeners {
			if len(ln.TLSSecretNames) > 0 {
				out = append(out, proxy.BuildEdgeGatewayHTTPSListener(gw.Namespace, gw.Name, ln.InternalPort, ln.TLSSecretNames))
			} else {
				out = append(out, proxy.BuildEdgeGatewayHTTPListener(gw.Namespace, gw.Name, ln.InternalPort, ln.HTTPRedirect))
			}
		}
	}
	return out
}

// edgeGatewayRouteConfigs generates per-Gateway RDS route configurations from
// edgeGateways. Each Gateway gets one route config with its attached VirtualHosts
// plus a catch-all 404. The route config name (edge_rt_<ns>_<gwname>) is unique
// per Gateway and matches the RDS reference in edgeGatewayListeners.
func (c *SnapshotCache) edgeGatewayRouteConfigs() []types.Resource {
	c.edgeMu.RLock()
	gws := slices.Clone(c.edgeGateways)
	c.edgeMu.RUnlock()

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	var out []types.Resource
	for _, gw := range gws {
		vhosts := c.gatewayVhostsLocked(gw.VirtualHosts)
		rc := proxy.BuildEdgeGatewayRouteConfiguration(gw.Namespace, gw.Name, vhosts)
		out = append(out, rc)
	}
	return out
}

// gatewayVhostsLocked builds Envoy virtual hosts for one Gateway's VirtualHost
// slice. It is identical to virtualHostVhosts but scoped to one Gateway's vhosts.
// Caller must hold clusterMu.
func (c *SnapshotCache) gatewayVhostsLocked(vhosts []VirtualHost) []*routev3.VirtualHost {
	return c.buildEdgeVhostsLocked(vhosts)
}

// buildEdgeVhostsLocked converts cache VirtualHosts into Envoy virtual hosts.
//
// Grouping: routes from all VirtualHosts that share a hostname are MERGED into
// one Envoy virtual host for that hostname (Gateway API allows multiple HTTPRoutes
// to share a hostname on one Gateway; the data plane merges them). Routes are
// ordered by Gateway API path-specificity (Exact > longer prefix > shorter prefix)
// and the reconciler's input order (creationTimestamp/namespace/name) serves as
// the stable tie-break. There can be only one Envoy vhost per hostname (Envoy
// NACKs duplicate domains) — merge, never keep-first-drop.
//
// Hostname-less vhosts (Hosts == nil/empty) represent routes whose effective
// hostname set spans all of the listener's hostnames; they are merged into the
// single catch-all "*" vhost (there can be only one "*" per route table).
//
// Caller must hold clusterMu.
func (c *SnapshotCache) buildEdgeVhostsLocked(vhosts []VirtualHost) []*routev3.VirtualHost {
	// domain → []Route accumulator; ordered first-seen for domain name ordering.
	domainRoutes := make(map[string][]Route)
	var domainOrder []string // first-seen order for deterministic output
	var catchAllRoutes []Route

	for _, v := range vhosts {
		// Collect admissible routes (has a service, redirect, or direct-response status).
		admitted := make([]Route, 0, len(v.Routes))
		for _, r := range v.Routes {
			if r.Redirect == nil && r.Service == "" && r.DirectResponseStatus == 0 {
				continue
			}
			admitted = append(admitted, r)
		}
		if len(admitted) == 0 {
			continue
		}

		// Determine the set of domains for this vhost. Empty Hosts = catch-all.
		var domains []string
		for _, h := range v.Hosts {
			if h != "" {
				domains = append(domains, h)
			}
		}
		if len(domains) == 0 {
			// Hostname-less route: accumulate into the single "*" catch-all vhost.
			catchAllRoutes = append(catchAllRoutes, admitted...)
			continue
		}

		// Group by domain: each domain in this vhost gets all admitted routes.
		// Multiple VirtualHosts sharing a domain are merged here.
		for _, h := range domains {
			if _, seen := domainRoutes[h]; !seen {
				domainOrder = append(domainOrder, h)
			}
			domainRoutes[h] = append(domainRoutes[h], admitted...)
		}
	}

	// buildEnvoyRoute converts a cache Route to an Envoy route, resolving each
	// backend's cluster name via edgeClusterNameLocked (mesh vs k8s cleartext).
	// Caller must hold clusterMu.
	buildEnvoyRoute := func(r Route) *routev3.Route {
		// DirectResponseStatus: emit a fixed direct_response (no backend).
		// Path/header/method/query matchers still apply so the route only matches
		// when the request would have gone to the (unresolvable) backend.
		if r.DirectResponseStatus != 0 {
			return proxy.BuildEdgeDirectResponseRoute(r.Prefix, r.Exact, r.Headers, r.Method, r.QueryParams, r.DirectResponseStatus)
		}
		// Weighted multi-backend path: when Backends is populated use it.
		if len(r.Backends) > 0 {
			weighted := make([]proxy.WeightedRouteBackend, 0, len(r.Backends))
			for _, b := range r.Backends {
				cn := c.edgeClusterNameLocked(b.Service, b.BackendNamespace, b.Port)
				weighted = append(weighted, proxy.WeightedRouteBackend{Cluster: cn, Weight: b.Weight})
			}
			return proxy.BuildEdgeRouteWeighted(r.Prefix, r.Exact, r.Headers, r.Method, r.QueryParams, weighted, r.HeaderMutation, r.Redirect, r.URLRewrite, r.Timeout)
		}
		// Legacy single-backend path (backward compat).
		cluster := ""
		if r.Service != "" {
			cluster = c.edgeClusterNameLocked(r.Service, r.BackendNamespace, r.Port)
		}
		return proxy.BuildEdgeRoute(r.Prefix, r.Exact, r.Headers, r.Method, r.QueryParams, cluster, r.HeaderMutation, r.Redirect, r.URLRewrite, r.Timeout)
	}

	// Emit one Envoy virtual host per domain group, sorted by path specificity.
	out := make([]*routev3.VirtualHost, 0, len(domainOrder))
	for _, domain := range domainOrder {
		merged := domainRoutes[domain]
		sortRoutesBySpecificity(merged)
		routes := make([]*routev3.Route, 0, len(merged))
		for _, r := range merged {
			routes = append(routes, buildEnvoyRoute(r))
		}
		out = append(out, proxy.BuildEdgeVirtualHost(domain, []string{domain}, routes))
	}

	if len(catchAllRoutes) > 0 {
		// Sort the merged catch-all routes by path specificity. Ties preserve the
		// input order (creationTimestamp/namespace/name tie-break from the reconciler).
		sortRoutesBySpecificity(catchAllRoutes)
		catchAll := make([]*routev3.Route, 0, len(catchAllRoutes))
		for _, r := range catchAllRoutes {
			catchAll = append(catchAll, buildEnvoyRoute(r))
		}
		out = append(out, proxy.BuildEdgeVirtualHost("*", []string{"*"}, catchAll))
	}
	return out
}

// hasPerGatewayAddressing reports whether per-Gateway addressing (Phase 2) is
// active. Phase 2 is active when there is at least one EdgeGatewayEntry with at
// least one listener configured.
func (c *SnapshotCache) hasPerGatewayAddressing() bool {
	c.edgeMu.RLock()
	defer c.edgeMu.RUnlock()
	for _, gw := range c.edgeGateways {
		if len(gw.Listeners) > 0 {
			return true
		}
	}
	return false
}

// SetVirtualHosts replaces the edge's exposed virtual hosts. It scopes the
// dependency set to the union of every route's backend service (plus L4 route
// backends) and rebuilds the snapshot's route table — including when only the
// hostnames or matches changed (the service set, and therefore the dependency set,
// didn't).
func (c *SnapshotCache) SetVirtualHosts(vhosts []VirtualHost) {
	c.edgeMu.Lock()
	changed := !equalVirtualHosts(c.virtualHosts, vhosts)
	c.virtualHosts = vhosts
	c.edgeMu.Unlock()

	// Rebuild the full static dependency set (HTTP vhosts + any L4 routes).
	c.rebuildEdgeDependencies()

	// A host/match-only change leaves the dependency set untouched, so signal a
	// rebuild directly; the send is coalesced, so the rebuildEdgeDependencies
	// signal (if any) is not duplicated.
	if changed {
		c.signalDependencyChange()
	}
}

// virtualHostVhosts builds the edge listener's Envoy virtual hosts from the
// configured VirtualHosts (see buildEdgeVhostsLocked): a host already claimed by an
// earlier vhost is dropped (keep-first; Envoy NACKs duplicate domains) — the
// runtime backstop to the controller webhook's duplicate-FQDN rejection — and
// hostname-less routes are merged into a single catch-all "*" vhost.
func (c *SnapshotCache) virtualHostVhosts() []*routev3.VirtualHost {
	c.edgeMu.RLock()
	vhosts := slices.Clone(c.virtualHosts)
	c.edgeMu.RUnlock()

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	return c.buildEdgeVhostsLocked(vhosts)
}

// edgeClusterNameLocked resolves a (service, backendNamespace, port) backend to
// the data-plane cluster it targets.
//
// Dual-mode decision: if the service IS in the registry (c.clusters), this is a
// mesh-registered backend — use the existing mTLS mesh cluster path (unchanged).
// If NOT in the registry, the backend is a plain k8s Service: return the cleartext
// STRICT_DNS cluster name (EdgeK8sClusterName) so the edge dials the k8s Service
// FQDN over cleartext instead of attempting a mesh mTLS connection to :15008.
//
// Mesh path: Port 0 -> the default cluster (ServiceClusterName). A non-zero port ->
// its per-port cluster if one exists; otherwise, if the port is the service's
// default port (no dedicated per-port cluster), the default cluster serves it.
//
// Caller must hold clusterMu.
func (c *SnapshotCache) edgeClusterNameLocked(service, backendNamespace string, port uint32) string {
	// Check whether the service is mesh-registered (in the registry cluster map).
	// The registry keys services by bare name (and by per-port name), so check both
	// the bare name and the per-port name.
	meshFQDN := proxy.ServiceClusterName(service, c.meshDomain)
	if _, ok := c.clusters[service]; ok {
		// Mesh path: service is in the registry.
		if port == 0 {
			return meshFQDN
		}
		portName := proxy.PortClusterName(service, c.meshDomain, port)
		if _, ok := c.clusters[portName]; ok {
			return portName
		}
		if entry, ok := c.clusters[service]; ok && entry.sni == strconv.Itoa(int(port)) {
			return meshFQDN
		}
		return portName
	}
	// Also check by mesh FQDN and per-port mesh name (some callers may have added
	// clusters under those keys directly).
	if _, ok := c.clusters[meshFQDN]; ok {
		if port == 0 {
			return meshFQDN
		}
		portName := proxy.PortClusterName(service, c.meshDomain, port)
		if _, ok := c.clusters[portName]; ok {
			return portName
		}
		return portName
	}
	if port != 0 {
		portName := proxy.PortClusterName(service, c.meshDomain, port)
		if _, ok := c.clusters[portName]; ok {
			return portName
		}
	}
	// Non-mesh path: service is not in the registry. Route to the k8s Service FQDN
	// over cleartext (STRICT_DNS, no transport socket). Port defaults to 80 if unset.
	p := port
	if p == 0 {
		p = 80
	}
	return proxy.EdgeK8sClusterName(backendNamespace, service, p)
}

// SetEdgeTCPRoutes replaces the edge's TCP listener routes (one per Gateway TCP
// listener port). On change it updates the dependency set and signals a rebuild
// so the new tcp_proxy listeners appear in the snapshot.
func (c *SnapshotCache) SetEdgeTCPRoutes(routes []proxy.EdgeL4TCPRoute) {
	c.edgeMu.Lock()
	changed := !equalEdgeTCPRoutes(c.edgeTCPRoutes, routes)
	c.edgeTCPRoutes = routes
	c.edgeMu.Unlock()

	if !changed {
		return
	}

	// Extend the static dependency set with the backends' services so their EDS
	// clusters generate. We re-derive the full set here (HTTP + L4) rather than
	// patching to keep the logic idempotent.
	c.rebuildEdgeDependencies()
	c.signalDependencyChange()
}

// SetEdgeTLSRoutes replaces the edge's TLS passthrough listener routes (one per
// Gateway TLS listener port). On change it updates the dependency set and signals
// a rebuild.
func (c *SnapshotCache) SetEdgeTLSRoutes(routes []proxy.EdgeL4TLSRoute) {
	c.edgeMu.Lock()
	changed := !equalEdgeTLSRoutes(c.edgeTLSRoutes, routes)
	c.edgeTLSRoutes = routes
	c.edgeMu.Unlock()

	if !changed {
		return
	}

	c.rebuildEdgeDependencies()
	c.signalDependencyChange()
}

// rebuildEdgeDependencies recomputes the static dependency set from the union of
// all edge route backends (HTTP virtual-hosts + per-Gateway virtual-hosts + TCP
// routes + TLS routes) and calls SetStaticDependencies. Called whenever any edge
// route set changes.
func (c *SnapshotCache) rebuildEdgeDependencies() {
	c.edgeMu.RLock()
	vhosts := slices.Clone(c.virtualHosts)
	gws := slices.Clone(c.edgeGateways)
	tcpRoutes := append([]proxy.EdgeL4TCPRoute(nil), c.edgeTCPRoutes...)
	tlsRoutes := append([]proxy.EdgeL4TLSRoute(nil), c.edgeTLSRoutes...)
	c.edgeMu.RUnlock()

	seen := make(map[string]struct{})
	var services []string

	addVhost := func(v VirtualHost) {
		// A hostname-less route is NOT inert: per Gateway API it matches every host
		// on its listener (the edge serves it via the catch-all "*" vhost), so its
		// backend services must be scoped in (their clusters loaded) too.
		for _, r := range v.Routes {
			// Collect from Backends (weighted multi-backend).
			for _, b := range r.Backends {
				if b.Service == "" {
					continue
				}
				if _, ok := seen[b.Service]; !ok {
					seen[b.Service] = struct{}{}
					services = append(services, b.Service)
				}
			}
			// Legacy single-backend field (also populated for single-backend routes).
			if r.Service == "" {
				continue
			}
			if _, ok := seen[r.Service]; !ok {
				seen[r.Service] = struct{}{}
				services = append(services, r.Service)
			}
		}
	}

	for _, v := range vhosts {
		addVhost(v)
	}
	// Per-Gateway vhosts (Phase 2).
	for _, gw := range gws {
		for _, v := range gw.VirtualHosts {
			addVhost(v)
		}
	}
	for _, r := range tcpRoutes {
		for _, b := range r.Backends {
			if b.Service == "" {
				continue
			}
			if _, ok := seen[b.Service]; !ok {
				seen[b.Service] = struct{}{}
				services = append(services, b.Service)
			}
		}
	}
	for _, r := range tlsRoutes {
		for _, rule := range r.Rules {
			for _, b := range rule.Backends {
				if b.Service == "" {
					continue
				}
				if _, ok := seen[b.Service]; !ok {
					seen[b.Service] = struct{}{}
					services = append(services, b.Service)
				}
			}
		}
	}
	c.SetStaticDependencies(services)
}

// edgeTCPListeners returns the edge's L4 listeners (TCP + TLS passthrough) derived
// from the current EdgeL4TCPRoute and EdgeL4TLSRoute sets. Called from Listeners()
// in edge mode.
func (c *SnapshotCache) edgeTCPListeners() []types.Resource {
	c.edgeMu.RLock()
	tcpRoutes := append([]proxy.EdgeL4TCPRoute(nil), c.edgeTCPRoutes...)
	tlsRoutes := append([]proxy.EdgeL4TLSRoute(nil), c.edgeTLSRoutes...)
	c.edgeMu.RUnlock()

	var out []types.Resource
	for _, r := range tcpRoutes {
		if ln := proxy.BuildEdgeTCPListener(r.Port, r.Backends); ln != nil {
			out = append(out, ln)
		}
	}
	for _, r := range tlsRoutes {
		if ln := proxy.BuildEdgeTLSPassthroughListener(r.Port, r.Rules); ln != nil {
			out = append(out, ln)
		}
	}
	return out
}

// edgeK8sBackendClusters returns cleartext STRICT_DNS clusters for every
// non-mesh backend referenced by the edge's HTTP routes (Phase 1 virtual hosts
// and Phase 2 per-Gateway virtual hosts). A backend is non-mesh when its service
// name is NOT present in the registry cluster map (c.clusters). One cluster is
// emitted per unique (BackendNamespace, service, port) triple, using the k8s
// Service FQDN <service>.<namespace>.svc.cluster.local.
//
// These clusters have NO transport socket (cleartext). They are appended to the
// CDS snapshot alongside the mTLS mesh clusters so Envoy can reach plain k8s
// Service backends that are not registered in the mesh.
//
// Caller need not hold any lock — all maps are read under their own locks.
func (c *SnapshotCache) edgeK8sBackendClusters() []types.Resource {
	c.edgeMu.RLock()
	vhosts := slices.Clone(c.virtualHosts)
	gws := slices.Clone(c.edgeGateways)
	c.edgeMu.RUnlock()

	// key: "namespace/service:port" — the cluster NAME is keyed by the service port,
	// matching edgeClusterNameLocked. dialPort is the resolved STRICT_DNS connect
	// port (differs from port for headless Services; see RouteBackend.DialPort).
	type k8sBackend struct {
		namespace string
		service   string
		port      uint32
		dialPort  uint32
	}
	seen := make(map[k8sBackend]struct{})
	var backends []k8sBackend

	addBackend := func(service, namespace string, port, dialPort uint32) {
		p := port
		if p == 0 {
			p = 80
		}
		dp := dialPort
		if dp == 0 {
			dp = p
		}
		bk := k8sBackend{namespace: namespace, service: service, port: p, dialPort: dp}
		if _, dup := seen[bk]; dup {
			return
		}
		seen[bk] = struct{}{}
		backends = append(backends, bk)
	}

	collectRoutes := func(routes []Route) {
		for _, r := range routes {
			// Weighted multi-backend: iterate each backend in the list.
			for _, b := range r.Backends {
				if b.Service == "" {
					continue
				}
				addBackend(b.Service, b.BackendNamespace, b.Port, b.DialPort)
			}
			// Legacy single-backend field (also populated for single-backend routes).
			if r.Service == "" {
				continue
			}
			addBackend(r.Service, r.BackendNamespace, r.Port, r.DialPort)
		}
	}

	for _, v := range vhosts {
		collectRoutes(v.Routes)
	}
	for _, gw := range gws {
		for _, v := range gw.VirtualHosts {
			collectRoutes(v.Routes)
		}
	}
	if len(backends) == 0 {
		return nil
	}

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	var out []types.Resource
	for _, bk := range backends {
		// Only emit a cleartext cluster if the service is NOT a registry cluster.
		// If it IS in the registry, the mesh mTLS cluster covers it.
		if _, ok := c.clusters[bk.service]; ok {
			continue
		}
		meshFQDN := proxy.ServiceClusterName(bk.service, c.meshDomain)
		if _, ok := c.clusters[meshFQDN]; ok {
			continue
		}
		portName := proxy.PortClusterName(bk.service, c.meshDomain, bk.port)
		if _, ok := c.clusters[portName]; ok {
			continue
		}
		name := proxy.EdgeK8sClusterName(bk.namespace, bk.service, bk.port)
		fqdn := bk.service + "." + bk.namespace + ".svc.cluster.local"
		// Dial bk.dialPort, not bk.port: for a headless Service the FQDN resolves to
		// pod IPs and must reach the targetPort; for ClusterIP they are equal.
		out = append(out, proxy.BuildEdgeK8sCluster(name, fqdn, bk.dialPort))
	}
	return out
}

// equalRoutes reports whether two Route slices are content-equal. Route carries
// pointer fields (*GammaHeaderMutation, *GammaRedirect, *GammaURLRewrite), so we
// cannot rely on slices.Equal (pointer identity) — we need a deep comparison.
func equalRoutes(a, b []Route) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ra, rb := a[i], b[i]
		if ra.Prefix != rb.Prefix || ra.Exact != rb.Exact || ra.Service != rb.Service || ra.Port != rb.Port || ra.BackendNamespace != rb.BackendNamespace || ra.DialPort != rb.DialPort {
			return false
		}
		if ra.Method != rb.Method || ra.DirectResponseStatus != rb.DirectResponseStatus {
			return false
		}
		if !equalRouteDuration(ra.Timeout, rb.Timeout) {
			return false
		}
		if !equalRouteBackends(ra.Backends, rb.Backends) {
			return false
		}
		if !equalHeaderMutation(ra.HeaderMutation, rb.HeaderMutation) {
			return false
		}
		if !equalRouteRedirect(ra.Redirect, rb.Redirect) {
			return false
		}
		if !equalRouteURLRewrite(ra.URLRewrite, rb.URLRewrite) {
			return false
		}
		if !equalHeaderMatches(ra.Headers, rb.Headers) {
			return false
		}
		if !equalQueryParamMatches(ra.QueryParams, rb.QueryParams) {
			return false
		}
	}
	return true
}

// equalHeaderMatches reports whether two RouteHeaderMatch slices are identical.
func equalHeaderMatches(a, b []proxy.RouteHeaderMatch) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalQueryParamMatches reports whether two RouteQueryParamMatch slices are identical.
func equalQueryParamMatches(a, b []proxy.RouteQueryParamMatch) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalRouteBackends reports whether two RouteBackend slices are identical.
func equalRouteBackends(a, b []RouteBackend) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalRouteDuration reports content equality for two *durationpb.Duration values.
func equalRouteDuration(a, b *durationpb.Duration) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Seconds == b.Seconds && a.Nanos == b.Nanos
}

// equalRouteRedirect reports content equality for two *proxy.GammaRedirect values.
func equalRouteRedirect(a, b *proxy.GammaRedirect) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// equalRouteURLRewrite reports content equality for two *proxy.GammaURLRewrite values.
func equalRouteURLRewrite(a, b *proxy.GammaURLRewrite) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// equalHeaderMutation reports content equality for two *GammaHeaderMutation
// values. Both nil = equal; one nil = not equal; otherwise fields are compared.
func equalHeaderMutation(a, b *proxy.GammaHeaderMutation) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return slices.Equal(a.RemoveRequest, b.RemoveRequest) &&
		slices.Equal(a.RemoveResponse, b.RemoveResponse) &&
		equalHeaderKVs(a.SetRequest, b.SetRequest) &&
		equalHeaderKVs(a.AddRequest, b.AddRequest) &&
		equalHeaderKVs(a.SetResponse, b.SetResponse) &&
		equalHeaderKVs(a.AddResponse, b.AddResponse)
}

func equalHeaderKVs(a, b []proxy.GammaHeaderKV) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalVirtualHosts reports whether two virtual-host slices are identical (order
// and contents), so a no-op SetVirtualHosts call skips a snapshot rebuild.
func equalVirtualHosts(a, b []VirtualHost) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].TLSSecret != b[i].TLSSecret ||
			!slices.Equal(a[i].Hosts, b[i].Hosts) ||
			!equalRoutes(a[i].Routes, b[i].Routes) {
			return false
		}
	}
	return true
}

// equalEdgeTCPRoutes reports whether two EdgeL4TCPRoute slices are identical.
func equalEdgeTCPRoutes(a, b []proxy.EdgeL4TCPRoute) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Port != b[i].Port || len(a[i].Backends) != len(b[i].Backends) {
			return false
		}
		for j := range a[i].Backends {
			if a[i].Backends[j] != b[i].Backends[j] {
				return false
			}
		}
	}
	return true
}

// equalEdgeTLSRoutes reports whether two EdgeL4TLSRoute slices are identical.
func equalEdgeTLSRoutes(a, b []proxy.EdgeL4TLSRoute) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Port != b[i].Port {
			return false
		}
		if len(a[i].Rules) != len(b[i].Rules) {
			return false
		}
		for j := range a[i].Rules {
			ra, rb := a[i].Rules[j], b[i].Rules[j]
			if !slices.Equal(ra.SNIHostnames, rb.SNIHostnames) {
				return false
			}
			if len(ra.Backends) != len(rb.Backends) {
				return false
			}
			for k := range ra.Backends {
				if ra.Backends[k] != rb.Backends[k] {
					return false
				}
			}
		}
	}
	return true
}

// equalEdgeGateways reports whether two EdgeGatewayEntry slices are identical.
func equalEdgeGateways(a, b []EdgeGatewayEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Namespace != b[i].Namespace || a[i].Name != b[i].Name {
			return false
		}
		if !equalEdgeGatewayListeners(a[i].Listeners, b[i].Listeners) {
			return false
		}
		if !equalVirtualHosts(a[i].VirtualHosts, b[i].VirtualHosts) {
			return false
		}
	}
	return true
}

// equalEdgeGatewayListeners reports whether two EdgeGatewayListenerEntry slices
// are identical (port allocations + TLS config).
func equalEdgeGatewayListeners(a, b []EdgeGatewayListenerEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ExternalPort != b[i].ExternalPort ||
			a[i].InternalPort != b[i].InternalPort ||
			a[i].HTTPRedirect != b[i].HTTPRedirect ||
			!slices.Equal(a[i].TLSSecretNames, b[i].TLSSecretNames) {
			return false
		}
	}
	return true
}
