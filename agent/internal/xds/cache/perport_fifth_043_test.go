package cache

// Fifth adversarial pass on the proposal-043 stack (head 3990f748). Tests
// adopted from the review.

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// R5-1. A service whose rows the node LISTS before the service is one of its
// dependencies (a listing that is not scoped to the dependency set: the
// registry client's RPC fallback while its watch is not ready, or a registry
// without watch scoping) has its ports derived and no cluster entry. When a
// service-wide filter or a route then makes it a dependency, the loopback
// table gives it NO virtual host until the next registry refresh: the cold
// stand-in is gated on "ports not known", the refusals on "has an entry".
// Its authority falls to the on-demand catch-all, without the filter and
// without the rule, while the capture table of the same snapshot has both.
func TestPerPort_ListedBeforeItIsADependency(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	echo := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	for _, withService := range []bool{false, true} {
		name := "a user's Service owns the name"
		if withService {
			name = "its generated Service is seen after the listing"
		}
		t.Run(name, func(t *testing.T) {
			c := a043Cache(t, nil)
			require.NotContains(t, c.DependencySet(), "demo/echo", "control: not a dependency yet")
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{echo})))
			// The fix itself, held directly (what follows only says what must
			// not be delivered, and nothing is once the classifier of the
			// loopback table exists, with or without this): the ports of a
			// service that is not a dependency are not derived at all.
			_, known := c.PodPortClasses(ctx, "demo/echo")
			require.False(t, known, "the node derived the ports of a service that is not one of its dependencies")

			c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": r3ChainCfg(t)})
			c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("rule")}}})
			if withService {
				a043Project(c, []a043Service{echo})
			}
			require.NoError(t, c.generateSnapshot(ctx))
			require.Contains(t, c.DependencySet(), "demo/echo")
			tables := a043Tables(t, c)

			// (No such table once the service's ports are not known before it
			// is a dependency: then nothing of it is.)
			if table := tables["cap_http_8080"]; table != nil {
				ctl := r5Resolve(t, table, "demo", "echo.demo."+a043Mesh+":8080", "/")
				require.Truef(t, ctl.delivered && ctl.filter, "control: the capture table of the same snapshot runs the filter: %s", ctl.sig)
			}

			for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
				for _, host := range []string{"echo.demo." + a043Mesh + ":8080", "echo:8080"} {
					a := r5Resolve(t, tables[tname], "demo", host, "/")
					if a.delivered {
						assert.Truef(t, a.filter, "%s Host %s is delivered without the service-wide filter: %s (vhost %q)", tname, host, a.sig, a.vhost)
						assert.Containsf(t, a.marks, "rule", "%s Host %s is delivered without the HTTPRoute rule: %s (vhost %q)", tname, host, a.sig, a.vhost)
					}
				}
			}

			// The refresh the filter event asks for repairs it.
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{echo})))
			require.NoError(t, c.generateSnapshot(ctx))
			after := r5Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], "demo", "echo.demo."+a043Mesh+":8080", "/")
			require.Truef(t, after.delivered && after.filter, "control: after the next refresh: %s", after.sig)
		})
	}
}

