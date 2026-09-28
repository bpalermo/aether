package cache

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// assertOwnEDSWithBaseMembership is the shared check of the aether#1013 gate:
// the cluster `name` is in the snapshot, subscribes to an EDS name that is its
// own (never the bare service's, which the default HTTP cluster holds), and
// the SAME snapshot carries a ClusterLoadAssignment under that name whose
// endpoints, named endpoints and policy are the bare one's.
func assertOwnEDSWithBaseMembership(t *testing.T, snap cachev3.ResourceSnapshot, name, bareEDS string) {
	t.Helper()
	clusters := snap.GetResources(resourcev3.ClusterType)
	clas := snap.GetResources(resourcev3.EndpointType)

	cl, ok := clusters[name].(*clusterv3.Cluster)
	require.True(t, ok, "cluster %s missing: %v", name, keysOf(clusters))
	eds := cl.GetEdsClusterConfig().GetServiceName()
	assert.Equal(t, name, eds, "%s must subscribe to its own EDS name", name)
	assert.NotEqual(t, bareEDS, eds,
		"%s shares the default cluster's bare-service EDS name: added in a later CDS update it is deduplicated by the delta-ADS WatchMap and warms for 15 s (aether#1013)", name)

	base, ok := clas[bareEDS].(*endpointv3.ClusterLoadAssignment)
	require.True(t, ok, "bare load assignment %s missing: %v", bareEDS, keysOf(clas))
	require.NotEmpty(t, base.GetEndpoints(), "fixture must carry endpoints for the comparison to mean anything")

	cla, ok := clas[eds].(*endpointv3.ClusterLoadAssignment)
	if !assert.True(t, ok, "no ClusterLoadAssignment under %s's EDS name %q in the snapshot that carries the cluster: %v", name, eds, keysOf(clas)) {
		return
	}
	assert.Equal(t, eds, cla.GetClusterName())
	renamed, _ := proto.Clone(cla).(*endpointv3.ClusterLoadAssignment)
	renamed.ClusterName = base.GetClusterName()
	assert.True(t, proto.Equal(base, renamed), "%s's load assignment must be the bare one's in every field but the name", name)
}

// assertRegenerationByteIdentical regenerates the snapshot with no input change
// and requires every EDS and CDS resource to marshal to the same bytes.
func assertRegenerationByteIdentical(t *testing.T, c *SnapshotCache, before cachev3.ResourceSnapshot) {
	t.Helper()
	require.NoError(t, c.generateSnapshot(context.Background()))
	again, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	for _, typ := range []resourcev3.Type{resourcev3.ClusterType, resourcev3.EndpointType} {
		prev := before.GetResources(typ)
		next := again.GetResources(typ)
		require.ElementsMatch(t, keysOf(prev), keysOf(next), "%s resource set moved on regeneration", typ)
		for name, res := range prev {
			a, err := proto.MarshalOptions{Deterministic: true}.Marshal(res)
			require.NoError(t, err)
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(next[name])
			require.NoError(t, err)
			assert.Equal(t, a, b, "%s %s: identical inputs must produce identical bytes", typ, name)
		}
	}
}

