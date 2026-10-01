package cache

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Issue #1105: the snapshot's delta version map is built by the agent, before
// SetSnapshot, from a memo of the previous build. These tests pin the contract
// that makes that safe: a resource's version changes iff its bytes change, the
// versions are byte-for-byte go-control-plane's own, and a removed resource
// is reported removed.

func memoCluster(name string, timeout time.Duration) *clusterv3.Cluster {
	return &clusterv3.Cluster{Name: name, ConnectTimeout: durationpb.New(timeout)}
}

func clusterSnapshot(t *testing.T, version string, clusters ...*clusterv3.Cluster) *cachev3.Snapshot {
	t.Helper()
	rs := make([]types.Resource, 0, len(clusters))
	for _, cl := range clusters {
		rs = append(rs, cl)
	}
	s, err := cachev3.NewSnapshot(version, map[resourcev3.Type][]types.Resource{resourcev3.ClusterType: rs})
	require.NoError(t, err)
	return s
}

// gcpVersions is go-control-plane's own version map for the same resources,
// computed on a fresh snapshot so nothing the memo did can leak into it.
func gcpVersions(t *testing.T, s *cachev3.Snapshot) map[string]map[string]string {
	t.Helper()
	res := map[resourcev3.Type][]types.Resource{}
	for i, group := range s.Resources {
		typeURL, err := cachev3.GetResponseTypeURL(types.ResponseType(i))
		require.NoError(t, err)
		for _, item := range group.Items {
			res[typeURL] = append(res[typeURL], item.Resource)
		}
	}
	fresh, err := cachev3.NewSnapshot(s.GetVersion(resourcev3.ClusterType), res)
	require.NoError(t, err)
	require.NoError(t, fresh.ConstructVersionMap())
	return fresh.VersionMap
}

// TestVersionMemoVersionChangesIffBytesChange: across two builds, the same
// proto object keeps its version without being re-hashed, a changed resource
// gets a new version, a new object with identical bytes keeps the old version,
// and a removed resource has none -- and every version equals the one
// go-control-plane computes itself.
func TestVersionMemoVersionChangesIffBytesChange(t *testing.T) {
	m := &versionMemo{auditEvery: time.Hour}
	now := time.Now()

	same := memoCluster("same", time.Second)
	changed := memoCluster("changed", time.Second)
	cloned := memoCluster("cloned", time.Second)
	removed := memoCluster("removed", time.Second)
	s1 := clusterSnapshot(t, "1", same, changed, cloned, removed)
	st, err := m.fill(s1, now)
	require.NoError(t, err)
	assert.True(t, st.audit, "the first build has nothing to reuse and is an audit")
	assert.Equal(t, 4, st.hashed)
	assert.Equal(t, gcpVersions(t, s1), s1.VersionMap, "versions are go-control-plane's own")
	v1 := s1.GetVersionMap(resourcev3.ClusterType)

	s2 := clusterSnapshot(t, "2",
		same,
		memoCluster("changed", 2*time.Second),
		proto.Clone(cloned).(*clusterv3.Cluster),
		memoCluster("added", time.Second),
	)
	st, err = m.fill(s2, now.Add(time.Second))
	require.NoError(t, err)
	assert.False(t, st.audit)
	assert.Equal(t, 1, st.hits, "only the identical proto object is reused")
	assert.Equal(t, 3, st.hashed, "changed, cloned and added are hashed")
	assert.Zero(t, st.mismatchN)
	assert.Equal(t, gcpVersions(t, s2), s2.VersionMap, "a memoized version equals the one go-control-plane computes")

	v2 := s2.GetVersionMap(resourcev3.ClusterType)
	assert.Equal(t, v1["same"], v2["same"], "unchanged resource -> same version")
	assert.NotEqual(t, v1["changed"], v2["changed"], "changed resource -> new version")
	assert.Equal(t, v1["cloned"], v2["cloned"], "a rebuilt resource with identical bytes keeps its version (no spurious delta push)")
	assert.NotContains(t, v2, "removed", "a removed resource has no version")
	assert.Contains(t, v2, "added")

	// The memo holds exactly the current build: nothing removed is retained.
	assert.ElementsMatch(t, []string{"same", "changed", "cloned", "added"}, slices.Collect(maps.Keys(m.entries[resourcev3.ClusterType])))
}

