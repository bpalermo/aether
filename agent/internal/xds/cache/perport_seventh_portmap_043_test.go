package cache

// Seventh adversarial review of the proposal-043 stack (head 7ecbd2a3). Tests
// adopted from the review, where each failed.

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// R7-N5b. The same retained entry, and a rule event this time. A change of a
// Service's port map advances the rule generation and asks for a refresh
// (SetServicePortMaps: signalDependencyChange, no snapshot build of its own).
// The refresh rebuilds the virtual hosts of the entries it LISTS and then
// records every virtual host as built at that generation
// (c.vhostRuleGen = loadRuleGen). A RETAINED entry was not rebuilt, and the
// snapshot build that follows finds nothing to do: its rule keeps the port
// map as it was.
//
// "back" is a mesh service behind a user's Service, 80 -> 8080 (HTTP). The
// rule of "front" names back:80. The front's pods go (retained). The user
// then points the Service's port 80 at 9090, which no pod declares: the
// backendRef is invalid (500, ResolvedRefs=False) on a node that starts now
// and on the capture table; the loopback and proxy-port tables still deliver
// it to the pods' 8080.
func TestPerPort_ARetainedEntryFollowsAPortMapChange(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	front := a043Service{name: "front", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.1", vip: "10.96.5.1"}
	back := a043Service{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.2"}
	host := "front.demo." + a043Mesh + ":8080"
	route := map[string][]proxy.GammaRoute{"demo/front": {{
		Matches:  []proxy.GammaMatch{{Prefix: "/"}},
		Backends: []proxy.GammaBackend{{Service: "demo/back", Cluster: "back.demo." + a043Mesh, Port: 80, Weight: 1}},
	}}}
	load := func(c *SnapshotCache, svcs ...a043Service) {
		// LoadClustersFromRegistry ends with a snapshot build, as in the agent.
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(svcs)))
	}
	answers := func(c *SnapshotCache) (loopback, proxyPort, captured r5Answer) {
		tables := a043Tables(t, c)
		require.NotNil(t, tables["cap_http_8080"])
		return r5Resolve(t, tables[proxy.OutboundHTTPRouteName], "demo", host, "/"),
			r5Resolve(t, tables["cap_http_18081"], "demo", host, "/"),
			r5Resolve(t, tables["cap_http_8080"], "demo", host, "/")
	}
	build := func(portMap map[uint32]uint32, retained bool) *SnapshotCache {
		c := a043Cache(t, nil)
		a043Project(c, []a043Service{front})
		c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/back": {80: 8080}})
		c.SetServiceRoutes(route)
		if retained {
			load(c, front, back)
			load(c, front, back)
			load(c, back) // the front's pods are gone: retained
		}
		// The event, exactly as the reconciler delivers it when only a
		// Service changed (the routes are the same: SetServiceRoutes is a
		// no-op), followed by the refresh it asks for.
		c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/back": portMap})
		c.SetServiceRoutes(route)
		load(c, back)
		load(c, back)
		return c
	}
	lb, pp, capt := answers(build(map[uint32]uint32{80: 8080}, true))
	require.True(t, lb.delivered && capt.delivered && pp.delivered, "control: with the port map unchanged every table delivers: %s / %s / %s", lb.sig, pp.sig, capt.sig)

	lb, pp, capt = answers(build(map[uint32]uint32{80: 9090}, true))
	flb, _, fcapt := answers(build(map[uint32]uint32{80: 9090}, false))
	t.Logf("port 80 of the user's Service now targets 9090 (no pod declares it): retained front: loopback %q, proxy port %q, captured :8080 %q; a node that starts now: loopback %q, captured %q",
		lb.sig, pp.sig, capt.sig, flb.sig, fcapt.sig)
	assert.Equal(t, capt.sig, lb.sig, "the loopback table against the dialled-port table of the same snapshot, two refreshes after the Service changed")
	assert.Equal(t, capt.sig, pp.sig, "the proxy-port table against the dialled-port table of the same snapshot")
	assert.Falsef(t, lb.delivered && !flb.delivered, "the loopback table DELIVERS the backendRef (%q) where a node that starts now does not (%q)", lb.sig, flb.sig)
}