// TestLatePortAliasSubscribesToItsOwnEDSResource is the unit gate for the HTTP
// port-alias half of aether#1013.
//
// A port alias ("<fqdn>:<port>", buildPortAliasesLocked) used to subscribe to
// the bare-service EDS name the default cluster already holds. When the alias
// arrives in a LATER CDS update than the default cluster -- here the service's
// default application port changes after the node already depends on it, which
// adds a ":8080" alias beside the existing mesh-port one -- Envoy's delta-ADS
// WatchMap sends no subscribe for the shared name and the alias warms for the
// full initial_fetch_timeout. So each alias must subscribe to its own name, and
// the load assignment under it must ride the SAME snapshot as the alias.
func TestLatePortAliasSubscribesToItsOwnEDSResource(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "client-0", Namespace: "demo", ServiceAccount: "client",
		NetworkNamespace: "/var/run/netns/cni-client-0",
	}, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "demo/echo")

	// First the service listens on the mesh port itself (one alias, :18081);
	// later its application port is 8080, which adds the ":8080" alias.
	var appPort atomic.Uint32
	appPort.Store(meshconst.ProxyOutboundPort)
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			p := appPort.Load()
			return map[string][]*registryv1.ServiceEndpoint{
				"demo/echo": {
					makeEndpoint("10.0.3.1", "cluster-1", "node-2", p),
					makeEndpoint("10.0.3.2", "cluster-1", "node-3", p),
				},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	fqdn := proxy.ServiceClusterName("demo/echo", c.meshDomain)
	meshAlias := fmt.Sprintf("%s:%d", fqdn, meshconst.ProxyOutboundPort)
	appAlias := fqdn + ":8080"

	first, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	def, ok := first.GetResources(resourcev3.ClusterType)[fqdn].(*clusterv3.Cluster)
	require.True(t, ok, "default cluster %s missing", fqdn)
	bareEDS := def.GetEdsClusterConfig().GetServiceName()
	require.Equal(t, "demo/echo", bareEDS, "the default cluster keeps the bare-service EDS name")
	_, hasAppAlias := first.GetResources(resourcev3.ClusterType)[appAlias]
	require.False(t, hasAppAlias, "fixture: the :8080 alias must NOT exist yet, or nothing is 'later'")
	assertOwnEDSWithBaseMembership(t, first, meshAlias, bareEDS)

	// The later CDS update: the :8080 alias is new, and its CLA must be in the
	// SAME snapshot, under a new EDS version.
	appPort.Store(8080)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	later, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	assert.NotEqual(t, first.GetVersion(resourcev3.EndpointType), later.GetVersion(resourcev3.EndpointType),
		"the snapshot introducing the alias's load assignment must carry a new EDS version")
	assertOwnEDSWithBaseMembership(t, later, appAlias, bareEDS)
	assertOwnEDSWithBaseMembership(t, later, meshAlias, bareEDS)

	assertRegenerationByteIdentical(t, c, later)
}

// TestLateTCPFloorSubscribesToItsOwnEDSResource is the unit gate for the TCP
// floor half of aether#1013.
//
// The TCP floor "tcp:<fqdn>" and its primary-port alias "tcp:<fqdn>:<port>"
// used to subscribe to the bare-service EDS name. For a service that also has
// an HTTP default cluster (pods split across protocols), the default cluster
// holds that subscription already; a floor added LATER -- the service joins the
// capture TCP set after it is in the dependency set -- is deduplicated by the
// delta-ADS WatchMap and warms for 15 s, killing every captured TCP connection
// to it in the meantime (tcp_proxy has no cold path).
func TestLateTCPFloorSubscribesToItsOwnEDSResource(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "mixed-0", Namespace: "aether-test", ServiceAccount: "mixed",
		NetworkNamespace: "/var/run/netns/cni-mixed",
	}, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/mixed")

	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{
					"aether-test/mixed": {makeEndpoint("10.0.0.20", "cluster-1", "node-2", 9000)},
				}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{
				"aether-test/mixed": {makeEndpoint("10.0.0.10", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	const fqdn = "mixed.aether-test.aether.internal"
	floor := "tcp:" + fqdn
	floorAlias := floor + ":9000"

	first, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	def, ok := first.GetResources(resourcev3.ClusterType)[fqdn].(*clusterv3.Cluster)
	require.True(t, ok, "default HTTP cluster %s missing", fqdn)
	bareEDS := def.GetEdsClusterConfig().GetServiceName()
	require.Equal(t, "aether-test/mixed", bareEDS)
	_, hasFloor := first.GetResources(resourcev3.ClusterType)[floor]
	require.False(t, hasFloor, "fixture: the floor must NOT exist yet, or nothing is 'later'")

	// The later CDS update: the service joins the capture TCP set.
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "aether-test/mixed", ClusterIP: "10.96.0.60", PrimaryIsTCP: true}})
	require.NoError(t, c.generateSnapshot(ctx))
	later, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	assert.NotEqual(t, first.GetVersion(resourcev3.EndpointType), later.GetVersion(resourcev3.EndpointType),
		"the snapshot introducing the floor's load assignment must carry a new EDS version")
	assertOwnEDSWithBaseMembership(t, later, floor, bareEDS)
	assertOwnEDSWithBaseMembership(t, later, floorAlias, bareEDS)

	assertRegenerationByteIdentical(t, c, later)
}

