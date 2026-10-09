package cache

import (
	"context"
	"fmt"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// snapshotVersion is the version of the snapshot the cache currently serves:
// what a delta response built from it carries as system_version_info.
func snapshotVersion(t *testing.T, c *SnapshotCache) string {
	t.Helper()
	snap, err := c.GetSnapshot(c.nodeName)
	require.NoError(t, err)
	v := snap.GetVersion(resourcev3.ClusterType)
	require.NotEmpty(t, v)
	return v
}

// TestAckedPinGaugeIsWhatTheProxyAcknowledged is the published/acknowledged
// half of #1425. The published gauge moves when the agent sets a snapshot; the
// acknowledged one moves only when a proxy acknowledges a cluster update, and
// to the state of the snapshot that update was built from. Between the two a
// reader can tell "the agent has pinned it" from "the proxy has it pinned".
func TestAckedPinGaugeIsWhatTheProxyAcknowledged(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))

	reg := &pinRegistry{http: map[string][]*registryv1.ServiceEndpoint{
		"demo/web": {nsEndpoint("10.0.0.10", "demo")},
		"demo/a":   {nsEndpoint("10.0.0.20", "")},
	}}
	declareDeps(c, "demo/web", "demo/a")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg.registry()))
	require.NoError(t, c.generateSnapshot(ctx))
	unpinnedVersion := snapshotVersion(t, c)

	_, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.False(t, ok, "published is not acknowledged: nothing is recorded before a cluster ACK")

	// An ACK of another resource type says nothing about clusters.
	c.ResponseAcked(ctx, resourcev3.EndpointType, unpinnedVersion)
	c.ResponseAcked(ctx, resourcev3.ListenerType, unpinnedVersion)
	_, ok = readPinGauge(t, reader, ackedTLSClustersGauge)
	require.False(t, ok, "only a cluster ACK moves the acknowledged gauge")

	rec.reset()
	c.ResponseAcked(ctx, resourcev3.ClusterType, unpinnedVersion)
	acked, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok)
	assert.Equal(t, int64(3), acked.pinned)
	assert.Equal(t, byCause(0, 3, 0), acked.unpinned)
	lines := rec.with(ackedClusterPinsMsg)
	require.Len(t, lines, 1, "the acknowledged state changed and something in it is unpinned: one line")
	assert.Equal(t, "3", lines[0].attrs["unpinned"])
	assert.Equal(t, "3", lines[0].attrs[string(cachemetrics.CauseNoNamespaceMetadata)])
	assert.Equal(t, "0", lines[0].attrs[string(cachemetrics.CauseTrustDomainUnknown)])
	assert.Equal(t, unpinnedVersion, lines[0].attrs["snapshot_version"])
	assert.NotContains(t, fmt.Sprint(lines[0].attrs), "a.demo.", "counts only: the names are on the snapshot's own line")

	// The agent publishes a snapshot that pins it. Published says pinned; the
	// proxy has not acknowledged it yet and still holds the unpinned cluster.
	reg.http["demo/a"] = []*registryv1.ServiceEndpoint{nsEndpoint("10.0.0.20", "demo")}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg.registry()))
	require.NoError(t, c.generateSnapshot(ctx))
	pinnedVersion := snapshotVersion(t, c)
	require.NotEqual(t, unpinnedVersion, pinnedVersion)

	published, _ := readPinGauge(t, reader, tlsClustersGauge)
	assert.Equal(t, byCause(0, 0, 0), published.unpinned, "published: pinned")
	acked, _ = readPinGauge(t, reader, ackedTLSClustersGauge)
	assert.Equal(t, byCause(0, 3, 0), acked.unpinned, "acknowledged: still the snapshot before it")

	// A second ACK of the old snapshot (another proxy generation, say) changes
	// nothing and is silent; so is a version the cache has never set.
	rec.reset()
	c.ResponseAcked(ctx, resourcev3.ClusterType, unpinnedVersion)
	c.ResponseAcked(ctx, resourcev3.ClusterType, "not-a-version")
	c.ResponseAcked(ctx, resourcev3.ClusterType, "")
	assert.Empty(t, rec.with(ackedClusterPinsMsg), "the acknowledged state did not change: no line")
	acked, _ = readPinGauge(t, reader, ackedTLSClustersGauge)
	assert.Equal(t, byCause(0, 3, 0), acked.unpinned, "an unknown version is not guessed at")

	// The proxy acknowledges the pinned snapshot.
	c.ResponseAcked(ctx, resourcev3.ClusterType, pinnedVersion)
	acked, _ = readPinGauge(t, reader, ackedTLSClustersGauge)
	assert.Equal(t, int64(6), acked.pinned)
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
	assert.Empty(t, rec.with(ackedClusterPinsMsg))
	require.Len(t, rec.with(ackedClusterPinsClearMsg), 1, "the return to all-pinned is said once")

	c.ResponseAcked(ctx, resourcev3.ClusterType, pinnedVersion)
	assert.Len(t, rec.with(ackedClusterPinsClearMsg), 1, "and not again while nothing changes")
}

