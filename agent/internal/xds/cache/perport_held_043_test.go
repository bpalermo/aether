package cache

import (
	"context"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestPerPort_SnapshotPublishesATableForEveryChainItPublishes: the route
// tables of a snapshot are those of the node's HTTP port set AND of every port
// a cap_http_<P> chain of a listener IN THAT SNAPSHOT names. A listener built
// against a port set that has since moved (the listeners are rebuilt when the
// set changes, on another path than the tables) must still find its table: a
// chain whose table is missing warms for the whole initial-fetch timeout.
//
// Here a published capture listener names a port outside the current set.
func TestPerPort_SnapshotPublishesATableForEveryChainItPublishes(t *testing.T) {
	ctx := context.Background()
	c := a043Cache(t, a043Fixture())
	require.NoError(t, c.generateSnapshot(ctx))
	require.NotContains(t, c.captureHTTPPorts(), uint32(1234))

	// The stored capture listener gains a chain for a port the set does not
	// hold. The set itself is unchanged, so nothing rebuilds the listener.
	c.listenerMu.Lock()
	var touched int
	for netns, entry := range c.listeners {
		l, ok := entry.capture.(*listenerv3.Listener)
		if !ok || l == nil {
			continue
		}
		l = proto.Clone(l).(*listenerv3.Listener)
		l.FilterChains = append(l.FilterChains, &listenerv3.FilterChain{Name: proxy.CaptureHTTPRouteName(1234)})
		entry.capture = l
		c.listeners[netns] = entry
		touched++
	}
	c.listenerMu.Unlock()
	require.Equal(t, 1, touched, "the fixture has one pod")

	require.NoError(t, c.generateSnapshot(ctx))
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	var named bool
	for _, r := range snap.GetResources(resourcev3.ListenerType) {
		for _, fc := range r.(*listenerv3.Listener).GetFilterChains() {
			named = named || fc.GetName() == "cap_http_1234"
		}
	}
	require.True(t, named, "the snapshot must carry the listener with the extra chain, or the test shows nothing")
	assert.Contains(t, snap.GetResources(resourcev3.RouteType), "cap_http_1234", "a chain this snapshot publishes names a route table it does not publish")
}

// capTableHas reports whether a set of capture tables holds a virtual host
// that claims domain in the table of port.
func capTableHas(tables []types.Resource, port uint32, domain string) bool {
	for _, r := range tables {
		rc := r.(*routev3.RouteConfiguration)
		if rc.GetName() != proxy.CaptureHTTPRouteName(port) {
			continue
		}
		return findVHost(rc.GetVirtualHosts(), domain) != nil
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
	require.False(t, capTableHas(c.captureRouteTables([]uint32{8080}), 8080, host), "not a dependency yet")

	c.ObserveDependency(ctx, "demo/obs")
	first := c.captureRouteTables([]uint32{8080})
	require.True(t, capTableHas(first, 8080, host), "observed: the name is in the table")
	again := c.captureRouteTables([]uint32{8080})
	require.Same(t, first[0], again[0], "unchanged inputs: the kept tables are returned")

	c.depMu.RLock()
	gen := c.depGen
	c.depMu.RUnlock()
	time.Sleep(300 * time.Millisecond)
	after := c.captureRouteTables([]uint32{8080})
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
		for _, r := range c.captureRouteTables([]uint32{8080}) {
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
