package cache

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// Issue #1115: a registry refresh reuses the proto objects of every service
// whose inputs did not change, and rebuilds -- as NEW objects with new
// versions -- exactly the ones whose inputs did. Every reuse in these tests is
// also audited against a fresh build (registryReuseAudit,
// versionmemo_strict_test.go), so a stale reuse panics even where an
// assertion below would not look.

const reuseSA = "sa-0"

// reuseFixture is a node with one local pod (so mTLS clusters are rendered and
// a QUIC twin can be observed) and a mutable registry listing.
type reuseFixture struct {
	c   *SnapshotCache
	reg *mockRegistry

	mu      sync.Mutex
	listing map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint
}

func reuseEndpoint(ip string) *registryv1.ServiceEndpoint {
	ep := makeEndpoint(ip, "cluster-1", "node-2", 8080)
	ep.KubernetesMetadata.Namespace = "demo"
	ep.KubernetesMetadata.PodName = "pod-" + ip
	ep.Locality = &registryv1.ServiceEndpoint_Locality{Region: "r1", Zone: "z1"}
	return ep
}

func newReuseFixture(t *testing.T) *reuseFixture {
	t.Helper()
	ctx := context.Background()
	f := &reuseFixture{
		c: newTestCache("node-1"),
		listing: map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint{
			registryv1.Service_PROTOCOL_HTTP: {
				"demo/echo":  {reuseEndpoint("10.2.0.1"), reuseEndpoint("10.2.0.2")},
				"demo/other": {reuseEndpoint("10.2.0.11"), reuseEndpoint("10.2.0.12")},
			},
			registryv1.Service_PROTOCOL_TCP: {
				"demo/db": {reuseEndpoint("10.2.0.21"), reuseEndpoint("10.2.0.22")},
			},
			registryv1.Service_PROTOCOL_UDP: {
				"demo/dns": {reuseEndpoint("10.2.0.31")},
			},
		},
	}
	f.reg = &mockRegistry{tcpAware: true, listAllEndpointsFunc: func(_ context.Context, p registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := make(map[string][]*registryv1.ServiceEndpoint, len(f.listing[p]))
		for svc, eps := range f.listing[p] {
			out[svc] = slices.Clone(eps)
		}
		return out, nil
	}}
	c := f.c
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "app-0", Namespace: "demo", ServiceAccount: reuseSA,
		NetworkNamespace: "/var/run/netns/cni-app-0", ContainerId: "c-0",
		Ips: []string{"10.1.0.1"}, Labels: map[string]string{"app": "client"},
	}, quicDemandTD))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetSecrets(ctx, []*tlsv3.Secret{
		fakeSVID(nodeIdentity), fakeSVID("ROOTCA"),
		fakeSVID(fmt.Sprintf("spiffe://%s/ns/demo/sa/%s", quicDemandTD, reuseSA)),
	}))
	declareDeps(c, "demo/echo", "demo/other", "demo/db", "demo/dns")
	f.refresh(t)
	c.markLocalPodsSynced()
	d, reason := c.recordQUICPair(testQUICStream, f.twin())
	require.NotEqual(t, QUICTwinRefused, d, reason)
	f.refresh(t)
	return f
}

