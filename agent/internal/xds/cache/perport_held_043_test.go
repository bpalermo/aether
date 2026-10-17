package cache

import (
	"context"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/xdseval"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPerPort_SnapshotPublishesATableForEveryChainItPublishes: every route
// table an HTTP chain of a published capture listener names is in the same
// snapshot. A chain whose table is missing warms for the whole initial-fetch
// timeout (#817). With the one-HCM capture listener the name is fixed
// (cap_http, the proxy port included since chart 3.0.2), so no port entering
// or leaving the mesh can leave a chain without its table; this holds the
// snapshot to it whatever the port set is, in both capture modes.
func TestPerPort_SnapshotPublishesATableForEveryChainItPublishes(t *testing.T) {
	ctx := context.Background()
	c := a043Cache(t, a043Fixture())
	for _, step := range []string{"fixture", "a port number leaves the mesh", "scoped capture"} {
		if step != "fixture" {
			c.SetServiceRoutes(nil)
			c.SetServicePortMaps(nil)
		}
		if step == "scoped capture" {
			// A cache that builds its listener in scoped mode from the start.
			c = newTestCache("node-1")
			c.SetCaptureEnabled(true)
			c.SetCaptureRedirectAll(false)
			require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: a043NS, ServiceAccount: "client", NetworkNamespace: "/var/run/netns/cni-client"}, a043Mesh))
			require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
			a043Project(c, a043Fixture())
			require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(a043Fixture())))
		}
		require.NoError(t, c.generateSnapshot(ctx))
		snap, err := c.GetSnapshot("node-1")
		require.NoError(t, err)
		routes := snap.GetResources(resourcev3.RouteType)
		named := 0
		for _, r := range snap.GetResources(resourcev3.ListenerType) {
			l := r.(*listenerv3.Listener)
			if l.GetName()[:min(8, len(l.GetName()))] != "capture_" {
				continue
			}
			for _, fc := range l.GetFilterChains() {
				for _, f := range fc.GetFilters() {
					hcm := &http_connection_managerv3.HttpConnectionManager{}
					if f.GetTypedConfig().UnmarshalTo(hcm) != nil {
						continue
					}
					named++
					assert.Contains(t, routes, hcm.GetRds().GetRouteConfigName(), "%s: chain %s names a table the snapshot does not publish", step, fc.GetName())
				}
			}
		}
		require.Equal(t, 1, named, "%s: one capture listener with its one HTTP chain", step)
		for name := range routes {
			_, perPort := capturePortOf(name)
			assert.False(t, perPort, "%s: a per-port table %s is published", step, name)
			assert.NotEqual(t, proxy.CaptureHTTPOtherRouteName, name, step)
		}
		assert.NotContains(t, routes, "cap_http_18081", "%s: the proxy port's table is compiled into cap_http, never published", step)
	}
}

// capTableHas reports whether a set of capture tables gives a request
// dialled to port, with Host domain, a virtual host of its own rather than the
// table's catch-all: in the per-port table of that port, or in the published
// cap_http.
func capTableHas(tables []types.Resource, port uint32, domain string) bool {
	for _, r := range tables {
		rc := r.(*routev3.RouteConfiguration)
		switch rc.GetName() {
		case proxy.CaptureHTTPRouteName(port):
			return findVHost(rc.GetVirtualHosts(), domain) != nil
		case proxy.CaptureHTTPChainName:
			vh := xdseval.PickVirtualHost(rc, domain)
			return vh != nil && vh.GetName() != "on_demand_catch_all"
		}
	}
	return false
}

// TestPerPort_TablesFollowADependencyThatExpiresByTheClock: the capture
// tables are kept between snapshots while their inputs are unchanged. The
// dependency set is one of them, and it changes with NO mutator running when
// an observed dependency passes its TTL: no generation moves. The kept tables
// must be dropped then, which only the identity of the dependency set says.
func TestPerPort_TablesFollowADependencyThatExpiresByTheClock(t *testing.T) {
	ctx := context.Background()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	c.observedTTL = 150 * time.Millisecond
	// A mesh Service without an address is no dependency by itself: it is in
	// the tables only while something depends on it. Here, an observation.
	c.SetCaptureAuthorities(map[string]string{"demo/obs": "obs.demo.svc.cluster.local"})
	const host = "obs.demo." + a043Mesh + ":8080"
	require.False(t, capTableHas(c.captureRouteTables([]uint32{8080}, nil), 8080, host), "not a dependency yet")

	c.ObserveDependency(ctx, "demo/obs")
	first := c.captureRouteTables([]uint32{8080}, nil)
	require.True(t, capTableHas(first, 8080, host), "observed: the name is in the table")
	again := c.captureRouteTables([]uint32{8080}, nil)
	require.Same(t, first[0], again[0], "unchanged inputs: the kept tables are returned")

	c.depMu.RLock()
	gen := c.depGen
	c.depMu.RUnlock()
	time.Sleep(300 * time.Millisecond)
	after := c.captureRouteTables([]uint32{8080}, nil)
	c.depMu.RLock()
	assert.Equal(t, gen, c.depGen, "nothing mutated: the expiry is the clock's alone")
	c.depMu.RUnlock()
	assert.False(t, capTableHas(after, 8080, host), "the observation expired and the kept tables still serve the name")
}