// TestPinHistoryIsBounded: the history is a ring. A version more than
// pinHistorySize snapshots old is forgotten, and an ACK naming it moves
// nothing: it is not attributed to some other snapshot.
func TestPinHistoryIsBounded(t *testing.T) {
	var h pinHistory
	for i := range pinHistorySize + 3 {
		h.remember(fmt.Sprintf("v%d", i), cachemetrics.PinCounts{Pinned: i})
	}
	for _, gone := range []string{"v0", "v1", "v2"} {
		_, known, _ := h.ack(gone)
		assert.False(t, known, "%s fell out of the ring", gone)
	}
	counts, known, changed := h.ack("v3")
	require.True(t, known, "the oldest version still in the ring")
	assert.True(t, changed, "the first acknowledged state is a change")
	assert.Equal(t, 3, counts.Pinned)
	counts, known, changed = h.ack(fmt.Sprintf("v%d", pinHistorySize+2))
	require.True(t, known)
	assert.True(t, changed)
	assert.Equal(t, pinHistorySize+2, counts.Pinned)
	_, _, changed = h.ack(fmt.Sprintf("v%d", pinHistorySize+2))
	assert.False(t, changed)
}

// TestPinReportCostsNoAllocationPerCluster: the report runs on every snapshot
// build, over every cluster entry. On a node where every cluster is pinned it
// allocates nothing, whatever the number of clusters, and remembering it in
// the history allocates nothing either: the counts are a fixed-size value and
// names are collected only for the unpinned.
func TestPinReportCostsNoAllocationPerCluster(t *testing.T) {
	c, _, _ := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	const n = 500
	for i := range n {
		addPinnedCluster(c, fmt.Sprintf("svc-%03d.aether-test.aether.internal", i))
	}

	var report pinReport
	allocs := testing.AllocsPerRun(20, func() {
		report = c.clusterPinReport()
		c.pins.remember("v", report.counts)
	})
	require.Equal(t, n, report.counts.Pinned, "fixture: every entry is a pinned TLS cluster")
	assert.Zero(t, allocs, "allocations per report over %d pinned clusters", n)
}

// TestPinReportIsOfTheClustersTheBuildRead: a snapshot's pin report is filed
// under that snapshot's version, so it has to describe the cluster entries the
// build collected its clusters from, not the cluster map as it is some time
// later. The build therefore takes both in one read. Here the map changes
// right after that read, as a registry reload landing mid-build would change
// it: the report still counts what the resources were built from.
func TestPinReportIsOfTheClustersTheBuildRead(t *testing.T) {
	c, _, _ := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	addPinnedCluster(c, bindingClusterName)

	clusters, _, _, pins := c.clustersEndpointsVhostsAndPins()
	addOutboundCluster(c, "late.aether-test.aether.internal") // unpinned, after the read

	require.Len(t, clusters, 1, "the build read one cluster")
	assert.Equal(t, cachemetrics.PinCounts{Pinned: 1}, pins.counts, "and its report is of that one")
	assert.Empty(t, pins.unpinned)
	later := c.clusterPinReport()
	assert.Equal(t, 1, later.counts.UnpinnedTotal(), "fixture: a later read of the map does see the new entry")
}