// edit replaces one service's rows in one protocol's listing. Rows are
// replaced, never mutated in place: the registry hands out shared, immutable
// rows (registrar.ListAllEndpoints).
func (f *reuseFixture) edit(p registryv1.Service_Protocol, svc string, fn func([]*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := make([]*registryv1.ServiceEndpoint, 0, len(f.listing[p][svc]))
	for _, ep := range f.listing[p][svc] {
		rows = append(rows, proto.Clone(ep).(*registryv1.ServiceEndpoint))
	}
	if rows = fn(rows); rows == nil {
		delete(f.listing[p], svc)
		return
	}
	f.listing[p][svc] = rows
}

func (f *reuseFixture) refresh(t *testing.T) *cachev3.Snapshot {
	t.Helper()
	require.NoError(t, f.c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", f.reg))
	return f.snapshot(t)
}

func (f *reuseFixture) snapshot(t *testing.T) *cachev3.Snapshot {
	t.Helper()
	s, err := f.c.GetSnapshot("node-1")
	require.NoError(t, err)
	snap, ok := s.(*cachev3.Snapshot)
	require.True(t, ok)
	return snap
}

func (f *reuseFixture) fqdn(svc string) string { return proxy.ServiceClusterName(svc, f.c.meshDomain) }

func (f *reuseFixture) twin() string {
	return proxy.QUICClusterName("demo/echo", f.c.meshDomain, "demo/"+reuseSA)
}

// res names one published resource.
type res struct{ typ, name string }

func cds(name string) res { return res{resourcev3.ClusterType, name} }
func eds(name string) res { return res{resourcev3.EndpointType, name} }

// requireRebuilt: a NEW object with a NEW version (the change reaches Envoy).
func requireRebuilt(t *testing.T, before, after *cachev3.Snapshot, r res) {
	t.Helper()
	b, a := before.GetResources(r.typ)[r.name], after.GetResources(r.typ)[r.name]
	require.NotNil(t, b, "%s %q missing before", r.typ, r.name)
	require.NotNil(t, a, "%s %q missing after", r.typ, r.name)
	assert.NotSame(t, b, a, "%s %q must be rebuilt as a new object", r.typ, r.name)
	assert.NotEqual(t, before.GetVersionMap(r.typ)[r.name], after.GetVersionMap(r.typ)[r.name],
		"%s %q must get a new version", r.typ, r.name)
}

// requireReused: the identical object and version (a memo hit, no push).
func requireReused(t *testing.T, before, after *cachev3.Snapshot, r res) {
	t.Helper()
	b, a := before.GetResources(r.typ)[r.name], after.GetResources(r.typ)[r.name]
	require.NotNil(t, b, "%s %q missing before", r.typ, r.name)
	assert.Same(t, b, a, "%s %q must be the reused object", r.typ, r.name)
	assert.Equal(t, before.GetVersionMap(r.typ)[r.name], after.GetVersionMap(r.typ)[r.name])
}

// requireAllReused: every cluster and load assignment is the identical object.
func requireAllReused(t *testing.T, before, after *cachev3.Snapshot) {
	t.Helper()
	for _, typ := range []string{resourcev3.ClusterType, resourcev3.EndpointType} {
		b, a := before.GetResources(typ), after.GetResources(typ)
		assert.ElementsMatch(t, slices.Collect(maps.Keys(b)), slices.Collect(maps.Keys(a)), typ)
		for name, r := range a {
			if name == proxy.PassthroughClusterName || strings.HasPrefix(name, "ew_ingress_") {
				continue // built per snapshot from local state; not registry-derived
			}
			assert.Same(t, b[name], r, "%s %q must be reused by an unchanged refresh", typ, name)
		}
	}
}

func TestRegistryRefreshUnchangedReusesEveryObject(t *testing.T) {
	f := newReuseFixture(t)
	s0 := f.snapshot(t)
	require.NotNil(t, s0.GetResources(resourcev3.ClusterType)[f.twin()], "fixture must carry the observed twin")
	s1 := f.refresh(t)
	requireAllReused(t, s0, s1)
	s2 := f.refresh(t)
	requireAllReused(t, s1, s2)
	assert.Zero(t, f.c.registryReuse.built, "an unchanged refresh builds nothing")
	assert.Equal(t, 4, f.c.registryReuse.reused, "echo, other (HTTP), db (TCP), dns (UDP)")
}

func TestRegistryRefreshRebuildsExactlyWhatChanged(t *testing.T) {
	setFirst := func(p registryv1.Service_Protocol, svc string, fn func(*registryv1.ServiceEndpoint)) func(*reuseFixture) {
		return func(f *reuseFixture) {
			f.edit(p, svc, func(eps []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
				fn(eps[0])
				return eps
			})
		}
	}
	http := registryv1.Service_PROTOCOL_HTTP

	type tc struct {
		name   string
		mutate func(f *reuseFixture)
		// rebuilt must be new objects with new versions; reused must be the
		// identical objects. Both are functions of the fixture (names carry
		// the mesh domain).
		rebuilt func(f *reuseFixture) []res
		reused  func(f *reuseFixture) []res
		// cdsVersionStable: the service's h2 cluster is rebuilt (its key
		// changed) but its bytes are not, so it must keep its version -- an
		// endpoint-only change never causes a CDS push.
		cdsVersionStable bool
	}
	echoEDS := func(f *reuseFixture) []res {
		fq := f.fqdn("demo/echo")
		return []res{eds("demo/echo"), eds(fq + ":18081"), eds(fq + ":8080"), eds(f.twin())}
	}
	others := func(f *reuseFixture) []res {
		return []res{
			cds(f.fqdn("demo/other")), eds("demo/other"), eds(f.fqdn("demo/other") + ":18081"),
			eds("demo/db"), eds("demo/dns"),
		}
	}
	cases := []tc{
		{
			name: "endpoint added",
			mutate: func(f *reuseFixture) {
				f.edit(http, "demo/echo", func(eps []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
					return append(eps, reuseEndpoint("10.2.0.3"))
				})
			},
			rebuilt: echoEDS, reused: others, cdsVersionStable: true,
		},
		{
			name: "endpoint removed",
			mutate: func(f *reuseFixture) {
				f.edit(http, "demo/echo", func(eps []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint { return eps[:1] })
			},
			rebuilt: echoEDS, reused: others, cdsVersionStable: true,
		},
		{
			name: "drain mark (DRAINING)",
			mutate: setFirst(http, "demo/echo", func(ep *registryv1.ServiceEndpoint) {
				ep.Health = registryv1.ServiceEndpoint_HEALTH_DRAINING
			}),
			rebuilt: echoEDS, reused: others, cdsVersionStable: true,
		},
		{
			name: "health flip (UNHEALTHY)",
			mutate: setFirst(http, "demo/echo", func(ep *registryv1.ServiceEndpoint) {
				ep.Health = registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
			}),
			rebuilt: echoEDS, reused: others, cdsVersionStable: true,
		},
		{
			name: "health-check mode (EDS)",
			mutate: setFirst(http, "demo/echo", func(ep *registryv1.ServiceEndpoint) {
				ep.HealthCheckMode = registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS
			}),
			rebuilt: echoEDS, reused: others, cdsVersionStable: true,
		},
		{
			name: "endpoint locality",
			mutate: setFirst(http, "demo/echo", func(ep *registryv1.ServiceEndpoint) {
				ep.Locality = &registryv1.ServiceEndpoint_Locality{Region: "r1", Zone: "z2"}
			}),
			rebuilt: echoEDS, reused: others, cdsVersionStable: true,
		},
		{
			name: "endpoint metadata (subset key)",
			mutate: setFirst(http, "demo/echo", func(ep *registryv1.ServiceEndpoint) {
				ep.Metadata = map[string]string{"version": "v2"}
			}),
			rebuilt: func(f *reuseFixture) []res { return append(echoEDS(f), cds(f.fqdn("demo/echo"))) },
			reused:  others,
		},
		{
			name: "endpoint namespace (SAN pin)",
			mutate: func(f *reuseFixture) {
				f.edit(http, "demo/echo", func(eps []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
					for _, ep := range eps {
						ep.KubernetesMetadata.Namespace = "demo2"
					}
					return eps
				})
			},
			rebuilt: func(f *reuseFixture) []res { return append(echoEDS(f), cds(f.fqdn("demo/echo"))) },
			reused:  others,
		},
		{
			name: "port change (SNI, aliases)",
			mutate: func(f *reuseFixture) {
				f.edit(http, "demo/echo", func(eps []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
					for _, ep := range eps {
						ep.Port = 9090
					}
					return eps
				})
			},
			// The default cluster's SNI is the port; the bare membership is
			// dialed at the mesh inbound whatever the app port, so the load
			// assignment keeps its bytes (and the :18081 alias its SNI).
			rebuilt: func(f *reuseFixture) []res { return []res{cds(f.fqdn("demo/echo"))} },
			reused:  others,
		},
		{
			name: "TCP service drain mark",
			mutate: setFirst(registryv1.Service_PROTOCOL_TCP, "demo/db", func(ep *registryv1.ServiceEndpoint) {
				ep.Health = registryv1.ServiceEndpoint_HEALTH_DRAINING
			}),
			rebuilt: func(*reuseFixture) []res { return []res{eds("demo/db")} },
			reused: func(f *reuseFixture) []res {
				return []res{cds(f.fqdn("demo/echo")), eds("demo/echo"), eds("demo/other"), eds("demo/dns")}
			},
		},
		{
			name: "UDP service endpoint added",
			mutate: func(f *reuseFixture) {
				f.edit(registryv1.Service_PROTOCOL_UDP, "demo/dns", func(eps []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
					return append(eps, reuseEndpoint("10.2.0.32"))
				})
			},
			rebuilt: func(*reuseFixture) []res { return []res{eds("demo/dns")} },
			reused: func(f *reuseFixture) []res {
				return []res{cds(f.fqdn("demo/echo")), eds("demo/echo"), eds("demo/other"), eds("demo/db")}
			},
		},
		{
			name: "node locality (EDS priority)",
			mutate: func(f *reuseFixture) {
				// Same region, other zone: every endpoint drops to priority 1.
				f.c.SetNodeLocality("r1", "z9")
			},
			rebuilt: func(f *reuseFixture) []res {
				return []res{eds("demo/echo"), eds("demo/other"), eds("demo/db"), eds("demo/dns"), eds(f.twin())}
			},
			reused: func(*reuseFixture) []res { return nil },
		},
		{
			name: "waypoint (remote endpoints dialed at the node tunnel)",
			mutate: func(f *reuseFixture) {
				f.c.SetWaypointConfig(true, 15009)
				f.edit(http, "demo/echo", func(eps []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
					eps[0].ClusterName = "cluster-2"
					eps[0].KubernetesMetadata.NodeIp = "198.51.100.9"
					return eps
				})
			},
			// Echo's cluster gains the waypoint transport socket matcher.
			rebuilt: func(f *reuseFixture) []res { return []res{eds("demo/echo"), cds(f.fqdn("demo/echo"))} },
			reused:  func(*reuseFixture) []res { return nil },
		},
		{
			name: "node identity (mTLS render)",
			mutate: func(f *reuseFixture) {
				require.NoError(t, f.c.SetNodeIdentity(context.Background(), "spiffe://aether.internal/ns/aether-system/sa/other-agent"))
			},
			rebuilt: func(f *reuseFixture) []res { return []res{cds(f.fqdn("demo/echo")), cds(f.fqdn("demo/other"))} },
			reused: func(*reuseFixture) []res {
				return []res{eds("demo/echo"), eds("demo/other"), eds("demo/db"), eds("demo/dns")}
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := newReuseFixture(t)
			s0 := f.snapshot(t)
			tt.mutate(f)
			s1 := f.refresh(t)
			for _, r := range tt.rebuilt(f) {
				requireRebuilt(t, s0, s1, r)
			}
			for _, r := range tt.reused(f) {
				requireReused(t, s0, s1, r)
			}
			if tt.cdsVersionStable {
				fq := f.fqdn("demo/echo")
				assert.Equal(t, s0.GetVersionMap(resourcev3.ClusterType)[fq], s1.GetVersionMap(resourcev3.ClusterType)[fq],
					"an endpoint-only change must not change the cluster's version (no CDS push)")
			}
			// And the next unchanged refresh reuses everything again.
			requireAllReused(t, s1, f.refresh(t))
		})
	}
}

// A row field the builder does not render (the registry weight) still
// rebuilds the service -- the key is the whole row, so an input is never
// missed -- but the rebuilt objects marshal identically and keep their
// versions: no push.
func TestRegistryRefreshUnrenderedFieldRebuildsWithoutPush(t *testing.T) {
	f := newReuseFixture(t)
	s0 := f.snapshot(t)
	f.edit(registryv1.Service_PROTOCOL_HTTP, "demo/echo", func(eps []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
		eps[0].Weight = 7
		return eps
	})
	s1 := f.refresh(t)
	for _, r := range []res{eds("demo/echo"), cds(f.fqdn("demo/echo"))} {
		assert.NotSame(t, s0.GetResources(r.typ)[r.name], s1.GetResources(r.typ)[r.name], "%q rebuilt", r.name)
		assert.Equal(t, s0.GetVersionMap(r.typ)[r.name], s1.GetVersionMap(r.typ)[r.name], "%q same version", r.name)
	}
	requireReused(t, s0, s1, eds("demo/other"))
}

// The TCP floor entry's ownership of the bare-name load assignment depends on
// the HTTP pass of the same refresh, so it is part of the TCP key: a service
// gaining an HTTP entry must take the CLA away from its reused TCP entry.
func TestRegistryRefreshTCPOwnershipFollowsHTTPPass(t *testing.T) {
	f := newReuseFixture(t)
	f.refresh(t)
	tcpOwns := func() bool {
		f.c.clusterMu.RLock()
		defer f.c.clusterMu.RUnlock()
		e, ok := f.c.tcpEntryLocked("demo/db")
		require.True(t, ok)
		return e.loadAssignment != nil
	}
	require.True(t, tcpOwns(), "a TCP-only service's floor entry owns the bare CLA")

	f.mu.Lock()
	f.listing[registryv1.Service_PROTOCOL_HTTP]["demo/db"] = slices.Clone(f.listing[registryv1.Service_PROTOCOL_TCP]["demo/db"])
	f.mu.Unlock()
	f.refresh(t)
	assert.False(t, tcpOwns(), "with an HTTP entry the TCP entry must stop owning the bare CLA (rows unchanged)")

	f.edit(registryv1.Service_PROTOCOL_HTTP, "demo/db", func([]*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint { return nil })
	f.c.serviceRetentionGrace = 1 // prune the retained HTTP entry at once
	f.refresh(t)
	f.refresh(t)
	assert.True(t, tcpOwns(), "and take it back once the HTTP entry is gone")
}

// Every writer of the cluster map other than the refresh invalidates the
// reuse: RemoveEndpoint replaces the load assignment, so the next refresh must
// rebuild the service from its rows instead of reusing the trimmed entry.
func TestRegistryRefreshRebuildsAfterRemoveEndpoint(t *testing.T) {
	f := newReuseFixture(t)
	s0 := f.snapshot(t)
	require.NoError(t, f.c.RemoveEndpoint(context.Background(), "demo/echo", "10.2.0.1"))
	trimmed := f.snapshot(t)
	requireRebuilt(t, s0, trimmed, eds("demo/echo"))
	s1 := f.refresh(t)
	// The registry still lists both endpoints: the refresh restores them.
	assert.Equal(t, s0.GetVersionMap(resourcev3.EndpointType)["demo/echo"], s1.GetVersionMap(resourcev3.EndpointType)["demo/echo"])
	assert.NotSame(t, trimmed.GetResources(resourcev3.EndpointType)["demo/echo"], s1.GetResources(resourcev3.EndpointType)["demo/echo"])
	cla, ok := s1.GetResources(resourcev3.EndpointType)["demo/echo"].(*endpointv3.ClusterLoadAssignment)
	require.True(t, ok)
	assert.Len(t, cla.GetEndpoints(), 2)
}

// productionReuse turns the reuse audit off (what production runs) for a
// benchmark, and returns the restore.
func productionReuse() func() {
	prev := registryReuseAudit
	registryReuseAudit = false
	return func() { registryReuseAudit = prev }
}

// TestRegistryRefreshMemoSplit pins the resource_versions split at the
// BenchmarkSnapshotBuild shape (1000+ resources): an unchanged registry refresh
// re-hashes only the handful of resources every snapshot builds from scratch
// (route tables, the subset-headers ECDS, the health-gateway listener, the
// passthrough cluster), and a one-service change adds only that service's.
func TestRegistryRefreshMemoSplit(t *testing.T) {
	ctx := context.Background()
	c, reg, flip := newBuildBenchCache(t)
	refresh := func() cachev3.ResourceSnapshot {
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		s, err := c.GetSnapshot("node-1")
		require.NoError(t, err)
		return s
	}
	s0 := refresh()
	s1 := refresh()
	hits, hashed := memoSplit(s0, s1)
	t.Logf("no-change refresh: memo=%d hashed=%d", hits, hashed)
	assert.Greater(t, hits, 1000)
	assert.LessOrEqual(t, hashed, 6, "only the per-snapshot builds are re-hashed")

	// Moving the perturbation from svc-000 to svc-007 changes two services.
	// Each re-hashes 8: its 3 h2 clusters (default + :18081 + :8080 aliases),
	// its bare and 2 alias load assignments, and its QUIC twin + twin CLA.
	flip.Store(7)
	s2 := refresh()
	hits, hashed2 := memoSplit(s1, s2)
	t.Logf("two-service refresh: memo=%d hashed=%d", hits, hashed2)
	assert.LessOrEqual(t, hashed2-hashed, 2*8, "a refresh re-hashes only the changed services")
}
