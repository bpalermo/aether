package cache

import (
	"context"
	"testing"
	"time"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// snapshotClusterNames returns the cluster resource names in the node's current
// published snapshot — what the proxy's delta CDS stream can be answered with.
func snapshotClusterNames(t *testing.T, c *SnapshotCache) map[string]types.Resource {
	t.Helper()
	snap, err := c.GetSnapshot(c.nodeName)
	require.NoError(t, err)
	return snap.GetResources(resourcev3.ClusterType)
}

// TestODCDS_DefaultPortAuthorityIsAnsweredInOnePush is the #682 regression test.
//
// The ODCDS catch-all resolves its cluster from the request :authority, so a
// client dialing "echo.aether-test.aether.internal:18081" makes the on_demand
// filter ask for a cluster of exactly that name. Before the fix the agent only
// ever published the PORTLESS "echo.aether-test.aether.internal" for the default
// port, so that on-demand name could never be satisfied: Envoy parked it in its
// delta subscription state as "waiting for server" and deduped every later
// subscribe for it, so once the demand set shrank and the service's vhost went
// away, no ODCDS request reached the agent again and the authority stayed dead
// until the ADS stream reset (~5 minutes in vivo).
//
// The assertion that matters: after the service is dropped from the dependency
// set, ONE on-demand observation plus ONE reload must put the requested name
// back in the published snapshot — no stream reset, no second round trip.
func TestODCDS_DefaultPortAuthorityIsAnsweredInOnePush(t *testing.T) {
	const (
		service   = "aether-test/echo"
		fqdn      = "echo.aether-test.aether.internal"
		authority = fqdn + ":18081"
	)

	ctx := context.Background()
	c := newTestCache("node-1")
	c.SetMeshDomain("aether.internal")
	// A short TTL so the idle expiry that shrinks the demand set is testable.
	c.observedTTL = 30 * time.Millisecond

	reg := &catalogListerRegistry{
		mockRegistry: &mockRegistry{
			listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
				return map[string][]*registryv1.ServiceEndpoint{
					service: {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 18081)},
				}, nil
			},
		},
		known: map[string]bool{service: true},
	}

	// Cold path: the proxy's on-demand subscribe for the authority is observed,
	// and the next reload publishes BOTH the portless cluster and the authority.
	c.ObserveDependency(ctx, service)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	warm := snapshotClusterNames(t, c)
	require.Contains(t, warm, fqdn, "portless cluster is published")
	require.Contains(t, warm, authority, "the ODCDS name for the default port must resolve to a cluster")

	// Demand-set shrink (the outage trigger): the observed dependency ages out,
	// the service leaves the dependency set and the reload drops its clusters.
	time.Sleep(40 * time.Millisecond)
	c.PruneObservedDependencies()
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	dropped := snapshotClusterNames(t, c)
	require.NotContains(t, dropped, fqdn, "the dropped service leaves the snapshot")
	require.NotContains(t, dropped, authority)

	// The very next request re-issues the on-demand subscribe for the authority
	// on the SAME stream. One observation + one reload must answer it.
	c.ObserveDependency(ctx, service)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	repaired := snapshotClusterNames(t, c)
	assert.Contains(t, repaired, authority,
		"the just-dropped cluster must be republished under the requested on-demand name in one push")
	assert.Contains(t, repaired, fqdn)
}

// TestObservedDependency_LiveOnDemandSubscriptionSurvivesTTL is the #682
// TTL-under-load regression test.
//
// The observed TTL was refreshed only by an ODCDS request, and an ODCDS request
// only happens on a MISS — once the cluster is warm the service's own vhost
// carries the traffic and the on_demand filter is never reached again. So a
// node serving a cross-node upstream continuously dropped it exactly one hour
// after first asking for it. The node proxy's LIVE on-demand subscription is
// the use signal that closes the gap: Envoy holds it for the life of the
// stream, so while it exists the dependency is refreshed, never expired.
func TestObservedDependency_LiveOnDemandSubscriptionSurvivesTTL(t *testing.T) {
	const (
		service   = "aether-test/echo"
		authority = "echo.aether-test.aether.internal:18081"
	)

	ctx := context.Background()
	c := newTestCache("node-1")
	c.SetMeshDomain("aether.internal")
	c.observedTTL = 20 * time.Millisecond

	c.ObserveDependency(ctx, service)
	c.TrackOnDemandCluster(1, authority, service)

	// Continuous use across several TTL windows: the entry never leaves the
	// dependency set, neither via the periodic prune nor via the read-time
	// (memoized) wall-clock expiry between prune ticks.
	for range 4 {
		time.Sleep(15 * time.Millisecond)
		assert.Contains(t, c.DependencySet(), service,
			"an upstream with a live on-demand subscription must stay in scope between prune ticks")
		c.PruneObservedDependencies()
		assert.Contains(t, c.DependencySet(), service,
			"the idle TTL must be refreshed by observed use, not only by an ODCDS miss")
	}

	// The stream ends (proxy or agent restart): the pins go with it and the
	// upstream ages out on the idle TTL like any other — demand scoping intact.
	c.CloseOnDemandStream(1)
	time.Sleep(30 * time.Millisecond)
	c.PruneObservedDependencies()
	assert.NotContains(t, c.DependencySet(), service,
		"a genuinely idle upstream still expires once no live subscription holds it")
}

// TestObservedDependency_UnsubscribeReleasesTheTTLExemption verifies an explicit
// unsubscribe releases the exemption, while a second live subscription for the
// same service (another authority spelling of it) keeps it.
func TestObservedDependency_UnsubscribeReleasesTheTTLExemption(t *testing.T) {
	const (
		service  = "aether-test/echo"
		portless = "echo.aether-test.aether.internal"
		withPort = portless + ":18081"
	)

	ctx := context.Background()
	c := newTestCache("node-1")
	c.SetMeshDomain("aether.internal")
	c.observedTTL = 20 * time.Millisecond

	c.ObserveDependency(ctx, service)
	c.TrackOnDemandCluster(1, portless, service)
	c.TrackOnDemandCluster(1, withPort, service)

	// One spelling released: the other still holds the service in use.
	c.UntrackOnDemandCluster(1, withPort)
	time.Sleep(30 * time.Millisecond)
	c.PruneObservedDependencies()
	require.Contains(t, c.DependencySet(), service, "a remaining live subscription keeps the exemption")

	// Both released: the service expires on the next tick past the TTL.
	c.UntrackOnDemandCluster(1, portless)
	time.Sleep(30 * time.Millisecond)
	c.PruneObservedDependencies()
	assert.NotContains(t, c.DependencySet(), service)
}