// TestSnapshotIsInThePinHistoryBeforeItCanBeAcked: the pin state is
// remembered before SetSnapshot, which is what makes the snapshot visible to a
// proxy. A watch that is already open is answered from inside SetSnapshot; by
// then the version must be known, or a fast ACK finds nothing.
func TestSnapshotIsInThePinHistoryBeforeItCanBeAcked(t *testing.T) {
	c, _, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	// What a connected proxy holds: the clusters of the snapshot so far.
	first := make(chan cachev3.DeltaResponse, 1)
	_, err := c.CreateDeltaWatch(&discoveryv3.DeltaDiscoveryRequest{
		Node:    &corev3.Node{Id: c.nodeName},
		TypeUrl: resourcev3.ClusterType,
	}, streamv3.NewDeltaSubscription(nil, nil, nil, true), first)
	require.NoError(t, err)
	held := map[string]string{}
	select {
	case raw := <-first:
		resp, err := raw.GetDeltaDiscoveryResponse()
		require.NoError(t, err)
		for _, r := range resp.GetResources() {
			held[r.GetName()] = r.GetVersion()
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first wildcard CDS request was not answered")
	}

	// Its next request (the ACK of that response) finds nothing new, so the
	// watch stays OPEN, to be answered by the next snapshot that changes a
	// cluster, from inside SetSnapshot.
	//
	// Two such watches, on unbuffered channels, make "the ACK arrives before
	// SetSnapshot returns" certain instead of likely: SetSnapshot writes one
	// response, the "proxy" below takes it and acknowledges it, and only then
	// takes the second, which SetSnapshot is blocked writing until that moment.
	watches := [2]chan cachev3.DeltaResponse{make(chan cachev3.DeltaResponse), make(chan cachev3.DeltaResponse)}
	for _, responses := range watches {
		cancel, err := c.CreateDeltaWatch(&discoveryv3.DeltaDiscoveryRequest{
			Node:          &corev3.Node{Id: c.nodeName},
			TypeUrl:       resourcev3.ClusterType,
			ResponseNonce: "n1",
		}, streamv3.NewDeltaSubscription(nil, nil, held, true), responses)
		require.NoError(t, err)
		require.NotNil(t, cancel, "fixture: the watch must be open, not answered at once")
		defer cancel()
	}
	acked := make(chan string, 1)
	go func() {
		var resp cachev3.DeltaResponse
		other := watches[1]
		select {
		case resp = <-watches[0]:
		case resp = <-watches[1]:
			other = watches[0]
		}
		version := resp.GetResponseVersion()
		c.ResponseAcked(ctx, resourcev3.ClusterType, version)
		<-other
		acked <- version
	}()

	addPinnedCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	var version string
	select {
	case version = <-acked:
	case <-time.After(10 * time.Second):
		t.Fatal("the open CDS watch was not answered by the snapshot")
	}
	assert.Equal(t, snapshotVersion(t, c), version, "a delta response carries the snapshot's version as system_version_info")

	got, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok, "the ACK found its snapshot in the history")
	assert.Equal(t, int64(1), got.pinned)
	assert.Equal(t, byCause(0, 0, 0), got.unpinned)
}