// R5-2. What is left of N2. The fix resets a service to what its generated
// Service declares only when that Service REPLACES one of the retained HTTP
// entries. When it replaces none, what the node remembered still decides for
// the whole grace (90 s), and a port the Service newly declares is refused
// 421 port-not-declared where a node that starts in the same state routes it.
func TestPerPort_NewlyDeclaredPortDuringTheGrace(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	for name, tc := range map[string]struct {
		before, after map[uint32]registryv1.PortProtocol
	}{
		"the HTTP port is kept, another is added": {map[uint32]registryv1.PortProtocol{8080: h}, map[uint32]registryv1.PortProtocol{8080: h, 9090: h}},
		"a raw-TCP service gains an HTTP port":    {map[uint32]registryv1.PortProtocol{9000: tcp}, map[uint32]registryv1.PortProtocol{9000: tcp, 9090: h}},
	} {
		t.Run(name, func(t *testing.T) {
			legacy := uint32(8080)
			if _, ok := tc.before[8080]; !ok {
				legacy = 9000
			}
			before := a043Service{name: "echo", ns: "demo", legacy: legacy, ports: tc.before, ip: "10.0.9.1", vip: "10.96.9.1"}
			after := a043Service{name: "echo", ns: "demo", legacy: legacy, ports: tc.after, ip: "10.0.9.2", vip: "10.96.9.1"}
			filter := func(c *SnapshotCache) {
				c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": r3ChainCfg(t)})
			}
			c := a043Cache(t, []a043Service{before})
			filter(c)
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{before})))
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
			a043Project(c, []a043Service{after})
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
			require.NoError(t, c.generateSnapshot(ctx))

			fresh := a043Cache(t, nil)
			filter(fresh)
			a043Project(fresh, []a043Service{after})
			require.NoError(t, fresh.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
			require.NoError(t, fresh.generateSnapshot(ctx))

			got, want := a043Tables(t, c), a043Tables(t, fresh)
			for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081", "cap_http_9090"} {
				require.NotNil(t, want[tname], tname)
				require.NotNilf(t, got[tname], "table %s exists on a fresh node only", tname)
				host := "echo.demo." + a043Mesh + ":9090"
				a, b := r5Resolve(t, got[tname], "demo", host, "/"), r5Resolve(t, want[tname], "demo", host, "/")
				assert.Equalf(t, b.sig, a.sig, "%s Host %s: after the transitions vs a node that starts in this state", tname, host)
			}
		})
	}
}

// R5-3. The data-plane half of what F7's status says nothing of: pods of a
// service behind a USER's Service disagree about the class of the target port
// (a rollout that retypes it). The table answers by the majority: with two
// pods declaring raw TCP and one HTTP, a backendRef to that port is invalid
// and answered 500. (The status half is in the gamma package's review test:
// the port stays "not checked", ResolvedRefs=True.)
func TestPerPort_BackendPortThePodsDisagreeAbout(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	front := a043Service{name: "front", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.1", vip: "10.96.5.1"}
	pods := []rtpPod{
		{svc: "demo/back", ip: "10.0.6.1", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: tcp}},
		{svc: "demo/back", ip: "10.0.6.2", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: tcp}},
		{svc: "demo/back", ip: "10.0.6.3", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}},
		{svc: "demo/front", ip: front.ip, legacy: 8080, ports: front.ports},
	}
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{front})
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/back": {80: 8080}})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/front": {{
		Matches:  []proxy.GammaMatch{{Prefix: "/"}},
		Backends: []proxy.GammaBackend{{Service: "demo/back", Cluster: "back.demo." + a043Mesh, Port: 80, Weight: 1}},
	}}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(pods)))
	require.NoError(t, c.generateSnapshot(ctx))
	for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_8080"} {
		got := a043Resolve(t, a043Tables(t, c)[tname], "front.demo."+a043Mesh+":8080")
		assert.Equalf(t, uint32(500), got.status, "%s: %+v", tname, got)
	}
	// What the route's status is judged by is this very class
	// (PodPortClasses hands out what the tables are built from): the status
	// says UnsupportedProtocol where the table answers 500.
	classes, known := c.PodPortClasses(ctx, "demo/back")
	require.True(t, known)
	assert.Equal(t, map[uint32]registryv1.PortProtocol{8080: tcp}, classes, "the status is handed another class than the one the table serves the port as")
}

