package cache

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The retention of #1635 keeps a service's HTTP cluster and virtual host for a
// grace period when its HTTP listing goes. It is kept for a REMOVAL: the
// listing is merely gone and nothing says what the service is now. It is not
// kept against a REPLACEMENT: positive evidence of what the service is now
// (the rows the node holds of it, or its generated Service's ports) that no
// longer declares the port as HTTP. Then the entry and its virtual host go at
// once, and the port is answered by what the evidence says.

const rrSvc = "demo/x"

// rrFixture is a cache holding one pod of demo/x serving HTTP on 8080, the
// state every case below starts from.
func rrFixture(t *testing.T, ports map[uint32]registryv1.PortProtocol, legacy uint32) (*SnapshotCache, *[]rtpPod) {
	t.Helper()
	pods := &[]rtpPod{{svc: rrSvc, ip: "10.0.8.1", legacy: legacy, ports: ports}}
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{{name: "x", ns: a043NS, vip: "10.96.8.1"}})
	rrLoad(t, c, *pods)
	return c, pods
}

func rrLoad(t *testing.T, c *SnapshotCache, pods []rtpPod) {
	t.Helper()
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", rtpRegistry(pods)))
}

func rrHost(port uint32) string { return fmt.Sprintf("x.demo.%s:%d", a043Mesh, port) }

// rrRouted is the cluster out_http routes Host x.demo.<mesh>:<port> to, "" when
// it routes it nowhere.
func rrRouted(t *testing.T, c *SnapshotCache, port uint32) a043Answer {
	t.Helper()
	return a043Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], rrHost(port))
}