// TestVersionMemoAuditCatchesInPlaceMutation documents the one way the memo
// can be wrong -- a published proto mutated in place keeps its stale version
// until the next audit -- and that the audit both counts it and publishes the
// correct version. In strict mode (every test in this package) it panics.
func TestVersionMemoAuditCatchesInPlaceMutation(t *testing.T) {
	now := time.Now()
	m := &versionMemo{auditEvery: time.Hour}
	cl := memoCluster("mutated", time.Second)
	s1 := clusterSnapshot(t, "1", cl)
	_, err := m.fill(s1, now)
	require.NoError(t, err)
	stale := s1.GetVersionMap(resourcev3.ClusterType)["mutated"]

	cl.ConnectTimeout = durationpb.New(5 * time.Second) // the forbidden in-place edit
	s2 := clusterSnapshot(t, "2", cl)
	st, err := m.fill(s2, now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, stale, s2.GetVersionMap(resourcev3.ClusterType)["mutated"],
		"between audits the memo cannot see an in-place mutation -- why the rule exists")
	assert.Equal(t, 1, st.hits)

	s3 := clusterSnapshot(t, "3", cl)
	st, err = m.fill(s3, now.Add(2*time.Hour))
	require.NoError(t, err)
	assert.True(t, st.audit)
	assert.Equal(t, 1, st.mismatchN, "the audit counts the mutated resource")
	assert.Equal(t, []string{resourcev3.ClusterType + "/mutated"}, st.mismatches)
	assert.Equal(t, gcpVersions(t, s3), s3.VersionMap, "and publishes its correct version")
	assert.NotEqual(t, stale, s3.GetVersionMap(resourcev3.ClusterType)["mutated"])

	strict := &versionMemo{strict: true}
	cl2 := memoCluster("mutated", time.Second)
	_, err = strict.fill(clusterSnapshot(t, "1", cl2), now)
	require.NoError(t, err)
	cl2.ConnectTimeout = durationpb.New(5 * time.Second)
	assert.Panics(t, func() { _, _ = strict.fill(clusterSnapshot(t, "2", cl2), now) },
		"strict mode (this package's tests) fails the test that mutated a published proto")
}

// TestSnapshotArrivesVersioned: generateSnapshot hands SetSnapshot a snapshot
// whose version map is already built -- even with no delta watch open -- and
// equal to go-control-plane's, so neither SetSnapshot nor CreateDeltaWatch
// has anything left to hash under the cache mutex.
func TestSnapshotArrivesVersioned(t *testing.T) {
	c, _, _ := newBuildBenchCache(t)
	got, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	snap, ok := got.(*cachev3.Snapshot)
	require.True(t, ok)
	require.NotNil(t, snap.VersionMap, "the snapshot must reach SetSnapshot versioned (#1105)")
	assert.Equal(t, gcpVersions(t, snap), snap.VersionMap)
}

// deltaDiff is what a delta response carried.
func deltaDiff(t *testing.T, ch chan cachev3.DeltaResponse) (resent map[string]string, removed []string) {
	t.Helper()
	select {
	case r := <-ch:
		resp, err := r.GetDeltaDiscoveryResponse()
		require.NoError(t, err)
		resent = map[string]string{}
		for _, res := range resp.GetResources() {
			resent[res.GetName()] = res.GetVersion()
		}
		return resent, resp.GetRemovedResources()
	case <-time.After(5 * time.Second):
		t.Fatal("no delta response")
		return nil, nil
	}
}

// expectedDelta is the exact delta an up-to-date subscriber must receive
// between two version maps: every resource whose version changed or is new,
// and every resource that disappeared.
func expectedDelta(before, after map[string]string) (resent map[string]string, removed []string) {
	resent = map[string]string{}
	for n, v := range after {
		if before[n] != v {
			resent[n] = v
		}
	}
	for n := range before {
		if _, ok := after[n]; !ok {
			removed = append(removed, n)
		}
	}
	return resent, removed
}

// assertVersionsAreContentHashes re-derives every version of the current
// snapshot from the resource bytes, independently of the memo.
func assertVersionsAreContentHashes(t *testing.T, c *SnapshotCache) {
	t.Helper()
	got, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	snap := got.(*cachev3.Snapshot)
	assert.Equal(t, gcpVersions(t, snap), snap.VersionMap)
}

