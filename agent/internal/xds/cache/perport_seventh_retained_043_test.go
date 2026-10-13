package cache

// Seventh adversarial review of the proposal-043 stack (head 7ecbd2a3). Tests
// adopted from the review, where each failed.

import (
	"context"
	"fmt"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// R7-N5. The sixth review's N5, on the entries its fix does not reach. The fix
// rebuilds the entries' loopback virtual hosts when a refresh FORGETS a
// service's ports (the count of remembered services drops in the prune). A
// RETAINED entry (a service whose rows are gone, kept for the grace) is never
// rebuilt by a refresh at all: it keeps the virtual host of the refresh that
// last listed it, with its rule's backendRef judged by what the BACKEND
// declared then. When the backend's ports change during the grace (nothing is
// forgotten, the count does not move, no rule generation advances), the
// loopback table keeps the old judgement; the capture table of the same
// snapshot, built from the ports as they are, has the new one.
//
// Two shapes: the backend GAINS the port the rule names (loopback 500 where
// the capture table routes), and the backend's pods are redeployed on another
// port (loopback DELIVERS the backendRef to the backend's default cluster,
// which is now another port's, where the capture table answers 500).
func TestPerPort_ARetainedEntryFollowsItsBackendsPorts(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	front := a043Service{name: "front", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.1", vip: "10.96.5.1"}
	host := "front.demo." + a043Mesh + ":8080"
	load := func(c *SnapshotCache, svcs ...a043Service) {
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(svcs)))
	}
	answers := func(c *SnapshotCache) (loopback, proxyPort, captured r5Answer) {
		require.NoError(t, c.generateSnapshot(ctx))
		tables := a043Tables(t, c)
		require.NotNil(t, tables["cap_http_8080"])
		return r5Resolve(t, tables[proxy.OutboundHTTPRouteName], "demo", host, "/"),
			r5Resolve(t, tables["cap_http_18081"], "demo", host, "/"),
			r5Resolve(t, tables["cap_http_8080"], "demo", host, "/")
	}
	for _, tc := range []struct {
		name          string
		port          uint32 // the backendRef's port
		before, after a043Service
	}{
		{
			name: "the backend gains the port the rule names", port: 9090,
			before: a043Service{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.2", vip: "10.96.5.2"},
			after:  a043Service{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.5.2", vip: "10.96.5.2"},
		},
		{
			name: "the backend's pods move to another port", port: 8080,
			before: a043Service{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.2", vip: "10.96.5.2"},
			after:  a043Service{name: "back", ns: "demo", legacy: 9090, ports: map[uint32]registryv1.PortProtocol{9090: h}, ip: "10.0.5.3", vip: "10.96.5.2"},
		},
	} {
		// projected: the registrar has written the backend's new ports to its
		// generated Service by the time the node sees the rows. When it has
		// not, the only thing that changed for the node is the ports it
		// derives from the rows: no rule input moved at all.
		for _, projected := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/projected=%v", tc.name, projected), func(t *testing.T) {
				declares := tc.before
				if projected {
					declares = tc.after
				}
				c := a043Cache(t, nil)
				a043Project(c, []a043Service{front, tc.before})
				c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/front": {{
					Matches:  []proxy.GammaMatch{{Prefix: "/"}},
					Backends: []proxy.GammaBackend{{Service: "demo/back", Cluster: "back.demo." + a043Mesh, Port: tc.port, Weight: 1}},
				}}})
				load(c, front, tc.before)
				load(c, front, tc.before)
				lb, pp, capt := answers(c)
				require.Equal(t, capt.sig, lb.sig, "control: the two tables agree while the front is listed")
				t.Logf("before: loopback %q, captured %q", lb.sig, capt.sig)

				// The front's pods are gone (a single replica restarting): its
				// entry is retained. In the same refresh, or any later one of the
				// grace, the backend's rows are what `after` declares; the
				// registrar writes them to its generated Service.
				a043Project(c, []a043Service{front, declares})
				load(c, tc.after)
				for i := 0; i < 3; i++ {
					lb, pp, capt = answers(c)
					t.Logf("refresh %d of the grace: loopback %q, proxy port %q, captured :8080 %q", i+1, lb.sig, pp.sig, capt.sig)
					load(c, tc.after)
				}
				lb, pp, capt = answers(c)
				assert.Equalf(t, capt.sig, lb.sig,
					"Host %s on the loopback table against the table of dialled port 8080 of the SAME snapshot, three refreshes after the backend changed (loopback vhost %q, captured vhost %q)", host, lb.vhost, capt.vhost)
				assert.Equal(t, capt.sig, pp.sig, "the proxy-port table against the dialled-port table")

				// What a node that starts now answers on its loopback table.
				fresh := a043Cache(t, nil)
				a043Project(fresh, []a043Service{front, declares})
				fresh.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/front": {{
					Matches:  []proxy.GammaMatch{{Prefix: "/"}},
					Backends: []proxy.GammaBackend{{Service: "demo/back", Cluster: "back.demo." + a043Mesh, Port: tc.port, Weight: 1}},
				}}})
				load(fresh, tc.after)
				load(fresh, tc.after)
				flb, _, fcapt := answers(fresh)
				t.Logf("a node that starts in this state: loopback %q, captured %q", flb.sig, fcapt.sig)
				wrong := lb.delivered && !flb.delivered
				assert.Falsef(t, wrong, "the loopback table DELIVERS the backendRef (%q) where a node that starts now does not (%q)", lb.sig, flb.sig)
			})
		}
	}
}

// TestPerPort_ACurrentRetainedEntryIsNotRebuiltOnEverySnapshot: the other
// side of the per-entry stamp. A retained entry's virtual host is rebuilt by
// the snapshot build that finds it stale, and that build says so on the
// entry: the next one, with nothing changed, rebuilds nothing. (A snapshot is
// built on every pod event, and the stored virtual hosts are what keeps the
// route table's bytes from being built and hashed again.)
func TestPerPort_ACurrentRetainedEntryIsNotRebuiltOnEverySnapshot(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	front := a043Service{name: "front", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.1", vip: "10.96.5.1"}
	back := a043Service{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.2", vip: "10.96.5.2"}
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{front, back})
	c.SetStaticDependencies([]string{"demo/front", "demo/back"})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{front, back})))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{back})))
	vhostOfFront := func() *routev3.VirtualHost {
		c.clusterMu.RLock()
		defer c.clusterMu.RUnlock()
		for _, e := range c.clusters {
			if e.service == "demo/front" && e.vhost != nil {
				require.False(t, e.absentSince.IsZero(), "control: the front's entry is retained")
				return e.vhost
			}
		}
		require.Fail(t, "no entry of the front has a virtual host")
		return nil
	}
	// A rule event: every virtual host is stale, the retained one too.
	require.NoError(t, c.generateSnapshot(ctx))
	before := vhostOfFront()
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/front": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("rule")}}})
	require.NoError(t, c.generateSnapshot(ctx))
	rebuilt := vhostOfFront()
	require.NotSame(t, before, rebuilt, "control: the snapshot build after a rule event rebuilds the retained entry's virtual host")
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Same(t, rebuilt, vhostOfFront(), "nothing changed, and the retained entry's virtual host was rebuilt again")
	// Nor does a refresh that changes nothing make it stale.
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{back})))
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Same(t, rebuilt, vhostOfFront(), "a refresh that changed nothing made the retained entry's virtual host stale")
}