// TestNoNonDefaultClusterSharesTheBareServiceEDSName sweeps a snapshot that
// carries every cluster kind at once -- default, per-port, port aliases, TCP
// floor + its alias, QUIC twins -- and requires that each EDS resource name is
// subscribed by exactly one cluster (aether#842 SDS, #1008 twins, #1013 aliases
// and floors: a delta-ADS subscriber must never share a resource name with a
// sibling), and that every subscribed name is published.
func TestNoNonDefaultClusterSharesTheBareServiceEDSName(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()
	for _, p := range []struct{ name, sa string }{{"a-0", "source-a"}, {"b-0", "source-b"}} {
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
			Name: p.name, Namespace: "demo", ServiceAccount: p.sa,
			NetworkNamespace: "/var/run/netns/cni-" + p.name,
		}, "aether.internal"))
	}
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "demo/echo", "demo/mixed")
	c.SetCaptureAuthorities(map[string]string{"demo/echo": "echo.demo.svc.cluster.local"})
	c.SetEastWestQUICServices([]string{"demo/echo"})
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "demo/mixed", ClusterIP: "10.96.0.61", PrimaryIsTCP: true}})

	multi := makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)
	multi.Ports = []uint32{8080, 9090}
	// demo/mixed's TCP pods also serve 8080 as raw TCP -- the SAME port its HTTP
	// pods use as their default. That puts the HTTP ":8080" alias and the TCP
	// per-port cluster on one port, the spelling collision the TCP per-port load
	// assignment used to have (it was named <fqdn>:<port>, the HTTP spelling).
	tcpMulti := makeEndpoint("10.0.0.20", "cluster-1", "node-2", 9000)
	tcpMulti.Ports = []uint32{9000, 8080}
	tcpMulti.PortProtocols = map[uint32]registryv1.PortProtocol{
		9000: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
		8080: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
	}
	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{
					"demo/mixed": {tcpMulti},
				}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{
				"demo/echo":  {multi, makeEndpoint("10.0.3.2", "cluster-1", "node-3", 8080)},
				"demo/mixed": {makeEndpoint("10.0.0.10", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	clusters := snap.GetResources(resourcev3.ClusterType)
	clas := snap.GetResources(resourcev3.EndpointType)
	kinds := map[string]bool{}
	owners := map[string][]string{}
	for name, res := range clusters {
		cl, _ := res.(*clusterv3.Cluster)
		if cl.GetType() != clusterv3.Cluster_EDS {
			continue
		}
		eds := cl.GetEdsClusterConfig().GetServiceName()
		if eds == "" {
			eds = name
		}
		owners[eds] = append(owners[eds], name)
		_, published := clas[eds]
		assert.True(t, published, "%s subscribes to %q but nothing publishes it", name, eds)
		switch {
		case len(name) > 5 && name[:5] == "quic:":
			kinds["quic"] = true
		case name == "tcp:mixed.demo.aether.internal":
			kinds["tcp"] = true
		case name == "tcp:mixed.demo.aether.internal:9000":
			kinds["tcp-alias"] = true
		case name == "tcp:mixed.demo.aether.internal:8080":
			kinds["tcp-port"] = true
		case name == fmt.Sprintf("echo.demo.aether.internal:%d", meshconst.ProxyOutboundPort):
			kinds["alias"] = true
		case name == "echo.demo.aether.internal:9090":
			kinds["port"] = true
		}
	}
	// Anti-vacuity: the sweep must actually have seen every kind it guards.
	assert.Equal(t, map[string]bool{"quic": true, "tcp": true, "tcp-alias": true, "tcp-port": true, "alias": true, "port": true}, kinds,
		"fixture must carry every cluster kind: %v", keysOf(clusters))
	for eds, names := range owners {
		assert.Len(t, names, 1, "EDS name %q is shared by %v: a later-added sharer is deduplicated into 15 s of warming", eds, names)
	}

	// And no EDS name is PUBLISHED twice. The snapshot is a map, so a duplicate
	// collapses silently (last writer wins); count the generator's raw output.
	_, entryCLAs, _ := c.clustersEndpointsAndVhosts()
	_, floorCLAs := c.captureTCPClusters()
	published := map[string]int{}
	for _, res := range append(entryCLAs, floorCLAs...) {
		published[res.(*endpointv3.ClusterLoadAssignment).GetClusterName()]++
	}
	for name, n := range published {
		assert.Equal(t, 1, n, "load assignment %q is published %d times", name, n)
	}
}