// rrEntries are the HTTP ports the cache holds a (non-floor) cluster entry of
// demo/x for, sorted. The default entry and its port alias are one port.
func rrEntries(c *SnapshotCache) []uint32 {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	out := []uint32{}
	for _, e := range c.clusters {
		if e.service == rrSvc && !e.l4Floor {
			out = append(out, primaryPortOf(e))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// rrPublished reports whether the snapshot publishes a cluster of that name.
func rrPublished(t *testing.T, c *SnapshotCache, name string) bool {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	_, ok := snap.GetResources(resourcev3.ClusterType)[name]
	return ok
}

// The different-port case. A service that served HTTP on 8080 now serves raw
// TCP on 9000 and nothing on 8080: the rows the node holds say so. Kept for
// the grace, the HTTP cluster of 8080 resolved its endpoints through the bare
// service name, which the live raw-TCP floor publishes: an HTTP request to
// x:8080 was delivered to the pods' raw-TCP port.
func TestRetained_ReplacedAtOnceWhenTheRowsDeclareAnotherPort(t *testing.T) {
	c, pods := rrFixture(t, map[uint32]registryv1.PortProtocol{8080: rtpH}, 8080)
	require.Equal(t, "x.demo."+a043Mesh, rrRouted(t, c, 8080).cluster, "control: routed while 8080 is HTTP")

	(*pods)[0].ports = map[uint32]registryv1.PortProtocol{9000: rtpTCP}
	(*pods)[0].legacy = 9000
	rrLoad(t, c, *pods)

	assert.Empty(t, rrEntries(c), "the HTTP entry of a port no row declares is kept beside rows that say what the service is now")
	got := rrRouted(t, c, 8080)
	assert.Emptyf(t, got.cluster, "Host %s is still routed, to %q, whose endpoints are the raw-TCP pods", rrHost(8080), got.cluster)
	assert.Equal(t, uint32(421), got.status)
	assert.Equal(t, proxy.RefusalPortNotDeclared, got.refusal)
	assert.False(t, rrPublished(t, c, "x.demo."+a043Mesh), "the h2 cluster of the replaced port is still published")
	// And the port the evidence declares is answered as what it is.
	assert.Equal(t, proxy.RefusalPortNotHTTP, rrRouted(t, c, 9000).refusal)
}

// The same with two HTTP ports: 9090 leaves, 8080 stays. The per-port entry
// of 9090 was kept with its virtual host for the grace (an empty cluster, a
// 503) although the service's own rows say it has no such port.
func TestRetained_ReplacedAtOnceWhenAnHTTPPortLeavesAndAnotherStays(t *testing.T) {
	c, pods := rrFixture(t, map[uint32]registryv1.PortProtocol{8080: rtpH, 9090: rtpH}, 8080)
	require.Equal(t, fmt.Sprintf("x.demo.%s:9090", a043Mesh), rrRouted(t, c, 9090).cluster, "control")

	(*pods)[0].ports = map[uint32]registryv1.PortProtocol{8080: rtpH}
	rrLoad(t, c, *pods)

	assert.Equal(t, []uint32{8080}, rrEntries(c), "only the entry of the port still declared stays")
	got := rrRouted(t, c, 9090)
	assert.Emptyf(t, got.cluster, "Host %s is still routed, to %q", rrHost(9090), got.cluster)
	assert.Equal(t, proxy.RefusalPortNotDeclared, got.refusal)
	assert.Equal(t, "x.demo."+a043Mesh, rrRouted(t, c, 8080).cluster, "the port that stays is untouched")
}

// The new port is routed in the same refresh the old one is refused in: a
// declared port moves from 8080 to 9090 and a second pod keeps the service's
// default entry on 7070.
func TestRetained_ReplacementRoutesThePortsNowDeclared(t *testing.T) {
	c, pods := rrFixture(t, map[uint32]registryv1.PortProtocol{7070: rtpH, 8080: rtpH}, 7070)
	require.NotEmpty(t, rrRouted(t, c, 8080).cluster, "control")

	(*pods)[0].ports = map[uint32]registryv1.PortProtocol{7070: rtpH, 9090: rtpH}
	rrLoad(t, c, *pods)

	assert.Equal(t, proxy.RefusalPortNotDeclared, rrRouted(t, c, 8080).refusal)
	assert.Equal(t, fmt.Sprintf("x.demo.%s:9090", a043Mesh), rrRouted(t, c, 9090).cluster)
}

// A REMOVAL keeps the entry. Every row of the service is gone and nothing
// says what it is now (its generated Service declares no port, as one does
// whose last endpoint left): a pod restart of a single-replica service looks
// exactly like this. The cluster and the virtual host stay for the grace, so
// the request is a retriable 503 and not a refusal (#1635, and the gap of
// TestLoadClustersFromRegistry_RetainsDisappearedServices).
func TestRetained_KeptWhenNothingSaysWhatTheServiceIsNow(t *testing.T) {
	c, _ := rrFixture(t, map[uint32]registryv1.PortProtocol{8080: rtpH, 9090: rtpH}, 8080)

	rrLoad(t, c, nil)

	assert.Equal(t, []uint32{8080, 9090}, rrEntries(c), "a loss with no evidence must keep the entries for the grace")
	assert.Equal(t, "x.demo."+a043Mesh, rrRouted(t, c, 8080).cluster)
	assert.Equal(t, fmt.Sprintf("x.demo.%s:9090", a043Mesh), rrRouted(t, c, 9090).cluster)
	assert.True(t, rrPublished(t, c, "x.demo."+a043Mesh))

	// A second refresh with nothing new changes nothing either.
	rrLoad(t, c, nil)
	assert.Equal(t, []uint32{8080, 9090}, rrEntries(c))
}

// The generated Service is evidence too, when it says something. With no row
// left, a Service that still declares 8080 as HTTP is no evidence against the
// entry (kept); one that declares the service's ports and not 8080 as HTTP is
// (replaced, on the refresh the declaration triggers).
func TestRetained_TheGeneratedServiceIsEvidenceWhenNoRowIsHeld(t *testing.T) {
	project := func(c *SnapshotCache, http, other []uint32) {
		c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: rrSvc, ClusterIP: "10.96.8.1", DeclaredHTTP: http, DeclaredOther: other}})
	}
	drain := func(c *SnapshotCache) bool {
		select {
		case <-c.depChanged:
			return true
		default:
			return false
		}
	}

	t.Run("still declares the port as HTTP: kept", func(t *testing.T) {
		c, _ := rrFixture(t, map[uint32]registryv1.PortProtocol{8080: rtpH}, 8080)
		project(c, []uint32{8080}, nil)
		rrLoad(t, c, nil)
		assert.Equal(t, []uint32{8080}, rrEntries(c))
		assert.Equal(t, "x.demo."+a043Mesh, rrRouted(t, c, 8080).cluster)
	})

	// The rows are the registry; the Service is derived from it, later. A
	// Service that still declares 8080 as HTTP does not keep the entry
	// against rows that say otherwise.
	t.Run("the rows outrank the Service", func(t *testing.T) {
		c, pods := rrFixture(t, map[uint32]registryv1.PortProtocol{8080: rtpH}, 8080)
		project(c, []uint32{8080}, nil)
		(*pods)[0].ports = map[uint32]registryv1.PortProtocol{9000: rtpTCP}
		(*pods)[0].legacy = 9000
		rrLoad(t, c, *pods)
		assert.Empty(t, rrEntries(c))
		assert.Empty(t, rrRouted(t, c, 8080).cluster)
	})

	t.Run("declares the port as raw TCP: replaced", func(t *testing.T) {
		c, _ := rrFixture(t, map[uint32]registryv1.PortProtocol{8080: rtpH}, 8080)
		rrLoad(t, c, nil)
		require.Equal(t, []uint32{8080}, rrEntries(c), "control: retained while nothing is known")
		drain(c)

		project(c, nil, []uint32{8080})
		assert.True(t, drain(c), "a change of the declared ports must ask for the refresh that replaces the entry")
		rrLoad(t, c, nil)
		assert.Empty(t, rrEntries(c))
		got := rrRouted(t, c, 8080)
		assert.Empty(t, got.cluster)
		assert.Equal(t, proxy.RefusalPortNotHTTP, got.refusal)
	})

	t.Run("declares another port only: replaced, and the new port is the service's", func(t *testing.T) {
		c, _ := rrFixture(t, map[uint32]registryv1.PortProtocol{8080: rtpH}, 8080)
		rrLoad(t, c, nil)
		project(c, []uint32{9090}, nil)
		rrLoad(t, c, nil)
		assert.Empty(t, rrEntries(c))
		assert.Equal(t, proxy.RefusalPortNotDeclared, rrRouted(t, c, 8080).refusal)
		got := rrRouted(t, c, 9090)
		assert.Equal(t, fmt.Sprintf("x.demo.%s:9090", a043Mesh), got.cluster, "the declared port of a service the node holds no endpoint of: its own cluster, fetched on demand")
	})
	// Two HTTP ports, no row left, and the Service declares one of them: only
	// the other is replaced, on every table, and the one still declared keeps
	// its retained entry. What the node remembers of the service must lose
	// the replaced port, or the tables built from it would still route it.
	t.Run("declares one of two HTTP ports: the other is replaced on every table", func(t *testing.T) {
		c, _ := rrFixture(t, map[uint32]registryv1.PortProtocol{8080: rtpH, 9090: rtpH}, 8080)
		rrLoad(t, c, nil)
		require.Equal(t, []uint32{8080, 9090}, rrEntries(c), "control: both retained while nothing is known")
		project(c, []uint32{8080}, nil)
		rrLoad(t, c, nil)
		assert.Equal(t, []uint32{8080}, rrEntries(c))
		c.clusterMu.RLock()
		assert.Equal(t, "8080 (http)", c.servicePorts[rrSvc].describe(), "the replaced port is still remembered as a port of the service")
		c.clusterMu.RUnlock()
		tables := a043Tables(t, c)
		assert.Equal(t, "x.demo."+a043Mesh, a043Resolve(t, tables[proxy.OutboundHTTPRouteName], rrHost(8080)).cluster)
		for name, rc := range tables {
			if name != proxy.OutboundHTTPRouteName && name != "cap_http_9090" && name != "cap_http_other" {
				continue
			}
			got := a043Resolve(t, rc, rrHost(9090))
			assert.Emptyf(t, got.cluster, "%s: Host %s is still routed, to %q, a cluster that is gone", name, rrHost(9090), got.cluster)
		}
	})
}
