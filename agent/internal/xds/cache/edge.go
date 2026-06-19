package cache

import (
	"context"
	"slices"
	"strconv"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
)

// EdgeRoute is one external-host -> mesh-service mapping the edge proxy serves
// (the cache's projection of an EdgeRoute CR, without the Kubernetes types). An
// empty Hosts means the service is reachable at its mesh FQDN; Port 0 means the
// service's default port.
type EdgeRoute struct {
	Hosts   []string
	Service string
	Port    uint32
	// TLSSecret is the provider-prefixed SDS name of the cert presented for this
	// route's hosts (empty = no cert for these hosts). Cert bytes are supplied
	// separately via SetEdgeTLSSecrets.
	TLSSecret string
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
// current edge routes, sorted for deterministic listener bytes.
func (c *SnapshotCache) edgeTLSSecretNames() []string {
	c.edgeMu.RLock()
	defer c.edgeMu.RUnlock()
	seen := make(map[string]struct{}, len(c.edgeRoutes))
	names := make([]string, 0, len(c.edgeRoutes))
	for _, r := range c.edgeRoutes {
		if r.TLSSecret == "" {
			continue
		}
		if _, ok := seen[r.TLSSecret]; ok {
			continue
		}
		seen[r.TLSSecret] = struct{}{}
		names = append(names, r.TLSSecret)
	}
	slices.Sort(names)
	return names
}

// SetEdgeRoutes replaces the edge's exposed routes. It scopes the dependency set
// to the referenced services (so the registrar watch and clusters follow the
// exposed set) and rebuilds the snapshot's route table from the new routes —
// including when only the hostnames changed (the service set, and therefore the
// dependency set, didn't).
func (c *SnapshotCache) SetEdgeRoutes(routes []EdgeRoute) {
	c.edgeMu.Lock()
	changed := !equalEdgeRoutes(c.edgeRoutes, routes)
	c.edgeRoutes = routes
	c.edgeMu.Unlock()

	// Dependency set = the distinct services of ROUTABLE routes (those with at
	// least one external host). A route without hosts exposes nothing — it must
	// not pull its service into scope, and it never becomes routable at the mesh
	// FQDN. Signals a reload when the set changed.
	seen := make(map[string]struct{}, len(routes))
	services := make([]string, 0, len(routes))
	for _, r := range routes {
		if r.Service == "" || len(r.Hosts) == 0 {
			continue
		}
		if _, ok := seen[r.Service]; ok {
			continue
		}
		seen[r.Service] = struct{}{}
		services = append(services, r.Service)
	}
	c.SetStaticDependencies(services)

	// A host-only change leaves the dependency set untouched, so signal a rebuild
	// directly; the send is coalesced, so the SetStaticDependencies signal (if
	// any) is not duplicated.
	if changed {
		c.signalDependencyChange()
	}
}

// edgeRouteVhosts builds the edge listener's virtual hosts from the configured
// routes: one vhost per target cluster, with the union of its routes' external
// hostnames as domains. The edge routes ONLY by explicit external host — a route
// without hosts is skipped (the internal mesh FQDN is never routable from the
// edge). Routes targeting the same cluster are merged so the vhost name (the
// cluster name) and the domain set stay unique — Envoy NACKs duplicate vhost
// names or domains.
func (c *SnapshotCache) edgeRouteVhosts() []*routev3.VirtualHost {
	c.edgeMu.RLock()
	routes := slices.Clone(c.edgeRoutes)
	c.edgeMu.RUnlock()

	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	byCluster := make(map[string][]string)
	order := make([]string, 0, len(routes))
	for _, r := range routes {
		if r.Service == "" || len(r.Hosts) == 0 {
			continue
		}
		clusterName := c.edgeClusterNameLocked(r)
		if _, ok := byCluster[clusterName]; !ok {
			order = append(order, clusterName)
		}
		byCluster[clusterName] = append(byCluster[clusterName], r.Hosts...)
	}

	vhosts := make([]*routev3.VirtualHost, 0, len(order))
	for _, clusterName := range order {
		domains := dedupeStrings(byCluster[clusterName])
		vhosts = append(vhosts, proxy.BuildOutboundClusterVirtualHost(clusterName, domains))
	}
	return vhosts
}

// edgeClusterNameLocked resolves an edge route to the data-plane cluster it
// targets. Port 0 -> the default cluster (ServiceClusterName). A non-zero port
// -> its per-port cluster if one exists; otherwise, if the port is the service's
// default port (no dedicated per-port cluster), the default cluster serves it.
// Caller must hold clusterMu.
func (c *SnapshotCache) edgeClusterNameLocked(r EdgeRoute) string {
	fqdn := proxy.ServiceClusterName(r.Service, c.meshDomain)
	if r.Port == 0 {
		return fqdn
	}
	portName := proxy.PortClusterName(r.Service, c.meshDomain, r.Port)
	if _, ok := c.clusters[portName]; ok {
		return portName
	}
	if entry, ok := c.clusters[r.Service]; ok && entry.sni == strconv.Itoa(int(r.Port)) {
		return fqdn
	}
	return portName
}

// dedupeStrings returns the input with duplicates removed, order preserved.
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// equalEdgeRoutes reports whether two route slices are identical (order and
// contents), so a no-op SetEdgeRoutes call skips a snapshot rebuild.
func equalEdgeRoutes(a, b []EdgeRoute) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Service != b[i].Service || a[i].Port != b[i].Port ||
			a[i].TLSSecret != b[i].TLSSecret || !slices.Equal(a[i].Hosts, b[i].Hosts) {
			return false
		}
	}
	return true
}