// TestAckTrackerFeedsTheAckedPinGauge wires the two as the agent does
// (ack.Tracker.SetAckObserver(cache.ResponseAcked)) and drives the tracker
// with a real delta response of the cache: the ACK of a cluster response moves
// the gauge, a NACK does not.
func TestAckTrackerFeedsTheAckedPinGauge(t *testing.T) {
	c, _, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	addOutboundCluster(c, bindingClusterName) // no namespace: unpinned
	require.NoError(t, c.generateSnapshot(ctx))

	tracker := ack.NewTracker(c.log)
	tracker.SetAckObserver(c.ResponseAcked)
	callbacks := tracker.Callbacks()

	// cdsResponse asks the cache for the cluster set, as a proxy that holds
	// `held` would, and stamps the nonce the xDS server would.
	cdsResponse := func(nonce string, held map[string]string) *discoveryv3.DeltaDiscoveryResponse {
		t.Helper()
		responses := make(chan cachev3.DeltaResponse, 1)
		cancel, err := c.CreateDeltaWatch(&discoveryv3.DeltaDiscoveryRequest{
			Node:    &corev3.Node{Id: c.nodeName},
			TypeUrl: resourcev3.ClusterType,
		}, streamv3.NewDeltaSubscription(nil, nil, held, true), responses)
		require.NoError(t, err)
		if cancel != nil {
			defer cancel()
		}
		select {
		case raw := <-responses:
			resp, err := raw.GetDeltaDiscoveryResponse()
			require.NoError(t, err)
			resp.Nonce = nonce
			return resp
		case <-time.After(10 * time.Second):
			t.Fatal("no CDS response")
			return nil
		}
	}

	const stream = int64(7)
	first := cdsResponse("n1", nil)
	require.Equal(t, snapshotVersion(t, c), first.GetSystemVersionInfo())
	held := map[string]string{}
	for _, r := range first.GetResources() {
		held[r.GetName()] = r.GetVersion()
	}
	callbacks.OnStreamDeltaResponse(stream, nil, first)
	_, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.False(t, ok, "sent is not acknowledged")

	require.NoError(t, callbacks.OnStreamDeltaRequest(stream, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResponseNonce: "n1",
	}))
	got, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok, "the ACK reached the cache")
	assert.Equal(t, byCause(0, 1, 0), got.unpinned)

	// The agent pins the cluster; the proxy REJECTS the update. It stays on
	// what it had, and so does the acknowledged gauge, while the published one
	// has moved on.
	addPinnedCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	second := cdsResponse("n2", held)
	require.NotEmpty(t, second.GetResources(), "fixture: the pinned cluster is a changed resource")
	callbacks.OnStreamDeltaResponse(stream, nil, second)
	require.NoError(t, callbacks.OnStreamDeltaRequest(stream, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResponseNonce: "n2",
		ErrorDetail: status.New(codes.InvalidArgument, "rejected").Proto(),
	}))
	published, _ := readPinGauge(t, reader, tlsClustersGauge)
	assert.Equal(t, byCause(0, 0, 0), published.unpinned)
	got, _ = readPinGauge(t, reader, ackedTLSClustersGauge)
	assert.Equal(t, byCause(0, 1, 0), got.unpinned, "a NACK acknowledges nothing")

	// Sent again and accepted.
	third := cdsResponse("n3", held)
	callbacks.OnStreamDeltaResponse(stream, nil, third)
	require.NoError(t, callbacks.OnStreamDeltaRequest(stream, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResponseNonce: "n3",
	}))
	got, _ = readPinGauge(t, reader, ackedTLSClustersGauge)
	assert.Equal(t, int64(1), got.pinned)
	assert.Equal(t, byCause(0, 0, 0), got.unpinned)
}

// cdsExchange asks the cache for the cluster set as the xDS server does for
// one request of a stream, and returns the response it writes, stamped with
// nonce, or nil when the cache leaves the watch open instead of answering.
// Like the server, it records what the response carried as returned on the
// subscription the moment it is written, accepted or not.
func cdsExchange(t *testing.T, c *SnapshotCache, sub *streamv3.Subscription, requestNonce, nonce string) *discoveryv3.DeltaDiscoveryResponse {
	t.Helper()
	responses := make(chan cachev3.DeltaResponse, 1)
	cancel, err := c.CreateDeltaWatch(&discoveryv3.DeltaDiscoveryRequest{
		Node:          &corev3.Node{Id: c.nodeName},
		TypeUrl:       resourcev3.ClusterType,
		ResponseNonce: requestNonce,
	}, *sub, responses)
	require.NoError(t, err)
	if cancel != nil {
		cancel()
		return nil
	}
	raw := <-responses
	resp, err := raw.GetDeltaDiscoveryResponse()
	require.NoError(t, err)
	resp.Nonce = nonce
	sub.SetReturnedResources(raw.GetNextVersionMap())
	return resp
}

// clusterVersions is the per-cluster versions of the snapshot the cache
// serves: what a proxy that holds exactly those clusters states in the
// initial_resource_versions of its first request on a new stream.
func clusterVersions(t *testing.T, c *SnapshotCache) map[string]string {
	t.Helper()
	snap, err := c.GetSnapshot(c.nodeName)
	require.NoError(t, err)
	versions := snap.GetVersionMap(resourcev3.ClusterType)
	require.NotEmpty(t, versions)
	return versions
}

