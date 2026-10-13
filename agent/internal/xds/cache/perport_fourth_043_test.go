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

// TestPerPort_RuleEventDoesNotGiveARetainedEntryItsAuthoritiesBack: a rule
// event between two registry refreshes rebuilds the loopback virtual hosts,
// and a RETAINED entry must give way to a live entry of the service then as
// it does in a refresh (yieldClaimedDomains).
//
// The state: a service behind a user's Service whose port map has no port for
// pod port 80. It serves 80 (HTTP) and 9000 (raw TCP), and the port its pods
// declare moves from 80 to 9000. The default HTTP entry (pod port 80) is
// retained, and so far claims nothing: its virtual host exists and has no
// domain, like the live per-port entry of 80. Then the Service goes (a port
// map event). Rebuilt like a live entry, the retained one claimed
// "<fqdn>:80" and the port-less name beside the live entry of port 80: two
// virtual hosts with one domain, or a route to the retained default cluster,
// whose endpoints are by then the raw-TCP floor's.
//
// (The first version of this test started from a retained entry that had
// ALREADY lost its virtual host, which a rule event does not touch: it passed
// on the code before the fix.)
func TestPerPort_RuleEventDoesNotGiveARetainedEntryItsAuthoritiesBack(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	ports := map[uint32]registryv1.PortProtocol{80: h, 9000: tcp}
	before := a043Service{name: "echo", ns: "demo", legacy: 80, ports: ports, ip: "10.0.9.1"}
	after := a043Service{name: "echo", ns: "demo", legacy: 9000, ports: ports, ip: "10.0.9.1"}
	const fqdn = "echo.demo." + a043Mesh

	c := a043Cache(t, nil)
	// A route makes the service a dependency and its Service's port map count.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}}}})
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/echo": {8000: 8080}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{before})))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{after})))
	require.NoError(t, c.generateSnapshot(ctx))

	entry := func(name string) clusterEntry {
		c.clusterMu.RLock()
		defer c.clusterMu.RUnlock()
		e, ok := c.clusters[name]
		require.Truef(t, ok, "no entry %q", name)
		return e
	}
	retained := entry("demo/echo")
	require.False(t, retained.absentSince.IsZero(), "the default entry of pod port 80 is retained, or the test shows nothing")
	require.NotNil(t, retained.vhost, "the retained entry still has a virtual host (it claims nothing yet), or a rule event does not touch it and the test shows nothing")
	require.Empty(t, retained.vhost.GetDomains())
	live := entry(fqdn + ":80")
	require.True(t, live.absentSince.IsZero(), "the live per-port entry of 80")

	// The Service goes: the service's ports are its pods' own again.
	c.SetServicePortMaps(map[string]map[uint32]uint32{})
	require.NoError(t, c.generateSnapshot(ctx))

	live = entry(fqdn + ":80")
	require.Contains(t, live.vhost.GetDomains(), fqdn+":80", "control: the live entry of 80 claims its authority after the event")
	retained = entry("demo/echo")
	assert.Emptyf(t, retained.vhost.GetDomains(), "the retained entry claims %v beside the live entry of the same port", retained.vhost.GetDomains())
	for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
		// a043Resolve fails the test on a domain claimed twice.
		got := a043Resolve(t, a043Tables(t, c)[tname], fqdn+":80")
		assert.Equalf(t, fqdn+":80", got.cluster, "%s: Host %s:80 is not answered by the live entry of port 80: %+v", tname, fqdn, got)
		got = a043Resolve(t, a043Tables(t, c)[tname], fqdn)
		assert.NotEqualf(t, fqdn, got.cluster, "%s: the port-less name is routed to the retained default cluster, whose endpoints are the raw-TCP floor's", tname)
	}
}