// TestDeltaPushesExactlyWhatChanged drives the real SnapshotCache through a
// registry refresh that changes ONE service and a pod removal, with the
// production memo posture (audits a minute apart, so unchanged resources are
// memo hits), and checks the delta an up-to-date ADS subscriber receives:
// exactly the changed resources and exactly the removed ones -- nothing
// unchanged re-pushed, nothing changed withheld.
func TestDeltaPushesExactlyWhatChanged(t *testing.T) {
	ctx := context.Background()
	c, reg, flip := newBuildBenchCache(t)
	productionMemo(c)

	versions := func(typ string) map[string]string {
		got, err := c.GetSnapshot("node-1")
		require.NoError(t, err)
		return maps.Clone(got.GetVersionMap(typ))
	}

	// 1. One service's endpoints change: EDS carries its load assignments
	//    (the bare one and its aliases), nothing else. flip 0 -> 42 changes
	//    svc-000 (back to 3 endpoints) and svc-042 (down to 2).
	edsBefore := versions(resourcev3.EndpointType)
	ldsBefore := versions(resourcev3.ListenerType)
	eds, cancelEDS := openUpToDateDeltaWatch(t, c, resourcev3.EndpointType)
	defer cancelEDS()
	flip.Store(42)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	assertVersionsAreContentHashes(t, c)
	resent, removed := deltaDiff(t, eds)
	wantResent, wantRemoved := expectedDelta(edsBefore, versions(resourcev3.EndpointType))
	assert.Equal(t, wantResent, resent)
	assert.ElementsMatch(t, wantRemoved, removed)
	require.NotEmpty(t, resent, "the changed service's load assignment must be pushed")
	for name := range resent {
		assert.True(t, strings.Contains(name, "svc-042") || strings.Contains(name, "svc-000"),
			"only the two changed services' load assignments may be re-pushed, got %s", name)
	}
	assert.Equal(t, ldsBefore, versions(resourcev3.ListenerType), "an endpoint change leaves every listener version alone")

	// 2. A pod goes away: its listeners are removed, the others are not
	//    re-pushed.
	lds, cancelLDS := openUpToDateDeltaWatch(t, c, resourcev3.ListenerType)
	defer cancelLDS()
	require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-app-07"))
	assertVersionsAreContentHashes(t, c)
	resent, removed = deltaDiff(t, lds)
	wantResent, wantRemoved = expectedDelta(ldsBefore, versions(resourcev3.ListenerType))
	assert.Equal(t, wantResent, resent)
	assert.ElementsMatch(t, wantRemoved, removed)
	require.NotEmpty(t, removed, "the removed pod's listeners must be reported removed")
	assert.Less(t, len(resent), len(ldsBefore)/4, "unchanged listeners must not be re-pushed")

	// 3. A pod comes back with identical config: its listeners are rebuilt
	//    (new proto objects) and re-sent, as new resources, once.
	lds, cancelLDS2 := openUpToDateDeltaWatch(t, c, resourcev3.ListenerType)
	defer cancelLDS2()
	before := versions(resourcev3.ListenerType)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "app-07", Namespace: "demo", ServiceAccount: "sa-7",
		NetworkNamespace: "/var/run/netns/cni-app-07", ContainerId: "c-07",
		Ips:    []string{"10.1.0.8"},
		Labels: map[string]string{"app": "svc-007"},
	}, quicDemandTD))
	assertVersionsAreContentHashes(t, c)
	resent, removed = deltaDiff(t, lds)
	wantResent, _ = expectedDelta(before, versions(resourcev3.ListenerType))
	assert.Equal(t, wantResent, resent)
	assert.Empty(t, removed)
}

// TestSnapshotBuildRacesADSStream (run it under --config=race): snapshot
// builds -- whose version maps are now written by the agent -- race delta
// watches opening, being answered and cancelling, as the ADS stream does, and
// readers of the published version maps. Every final version must still be
// the content hash.
func TestSnapshotBuildRacesADSStream(t *testing.T) {
	ctx := context.Background()
	c, reg, flip := newBuildBenchCache(t)
	productionMemo(c)

	const rounds = 30
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // registry refreshes
		defer wg.Done()
		for i := range rounds {
			flip.Store(int64(i))
			assert.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		}
	}()
	go func() { // pod churn
		defer wg.Done()
		for i := range rounds {
			netns := fmt.Sprintf("/var/run/netns/cni-churn-%d", i%3)
			if i%2 == 0 {
				assert.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
					Name: fmt.Sprintf("churn-%d", i%3), Namespace: "demo", ServiceAccount: "sa-1",
					NetworkNamespace: netns, ContainerId: "c", Ips: []string{"10.1.1.1"},
				}, quicDemandTD))
			} else {
				assert.NoError(t, c.RemovePod(ctx, netns))
			}
		}
	}()
	go func() { // the ADS stream
		defer wg.Done()
		for i := range rounds * 4 {
			typ := buildBenchTypes[i%len(buildBenchTypes)]
			got, err := c.GetSnapshot("node-1")
			if !assert.NoError(t, err) {
				return
			}
			returned := maps.Clone(got.GetVersionMap(typ))
			sub := streamv3.NewDeltaSubscription(nil, nil, returned, true)
			req := &discoveryv3.DeltaDiscoveryRequest{Node: &corev3.Node{Id: "node-1"}, TypeUrl: typ, ResponseNonce: "1"}
			ch := make(chan cachev3.DeltaResponse, 1)
			cancel, err := c.CreateDeltaWatch(req, &sub, ch)
			assert.NoError(t, err)
			select {
			case r := <-ch:
				_, err := r.GetDeltaDiscoveryResponse()
				assert.NoError(t, err)
			case <-time.After(time.Millisecond):
			}
			if cancel != nil {
				cancel()
			}
		}
	}()
	wg.Wait()
	require.NoError(t, c.generateSnapshot(ctx))
	assertVersionsAreContentHashes(t, c)
}