// TestAckedPinGaugeAfterAnAgentRestartAgainstAnInSyncProxy is #1483: an agent
// that restarts finds a proxy holding exactly the clusters it publishes. The
// proxy says so in its first request, the cache answers with an empty response
// naming the snapshot, the proxy ACKs it, and that is the acknowledged gauge's
// first sample. Before, the gauge had none until a cluster next changed.
func TestAckedPinGaugeAfterAnAgentRestartAgainstAnInSyncProxy(t *testing.T) {
	c, _, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	addOutboundCluster(c, bindingClusterName) // no namespace: unpinned
	require.NoError(t, c.generateSnapshot(ctx))

	tracker := ack.NewTracker(c.log)
	tracker.SetAckObserver(c.ResponseAcked)
	callbacks := tracker.Callbacks()
	const stream = int64(1)

	// The proxy's first request on the new stream: it holds every cluster the
	// restarted agent publishes, at the version it publishes it.
	sub := streamv3.NewDeltaSubscription(nil, nil, clusterVersions(t, c), true)
	opening := cdsExchange(t, c, &sub, "", "n1")
	require.NotNil(t, opening, "the first wildcard request of a stream is answered even when nothing is owed")
	require.Empty(t, opening.GetResources())
	require.Empty(t, opening.GetRemovedResources())
	require.Equal(t, snapshotVersion(t, c), opening.GetSystemVersionInfo())

	callbacks.OnStreamDeltaResponse(stream, nil, opening)
	_, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.False(t, ok, "sent is not acknowledged")
	require.NoError(t, callbacks.OnStreamDeltaRequest(stream, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResponseNonce: "n1",
	}))

	acked, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok, "the proxy acknowledged the snapshot it stated it holds: the gauge has its sample")
	published, _ := readPinGauge(t, reader, tlsClustersGauge)
	assert.Equal(t, published, acked, "an in-sync proxy holds what is published")
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned)
}

// TestAckedPinGaugeMakesNoClaimForAProxyThatIsNotInSync is the other side: a
// reconnecting proxy whose stated clusters are not the snapshot's is sent the
// difference, and only its ACK of that moves the gauge. And an empty response
// later on the stream moves nothing, whatever it names.
//
// The second half runs the pinned go-control-plane cache through the case that
// makes the first-response rule necessary: after the proxy REJECTS a cluster
// update, a request without a nonce (an on-demand subscription) is answered
// with an empty response that names the rejected snapshot, and the proxy ACKs
// it.
func TestAckedPinGaugeMakesNoClaimForAProxyThatIsNotInSync(t *testing.T) {
	c, _, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	addOutboundCluster(c, bindingClusterName) // no namespace: unpinned
	require.NoError(t, c.generateSnapshot(ctx))
	unpinned := clusterVersions(t, c)

	// The agent pins the cluster. The proxy still holds the unpinned one.
	addPinnedCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	pinnedVersion := snapshotVersion(t, c)

	tracker := ack.NewTracker(c.log)
	tracker.SetAckObserver(c.ResponseAcked)
	callbacks := tracker.Callbacks()
	const stream = int64(1)

	sub := streamv3.NewDeltaSubscription(nil, nil, unpinned, true)
	opening := cdsExchange(t, c, &sub, "", "n1")
	require.NotNil(t, opening)
	require.NotEmpty(t, opening.GetResources(), "fixture: the proxy is owed the pinned cluster")
	callbacks.OnStreamDeltaResponse(stream, nil, opening)

	// The proxy rejects it.
	require.NoError(t, callbacks.OnStreamDeltaRequest(stream, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResponseNonce: "n1",
		ErrorDetail: status.New(codes.InvalidArgument, "rejected").Proto(),
	}))
	_, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.False(t, ok, "a NACK acknowledges nothing")

	// The NACK is also the next request: nothing changed since, so the watch
	// stays open and nothing is re-sent. The cache takes the rejected clusters
	// as delivered.
	require.Nil(t, cdsExchange(t, c, &sub, "n1", ""), "go-control-plane does not re-send what a proxy rejected")

	// An on-demand subscription: a request with no nonce. It is answered,
	// empty, in the name of the snapshot the proxy rejected, and ACKed.
	later := cdsExchange(t, c, &sub, "", "n2")
	require.NotNil(t, later, "fixture: a wildcard request without a nonce is answered")
	require.Empty(t, later.GetResources())
	require.Empty(t, later.GetRemovedResources())
	require.Equal(t, pinnedVersion, later.GetSystemVersionInfo(), "fixture: the empty response names the rejected snapshot")
	callbacks.OnStreamDeltaResponse(stream, nil, later)
	require.NoError(t, callbacks.OnStreamDeltaRequest(stream, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResponseNonce: "n2",
	}))
	_, ok = readPinGauge(t, reader, ackedTLSClustersGauge)
	assert.False(t, ok, "the proxy rejected the pinned cluster: an empty response later on the stream must not be read as it holding that snapshot")
}
