package cache

import (
	"context"
	"fmt"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fourth review of proposal 043: the cases its reproducers and its
// surviving mutations asked for, each as a test of its own.

// TestPerPort_ServiceReplacedByItsServiceAnswersAsAFreshNode: a service with
// an HTTP port, a raw-TCP port and a service-wide filter loses every row (its
// pod restarts), and its generated Service then declares OTHER ports. The
// node's answers must be the ones of a node that starts in that state: the
// evidence decides, not the history. Before, what the node remembered of the
// service lost the replaced HTTP port and kept the rest, so a port the Service
// newly declared was refused 421 port-not-declared where a fresh node routes
// it, with the filter.
func TestPerPort_ServiceReplacedByItsServiceAnswersAsAFreshNode(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	before := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}, ip: "10.0.9.1", vip: "10.96.9.1"}
	for name, after := range map[string]a043Service{
		"another HTTP port, the raw-TCP port kept": {name: "echo", ns: "demo", legacy: 9090, ports: map[uint32]registryv1.PortProtocol{9090: h, 9000: tcp}, ip: "10.0.9.2", vip: "10.96.9.1"},
		"another HTTP port only":                   {name: "echo", ns: "demo", legacy: 9090, ports: map[uint32]registryv1.PortProtocol{9090: h}, ip: "10.0.9.2", vip: "10.96.9.1"},
		"the HTTP port becomes raw TCP":            {name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: tcp, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.1"},
	} {
		t.Run(name, func(t *testing.T) {
			rules := func(c *SnapshotCache) {
				c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": r3ChainCfg(t)})
				c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("rule")}}})
			}
			c := a043Cache(t, []a043Service{before})
			rules(c)
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{before})))
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
			a043Project(c, []a043Service{after})
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
			require.NoError(t, c.generateSnapshot(ctx))

			fresh := a043Cache(t, nil)
			rules(fresh)
			a043Project(fresh, []a043Service{after})
			require.NoError(t, fresh.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
			require.NoError(t, fresh.generateSnapshot(ctx))

			got, want := a043Tables(t, c), a043Tables(t, fresh)
			compared := 0
			for tname, table := range want {
				require.NotNilf(t, got[tname], "table %s exists on a fresh node only", tname)
				for _, host := range []string{
					"echo.demo." + a043Mesh + ":8080", "echo.demo." + a043Mesh + ":9090", "echo.demo." + a043Mesh + ":9000", "echo.demo." + a043Mesh,
					"echo:8080", "echo:9090", "echo.demo.svc.cluster.local:8080", "echo.demo.svc.cluster.local:9090",
				} {
					a, b := r3Resolve(t, got[tname], "demo", host, nil), r3Resolve(t, table, "demo", host, nil)
					assert.Equalf(t, b.first, a.first, "%s, Host %s: after the transitions vs a node that starts in this state", tname, host)
					compared++
				}
			}
			assert.Len(t, got, len(want))
			require.NotZero(t, compared)
			// Not vacuous: the port the Service newly declares is routed with
			// the filter on the loopback table.
			sig := r3Resolve(t, got[proxy.OutboundHTTPRouteName], "demo", "echo.demo."+a043Mesh+":9090", nil)
			assert.Contains(t, sig.first, "cluster(", "the newly declared port is not routed: %s", sig.first)
			assert.Contains(t, sig.first, r3Filter)
		})
	}
}

// TestPerPort_RuleEventDoesNotGiveARetainedEntryItsAuthoritiesBack: a
// retained entry that gave its authorities up to a live entry of the service
// (yieldClaimedDomains) stays without them when a rule event rebuilds the
// virtual hosts between two registry refreshes. It got all of them back: for
// that one snapshot "<fqdn>:80" and the port-less name were claimed by the
// retained default cluster, whose endpoints are the raw-TCP floor's.
func TestPerPort_RuleEventDoesNotGiveARetainedEntryItsAuthoritiesBack(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	ports := map[uint32]registryv1.PortProtocol{80: h, 9000: tcp}
	before := a043Service{name: "echo", ns: "demo", legacy: 80, ports: ports, ip: "10.0.9.1", vip: "10.96.9.1"}
	after := a043Service{name: "echo", ns: "demo", legacy: 9000, ports: ports, ip: "10.0.9.1", vip: "10.96.9.1"}
	c := a043Cache(t, []a043Service{before})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{after})))

	retained := func() (clusterEntry, bool) {
		c.clusterMu.RLock()
		defer c.clusterMu.RUnlock()
		e, ok := c.clusters["demo/echo"]
		return e, ok && !e.absentSince.IsZero()
	}
	e, ok := retained()
	require.True(t, ok, "the default entry of port 80 is retained (the service still declares 80 as HTTP), or the test shows nothing")
	require.Nil(t, e.vhost, "control: the refresh took the retained entry's authorities away")

	for _, event := range []struct {
		name string
		do   func()
	}{
		{"a port map", func() { c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/unrelated": {80: 8080}}) }},
		{"a route", func() {
			c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/unrelated": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}}}})
		}},
		{"a service-wide filter", func() { c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/unrelated": r3ChainCfg(t)}) }},
	} {
		event.do()
		require.NoError(t, c.generateSnapshot(ctx))
		e, ok = retained()
		require.True(t, ok)
		assert.Nilf(t, e.vhost, "after %s event the retained entry claims %v again", event.name, e.vhost.GetDomains())
		for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
			for _, host := range []string{"echo.demo." + a043Mesh + ":80", "echo.demo." + a043Mesh} {
				got := a043Resolve(t, a043Tables(t, c)[tname], host)
				assert.NotEqualf(t, "echo.demo."+a043Mesh, got.cluster, "after %s event, %s Host %s is routed to the retained default cluster, whose endpoints are the raw-TCP floor's", event.name, tname, host)
			}
		}
	}
}

// TestPerPort_RefusalsOfAUserServiceNameItsServicePorts: on the loopback
// table the port in an authority is the Service's. For a name a user's
// Service owns, a raw-TCP pod port is refused "port-not-http" under the
// SERVICE port in front of it; the pod port's own number is no port of the
// name. (Naming the pod port there refused the wrong authority by the wrong
// reason.)
func TestPerPort_RefusalsOfAUserServiceNameItsServicePorts(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	x := a043Service{name: "x", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}, ip: "10.0.9.1"}
	c := a043Cache(t, nil)
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/x": {80: 8080, 5432: 9000, 5433: 9000}})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/x": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}}}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{x})))
	require.NoError(t, c.generateSnapshot(ctx))
	table := a043Tables(t, c)[proxy.OutboundHTTPRouteName]
	for _, sp := range []uint32{5432, 5433} {
		got := a043Resolve(t, table, fmt.Sprintf("x.demo.%s:%d", a043Mesh, sp))
		assert.Equalf(t, proxy.RefusalPortNotHTTP, got.refusal, "Service port %d, in front of a raw-TCP pod port: %+v", sp, got)
	}
	got := a043Resolve(t, table, fmt.Sprintf("x.demo.%s:9000", a043Mesh))
	assert.Equalf(t, proxy.RefusalPortNotDeclared, got.refusal, "9000 is the pod port's number, not a port of the Service: %+v", got)
	assert.NotEmpty(t, a043Resolve(t, table, fmt.Sprintf("x.demo.%s:80", a043Mesh)).cluster, "control: the HTTP Service port is routed")
}

