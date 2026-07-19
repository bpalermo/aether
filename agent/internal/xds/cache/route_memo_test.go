package cache

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/common/serviceref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the write-time-precomputed GAMMA route state (issue #540): the
// effective (local ∪ imported, extension-stripped) route set and the
// per-route-target cap_http domain lists are memoized on generation counters
// (the #539 depGen idiom) — the snapshot path only reads; mutators invalidate.

// referenceStripUnavailableExtensions replicates the pre-#540 inline strip
// (gamma.go stripUnavailableExtensions as run per snapshot) so the memoized
// output can be asserted byte-identical to the old computation.
func referenceStripUnavailableExtensions(authzSidecar bool, rules []proxy.GammaRoute) []proxy.GammaRoute {
	if authzSidecar {
		return rules
	}
	needsStrip := false
	for _, r := range rules {
		for _, ef := range r.ExtensionFilters {
			if ef.Name == proxy.ExtAuthzFilterName {
				needsStrip = true
			}
		}
	}
	if !needsStrip {
		return rules
	}
	out := make([]proxy.GammaRoute, len(rules))
	copy(out, rules)
	for i := range out {
		var kept []proxy.ExtensionFilter
		for _, ef := range out[i].ExtensionFilters {
			if ef.Name == proxy.ExtAuthzFilterName {
				continue
			}
			kept = append(kept, ef)
		}
		out[i].ExtensionFilters = kept
	}
	return out
}

// referenceRouteTargetDomains replicates the pre-#540 inline domain build
// (capture.go routeTargetDomains as run per snapshot).
func referenceRouteTargetDomains(svc, fqdn, mesh string, bareNameCount map[string]int, ports []uint32) []string {
	baseNames := []string{fqdn, mesh}
	if ref, ok := serviceref.ParseKey(svc); ok {
		baseNames = append(baseNames, ref.Name+"."+ref.Namespace, ref.Name+"."+ref.Namespace+".svc")
		if bareNameCount[ref.Name] == 1 {
			baseNames = append(baseNames, ref.Name)
		}
	}
	domains := make([]string, 0, len(baseNames)*2)
	for _, n := range baseNames {
		domains = append(domains, n, fmt.Sprintf("%s:%d", n, constants.ProxyOutboundPort))
	}
	for _, p := range ports {
		if p == 0 || p == constants.ProxyOutboundPort {
			continue
		}
		for _, n := range baseNames {
			domains = append(domains, fmt.Sprintf("%s:%d", n, p))
		}
	}
	return domains
}

// routesMemoPointer returns the identity of the memoized effective-routes map.
func routesMemoPointer(c *SnapshotCache) uintptr {
	return reflect.ValueOf(c.serviceRoutesSnapshot()).Pointer()
}

// domainsMemoPointer returns the identity of the memoized route-domains map.
func domainsMemoPointer(c *SnapshotCache) uintptr {
	_, d := c.serviceRoutesAndDomainsSnapshot()
	return reflect.ValueOf(d).Pointer()
}

func extFilterRoutes(target, backend string, filters ...proxy.ExtensionFilter) map[string][]proxy.GammaRoute {
	return map[string][]proxy.GammaRoute{
		target: {{
			Backends:         []proxy.GammaBackend{{Service: backend, Cluster: backend + ".test", Weight: 1}},
			ExtensionFilters: filters,
		}},
	}
}

// TestServiceRoutesMemo_EquivalentToInlineStrip asserts the memoized effective
// route set equals the old per-snapshot computation for the representative
// input classes: extension filters present/absent, authz on/off, and
// imported+local merge (local wins).
func TestServiceRoutesMemo_EquivalentToInlineStrip(t *testing.T) {
	extAuthz := proxy.ExtensionFilter{Name: proxy.ExtAuthzFilterName}
	headerMut := proxy.ExtensionFilter{Name: "envoy.filters.http.header_mutation"}

	t.Run("authz off strips ext_authz, keeps others", func(t *testing.T) {
		c := newTestCache("node-1")
		local := extFilterRoutes("team-a/echo", "team-a/echo-v1", extAuthz, headerMut)
		c.SetServiceRoutes(local)

		got := c.serviceRoutesSnapshot()
		want := map[string][]proxy.GammaRoute{
			"team-a/echo": referenceStripUnavailableExtensions(false, local["team-a/echo"]),
		}
		assert.Equal(t, want, got)
		require.Len(t, got["team-a/echo"][0].ExtensionFilters, 1)
		assert.Equal(t, headerMut.Name, got["team-a/echo"][0].ExtensionFilters[0].Name)
		// The stored routes must be untouched (strip copies).
		c.depMu.RLock()
		assert.Len(t, c.serviceRoutes["team-a/echo"][0].ExtensionFilters, 2, "strip must not mutate the stored rules")
		c.depMu.RUnlock()
	})

	t.Run("authz off, nothing to strip → rules unchanged", func(t *testing.T) {
		c := newTestCache("node-1")
		local := extFilterRoutes("team-a/echo", "team-a/echo-v1", headerMut)
		c.SetServiceRoutes(local)
		got := c.serviceRoutesSnapshot()
		assert.Equal(t, referenceStripUnavailableExtensions(false, local["team-a/echo"]), got["team-a/echo"])
		assert.Len(t, got["team-a/echo"][0].ExtensionFilters, 1)
	})

	t.Run("authz on keeps ext_authz", func(t *testing.T) {
		c := newTestCache("node-1")
		c.SetAuthzSidecar(200*time.Millisecond, false)
		local := extFilterRoutes("team-a/echo", "team-a/echo-v1", extAuthz, headerMut)
		c.SetServiceRoutes(local)
		got := c.serviceRoutesSnapshot()
		assert.Equal(t, referenceStripUnavailableExtensions(true, local["team-a/echo"]), got["team-a/echo"])
		assert.Len(t, got["team-a/echo"][0].ExtensionFilters, 2)
	})

	t.Run("imported merged, local wins", func(t *testing.T) {
		c := newTestCache("node-1")
		c.SetServiceRoutes(extFilterRoutes("team-a/echo", "team-a/echo-local"))
		c.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{
			"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-peer", Cluster: "peer.test", Weight: 1}}}},
			"peer/only":   {{Backends: []proxy.GammaBackend{{Service: "peer/only-v1", Cluster: "only.test", Weight: 1}}}},
		})
		got := c.serviceRoutesSnapshot()
		require.Len(t, got, 2)
		assert.Equal(t, "team-a/echo-local", got["team-a/echo"][0].Backends[0].Service, "local route wins the key collision")
		assert.Contains(t, got, "peer/only")
	})
}