// TestPodPortClasses_IsWhatTheTablesAreBuiltFrom: PodPortClasses, which a
// route's status is judged by, is servicePorts and nothing else, for every
// service and every shape: one class per port (the majority's where pods
// disagree), no port reserved for the mesh, "not known" exactly where the node
// derived nothing.
func TestPodPortClasses_IsWhatTheTablesAreBuiltFrom(t *testing.T) {
	ctx := context.Background()
	h, tcp, udp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP, registryv1.PortProtocol_PORT_PROTOCOL_UDP
	pods := []rtpPod{
		{svc: "demo/mixed", ip: "10.0.6.1", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp, 5353: udp}},
		{svc: "demo/split", ip: "10.0.6.2", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: tcp}},
		{svc: "demo/split", ip: "10.0.6.3", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: tcp}},
		{svc: "demo/split", ip: "10.0.6.4", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}},
		{svc: "demo/tie", ip: "10.0.6.5", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: tcp}},
		{svc: "demo/tie", ip: "10.0.6.6", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}},
	}
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{{name: "mixed", ns: "demo", vip: "10.96.6.1"}, {name: "split", ns: "demo", vip: "10.96.6.2"}, {name: "tie", ns: "demo", vip: "10.96.6.3"}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(pods)))

	want := map[string]map[uint32]registryv1.PortProtocol{
		"demo/mixed": {8080: h, 9000: tcp, 5353: udp},
		"demo/split": {8080: tcp}, // most pods
		"demo/tie":   {8080: h},   // a tie goes to HTTP
	}
	for svc, classes := range want {
		got, known := c.PodPortClasses(ctx, svc)
		require.Truef(t, known, "%s", svc)
		assert.Equalf(t, classes, got, "%s", svc)
		// And it is servicePorts: the same answer the tables read.
		c.clusterMu.RLock()
		sp := c.servicePorts[svc]
		c.clusterMu.RUnlock()
		for port, class := range got {
			assert.Equalf(t, class, sp.classOf(port), "%s port %d", svc, port)
		}
		assert.Lenf(t, got, len(sp.http)+len(sp.tcp)+len(sp.udp), "%s", svc)
	}
	_, known := c.PodPortClasses(ctx, "demo/nothing")
	assert.False(t, known, "a service the node derived nothing for")
}

// TestPerPort_ParentRefToAUserServicePortNoPodDeclaresAttachesNothing: a route
// whose parentRef names a port of a user's Service whose target pod port no
// pod declares (80 -> 7070, the pods serve 8080). The status says
// Accepted=False for it (agent/internal/gamma); this is the table's half, run:
// the rule is placed nowhere, and the port itself is refused by name.
func TestPerPort_ParentRefToAUserServicePortNoPodDeclaresAttachesNothing(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	x := a043Service{name: "x", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1"}
	c := a043Cache(t, nil)
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/x": {80: 7070, 8000: 8080}})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/x": {{
		ParentPort: 80, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("on-80"),
	}}})
	c.SetRouteTargetPorts(map[string][]uint32{"demo/x": {80}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{x})))
	require.NoError(t, c.generateSnapshot(ctx))
	classes, known := c.PodPortClasses(ctx, "demo/x")
	require.True(t, known)
	require.NotContains(t, classes, uint32(7070), "control: no pod declares the target port")

	tables := a043Tables(t, c)
	for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
		got := a043Resolve(t, tables[tname], "x.demo."+a043Mesh+":80")
		assert.Emptyf(t, got.cluster, "%s: Service port 80, whose target port no pod declares, is routed: %+v", tname, got)
		assert.Equalf(t, uint32(421), got.status, "%s: %+v", tname, got)
		assert.Emptyf(t, rtpMarks(t, tables[tname], "x.demo."+a043Mesh+":80"), "%s: the rule is attached to the port", tname)
		// The Service's other port is served, without the rule of port 80.
		other := a043Resolve(t, tables[tname], "x.demo."+a043Mesh+":8000")
		assert.NotEmptyf(t, other.cluster, "%s: control, the Service port whose target the pods declare: %+v", tname, other)
		assert.Emptyf(t, rtpMarks(t, tables[tname], "x.demo."+a043Mesh+":8000"), "%s: the rule of port 80 is on port 8000", tname)
	}
}