// TestPerPort_RuleEventReachesARetainedEntry: a retained entry is rebuilt on a
// rule event like any other. A service whose rows are gone and whose entry is
// kept for the grace gets a service-wide filter: the next snapshot's loopback
// table runs it on the retained virtual host. (Skipping the retained entries
// in the rebuild left them with the rules of the refresh that retained them.)
func TestPerPort_RuleEventReachesARetainedEntry(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	echo := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	c := a043Cache(t, []a043Service{echo})
	a043ProjectNamesOnly(c, []a043Service{echo})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	require.NoError(t, c.generateSnapshot(ctx))
	c.clusterMu.RLock()
	e, ok := c.clusters["demo/echo"]
	c.clusterMu.RUnlock()
	require.True(t, ok && !e.absentSince.IsZero() && len(e.vhost.GetDomains()) > 0, "the entry is retained with its virtual host, or the test shows nothing")
	const host = "echo.demo." + a043Mesh + ":8080"
	before := r3Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], "demo", host, nil)
	require.NotContains(t, before.first, r3Filter, "control: no filter yet")

	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": r3ChainCfg(t)})
	require.NoError(t, c.generateSnapshot(ctx))
	for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
		after := r3Resolve(t, a043Tables(t, c)[tname], "demo", host, nil)
		assert.Containsf(t, after.first, r3Filter, "%s: a filter attached while the service's entry is retained is not in force: %s", tname, after.first)
	}
}

// TestPerPort_AListedServiceKeepsItsRetainedFloor: the reset of a service to
// what its generated Service declares is for a service NO row is listed of.
// One that is listed is what its rows say, whatever its Service says (the
// Service is derived from the registry, later): it loses its raw-TCP port in
// the rows, and the floor of that port keeps its grace, as before.
func TestPerPort_AListedServiceKeepsItsRetainedFloor(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	both := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}, ip: "10.0.9.1", vip: "10.96.9.1"}
	httpOnly := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	// Its Service declares something else again: 9090, which no row has.
	stale := a043Service{name: "echo", ns: "demo", legacy: 9090, ports: map[uint32]registryv1.PortProtocol{9090: h}, ip: "10.0.9.1", vip: "10.96.9.1"}

	c := a043Cache(t, []a043Service{both})
	a043Project(c, []a043Service{stale})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{httpOnly})))
	require.NoError(t, c.generateSnapshot(ctx))

	c.clusterMu.RLock()
	floor, ok := c.clusters[proxy.TCPClusterName("demo/echo", a043Mesh)]
	ports := c.servicePorts["demo/echo"]
	_, http := c.clusters["demo/echo"]
	c.clusterMu.RUnlock()
	require.True(t, ok, "the raw-TCP floor of a listed service was dropped on its Service's word")
	assert.False(t, floor.absentSince.IsZero(), "the floor is retained for its grace")
	assert.True(t, http, "the live HTTP entry of a listed service")
	assert.Equal(t, "8080 (http)", ports.describe(), "a listed service is what its rows say")
	assert.Equal(t, "echo.demo."+a043Mesh, a043Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], "echo.demo."+a043Mesh+":8080").cluster)
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

// TestPerPort_ServiceWhoseServiceGainsARawTCPPortAnswersAsAFreshNode: the
// Service's word replaces what the node remembers whenever the two differ,
// and not only in their HTTP ports. A service retained with 8080 (HTTP) whose
// Service then declares 8080 and a raw-TCP 5432: a node that starts there
// refuses "<name>:5432" as "not HTTP", and so must this one (it answered
// "no such port", by what it remembered).
func TestPerPort_ServiceWhoseServiceGainsARawTCPPortAnswersAsAFreshNode(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	before := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	after := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 5432: tcp}, ip: "10.0.9.2", vip: "10.96.9.1"}

	c := a043Cache(t, []a043Service{before})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	a043Project(c, []a043Service{after})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	require.NoError(t, c.generateSnapshot(ctx))

	fresh := a043Cache(t, nil)
	a043Project(fresh, []a043Service{after})
	require.NoError(t, fresh.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	require.NoError(t, fresh.generateSnapshot(ctx))

	const host = "echo.demo." + a043Mesh + ":5432"
	want := a043Resolve(t, a043Tables(t, fresh)[proxy.OutboundHTTPRouteName], host)
	require.Equal(t, proxy.RefusalPortNotHTTP, want.refusal, "control: what a node that starts in this state answers")
	got := a043Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], host)
	assert.Equal(t, want.refusal, got.refusal, "after the transitions: %+v", got)
	for _, port := range []string{":8080", ""} {
		a := r3Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], "demo", "echo.demo."+a043Mesh+port, nil)
		b := r3Resolve(t, a043Tables(t, fresh)[proxy.OutboundHTTPRouteName], "demo", "echo.demo."+a043Mesh+port, nil)
		assert.Equalf(t, b.first, a.first, "Host echo.demo.%s%s", a043Mesh, port)
	}
}