// TestPerPort_TablesFollowAGenerationAlone: the other half. A rule input that
// changes moves the generation and nothing else (the dependency set is the
// same map): the kept tables must be rebuilt.
func TestPerPort_TablesFollowAGenerationAlone(t *testing.T) {
	c := a043Cache(t, a043Fixture())
	const host = "multi.demo." + a043Mesh + ":8080"
	marks := func() []string {
		for _, r := range perPortView(t, c, []uint32{8080}) {
			if rc := r.(*routev3.RouteConfiguration); rc.GetName() == "cap_http_8080" {
				return rtpMarks(t, rc, host)
			}
		}
		return nil
	}
	require.Empty(t, marks())
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/multi": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("gen")}}})
	assert.Equal(t, []string{"gen"}, marks(), "a route event alone must rebuild the kept tables")
}

// TestServicePorts_TheRawTCPMeshPortIsNeverAnHTTPPort: 18082 is the mesh's
// raw-TCP port. The capture listener matches raw-TCP chains on that number,
// so it cannot also select an HTTP chain: a pod that declares 18082 as HTTP is
// not served on it as HTTP, and the derivation says why.
func TestServicePorts_TheRawTCPMeshPortIsNeverAnHTTPPort(t *testing.T) {
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	row := makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)
	row.PortProtocols = map[uint32]registryv1.PortProtocol{8080: h, 18082: h}
	row.Ports = []uint32{8080, 18082}
	got := deriveServicePorts(listedEndpoints{protocol: registryv1.Service_PROTOCOL_HTTP, endpoints: []*registryv1.ServiceEndpoint{row}})
	assert.Equal(t, []uint32{8080}, got.http, "18082 is not served as HTTP")
	assert.False(t, got.hasHTTP(18082))
	assert.Equal(t, []string{"18082: reserved for the mesh, not served (1 endpoints declare it)"}, got.conflicts)

	// Declared raw TCP it is not served either: the number is the mesh's.
	row.PortProtocols[18082] = registryv1.PortProtocol_PORT_PROTOCOL_TCP
	got = deriveServicePorts(
		listedEndpoints{protocol: registryv1.Service_PROTOCOL_HTTP, endpoints: []*registryv1.ServiceEndpoint{row}},
		listedEndpoints{protocol: registryv1.Service_PROTOCOL_TCP, endpoints: []*registryv1.ServiceEndpoint{row}},
	)
	assert.Empty(t, got.tcp)
	assert.Equal(t, []uint32{8080}, got.http)
	assert.Equal(t, []string{"18082: reserved for the mesh, not served (1 endpoints declare it)"}, got.conflicts)
}

// TestPerPort_RetainedEntryOfAPortThatStoppedBeingHTTP: a service that served
// HTTP on 8080 becomes raw-TCP-only on the same number. Its HTTP default
// entry is not retained: its cluster would resolve through the bare service
// name, which the live raw-TCP floor now publishes, and an HTTP request to
// the service would be delivered to the raw-TCP endpoints. (It used to be
// retained without its virtual host; the rows are evidence of a replacement,
// retentionEvidence, and nothing of the port is kept.)
func TestPerPort_RetainedEntryOfAPortThatStoppedBeingHTTP(t *testing.T) {
	ctx := context.Background()
	pods := []rtpPod{{svc: "demo/x", ip: "10.0.8.1", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: rtpH}}}
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{{name: "x", ns: a043NS, vip: "10.96.8.1"}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(pods)))
	const host = "x.demo." + a043Mesh + ":8080"
	got := a043Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], host)
	require.Equal(t, "x.demo."+a043Mesh, got.cluster, "control: routed while 8080 is HTTP")

	pods[0].ports = map[uint32]registryv1.PortProtocol{8080: rtpTCP}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", rtpRegistry(pods)))
	c.clusterMu.RLock()
	_, ok := c.clusters["demo/x"]
	c.clusterMu.RUnlock()
	assert.False(t, ok, "the HTTP default entry is kept for a port the service now serves as raw TCP")
	for _, table := range []string{proxy.OutboundHTTPRouteName, "cap_http_8080"} {
		rc := a043Tables(t, c)[table]
		if rc == nil {
			continue // no service declares 8080 as HTTP any more: no such table
		}
		got := a043Resolve(t, rc, host)
		assert.Emptyf(t, got.cluster, "%s: Host %s is still routed, to %q", table, host, got.cluster)
	}
}
