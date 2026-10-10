package cache

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1086: a pod's first use of many QUIC destinations at once.
//
// The k6 loader's `default` ServiceAccount dialled 7-10 QUIC destinations
// within ~110 ms of starting, and every first request routed to a `quic:` twin
// the snapshot did not carry yet, so the node proxy asked for each over ODCDS.
// Each admission started its own full snapshot rebuild; they serialized behind
// one another (the first already carried every pair recorded so far), each
// costing 200-870 ms of wall clock on the CPU-capped reference-cluster agent, and the
// twins delivered mid-burst could not warm (EDS) before the 2 s on_demand
// timeout: 66 x 503 NC cluster_not_found on the 2026-10-01 soak (141 and 116
// on the two before).
//
// This file only uses what main already has, so it shows the regression red
// on main: there, N simultaneous admissions cost N snapshot builds.

const burstSA = "loader"

// burstDestinations is the 10-destination first use of the k6 loader.
var burstDestinations = func() []string {
	out := make([]string, 10)
	for i := range out {
		out[i] = fmt.Sprintf("demo/svc-%d", i)
	}
	return out
}()

// newQUICBurstCache is a node with one local ServiceAccount and ten
// QUIC-eligible destinations in its dependency set, none dialled yet.
func newQUICBurstCache(t testing.TB) *SnapshotCache {
	t.Helper()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: burstSA + "-0", Namespace: "demo", ServiceAccount: burstSA,
		NetworkNamespace: "/var/run/netns/cni-" + burstSA,
	}, quicDemandTD))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	authorities := map[string]string{}
	endpoints := map[string][]*registryv1.ServiceEndpoint{}
	for i, svc := range burstDestinations {
		name := strings.TrimPrefix(svc, "demo/")
		authorities[svc] = name + ".demo.svc.cluster.local"
		endpoints[svc] = []*registryv1.ServiceEndpoint{makeEndpoint(fmt.Sprintf("10.0.4.%d", i+1), "cluster-1", "node-2", 8080)}
	}
	c.SetCaptureAuthorities(authorities)
	declareDeps(c, burstDestinations...)
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return endpoints, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	c.markLocalPodsSynced()
	require.NoError(t, c.generateSnapshot(ctx))
	return c
}

// burstTwins are the `quic:` twin names the loader's first requests ask for.
func burstTwins(c *SnapshotCache) []string {
	out := make([]string, len(burstDestinations))
	for i, svc := range burstDestinations {
		out[i] = proxy.QUICClusterName(svc, c.meshDomain, "demo/"+burstSA)
	}
	return out
}

// twinsInSnapshot counts the given twins the current snapshot carries, as a
// cluster AND as its own load assignment (the #1008 rule).
func twinsInSnapshot(t testing.TB, c *SnapshotCache, twins []string) int {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)
	clas := snap.GetResources(resourcev3.EndpointType)
	n := 0
	for _, name := range twins {
		_, cl := clusters[name]
		_, cla := clas[name]
		if cl && cla {
			n++
		}
	}
	return n
}

// burstResult is what one simultaneous first use cost.
type burstResult struct {
	// allPublished: burst start until a snapshot carries every twin.
	allPublished time.Duration
	// settled: burst start until the last snapshot build the burst caused.
	settled time.Duration
	// builds is how many snapshots the burst caused.
	builds uint64
}

// fireQUICBurst admits every twin at once, from as many goroutines (one per
// ODCDS request, as the stream callback would see them back to back), and
// waits until the cache has gone quiet: no new snapshot version for quiet.
func fireQUICBurst(t testing.TB, c *SnapshotCache, twins []string, quiet time.Duration) burstResult {
	t.Helper()
	ctx := context.Background()
	before := c.version.Load()

	var ready, done sync.WaitGroup
	gate := make(chan struct{})
	decisions := make([]QUICTwinDecision, len(twins))
	for i, name := range twins {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-gate
			decisions[i], _ = c.ObserveQUICTwin(ctx, testQUICStream, name)
		}()
	}
	ready.Wait()
	start := time.Now()
	close(gate)
	done.Wait()
	for i, d := range decisions {
		require.Equal(t, QUICTwinAdded, d, "twin %s must be admitted", twins[i])
	}

	var res burstResult
	last, lastChange := before, start
	for {
		now := time.Now()
		if v := c.version.Load(); v != last {
			last, lastChange = v, now
		}
		if res.allPublished == 0 && twinsInSnapshot(t, c, twins) == len(twins) {
			res.allPublished = now.Sub(start)
		}
		if res.allPublished != 0 && now.Sub(lastChange) >= quiet {
			break
		}
		require.Less(t, now.Sub(start), 30*time.Second, "the burst never settled")
		time.Sleep(time.Millisecond)
	}
	res.settled = lastChange.Sub(start)
	res.builds = last - before
	return res
}

// TestQUICAdmissionBurstCoalesces: ten simultaneous first-use admissions are
// published by a bounded number of snapshot builds -- one, or two when the
// burst straddles a build -- not one each, and every twin is published, each
// with its load assignment, well inside the on_demand budget.
func TestQUICAdmissionBurstCoalesces(t *testing.T) {
	c := newQUICBurstCache(t)
	twins := burstTwins(c)
	require.Zero(t, twinsInSnapshot(t, c, twins), "no twin may exist before first use (#1020)")

	res := fireQUICBurst(t, c, twins, 300*time.Millisecond)
	t.Logf("N=%d simultaneous admissions: %d snapshot builds; all twins published after %v; last build after %v",
		len(twins), res.builds, res.allPublished, res.settled)

	assert.Equal(t, len(twins), twinsInSnapshot(t, c, twins), "every admitted twin is published with its load assignment")
	assert.LessOrEqual(t, res.builds, uint64(2),
		"a burst of %d admissions must cost at most two snapshot builds, not one per admission (issue #1086)", len(twins))
	assert.Less(t, res.allPublished, 500*time.Millisecond,
		"the burst's twins must be published well inside the 2 s on_demand timeout")
	for _, name := range twins {
		assert.True(t, c.HasQUICPair(name), "the pair behind %s is recorded", name)
	}
}

// BenchmarkQUICAdmissionBurst reports what a 10-destination first use costs:
// snapshot builds per burst and the time until the last of them.
func BenchmarkQUICAdmissionBurst(b *testing.B) {
	var builds uint64
	var settled, published time.Duration
	for range b.N {
		b.StopTimer()
		c := newQUICBurstCache(b)
		twins := burstTwins(c)
		b.StartTimer()
		res := fireQUICBurst(b, c, twins, 100*time.Millisecond)
		builds += res.builds
		settled += res.settled
		published += res.allPublished
	}
	n := float64(b.N)
	b.ReportMetric(float64(builds)/n, "builds/burst")
	b.ReportMetric(float64(published.Milliseconds())/n, "published-ms/burst")
	b.ReportMetric(float64(settled.Milliseconds())/n, "settled-ms/burst")
}