// TestServiceRoutesMemo_CacheHitAndInvalidation asserts repeated snapshots
// without mutation are served from the memo (same underlying map), and every
// input mutator — local routes, imported routes, authz availability — rebuilds.
func TestServiceRoutesMemo_CacheHitAndInvalidation(t *testing.T) {
	extAuthz := proxy.ExtensionFilter{Name: proxy.ExtAuthzFilterName}
	c := newTestCache("node-1")
	c.SetServiceRoutes(extFilterRoutes("team-a/echo", "team-a/echo-v1", extAuthz))

	p1 := routesMemoPointer(c)
	p2 := routesMemoPointer(c)
	assert.Equal(t, p1, p2, "no mutation: the memoized map must be served")

	c.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{
		"peer/svc": {{Backends: []proxy.GammaBackend{{Service: "peer/svc-v1", Cluster: "svc.test", Weight: 1}}}},
	})
	p3 := routesMemoPointer(c)
	assert.NotEqual(t, p2, p3, "SetImportedServiceRoutes must invalidate")
	assert.Contains(t, c.serviceRoutesSnapshot(), "peer/svc")

	// Authz availability flips the strip result: before, ext_authz is stripped;
	// after SetAuthzSidecar the memo must rebuild and retain it.
	require.Empty(t, c.serviceRoutesSnapshot()["team-a/echo"][0].ExtensionFilters)
	c.SetAuthzSidecar(200*time.Millisecond, false)
	got := c.serviceRoutesSnapshot()
	require.Len(t, got["team-a/echo"][0].ExtensionFilters, 1, "authz-availability change must recompute the stripped set")
	assert.Equal(t, proxy.ExtAuthzFilterName, got["team-a/echo"][0].ExtensionFilters[0].Name)

	c.SetServiceRoutes(extFilterRoutes("team-b/other", "team-b/other-v1"))
	assert.Contains(t, c.serviceRoutesSnapshot(), "team-b/other", "SetServiceRoutes must invalidate")
}

