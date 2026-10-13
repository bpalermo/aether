package cache

import (
	"reflect"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	meshconst "aethermesh.dev/common/constants/mesh"
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

// routesMemoPointer returns the identity of the memoized effective-routes map.
func routesMemoPointer(c *SnapshotCache) uintptr {
	return reflect.ValueOf(c.serviceRoutesSnapshot()).Pointer()
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

// namesOf returns the port-less names the capture tables answer for svc, nil
// when the tables do not know the service.
func namesOf(c *SnapshotCache, svc string) []string {
	for _, n := range c.captureTableInputs().names {
		if n.svc == svc {
			return n.names
		}
	}
	return nil
}

// TestRouteTargetDomainsMemo_EquivalentToInline holds the NAMES a route target
// is addressed by across the representative shapes: an SA-backed target, a
// route-only target, and a bare-name collision across namespaces.
//
// It used to compare a memoized domain list, ports folded in, with an inline
// reference (#540). Since proposal 043 a table is per dialled port, so the
// names carry no port (the table adds its own) and are computed per build; what
// this test still holds is which names exist and in what order.
func TestRouteTargetDomainsMemo_EquivalentToInline(t *testing.T) {
	c := newTestCache("node-1")
	routes := map[string][]proxy.GammaRoute{
		// SA-backed (mesh authority exists), bare name "echo" shared with
		// team-b (collision → no bare name).
		"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "a.test", Weight: 1}}}},
		// Route-only target (no authority; fqdn derived from the key).
		"team-b/echo": {{Backends: []proxy.GammaBackend{{Service: "team-b/echo-v2", Cluster: "b.test", Weight: 1}}}},
		// Unique bare name → bare name emitted.
		"team-c/solo": {{Backends: []proxy.GammaBackend{{Service: "team-c/solo-v1", Cluster: "c.test", Weight: 1}}}},
	}
	c.SetServiceRoutes(routes)
	c.SetCaptureAuthorities(map[string]string{
		"team-a/echo": "echo.team-a.svc.cluster.local",
	})
	c.SetRouteTargetPorts(map[string][]uint32{
		"team-a/echo": {80, 8080, 0, meshconst.ProxyOutboundPort},
		"team-b/echo": {9000},
	})

	assert.Equal(t, []string{"echo.team-a.svc.cluster.local", proxy.ServiceClusterName("team-a/echo", c.meshDomain), "echo.team-a", "echo.team-a.svc"},
		namesOf(c, "team-a/echo"), "a bare name two namespaces share is nobody's name")
	assert.Equal(t, []string{"echo.team-b.svc.cluster.local", proxy.ServiceClusterName("team-b/echo", c.meshDomain), "echo.team-b", "echo.team-b.svc"},
		namesOf(c, "team-b/echo"), "a route-only target derives its cluster.local name from the key")
	assert.Equal(t, []string{"solo.team-c.svc.cluster.local", proxy.ServiceClusterName("team-c/solo", c.meshDomain), "solo.team-c", "solo.team-c.svc", "solo"},
		namesOf(c, "team-c/solo"), "a unique bare name is a name of the service")

	// The ports fold into every spelling, in the table of each port, and
	// nowhere else; port 0 names no table.
	assert.Contains(t, domainsOf(captureTable(c, 8080)), "echo.team-a:8080", "real Service port folds into every spelling")
	assert.NotContains(t, domainsOf(captureTable(c, 80)), "echo.team-a:8080")
	assert.NotContains(t, c.captureHTTPPorts(), uint32(0), "port 0 is skipped")
	assert.Contains(t, c.captureHTTPPorts(), uint32(9000), "a route target's parentRef port is a port of the mesh")
	for _, port := range c.captureHTTPPorts() {
		for _, d := range domainsOf(captureTable(c, port)) {
			assert.NotContains(t, d, ":0")
		}
	}
}

// TestRouteTargetDomainsMemo_CacheHitAndInvalidation asserts each input
// mutator (routes, route-target ports, capture authorities) is seen by the
// next build of the tables. There is no memo to hit any more: the tables are
// rebuilt from the current inputs on every snapshot.
func TestRouteTargetDomainsMemo_CacheHitAndInvalidation(t *testing.T) {
	c := newTestCache("node-1")
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "a.test", Weight: 1}}}},
	})
	c.SetCaptureAuthorities(map[string]string{"team-a/echo": "echo.team-a.svc.cluster.local"})
	assert.NotContains(t, c.captureHTTPPorts(), uint32(9090))

	// SetRouteTargetPorts: the new port shows up, as a table and in it.
	c.SetRouteTargetPorts(map[string][]uint32{"team-a/echo": {9090}})
	assert.Contains(t, c.captureHTTPPorts(), uint32(9090))
	assert.Contains(t, domainsOf(captureTable(c, 9090)), "echo.team-a.svc.cluster.local:9090")

	// An authority CHANGE: the new fqdn shows up and the old one is gone.
	c.SetCaptureAuthorities(map[string]string{"team-a/echo": "echo.renamed.svc.cluster.local"})
	assert.Contains(t, namesOf(c, "team-a/echo"), "echo.renamed.svc.cluster.local")
	assert.NotContains(t, namesOf(c, "team-a/echo"), "echo.team-a.svc.cluster.local")

	// A route mutation: the new target gets its names.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo":  {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "a.test", Weight: 1}}}},
		"team-b/fresh": {{Backends: []proxy.GammaBackend{{Service: "team-b/fresh-v1", Cluster: "f.test", Weight: 1}}}},
	})
	assert.Contains(t, namesOf(c, "team-b/fresh"), "fresh.team-b.svc.cluster.local", "route-only target derives its fqdn from the key")
}

// TestRouteTargetDomainsMemo_UnparseableKeySkipped: a route target whose key is
// not "<ns>/<svc>" gets no derived authority, no names and therefore no vhost.
func TestRouteTargetDomainsMemo_UnparseableKeySkipped(t *testing.T) {
	c := newTestCache("node-1")
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"not-a-key": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "a.test", Weight: 1}}}},
	})
	assert.Nil(t, namesOf(c, "not-a-key"))
	for _, port := range append(c.captureHTTPPorts(), 0) {
		for _, vh := range captureTable(c, port) {
			assert.NotContains(t, vh.GetName(), "not-a-key")
		}
	}
}
