package cache

// Sixth adversarial pass on the proposal-043 stack (head 270dd5b8). Tests
// adopted from the review.

import (
	"context"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func s6ExpireGrace(c *SnapshotCache) {
	c.clusterMu.Lock()
	defer c.clusterMu.Unlock()
	for name, e := range c.clusters {
		if !e.absentSince.IsZero() {
			e.absentSince = time.Now().Add(-2 * c.retentionGrace())
			c.clusters[name] = e
		}
	}
}

// Ruling 4, "an incrementally built cache answers like a freshly started one".
// A refresh builds the entries' loopback virtual hosts FIRST, with the ports
// the cache remembered of every service, and only then drops what it
// remembered of a service whose grace ran out (pruneServicePortsLocked) or
// that its generated Service replaced (resetByServiceLocked). A rule of
// ANOTHER service whose backendRef names that service was resolved by the
// ports that the same refresh then forgot. Nothing advances the rule
// generation, so the loopback table keeps the answer; the capture table of
// the same snapshot, built afterwards, has the other one.
func TestPerPort_BackendJudgedByPortsTheSameRefreshForgets(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	front := a043Service{name: "front", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.1", vip: "10.96.5.1"}
	host := "front.demo." + a043Mesh + ":8080"
	route := func(c *SnapshotCache) {
		c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/front": {{
			Matches:  []proxy.GammaMatch{{Prefix: "/"}},
			Backends: []proxy.GammaBackend{{Service: "demo/back", Cluster: "back.demo." + a043Mesh, Port: 8080, Weight: 1}},
		}}})
	}
	load := func(c *SnapshotCache, svcs ...a043Service) {
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(svcs)))
	}
	answers := func(c *SnapshotCache) (loopback, captured string) {
		require.NoError(t, c.generateSnapshot(ctx))
		tables := a043Tables(t, c)
		return r5Resolve(t, tables[proxy.OutboundHTTPRouteName], "demo", host, "/").sig, r5Resolve(t, tables["cap_http_8080"], "demo", host, "/").sig
	}

	t.Run("the backend's grace runs out", func(t *testing.T) {
		back := a043Service{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: tcp}, ip: "10.0.5.2", vip: "10.96.5.2"}
		project := func(c *SnapshotCache) { a043ProjectNamesOnly(c, []a043Service{front, back}) }
		c := a043Cache(t, nil)
		project(c)
		route(c)
		load(c, front, back)
		load(c, front, back)
		lb, capt := answers(c)
		require.Equal(t, "direct 500 ", lb, "control: the backendRef names a raw-TCP port")
		require.Equal(t, "direct 500 ", capt)
		load(c, front) // the backend's rows are gone: retained
		s6ExpireGrace(c)
		load(c, front) // the grace ran out: pruned, and nothing is known of the backend

		fresh := a043Cache(t, nil)
		project(fresh)
		route(fresh)
		load(fresh, front)
		load(fresh, front)
		wantLB, wantCap := answers(fresh)
		lb, capt = answers(c)
		assert.Equal(t, wantCap, capt, "control: the capture table answers as a fresh node's")
		assert.Equalf(t, wantLB, lb, "loopback, Host %s: after the backend's grace ran out vs a node that starts in this state (the capture table of the same snapshot: %s)", host, capt)
		load(c, front)
		lb, _ = answers(c)
		t.Logf("after one more refresh the loopback table answers %q (fresh: %q)", lb, wantLB)
	})

	t.Run("the backend is replaced by its Service", func(t *testing.T) {
		back := a043Service{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.2", vip: "10.96.5.2"}
		after := a043Service{name: "back", ns: "demo", legacy: 9000, ports: map[uint32]registryv1.PortProtocol{9000: tcp}, ip: "10.0.5.3", vip: "10.96.5.2"}
		c := a043Cache(t, nil)
		a043Project(c, []a043Service{front, back})
		route(c)
		load(c, front, back)
		load(c, front, back)
		lb, _ := answers(c)
		require.Contains(t, lb, "back.demo."+a043Mesh, "control: routed to the backend")
		// The backend's rows are gone and its generated Service now declares
		// another shape: reset to what the Service says.
		a043Project(c, []a043Service{front, after})
		load(c, front)

		fresh := a043Cache(t, nil)
		a043Project(fresh, []a043Service{front, after})
		route(fresh)
		load(fresh, front)
		load(fresh, front)
		wantLB, wantCap := answers(fresh)
		lb, capt := answers(c)
		assert.Equal(t, wantCap, capt, "control: the capture table answers as a fresh node's")
		assert.Equalf(t, wantLB, lb, "loopback, Host %s: after the backend was reset to its Service vs a node that starts in this state (the capture table of the same snapshot: %s)", host, capt)
		load(c, front)
		lb, _ = answers(c)
		t.Logf("after one more refresh the loopback table answers %q (fresh: %q)", lb, wantLB)
	})
}