// TestPerPort_DeclaredPortsAreARuleInput: what a generated Service declares
// decides whether a backendRef to a service the node holds no endpoint of is
// valid. A change of it rebuilds the entries' loopback virtual hosts at the
// next snapshot, like any rule event, with no registry refresh in between.
func TestPerPort_DeclaredPortsAreARuleInput(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	front := a043Service{name: "front", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.1", vip: "10.96.5.1"}
	back := a043Service{name: "back", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.2", vip: "10.96.5.2"}
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{front, back})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/front": {{
		Matches:  []proxy.GammaMatch{{Prefix: "/"}},
		Backends: []proxy.GammaBackend{{Service: "demo/back", Cluster: "back.demo." + a043Mesh, Port: 9090, Weight: 1}},
	}}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{front})))
	require.NoError(t, c.generateSnapshot(ctx))
	const host = "front.demo." + a043Mesh + ":8080"
	tables := []string{proxy.OutboundHTTPRouteName, "cap_http_18081", "cap_http_8080"}
	for _, tname := range tables {
		got := a043Resolve(t, a043Tables(t, c)[tname], host)
		require.Equalf(t, uint32(500), got.status, "control, %s: back declares 8080 only: %+v", tname, got)
	}

	// back's generated Service now declares 9090 too. Nothing else happens.
	back.ports = map[uint32]registryv1.PortProtocol{8080: h, 9090: h}
	a043Project(c, []a043Service{front, back})
	require.NoError(t, c.generateSnapshot(ctx))
	for _, tname := range tables {
		got := a043Resolve(t, a043Tables(t, c)[tname], host)
		assert.Equalf(t, "back.demo."+a043Mesh+":9090", got.cluster, "%s: the backendRef is valid now, and the table still answers by the Service's old ports: %+v", tname, got)
	}
}

// TestPerPort_APortMapAloneMakesNoColdName: the agent holds the port map of
// EVERY Service in the cluster. One that carries no route and no service-wide
// filter, and that the node holds no endpoint of, gets no stand-in on the
// loopback table: its mesh name is a name nothing is known of (the catch-all),
// and the table does not grow by a virtual host per Service port of the
// cluster.
func TestPerPort_APortMapAloneMakesNoColdName(t *testing.T) {
	ctx := context.Background()
	c := a043Cache(t, nil)
	require.NoError(t, c.generateSnapshot(ctx))
	before := len(a043Tables(t, c)[proxy.OutboundHTTPRouteName].GetVirtualHosts())

	maps := map[string]map[uint32]uint32{}
	for i := range 25 {
		maps[fmt.Sprintf("demo/plain-%d", i)] = map[uint32]uint32{80: 8080, 443: 8443}
	}
	c.SetServicePortMaps(maps)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	require.NoError(t, c.generateSnapshot(ctx))
	table := a043Tables(t, c)[proxy.OutboundHTTPRouteName]
	assert.Len(t, table.GetVirtualHosts(), before, "a virtual host was added for a Service that is only a port map")
	for _, host := range []string{"plain-3.demo." + a043Mesh + ":80", "plain-3.demo." + a043Mesh + ":8080", "plain-3.demo." + a043Mesh} {
		assert.Equalf(t, "on_demand_catch_all", r3Pick(t, table, host).GetName(), "Host %s", host)
	}

	// The control: the same Service with a service-wide filter IS a name of
	// the mesh, routed and refused by its port map.
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/plain-3": r3ChainCfg(t)})
	require.NoError(t, c.generateSnapshot(ctx))
	table = a043Tables(t, c)[proxy.OutboundHTTPRouteName]
	assert.NotEqual(t, "on_demand_catch_all", r3Pick(t, table, "plain-3.demo."+a043Mesh+":80").GetName())
	assert.Equal(t, "on_demand_catch_all", r3Pick(t, table, "plain-4.demo."+a043Mesh+":80").GetName())
	_ = capture.CaptureTCPService{}
}