// TestRouteTargetDomainsMemo_EquivalentToInline asserts the memoized domain
// lists are byte-identical (same domains, same order) to the old per-snapshot
// inline computation across the representative shapes: SA-backed target with
// real ports, route-only target, bare-name collision across namespaces, and
// the port-0 / mesh-port skip rules.
func TestRouteTargetDomainsMemo_EquivalentToInline(t *testing.T) {
	c := newTestCache("node-1")
	routes := map[string][]proxy.GammaRoute{
		// SA-backed (mesh authority exists) with real Service ports, bare name
		// "echo" shared with team-b (collision → no bare domain).
		"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "a.test", Weight: 1}}}},
		// Route-only target (no authority; fqdn derived from the key).
		"team-b/echo": {{Backends: []proxy.GammaBackend{{Service: "team-b/echo-v2", Cluster: "b.test", Weight: 1}}}},
		// Unique bare name → bare domain emitted; no ports registered.
		"team-c/solo": {{Backends: []proxy.GammaBackend{{Service: "team-c/solo-v1", Cluster: "c.test", Weight: 1}}}},
	}
	c.SetServiceRoutes(routes)
	c.SetCaptureAuthorities(map[string]string{
		"team-a/echo": "echo.team-a.svc.cluster.local",
	})
	c.SetRouteTargetPorts(map[string][]uint32{
		"team-a/echo": {80, 8080, 0, constants.ProxyOutboundPort}, // 0 and the mesh port must be skipped
		"team-b/echo": {9000},
	})

	// The old inline computation counted bare names over route targets in the
	// dependency set; every route target is unconditionally in the set, so this
	// mirrors the pre-#540 count exactly.
	bareNameCount := map[string]int{"echo": 2, "solo": 1}
	want := map[string][]string{
		"team-a/echo": referenceRouteTargetDomains("team-a/echo", "echo.team-a.svc.cluster.local",
			proxy.ServiceClusterName("team-a/echo", c.meshDomain), bareNameCount, []uint32{80, 8080, 0, constants.ProxyOutboundPort}),
		"team-b/echo": referenceRouteTargetDomains("team-b/echo", "echo.team-b.svc.cluster.local",
			proxy.ServiceClusterName("team-b/echo", c.meshDomain), bareNameCount, []uint32{9000}),
		"team-c/solo": referenceRouteTargetDomains("team-c/solo", "solo.team-c.svc.cluster.local",
			proxy.ServiceClusterName("team-c/solo", c.meshDomain), bareNameCount, nil),
	}

	gotRoutes, gotDomains := c.serviceRoutesAndDomainsSnapshot()
	assert.Equal(t, want, gotDomains, "cached domains must be byte-identical (content and order) to the inline computation")
	assert.Len(t, gotRoutes, 3, "the routes returned with the domains come from the same generation")

	// Spot-check the collision guard and port folding directly.
	assert.NotContains(t, gotDomains["team-a/echo"], "echo", "colliding bare name must not be emitted")
	assert.Contains(t, gotDomains["team-c/solo"], "solo", "unique bare name is emitted")
	assert.Contains(t, gotDomains["team-a/echo"], "echo.team-a:8080", "real Service port folds into every spelling")
	assert.NotContains(t, gotDomains["team-a/echo"], "echo.team-a.svc.cluster.local:0", "port 0 is skipped")
}

// TestRouteTargetDomainsMemo_CacheHitAndInvalidation asserts repeated snapshots
// without mutation serve the memoized map, that each input mutator (routes,
// route-target ports, capture authorities) rebuilds it, and that a
// content-equal authority replacement does NOT invalidate.
func TestRouteTargetDomainsMemo_CacheHitAndInvalidation(t *testing.T) {
	c := newTestCache("node-1")
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "a.test", Weight: 1}}}},
	})
	c.SetCaptureAuthorities(map[string]string{"team-a/echo": "echo.team-a.svc.cluster.local"})

	p1 := domainsMemoPointer(c)
	p2 := domainsMemoPointer(c)
	assert.Equal(t, p1, p2, "no mutation: the memoized map must be served")

	// SetRouteTargetPorts must invalidate; the new port shows up.
	c.SetRouteTargetPorts(map[string][]uint32{"team-a/echo": {9090}})
	p3 := domainsMemoPointer(c)
	assert.NotEqual(t, p2, p3, "SetRouteTargetPorts must invalidate")
	_, domains := c.serviceRoutesAndDomainsSnapshot()
	assert.Contains(t, domains["team-a/echo"], "echo.team-a.svc.cluster.local:9090")

	// A content-equal authority replacement must NOT invalidate.
	c.SetCaptureAuthorities(map[string]string{"team-a/echo": "echo.team-a.svc.cluster.local"})
	assert.Equal(t, p3, domainsMemoPointer(c), "content-equal authorities must not rebuild")

	// An authority CHANGE must invalidate; the new fqdn shows up.
	c.SetCaptureAuthorities(map[string]string{"team-a/echo": "echo.renamed.svc.cluster.local"})
	p4 := domainsMemoPointer(c)
	assert.NotEqual(t, p3, p4, "SetCaptureAuthorities change must invalidate")
	_, domains = c.serviceRoutesAndDomainsSnapshot()
	assert.Contains(t, domains["team-a/echo"], "echo.renamed.svc.cluster.local")

	// A route mutation must invalidate; the new target gets a domain list.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo":  {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "a.test", Weight: 1}}}},
		"team-b/fresh": {{Backends: []proxy.GammaBackend{{Service: "team-b/fresh-v1", Cluster: "f.test", Weight: 1}}}},
	})
	_, domains = c.serviceRoutesAndDomainsSnapshot()
	assert.Contains(t, domains, "team-b/fresh", "route mutation must rebuild the domain set")
	assert.Contains(t, domains["team-b/fresh"], "fresh.team-b.svc.cluster.local", "route-only target derives its fqdn from the key")
}

// TestRouteTargetDomainsMemo_UnparseableKeySkipped mirrors the pre-#540
// captureVhosts behavior: a route target whose key is not "<ns>/<svc>" gets no
// derived authority and no domain entry (and therefore no vhost).
func TestRouteTargetDomainsMemo_UnparseableKeySkipped(t *testing.T) {
	c := newTestCache("node-1")
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"not-a-key": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "a.test", Weight: 1}}}},
	})
	_, domains := c.serviceRoutesAndDomainsSnapshot()
	assert.NotContains(t, domains, "not-a-key")
}