// productionMemo gives a benchmark cache the production version-memo posture
// (audit once a minute, count rather than panic) instead of this package's
// strict every-build test audit.
func productionMemo(c *SnapshotCache) {
	c.versions.auditEvery = defaultVersionMemoAuditEvery
	c.versions.strict = false
}

// BenchmarkSnapshotVersioning splits the versioning cost at the
// BenchmarkSnapshotBuild shape: go-control-plane's full ConstructVersionMap
// (fullhash), the memo versioning the same unchanged snapshot (memofill), and
// SetSnapshot with up-to-date delta watches open, for an unversioned snapshot
// (what main handed it: the hashing ran under the cache mutex) and for a
// memo-versioned one (what #1105 hands it).
func BenchmarkSnapshotVersioning(b *testing.B) {
	ctx := context.Background()
	c, _, _ := newBuildBenchCache(b)
	productionMemo(c)
	var hash, memo, setMain, setFix []time.Duration
	var n int
	for _, typ := range buildBenchTypes {
		snap, err := c.GetSnapshot("node-1")
		require.NoError(b, err)
		n += len(snap.GetResources(typ))
	}
	// setSnapshot times SetSnapshot with up-to-date delta watches open.
	setSnapshot := func(s *cachev3.Snapshot) time.Duration {
		b.StopTimer()
		cancels := armDeltaWatches(b, c)
		b.StartTimer()
		t0 := time.Now()
		require.NoError(b, c.SnapshotCache.SetSnapshot(ctx, "node-1", s))
		d := time.Since(t0)
		b.StopTimer()
		for _, cancel := range cancels {
			cancel()
		}
		b.StartTimer()
		return d
	}
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		s := cloneSnapshot(b, c, fmt.Sprintf("h%d", i))
		b.StartTimer()
		t0 := time.Now()
		require.NoError(b, s.ConstructVersionMap())
		hash = append(hash, time.Since(t0))

		b.StopTimer()
		s = cloneSnapshot(b, c, fmt.Sprintf("m%d", i))
		b.StartTimer()
		t0 = time.Now()
		_, err := c.versions.fill(s, time.Now())
		require.NoError(b, err)
		memo = append(memo, time.Since(t0))

		// main: the snapshot reaches SetSnapshot unversioned, so the
		// first delta watch hashes it under the mutex.
		b.StopTimer()
		unversioned := cloneSnapshot(b, c, fmt.Sprintf("u%d", i))
		b.StartTimer()
		setMain = append(setMain, setSnapshot(unversioned))
		// #1105: the snapshot arrives versioned.
		setFix = append(setFix, setSnapshot(s))
	}
	b.StopTimer()
	b.ReportMetric(float64(n), "resources")
	b.ReportMetric(pct(hash, 0.5), "fullhash-p50-ms")
	b.ReportMetric(pct(hash, 0.99), "fullhash-p99-ms")
	b.ReportMetric(pct(memo, 0.5), "memofill-p50-ms")
	b.ReportMetric(pct(memo, 0.99), "memofill-p99-ms")
	b.ReportMetric(pct(setMain, 0.5), "set-unversioned-p50-ms")
	b.ReportMetric(pct(setMain, 0.99), "set-unversioned-p99-ms")
	b.ReportMetric(pct(setFix, 0.5), "set-versioned-p50-ms")
	b.ReportMetric(pct(setFix, 0.99), "set-versioned-p99-ms")
}
