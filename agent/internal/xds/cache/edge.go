package cache

import (
	"context"
	"slices"
	"strconv"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
)

// VirtualHost is the cache's projection of a VirtualHost CR (without the
// Kubernetes types): a set of external hosts carrying an ordered list of
// path-matched routes to mesh services, optionally served under a downstream
// cert. Empty Hosts or empty Routes makes it inert (it exposes nothing — the
// internal mesh FQDN is never routable from the edge).
type VirtualHost struct {
	Hosts  []string
	Routes []Route
	// TLSSecret is the provider-prefixed SDS name of the cert presented for this
	// virtual host's hosts (empty = no cert). Cert bytes are supplied separately
	// via SetEdgeTLSSecrets.
	TLSSecret string
}

// Route is one path-match -> backend rule within a VirtualHost. Exactly one of
// Prefix/Exact is set; Port 0 means the service's default port.
// HeaderMutation carries the merged request/response header modifier filters for
// this rule (nil = no header mutations).
// When Redirect is non-nil the route returns a redirect response (no backend).
// When URLRewrite is non-nil the route rewrites the request URL before forwarding.
type Route struct {
	Prefix         string
	Exact          string
	Service        string
	Port           uint32
	HeaderMutation *proxy.GammaHeaderMutation
	Redirect       *proxy.GammaRedirect
	URLRewrite     *proxy.GammaURLRewrite
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
// configured VirtualHosts: one Envoy vhost per CR — its hosts as domains, its
// routes emitted IN ORDER (first match wins). A host already claimed by an
// earlier vhost is dropped from later ones (Envoy NACKs duplicate domains): the
// runtime backstop to the controller webhook's duplicate-FQDN rejection, with
// keep-first determinism (the reconciler sorts the input by namespace/name). A
// vhost left with no domains, or with no routable routes, is skipped.
func (c *SnapshotCache) virtualHostVhosts() []*routev3.VirtualHost {
	c.edgeMu.RLock()
	vhosts := slices.Clone(c.virtualHosts)
	c.edgeMu.RUnlock()

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	used := make(map[string]struct{})
	out := make([]*routev3.VirtualHost, 0, len(vhosts))
	for _, v := range vhosts {
		domains := make([]string, 0, len(v.Hosts))
		for _, h := range v.Hosts {
			if h == "" {
				continue
			}
			if _, ok := used[h]; ok {
				continue
			}
			used[h] = struct{}{}
			domains = append(domains, h)
		}
		if len(domains) == 0 {
			continue
		}
		routes := make([]*routev3.Route, 0, len(v.Routes))
		for _, r := range v.Routes {
			if r.Redirect == nil && r.Service == "" {
				continue
			}
			cluster := ""
			if r.Service != "" {
				cluster = c.edgeClusterNameLocked(r.Service, r.Port)
			}
			routes = append(routes, proxy.BuildEdgeRoute(r.Prefix, r.Exact, cluster, r.HeaderMutation, r.Redirect, r.URLRewrite))
		}
		if len(routes) == 0 {
			continue
		}
		out = append(out, proxy.BuildEdgeVirtualHost(domains[0], domains, routes))
	}
	return out
}

// edgeClusterNameLocked resolves a (service, port) backend to the data-plane
// cluster it targets. Port 0 -> the default cluster (ServiceClusterName). A
// non-zero port -> its per-port cluster if one exists; otherwise, if the port is
// the service's default port (no dedicated per-port cluster), the default
// cluster serves it. Caller must hold clusterMu.
func (c *SnapshotCache) edgeClusterNameLocked(service string, port uint32) string {
	fqdn := proxy.ServiceClusterName(service, c.meshDomain)
	if port == 0 {
		return fqdn
	}
	portName := proxy.PortClusterName(service, c.meshDomain, port)
	if _, ok := c.clusters[portName]; ok {
		return portName
	}
	if entry, ok := c.clusters[service]; ok && entry.sni == strconv.Itoa(int(port)) {
		return fqdn
	}
	return portName
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
// all edge route backends (HTTP virtual-hosts + TCP routes + TLS routes) and calls
// SetStaticDependencies. Called whenever any edge route set changes.
func (c *SnapshotCache) rebuildEdgeDependencies() {
	c.edgeMu.RLock()
	vhosts := slices.Clone(c.virtualHosts)
	tcpRoutes := append([]proxy.EdgeL4TCPRoute(nil), c.edgeTCPRoutes...)
	tlsRoutes := append([]proxy.EdgeL4TLSRoute(nil), c.edgeTLSRoutes...)
	c.edgeMu.RUnlock()

	seen := make(map[string]struct{})
	var services []string

	for _, v := range vhosts {
		if len(v.Hosts) == 0 {
			continue
		}
		for _, r := range v.Routes {
			if r.Service == "" {
				continue
			}
			if _, ok := seen[r.Service]; !ok {
				seen[r.Service] = struct{}{}
				services = append(services, r.Service)
			}
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

// equalRoutes reports whether two Route slices are content-equal. Route carries
// pointer fields (*GammaHeaderMutation, *GammaRedirect, *GammaURLRewrite), so we
// cannot rely on slices.Equal (pointer identity) — we need a deep comparison.
func equalRoutes(a, b []Route) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ra, rb := a[i], b[i]
		if ra.Prefix != rb.Prefix || ra.Exact != rb.Exact || ra.Service != rb.Service || ra.Port != rb.Port {
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
	}
	return true
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
