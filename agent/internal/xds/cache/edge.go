package cache

import (
	"context"
	"slices"
	"strconv"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
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
type Route struct {
	Prefix  string
	Exact   string
	Service string
	Port    uint32
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
// dependency set to the union of every route's backend service (so the registrar
// watch and clusters follow the exposed set) and rebuilds the snapshot's route
// table — including when only the hostnames or matches changed (the service set,
// and therefore the dependency set, didn't).
func (c *SnapshotCache) SetVirtualHosts(vhosts []VirtualHost) {
	c.edgeMu.Lock()
	changed := !equalVirtualHosts(c.virtualHosts, vhosts)
	c.virtualHosts = vhosts
	c.edgeMu.Unlock()

	// Dependency set = the distinct backend services of every ROUTABLE virtual
	// host (those with at least one external host). A vhost without hosts exposes
	// nothing — it must not pull its services into scope. Signals a reload when
	// the set changed.
	seen := make(map[string]struct{})
	services := make([]string, 0, len(vhosts))
	for _, v := range vhosts {
		if len(v.Hosts) == 0 {
			continue
		}
		for _, r := range v.Routes {
			if r.Service == "" {
				continue
			}
			if _, ok := seen[r.Service]; ok {
				continue
			}
			seen[r.Service] = struct{}{}
			services = append(services, r.Service)
		}
	}
	c.SetStaticDependencies(services)

	// A host/match-only change leaves the dependency set untouched, so signal a
	// rebuild directly; the send is coalesced, so the SetStaticDependencies
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
			if r.Service == "" {
				continue
			}
			cluster := c.edgeClusterNameLocked(r.Service, r.Port)
			routes = append(routes, proxy.BuildEdgeRoute(r.Prefix, r.Exact, cluster))
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

// equalVirtualHosts reports whether two virtual-host slices are identical (order
// and contents), so a no-op SetVirtualHosts call skips a snapshot rebuild.
func equalVirtualHosts(a, b []VirtualHost) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].TLSSecret != b[i].TLSSecret ||
			!slices.Equal(a[i].Hosts, b[i].Hosts) ||
			!slices.Equal(a[i].Routes, b[i].Routes) {
			return false
		}
	}
	return true
}
