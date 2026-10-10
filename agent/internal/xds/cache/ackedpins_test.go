package cache

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
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

// clusterVersions is the per-cluster versions of the snapshot the cache
// serves: what a proxy that holds exactly those clusters states in the
// initial_resource_versions of its first request on a new stream.
func clusterVersions(t *testing.T, c *SnapshotCache) map[string]string {
	t.Helper()
	snap, err := c.GetSnapshot(c.nodeName)
	require.NoError(t, err)
	versions := snap.GetVersionMap(resourcev3.ClusterType)
	require.NotEmpty(t, versions)
	return maps.Clone(versions)
}

// wholeSnapshotAccepted is what the tracker tells of a proxy that held no
// cluster, opened a stream, was sent every cluster of the snapshot the cache
// serves now, and acknowledged them.
func wholeSnapshotAccepted(t *testing.T, c *SnapshotCache) ack.Accepted {
	t.Helper()
	accepted := ack.Accepted{
		TypeURL:       resourcev3.ClusterType,
		SystemVersion: snapshotVersion(t, c),
		Opening:       true,
		Stated:        map[string]string{},
	}
	for name, version := range clusterVersions(t, c) {
		accepted.Added = append(accepted.Added, ack.Resource{Name: name, Version: version})
	}
	return accepted
}

// cdsProxy plays one proxy generation's Cluster exchange on one delta stream
// against the cache's real go-control-plane watch logic, through the agent's
// real ACK tracker: the requests it sends reach the tracker's callbacks as the
// xDS server would deliver them, and so do the responses the cache writes.
type cdsProxy struct {
	t         *testing.T
	c         *SnapshotCache
	callbacks serverv3.Callbacks
	stream    int64
	sub       streamv3.Subscription
	nonces    int
	last      string
	// accepted is what this proxy would state on a new stream: the version of
	// every cluster it acknowledged.
	accepted map[string]string
}

// connectCDSProxy opens stream for a proxy that states it holds `stated` (nil
// for a proxy with no cluster), as its first Cluster request does.
func connectCDSProxy(t *testing.T, c *SnapshotCache, tracker *ack.Tracker, stream int64, stated map[string]string) *cdsProxy {
	t.Helper()
	p := &cdsProxy{
		t: t, c: c, callbacks: tracker.Callbacks(), stream: stream,
		sub:      streamv3.NewDeltaSubscription(nil, nil, stated, true),
		accepted: maps.Clone(stated),
	}
	if p.accepted == nil {
		p.accepted = map[string]string{}
	}
	require.NoError(t, p.callbacks.OnStreamDeltaRequest(stream, &discoveryv3.DeltaDiscoveryRequest{
		Node: &corev3.Node{Id: c.nodeName}, TypeUrl: resourcev3.ClusterType, InitialResourceVersions: stated,
	}))
	return p
}

// next is the response the cache writes to this proxy's pending request, or
// nil when it owes nothing and leaves the watch open. The response reaches the
// tracker before it is returned.
func (p *cdsProxy) next() *discoveryv3.DeltaDiscoveryResponse {
	p.t.Helper()
	resp := p.build()
	if resp == nil {
		return nil
	}
	p.written(resp)
	return resp
}

// build is the first half of next: the cache builds the response to this
// proxy's pending request and hands it over, and the stream has not written it
// yet, so the tracker has not been told. In the agent that is the time a
// response spends in the watch's channel, and snapshots can be built in it.
func (p *cdsProxy) build() *discoveryv3.DeltaDiscoveryResponse {
	p.t.Helper()
	p.nonces++
	return cdsExchange(p.t, p.c, &p.sub, p.last, "n"+strconv.Itoa(p.nonces))
}

// written is the second half: the stream writes resp, which is when the
// tracker learns of it.
func (p *cdsProxy) written(resp *discoveryv3.DeltaDiscoveryResponse) {
	p.t.Helper()
	p.callbacks.OnStreamDeltaResponse(p.stream, nil, resp)
}

// onDemand is the response to a request with no nonce in the middle of the
// stream, as an on-demand cluster subscription sends.
func (p *cdsProxy) onDemand() *discoveryv3.DeltaDiscoveryResponse {
	p.t.Helper()
	require.NoError(p.t, p.callbacks.OnStreamDeltaRequest(p.stream, &discoveryv3.DeltaDiscoveryRequest{TypeUrl: resourcev3.ClusterType}))
	last := p.last
	p.last = ""
	defer func() { p.last = last }()
	return p.next()
}

// ack acknowledges resp.
func (p *cdsProxy) ack(resp *discoveryv3.DeltaDiscoveryResponse) {
	p.t.Helper()
	require.NotNil(p.t, resp, "no response to acknowledge")
	for _, r := range resp.GetResources() {
		p.accepted[r.GetName()] = r.GetVersion()
	}
	for _, name := range resp.GetRemovedResources() {
		delete(p.accepted, name)
	}
	p.last = resp.GetNonce()
	require.NoError(p.t, p.callbacks.OnStreamDeltaRequest(p.stream, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResponseNonce: resp.GetNonce(),
	}))
}

// nack rejects resp. It changes nothing the proxy would state.
func (p *cdsProxy) nack(resp *discoveryv3.DeltaDiscoveryResponse) {
	p.t.Helper()
	require.NotNil(p.t, resp, "no response to reject")
	p.last = resp.GetNonce()
	require.NoError(p.t, p.callbacks.OnStreamDeltaRequest(p.stream, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType, ResponseNonce: resp.GetNonce(),
		ErrorDetail: status.New(codes.InvalidArgument, "rejected").Proto(),
	}))
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

// deltaNames is the names of the resources a delta response carries.
func deltaNames(resp *discoveryv3.DeltaDiscoveryResponse) []string {
	names := make([]string, 0, len(resp.GetResources()))
	for _, r := range resp.GetResources() {
		names = append(names, r.GetName())
	}
	return names
}

// ackedUnknownGauge is the gauge that says how many mesh clusters the proxy
// holds whose pin state the agent cannot determine (#1509).
const ackedUnknownGauge = "aether.agent.xds.acked_tls_clusters_unknown"

// readUnknownGauge reads that gauge: its one series, which carries no
// attribute. ok is false while it has no sample.
func readUnknownGauge(t *testing.T, reader *sdkmetric.ManualReader) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != ackedUnknownGauge {
				continue
			}
			gauge, isGauge := m.Data.(metricdata.Gauge[int64])
			require.True(t, isGauge, "%s is %T, want Gauge[int64]", ackedUnknownGauge, m.Data)
			if len(gauge.DataPoints) == 0 {
				return 0, false
			}
			require.Len(t, gauge.DataPoints, 1, "one series per agent")
			assert.Zero(t, gauge.DataPoints[0].Attributes.Len(), "no attribute: %v", gauge.DataPoints[0].Attributes)
			return gauge.DataPoints[0].Value, true
		}
	}
	return 0, false
}

// publish and accept are publishLocked and acceptLocked for a test that reads
// the update itself instead of having the cache report it.
func (h *ackedPins) publish(entries []entryClass, versions map[string]string, promoted bool) ackedPinsUpdate {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.publishLocked(entries, versions, promoted)
}

func (h *ackedPins) accept(a ack.Accepted) ackedPinsUpdate {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.acceptLocked(a)
}

// classOfOffered is the class the version was published with, when it is one
// of the published versions on record.
func (s *clusterAck) classOfOffered(version string) (pinClass, bool) {
	for _, o := range s.offered {
		if o.version == version && version != "" {
			return o.class, true
		}
	}
	return pinClassNone, false
}

// stepClock is a clock a test moves by hand (ackedPins.now).
type stepClock struct{ t time.Time }

func newStepClock() *stepClock { return &stepClock{t: time.Unix(1_700_000_000, 0)} }

func (c *stepClock) now() time.Time          { return c.t }
func (c *stepClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// buildPastTheGoneWindow moves the acknowledged pin state's clock on by
// goneAge and builds goneBuilds snapshots, after which the record of an entry
// that had left the snapshot before the call, is not held and is not in flight
// is dropped (forgetIfGoneLocked: both bounds have passed).
func buildPastTheGoneWindow(t *testing.T, c *SnapshotCache) {
	t.Helper()
	c.acked.mu.Lock()
	earlier := c.acked.now
	if earlier == nil {
		earlier = time.Now
	}
	c.acked.now = func() time.Time { return earlier().Add(goneAge) }
	c.acked.mu.Unlock()
	for range goneBuilds {
		require.NoError(t, c.generateSnapshot(context.Background()))
	}
}

// ackedPinFixture is a cache with a node identity and a trust domain, so every
// cluster it publishes is a TLS cluster, and a tracker wired to it as the
// agent wires them.
func ackedPinFixture(t *testing.T) (*SnapshotCache, *recorder, *sdkmetric.ManualReader, *ack.Tracker) {
	t.Helper()
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	tracker := ack.NewTracker(c.log)
	tracker.SetAckObserver(c.ResponseAccepted)
	tracker.SetDeliveryObserver(c.ResponseDelivery)
	return c, rec, reader, tracker
}

const (
	otherClusterName = "other.aether-test.aether.internal"
	addedClusterName = "added.aether-test.aether.internal"
)

// TestAckedPinGaugeIsWhatTheProxyAcknowledged is the published/acknowledged
// half of #1425. The published gauge moves when the agent sets a snapshot; the
// acknowledged one moves only when a proxy acknowledges clusters, and to the
// state of the clusters it acknowledged. Between the two a reader can tell
// "the agent has pinned it" from "the proxy has it pinned".
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
	unpinnedSnapshot := wholeSnapshotAccepted(t, c)

	_, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.False(t, ok, "published is not acknowledged: nothing is recorded before a cluster ACK")

	// An answer about another resource type says nothing about clusters.
	for _, typeURL := range []string{resourcev3.EndpointType, resourcev3.ListenerType} {
		other := unpinnedSnapshot
		other.TypeURL = typeURL
		c.ResponseAccepted(ctx, other)
	}
	_, ok = readPinGauge(t, reader, ackedTLSClustersGauge)
	require.False(t, ok, "only a cluster ACK moves the acknowledged gauge")

	rec.reset()
	c.ResponseAccepted(ctx, unpinnedSnapshot)
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
	pinnedSnapshot := wholeSnapshotAccepted(t, c)
	require.NotEqual(t, unpinnedVersion, pinnedSnapshot.SystemVersion)

	published, _ := readPinGauge(t, reader, tlsClustersGauge)
	assert.Equal(t, byCause(0, 0, 0), published.unpinned, "published: pinned")
	acked, _ = readPinGauge(t, reader, ackedTLSClustersGauge)
	assert.Equal(t, byCause(0, 3, 0), acked.unpinned, "acknowledged: still the clusters before it")

	// A second acknowledgement of the old clusters (another proxy generation,
	// say) changes nothing and is silent.
	rec.reset()
	c.ResponseAccepted(ctx, unpinnedSnapshot)
	assert.Empty(t, rec.with(ackedClusterPinsMsg), "the acknowledged state did not change: no line")
	acked, _ = readPinGauge(t, reader, ackedTLSClustersGauge)
	assert.Equal(t, byCause(0, 3, 0), acked.unpinned)

	// The proxy acknowledges the pinned clusters.
	c.ResponseAccepted(ctx, pinnedSnapshot)
	acked, _ = readPinGauge(t, reader, ackedTLSClustersGauge)
	assert.Equal(t, int64(6), acked.pinned)
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
	assert.Empty(t, rec.with(ackedClusterPinsMsg))
	require.Len(t, rec.with(ackedClusterPinsClearMsg), 1, "the return to all-pinned is said once")

	c.ResponseAccepted(ctx, pinnedSnapshot)
	assert.Len(t, rec.with(ackedClusterPinsClearMsg), 1, "and not again while nothing changes")
}

// TestAckedPinGaugeAfterARejectedClusterUpdate is #1508. The proxy rejects the
// update that pins a cluster; then an unrelated cluster is added and the proxy
// acknowledges that. The pinned go-control-plane cache does not send the
// rejected cluster again, so the acknowledged response carries the new cluster
// alone, in the name of a snapshot whose pin state counts the rejected cluster
// as pinned. The proxy still holds the unpinned one, and the gauge must say so
// until a response that carries the cluster is acknowledged.
func TestAckedPinGaugeAfterARejectedClusterUpdate(t *testing.T) {
	c, _, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addOutboundCluster(c, bindingClusterName) // no namespace: unpinned
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	// A new proxy: it states nothing and is sent both clusters.
	proxy := connectCDSProxy(t, c, tracker, 1, nil)
	proxy.ack(proxy.next())
	acked, ok := ackedGauge()
	require.True(t, ok)
	require.Equal(t, int64(1), acked.pinned)
	require.Equal(t, byCause(0, 1, 0), acked.unpinned)

	// The agent pins the cluster; the proxy rejects the update.
	addPinnedCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	pinning := proxy.next()
	require.Equal(t, []string{bindingClusterName}, deltaNames(pinning))
	proxy.nack(pinning)
	require.Nil(t, proxy.next(), "fixture: go-control-plane does not re-send what a proxy rejected")

	// An unrelated cluster is added, and the proxy acknowledges it.
	addPinnedCluster(c, addedClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	unrelated := proxy.next()
	require.Equal(t, []string{addedClusterName}, deltaNames(unrelated), "fixture: the acknowledged response carries the new cluster alone")
	require.Equal(t, snapshotVersion(t, c), unrelated.GetSystemVersionInfo())
	proxy.ack(unrelated)

	published, _ := readPinGauge(t, reader, tlsClustersGauge)
	require.Equal(t, int64(3), published.pinned, "fixture: the agent publishes three pinned clusters")
	require.Equal(t, byCause(0, 0, 0), published.unpinned)
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned,
		"the proxy rejected the pinned cluster and holds the unpinned one: the ACK of another cluster must not move it (#1508)")
	assert.Equal(t, int64(2), acked.pinned, "the two clusters the proxy did acknowledge")

	// Nor does an empty response later on the stream, which names the newest
	// snapshot and is acknowledged.
	empty := proxy.onDemand()
	require.NotNil(t, empty, "fixture: a wildcard request without a nonce is answered")
	require.Empty(t, empty.GetResources())
	proxy.ack(empty)
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned)

	// The stream is lost and the proxy reconnects. It states the unpinned
	// version, the one it accepted; it is sent the pinned one again and this
	// time takes it.
	require.NotEqual(t, clusterVersions(t, c)[bindingClusterName], proxy.accepted[bindingClusterName],
		"fixture: the proxy still states the version it held before the rejected update")
	again := connectCDSProxy(t, c, tracker, 2, proxy.accepted)
	resent := again.next()
	require.Equal(t, []string{bindingClusterName}, deltaNames(resent), "a new stream is owed what the proxy does not state")
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned, "sent is not acknowledged")
	again.ack(resent)
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned, "a response that carried the cluster was acknowledged")
	assert.Equal(t, int64(3), acked.pinned)

	// A removal the proxy acknowledges takes the cluster out, and its record
	// with it once the builds a response from before the removal could still
	// be written in have passed (goneBuilds).
	require.NoError(t, c.RemoveCluster(ctx, addedClusterName))
	removal := again.next()
	require.Equal(t, []string{addedClusterName}, removal.GetRemovedResources())
	acked, _ = ackedGauge()
	assert.Equal(t, int64(3), acked.pinned, "sent is not acknowledged")
	again.ack(removal)
	acked, _ = ackedGauge()
	assert.Equal(t, int64(2), acked.pinned)
	buildPastTheGoneWindow(t, c)
	assert.NotContains(t, c.acked.clusters, addedClusterName)
}

// TestAckedPinGaugeKeepsAClusterWhoseRemovalWasRejected: delta xDS removals.
// A cluster leaves the snapshot and the proxy rejects the response that
// removes it: the proxy still holds it, and it stays counted, at the class it
// was accepted at, although no snapshot publishes it any more. Its record goes
// when the proxy accepts the removal.
func TestAckedPinGaugeKeepsAClusterWhoseRemovalWasRejected(t *testing.T) {
	c, _, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addOutboundCluster(c, bindingClusterName) // unpinned
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	proxy := connectCDSProxy(t, c, tracker, 1, nil)
	proxy.ack(proxy.next())

	// The unpinned cluster is removed. The response that removes it also
	// carries a cluster the proxy refuses, so the whole of it is rejected.
	require.NoError(t, c.RemoveCluster(ctx, bindingClusterName))
	addPinnedCluster(c, addedClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	removal := proxy.next()
	require.Equal(t, []string{bindingClusterName}, removal.GetRemovedResources())
	proxy.nack(removal)

	acked, _ := ackedGauge()
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned, "a rejected removal removes nothing")

	// More builds and an unrelated ACK later it is still held and counted.
	addPinnedCluster(c, "more.aether-test.aether.internal")
	require.NoError(t, c.generateSnapshot(ctx))
	proxy.ack(proxy.next())
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned, "the proxy holds a cluster the agent no longer publishes")
	assert.Equal(t, int64(2), acked.pinned, "other and more; the cluster rejected with the removal is not acknowledged")
	require.Contains(t, c.acked.clusters, bindingClusterName, "its record is kept while the proxy holds it")

	// A new stream: the proxy states it, the cache removes it again, and the
	// proxy accepts.
	again := connectCDSProxy(t, c, tracker, 2, proxy.accepted)
	second := again.next()
	require.Equal(t, []string{bindingClusterName}, second.GetRemovedResources())
	again.ack(second)
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
	assert.Equal(t, int64(3), acked.pinned)
	buildPastTheGoneWindow(t, c)
	assert.NotContains(t, c.acked.clusters, bindingClusterName, "nothing is kept for good for a cluster neither published nor held")
}

// TestAckedPinGaugeDoesNotCountAClusterNeverAcknowledged: a cluster whose
// first response the proxy rejected has no acknowledged version. It is in no
// series, whatever the proxy acknowledges afterwards, until a response that
// carries it is acknowledged.
func TestAckedPinGaugeDoesNotCountAClusterNeverAcknowledged(t *testing.T) {
	c, _, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	proxy := connectCDSProxy(t, c, tracker, 1, nil)
	proxy.ack(proxy.next())

	addOutboundCluster(c, bindingClusterName) // new, unpinned
	require.NoError(t, c.generateSnapshot(ctx))
	proxy.nack(proxy.next())
	addPinnedCluster(c, addedClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	proxy.ack(proxy.next())

	acked, _ := ackedGauge()
	assert.Equal(t, int64(2), acked.pinned)
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned, "the proxy never accepted the unpinned cluster: it does not hold it")
	published, _ := readPinGauge(t, reader, tlsClustersGauge)
	assert.Equal(t, byCause(0, 1, 0), published.unpinned, "fixture: it is published")
}

// TestAckedPinGaugeWhenTheAgentRestartsWhileTheProxyRejects is the state
// #1509 is about, seen from this side: a new agent process finds a proxy that
// holds a cluster at a version this process never published (the proxy has
// been rejecting the update since before the restart).
//
// The agent cannot know the pin state of that version. It must not report a
// count that leaves the cluster out, and before #1508 it reported worse: the
// next unrelated ACK moved the gauge to the whole newest snapshot.
//
// Not reporting is silence, and a rule cannot match silence (#1509: the alert
// on the acknowledged gauge resolved here, with the proxy unchanged). So the
// state has a sample of its own: the number of held clusters the agent cannot
// place, which is zero whenever the acknowledged gauge is written.
func TestAckedPinGaugeWhenTheAgentRestartsWhileTheProxyRejects(t *testing.T) {
	c, rec, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addPinnedCluster(c, bindingClusterName)
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	_, ok := readUnknownGauge(t, reader)
	require.False(t, ok, "no proxy has answered: nothing is known, and nothing is claimed unknown either")

	// The proxy holds `other` as this process publishes it, and the binding
	// cluster at a version from the process before this one.
	stated := clusterVersions(t, c)
	stated[bindingClusterName] = "a-version-the-previous-agent-process-published"
	proxy := connectCDSProxy(t, c, tracker, 1, stated)
	update := proxy.next()
	require.Equal(t, []string{bindingClusterName}, deltaNames(update), "fixture: the proxy is owed this process's version")
	proxy.nack(update)

	_, ok = ackedGauge()
	assert.False(t, ok, "the pin state of one held cluster is not known: nothing is reported")
	require.Len(t, rec.with(ackedClusterPinsUnknownMsg), 1, "and the agent says why, once")
	assert.Equal(t, "1", rec.with(ackedClusterPinsUnknownMsg)[0].attrs["clusters"])
	unknown, ok := readUnknownGauge(t, reader)
	require.True(t, ok, "the withdrawal is a sample")
	assert.Equal(t, int64(1), unknown, "of the one cluster the agent cannot place")

	// An unrelated cluster is added and acknowledged.
	addPinnedCluster(c, addedClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	unrelated := proxy.next()
	require.Equal(t, []string{addedClusterName}, deltaNames(unrelated))
	proxy.ack(unrelated)
	_, ok = ackedGauge()
	assert.False(t, ok, "the ACK of another cluster does not make the rejected one known (#1508)")
	assert.Len(t, rec.with(ackedClusterPinsUnknownMsg), 1, "not said again while it lasts")
	unknown, ok = readUnknownGauge(t, reader)
	require.True(t, ok)
	assert.Equal(t, int64(1), unknown, "and it is still one")

	// The proxy reconnects and accepts the cluster.
	again := connectCDSProxy(t, c, tracker, 2, proxy.accepted)
	again.ack(again.next())
	acked, ok := ackedGauge()
	require.True(t, ok, "every cluster the proxy holds is at a version this process published")
	assert.Equal(t, int64(3), acked.pinned)
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
	assert.Len(t, rec.with(ackedClusterPinsKnownMsg), 1)
	unknown, ok = readUnknownGauge(t, reader)
	require.True(t, ok, "known again is a sample too, so the series falls to zero instead of ending")
	assert.Zero(t, unknown)
}

// TestAckedPinGaugeIsWithdrawnWhenAHeldClusterBecomesUnknown: "not written
// while unknown" has to hold for a gauge that was ALREADY written too. A gauge
// that is merely not recorded again goes on exporting its last values, which
// then read as a current, valid acknowledged state. When the proxy turns out
// to hold a cluster at a version this agent has no class for, the gauge has no
// sample at all until the state is known again.
func TestAckedPinGaugeIsWithdrawnWhenAHeldClusterBecomesUnknown(t *testing.T) {
	c, rec, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addOutboundCluster(c, bindingClusterName) // unpinned
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	proxy := connectCDSProxy(t, c, tracker, 1, nil)
	proxy.ack(proxy.next())
	acked, ok := ackedGauge()
	require.True(t, ok)
	require.Equal(t, byCause(0, 1, 0), acked.unpinned)

	// A stream opens on which the proxy states a version of the cluster this
	// agent has no class for, and rejects what it is sent instead.
	stated := clusterVersions(t, c)
	stated[bindingClusterName] = "a-version-this-agent-has-no-class-for"
	again := connectCDSProxy(t, c, tracker, 2, stated)
	again.nack(again.next())

	_, ok = ackedGauge()
	assert.False(t, ok, "the last known values must not go on being exported as the acknowledged state")
	require.Len(t, rec.with(ackedClusterPinsUnknownMsg), 1)

	// It comes back with the proxy's acceptance of the cluster.
	third := connectCDSProxy(t, c, tracker, 3, again.accepted)
	third.ack(third.next())
	acked, ok = ackedGauge()
	require.True(t, ok)
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned)
	assert.Equal(t, int64(1), acked.pinned)
}

// TestAckedPinGaugeWithTwoProxyGenerations: the hot-restart residue, written
// down. There is one record per cluster, not one per generation. A new
// generation's opening exchange replaces the set with its own; after that each
// cluster shows the last answer from either stream, so a late ACK of an older
// version from the generation that is leaving moves THAT cluster back, and no
// other, until that generation is sent the newer version and answers.
func TestAckedPinGaugeWithTwoProxyGenerations(t *testing.T) {
	c, _, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addOutboundCluster(c, bindingClusterName) // unpinned
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	parent := connectCDSProxy(t, c, tracker, 1, nil)
	parent.ack(parent.next())

	// The agent pins the cluster; the parent is sent the update and has not
	// answered when the agent unpins it again and the child starts.
	addPinnedCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	pending := parent.next()
	require.Equal(t, []string{bindingClusterName}, deltaNames(pending))
	addOutboundCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	// The child holds nothing, is sent everything, and acknowledges it.
	child := connectCDSProxy(t, c, tracker, 2, nil)
	child.ack(child.next())
	acked, _ := ackedGauge()
	assert.Equal(t, int64(1), acked.pinned, "the child's opening exchange is the whole set")
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned)

	// The parent's late ACK of the pinned version: that one cluster shows the
	// parent's answer now, although the child, which is the proxy from here
	// on, holds it unpinned. The residue.
	parent.ack(pending)
	acked, _ = ackedGauge()
	assert.Equal(t, int64(2), acked.pinned)
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)

	// It lasts until the parent is sent the newer version and answers.
	parent.ack(parent.next())
	acked, _ = ackedGauge()
	assert.Equal(t, int64(1), acked.pinned)
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned)
}

// TestAckedPinGaugeFollowsAReclassificationOfWhatTheProxyHolds: the same bytes
// can be counted differently by a later snapshot. An HTTP cluster published
// bare is tls_not_published while the node cannot publish TLS, and the build
// that sees the node identity counts every such entry as the validation gap
// one snapshot before their TLS goes out (promoteTLSNotPublished). No cluster
// changed, so there is no ACK: the build itself moves the acknowledged gauge,
// or it would disagree with the published one about the same cluster.
func TestAckedPinGaugeFollowsAReclassificationOfWhatTheProxyHolds(t *testing.T) {
	var h ackedPins
	notPublished := unpinnedClass(cachemetrics.CauseTLSNotPublished)
	entries := []entryClass{{name: "a", class: notPublished}, {name: "b", class: pinClassPinned}}
	versions := map[string]string{"a": "ha", "b": "hb"}

	u := h.publish(entries, versions, false)
	assert.False(t, u.report, "a build before any answer reports nothing")
	u = h.accept(ack.Accepted{Opening: true, Stated: map[string]string{}, Added: []ack.Resource{{Name: "a", Version: "ha"}, {Name: "b", Version: "hb"}}})
	require.True(t, u.report)
	assert.Equal(t, cachemetrics.PinCounts{Pinned: 1, Unpinned: [cachemetrics.NumUnpinnedCauses]int{0, 1, 0, 0}}, u.counts)

	u = h.publish(entries, versions, false)
	assert.False(t, u.report, "a build that changes nothing the proxy holds reports nothing")

	u = h.publish(entries, versions, true)
	require.True(t, u.report, "the same bytes, counted as the gap from this snapshot on")
	assert.True(t, u.changed)
	assert.Equal(t, cachemetrics.PinCounts{Pinned: 1, Unpinned: [cachemetrics.NumUnpinnedCauses]int{0, 0, 1, 0}}, u.counts)
}

// TestAckedPinsReadALateAckWithTheClassOfNow: a build can count the very bytes
// that are in flight under another reason before the proxy answers for them
// (the tls_not_published promotion). The ACK that then arrives is of bytes the
// newest snapshot still publishes, so it is counted as that snapshot counts
// them, not as they were counted when they were sent: the two gauges must
// agree about one cluster. The class kept with a sent version is for a version
// no snapshot remembers any more.
func TestAckedPinsReadALateAckWithTheClassOfNow(t *testing.T) {
	var h ackedPins
	notPublished := unpinnedClass(cachemetrics.CauseTLSNotPublished)
	entries := []entryClass{{name: "a", class: notPublished}}
	versions := map[string]string{"a": "ha"}
	sent := []ack.Resource{{Name: "a", Version: "ha"}}

	h.publish(entries, versions, false)
	h.deliver(ack.Delivery{Resources: sent})
	h.publish(entries, versions, true) // the same bytes, the gap from now on
	u := h.accept(ack.Accepted{Added: sent})
	h.deliver(ack.Delivery{Resources: sent, Ended: true})
	require.True(t, u.report)
	assert.Equal(t, cachemetrics.PinCounts{Unpinned: [cachemetrics.NumUnpinnedCauses]int{0, 0, 1, 0}}, u.counts)
}

// TestAckedPinsDoNotCountAnEntryWhoseClusterWasNeverPublished: an entry whose
// cluster no snapshot has published (a TCP floor that was never captured) is
// published as an entry and no proxy has been sent a cluster for it. It is in
// the published gauge and not in this one. (An entry whose cluster WAS
// published and has left the snapshot is another matter: it stays counted
// while the proxy holds the cluster,
// TestAckedPinGaugeKeepsAFloorWhoseRemovalWasRejected.)
func TestAckedPinsDoNotCountAnEntryWhoseClusterWasNeverPublished(t *testing.T) {
	var h ackedPins
	entries := []entryClass{
		{name: "tcp:floor", class: pinClassPinned}, // no cluster in this snapshot or any before it
		{name: "http", class: pinClassPinned},
		{name: "udp:floor", class: pinClassNone},
	}
	versions := map[string]string{"http": "h1", "udp:floor": "h2", "app-cluster": "h3"}
	h.publish(entries, versions, false)
	u := h.accept(ack.Accepted{Opening: true, Stated: map[string]string{}, Added: []ack.Resource{
		{Name: "http", Version: "h1"}, {Name: "udp:floor", Version: "h2"}, {Name: "app-cluster", Version: "h3"},
	}})
	require.True(t, u.report)
	assert.Equal(t, cachemetrics.PinCounts{Pinned: 1}, u.counts)
	assert.NotContains(t, h.clusters, "app-cluster", "a cluster that is not a cluster entry has no record")
}

// TestAckedPinsAreBoundedByTheClusters: memory. One fixed-size record per
// cluster entry of the newest snapshot, plus the entries a proxy still holds,
// plus the entries that left in the last goneBuilds-1 builds; nothing per
// snapshot, per ACK or per stream. And the versions remembered per
// cluster are a fixed few: an acknowledgement of one that has fallen out is
// not attributed to another.
//
// The builds here are goneAge apart, so no record is kept by its age: what
// that adds, and its cap, is TestAckedPinsKeptByAgeAreCapped.
func TestAckedPinsAreBoundedByTheClusters(t *testing.T) {
	clock := newStepClock()
	h := ackedPins{now: clock.now}
	const live = 10
	for build := range 500 {
		clock.advance(goneAge)
		entries := make([]entryClass, 0, live)
		versions := map[string]string{}
		var added []ack.Resource
		for i := range live {
			// Every build replaces one cluster name and re-versions the rest.
			name := fmt.Sprintf("c%d", build+i)
			version := fmt.Sprintf("%s@%d", name, build)
			entries = append(entries, entryClass{name: name, class: pinClassPinned})
			versions[name] = version
			added = append(added, ack.Resource{Name: name, Version: version})
		}
		h.publish(entries, versions, false)
		if build%3 == 0 {
			// The proxy takes this build, on a new stream every so often.
			accepted := ack.Accepted{Added: added}
			if build%30 == 0 {
				accepted.Opening, accepted.Stated = true, map[string]string{}
			}
			h.accept(accepted)
		}
		// The entries of this build, the one each of the last goneBuilds-1
		// builds dropped, and at most the ones a proxy was told to drop and
		// has not: removals are never acknowledged here, so only an opening
		// exchange clears them.
		require.LessOrEqual(t, len(h.clusters), live+(goneBuilds-1)+30, "build %d", build)
	}
	u := h.accept(ack.Accepted{Opening: true, Stated: map[string]string{}})
	require.True(t, u.report)
	assert.Equal(t, cachemetrics.PinCounts{}, u.counts)
	assert.Len(t, h.clusters, live+(goneBuilds-1), "a proxy that holds nothing leaves the newest snapshot's entries and the ones that left in the builds just before")
	for range goneBuilds - 1 {
		clock.advance(goneAge)
		h.publish(nil, nil, false)
	}
	require.Len(t, h.clusters, live, "fixture: the newest entries are kept as long as the ones before them were")
	clock.advance(goneAge)
	h.publish(nil, nil, false)
	assert.Empty(t, h.clusters, "and nothing is kept for good")

	// A version older than the remembered ones.
	var s ackedPins
	for v := range offeredVersions + 1 {
		s.publish([]entryClass{{name: "a", class: unpinnedClass(cachemetrics.CauseNoNamespaceMetadata)}}, map[string]string{"a": fmt.Sprintf("v%d", v)}, false)
	}
	u = s.accept(ack.Accepted{Added: []ack.Resource{{Name: "a", Version: "v0"}}})
	assert.False(t, u.report, "the class of a version that fell out is not guessed at")
	assert.Equal(t, 1, u.unclassified)
	u = s.accept(ack.Accepted{Added: []ack.Resource{{Name: "a", Version: "v1"}}})
	require.True(t, u.report, "the oldest version still remembered")
	assert.Equal(t, 1, u.counts.UnpinnedTotal())
}

// addClusterVariant stores an outbound cluster entry whose bytes differ with
// variant, pinned or not: a rebuild of the same cluster.
func addClusterVariant(c *SnapshotCache, name string, variant int, pinned bool) {
	entry := clusterEntry{
		cluster: &clusterv3.Cluster{Name: name, ConnectTimeout: durationpb.New(time.Duration(variant+1) * time.Second)},
		service: "aether-test/echo",
	}
	if pinned {
		entry.sanNamespaces = []string{"aether-test"}
	}
	c.clusterMu.Lock()
	c.clusters[name] = entry
	c.clusterMu.Unlock()
	c.recomputeMTLSClusters()
}

// TestAckedPinGaugeKnowsAVersionHoweverLongItsAckTakes: a response waits for
// its ACK for as long as the proxy takes, and the agent goes on building.
// However many times a cluster is rebuilt in that time, the version in flight
// is one this agent sent, and when the proxy acknowledges it the agent must
// know what it sent: the gauge moves to it. It must not be left on what it
// showed before because the version was forgotten.
func TestAckedPinGaugeKnowsAVersionHoweverLongItsAckTakes(t *testing.T) {
	c, rec, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addClusterVariant(c, bindingClusterName, 0, false)
	require.NoError(t, c.generateSnapshot(ctx))
	proxy := connectCDSProxy(t, c, tracker, 1, nil)
	proxy.ack(proxy.next())
	acked, _ := ackedGauge()
	require.Equal(t, byCause(0, 1, 0), acked.unpinned)

	// The pinned version is sent, and the proxy is slow to answer.
	addClusterVariant(c, bindingClusterName, 1, true)
	require.NoError(t, c.generateSnapshot(ctx))
	pending := proxy.next()
	require.Equal(t, []string{bindingClusterName}, deltaNames(pending))

	// Meanwhile the cluster is rebuilt many times over, unpinned again.
	const rebuilds = 3 * offeredVersions
	for i := range rebuilds {
		addClusterVariant(c, bindingClusterName, 10+i, false)
		require.NoError(t, c.generateSnapshot(ctx))
	}

	proxy.ack(pending)
	acked, _ = ackedGauge()
	assert.Equal(t, int64(1), acked.pinned, "the proxy acknowledged the pinned version it was sent")
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
	assert.Empty(t, rec.with(ackedClusterPinsUnknownMsg), "a version that was on record when its response was written stays known until the answer")

	// And what is kept for it is released with the answer: after the proxy
	// takes the newest version, the record holds nothing in flight.
	proxy.ack(proxy.next())
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned)
	assert.Empty(t, c.acked.clusters[bindingClusterName].sent)
}

// TestAckedPinGaugeCountsAClusterRemovedWhileItsResponseWasInFlight: the agent
// drops a cluster it has sent and the proxy has not answered for. The ACK that
// then arrives is of a cluster no snapshot has any more, and the proxy does
// hold it, unpinned, until it takes the removal.
func TestAckedPinGaugeCountsAClusterRemovedWhileItsResponseWasInFlight(t *testing.T) {
	c, _, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	proxy := connectCDSProxy(t, c, tracker, 1, nil)
	proxy.ack(proxy.next())

	addOutboundCluster(c, bindingClusterName) // unpinned
	require.NoError(t, c.generateSnapshot(ctx))
	pending := proxy.next()
	require.Equal(t, []string{bindingClusterName}, deltaNames(pending))
	require.NoError(t, c.RemoveCluster(ctx, bindingClusterName))
	addPinnedCluster(c, addedClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	proxy.ack(pending)
	acked, _ := ackedGauge()
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned, "the proxy accepted the cluster it was sent, published or not")

	removal := proxy.next()
	require.Equal(t, []string{bindingClusterName}, removal.GetRemovedResources())
	proxy.ack(removal)
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
	buildPastTheGoneWindow(t, c)
	assert.NotContains(t, c.acked.clusters, bindingClusterName)
}

// TestAckedPinGaugeCountsAClusterRemovedBeforeItsResponseWasWritten: the
// window before the one above. go-control-plane builds a response from the
// snapshot of the moment and hands it to the stream, and the tracker (and so
// the acknowledged pin state) learns of it only when the stream writes it. A
// build that drops the cluster in between finds its record neither published,
// nor in flight, nor held. The response is written and acknowledged all the
// same: the proxy holds the cluster, unpinned, until it takes the removal, and
// the gauge must count it.
func TestAckedPinGaugeCountsAClusterRemovedBeforeItsResponseWasWritten(t *testing.T) {
	c, rec, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	proxy := connectCDSProxy(t, c, tracker, 1, nil)
	proxy.ack(proxy.next())

	addOutboundCluster(c, bindingClusterName) // unpinned
	require.NoError(t, c.generateSnapshot(ctx))
	pending := proxy.build()
	require.Equal(t, []string{bindingClusterName}, deltaNames(pending))
	// Built, not written yet: the agent drops the cluster.
	require.NoError(t, c.RemoveCluster(ctx, bindingClusterName))
	require.NoError(t, c.generateSnapshot(ctx))

	proxy.written(pending)
	proxy.ack(pending)
	acked, ok := ackedGauge()
	require.True(t, ok)
	assert.Equal(t, int64(1), acked.pinned)
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned, "the proxy accepted the cluster it was sent: it holds it, whatever was built since")
	assert.Empty(t, rec.with(ackedClusterPinsUnknownMsg), "and its class is the one it was published with")

	removal := proxy.next()
	require.Equal(t, []string{bindingClusterName}, removal.GetRemovedResources())
	proxy.ack(removal)
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
}

// TestAckedPinsKeepAGoneClusterForAFewBuilds: what the test above relies on,
// and its bound. The record of an entry that left the snapshot is kept, with
// the classes of its versions, through the builds a response built before it
// left can still be waiting to be written in (goneBuilds), and dropped by the
// build after: memory stays bounded by the clusters of the last few snapshots.
//
// The builds here are goneAge apart, so this is the build bound alone;
// TestAckedPinsKeepAGoneClusterForAWhile has the two together.
func TestAckedPinsKeepAGoneClusterForAFewBuilds(t *testing.T) {
	clock := newStepClock()
	h := ackedPins{now: clock.now}
	gap := unpinnedClass(cachemetrics.CauseNoNamespaceMetadata)
	both := []entryClass{{name: "a", class: pinClassPinned}, {name: "gone", class: gap}}
	rest := both[:1]
	versions := map[string]string{"a": "ha", "gone": "hg"}
	sent := []ack.Resource{{Name: "gone", Version: "hg"}}

	h.publish(both, versions, false)
	for build := 1; build < goneBuilds; build++ {
		clock.advance(goneAge)
		h.publish(rest, versions, false)
		require.Contains(t, h.clusters, "gone", "%d build(s) after it left", build)
	}
	// The response built while it was published is written and acknowledged.
	h.deliver(ack.Delivery{Resources: sent})
	u := h.accept(ack.Accepted{Added: sent})
	h.deliver(ack.Delivery{Resources: sent, Ended: true})
	require.True(t, u.report)
	assert.Equal(t, cachemetrics.PinCounts{Unpinned: [cachemetrics.NumUnpinnedCauses]int{0, 0, 1, 0}}, u.counts)

	// Held: kept for as long as the proxy holds it, however many builds.
	for range 2 * goneBuilds {
		clock.advance(goneAge)
		h.publish(rest, versions, false)
	}
	require.Contains(t, h.clusters, "gone")
	// Released by the proxy, and already gone for longer than the window: the
	// record goes with the answer.
	h.accept(ack.Accepted{Removed: []string{"gone"}})
	assert.NotContains(t, h.clusters, "gone")

	// Never sent: dropped by the build that ends the window.
	h.publish(both, versions, false)
	for range goneBuilds - 1 {
		clock.advance(goneAge)
		h.publish(rest, versions, false)
	}
	require.Contains(t, h.clusters, "gone")
	clock.advance(goneAge)
	h.publish(rest, versions, false)
	assert.NotContains(t, h.clusters, "gone", "nothing is kept for good for a cluster neither published, nor in flight, nor held")
}

// TestAckedPinGaugeCountsAClusterWhoseResponseWaitedManyBuildsToBeWritten is
// #1551. The step between go-control-plane building a response and the stream
// writing it is measured in time, not in builds: the response waits in the
// stream's channel while the stream's goroutine is busy, and the agent builds
// as often as the node changes. A response that carries a cluster the agent
// dropped right after, and is written many builds but only seconds later, is
// acknowledged like any other: the proxy holds the cluster and the gauge
// counts it.
func TestAckedPinGaugeCountsAClusterWhoseResponseWaitedManyBuildsToBeWritten(t *testing.T) {
	c, rec, reader, tracker := ackedPinFixture(t)
	clock := newStepClock()
	c.acked.now = clock.now
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	proxy := connectCDSProxy(t, c, tracker, 1, nil)
	proxy.ack(proxy.next())

	addOutboundCluster(c, bindingClusterName) // unpinned
	require.NoError(t, c.generateSnapshot(ctx))
	pending := proxy.build()
	require.Equal(t, []string{bindingClusterName}, deltaNames(pending))
	// Built, not written yet: the agent drops the cluster, and builds many
	// times over in less than goneAge.
	require.NoError(t, c.RemoveCluster(ctx, bindingClusterName))
	const builds = 4 * goneBuilds
	for range builds {
		clock.advance(goneAge / (2 * builds))
		require.NoError(t, c.generateSnapshot(ctx))
	}

	proxy.written(pending)
	proxy.ack(pending)
	acked, ok := ackedGauge()
	require.True(t, ok)
	assert.Equal(t, int64(1), acked.pinned)
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned, "the proxy accepted the cluster it was sent, however many builds went by before it was written")
	assert.Empty(t, rec.with(ackedClusterPinsUnknownMsg), "and its class is the one it was published with")

	removal := proxy.next()
	require.Equal(t, []string{bindingClusterName}, removal.GetRemovedResources())
	proxy.ack(removal)
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)

	// The bound. Released, the record outlives the entry by goneAge from the
	// first build without it, and is dropped by the first build after that.
	require.Contains(t, c.acked.clusters, bindingClusterName)
	clock.advance(goneAge / 2)
	require.NoError(t, c.generateSnapshot(ctx))
	assert.NotContains(t, c.acked.clusters, bindingClusterName, "nothing is kept for good")
}

// TestAckedPinsKeepAGoneClusterForAWhile: the two bounds on the record of an
// entry that left the snapshot, is not held and is not in flight. It is kept
// while EITHER fewer than goneBuilds builds without the entry have run OR less
// than goneAge has passed since the first of them, and dropped by the first
// build after both.
func TestAckedPinsKeepAGoneClusterForAWhile(t *testing.T) {
	gap := unpinnedClass(cachemetrics.CauseNoNamespaceMetadata)
	both := []entryClass{{name: "a", class: pinClassPinned}, {name: "gone", class: gap}}
	rest := both[:1]
	versions := map[string]string{"a": "ha", "gone": "hg"}
	sent := []ack.Resource{{Name: "gone", Version: "hg"}}

	t.Run("many builds in less than goneAge", func(t *testing.T) {
		clock := newStepClock()
		h := ackedPins{now: clock.now}
		h.publish(both, versions, false)
		h.publish(rest, versions, false) // the first build without it
		for range 10 * goneBuilds {
			h.publish(rest, versions, false)
		}
		clock.advance(goneAge - time.Nanosecond)
		h.publish(rest, versions, false)
		require.Contains(t, h.clusters, "gone", "younger than goneAge")
		// A response built while it was published is written and acknowledged.
		h.deliver(ack.Delivery{Resources: sent})
		u := h.accept(ack.Accepted{Added: sent})
		h.deliver(ack.Delivery{Resources: sent, Ended: true})
		require.True(t, u.report)
		assert.Equal(t, cachemetrics.PinCounts{Unpinned: [cachemetrics.NumUnpinnedCauses]int{0, 0, 1, 0}}, u.counts)
		h.accept(ack.Accepted{Removed: []string{"gone"}})
		require.Contains(t, h.clusters, "gone", "an answer does not drop it early either")

		clock.advance(time.Nanosecond)
		h.publish(rest, versions, false)
		assert.NotContains(t, h.clusters, "gone", "goneAge after the first build without it, and more than goneBuilds builds")
	})

	t.Run("goneAge passes in fewer than goneBuilds builds", func(t *testing.T) {
		clock := newStepClock()
		h := ackedPins{now: clock.now}
		h.publish(both, versions, false)
		h.publish(rest, versions, false)
		clock.advance(10 * goneAge)
		for build := 2; build < goneBuilds; build++ {
			h.publish(rest, versions, false)
			require.Contains(t, h.clusters, "gone", "%d build(s) without it", build)
		}
		h.publish(rest, versions, false)
		assert.NotContains(t, h.clusters, "gone")
	})

	t.Run("the age is of the last time it left", func(t *testing.T) {
		clock := newStepClock()
		h := ackedPins{now: clock.now}
		h.publish(both, versions, false)
		h.publish(rest, versions, false) // left
		h.publish(both, versions, false) // back
		clock.advance(10 * goneAge)
		h.publish(rest, versions, false) // left again, now
		for range 10 * goneBuilds {
			h.publish(rest, versions, false)
		}
		assert.Contains(t, h.clusters, "gone", "it left a moment ago, whenever it had left before")
	})

	t.Run("an answer after both bounds drops it without a build", func(t *testing.T) {
		clock := newStepClock()
		h := ackedPins{now: clock.now}
		h.publish(both, versions, false)
		h.deliver(ack.Delivery{Resources: sent})
		h.accept(ack.Accepted{Added: sent})
		h.deliver(ack.Delivery{Resources: sent, Ended: true})
		for range goneBuilds {
			h.publish(rest, versions, false)
		}
		h.accept(ack.Accepted{Removed: []string{"gone"}})
		require.Contains(t, h.clusters, "gone", "released inside goneAge: kept")
		h.accept(ack.Accepted{Added: sent})
		clock.advance(goneAge)
		h.accept(ack.Accepted{Removed: []string{"gone"}})
		assert.NotContains(t, h.clusters, "gone", "released after it: gone with the answer")
	})

	// The other two things that can leave a record with nothing to keep it:
	// neither drops it inside goneAge, and each drops it after.
	for _, tc := range []struct {
		name string
		// keep makes something hold the record; end takes that away.
		keep, end func(h *ackedPins)
	}{
		{
			name: "a response that is never acknowledged",
			keep: func(h *ackedPins) { h.deliver(ack.Delivery{Resources: sent}) },
			end:  func(h *ackedPins) { h.deliver(ack.Delivery{Resources: sent, Ended: true}) },
		},
		{
			name: "a stream opened without the cluster",
			keep: func(h *ackedPins) { h.accept(ack.Accepted{Added: sent}) },
			end: func(h *ackedPins) {
				h.accept(ack.Accepted{Opening: true, Rejected: true, Stated: map[string]string{"a": "ha"}})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newStepClock()
			h := ackedPins{now: clock.now}
			h.publish(both, versions, false)
			tc.keep(&h)
			for range goneBuilds {
				h.publish(rest, versions, false)
			}
			tc.end(&h)
			require.Contains(t, h.clusters, "gone", "inside goneAge: kept")
			tc.keep(&h)
			clock.advance(goneAge)
			tc.end(&h)
			assert.NotContains(t, h.clusters, "gone", "after it: gone at once")
		})
	}
}

// TestAckedPinsKeptByAgeAreCapped: memory. The age bound alone would keep one
// record per entry that left in the last goneAge, which is as many as the node
// can churn in that time. So it applies only while the records that are not of
// an entry of the newest snapshot number at most maxAgedRecords; a build that
// finds more keeps none of them by age, and they fall back to the build bound.
func TestAckedPinsKeptByAgeAreCapped(t *testing.T) {
	entries := func(n int) ([]entryClass, map[string]string) {
		out := make([]entryClass, 0, n+1)
		versions := make(map[string]string, n+1)
		for i := range n + 1 {
			name := "c" + strconv.Itoa(i)
			out = append(out, entryClass{name: name, class: pinClassPinned})
			versions[name] = "v"
		}
		return out, versions
	}
	for _, tc := range []struct {
		name string
		gone int
		kept int
	}{
		{name: "at the cap", gone: maxAgedRecords, kept: maxAgedRecords},
		{name: "over it", gone: maxAgedRecords + 1, kept: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newStepClock()
			h := ackedPins{now: clock.now}
			all, versions := entries(tc.gone)
			// What stays is one record that two entries publish under: the
			// cap counts records, not entries.
			stays := []entryClass{all[0], all[0]}
			h.publish(all, versions, false)
			for build := 1; build < goneBuilds; build++ {
				h.publish(stays, versions, false)
				require.Len(t, h.clusters, 1+tc.gone, "%d build(s) without them: the build bound keeps them all", build)
			}
			h.publish(stays, versions, false)
			assert.Len(t, h.clusters, 1+tc.kept)
		})
	}
}

// TestAckedPinsKeptByAgeAreCappedWithoutABuild: the cap holds between builds
// too. Records that outlived their entries because a proxy held their clusters,
// or had not answered for them, are released by answers, inside goneAge and
// with no build after. Each release keeps the record by age only while the
// records that are not of an entry number at most maxAgedRecords, so that many
// are left and no more, whichever path released them.
func TestAckedPinsKeptByAgeAreCappedWithoutABuild(t *testing.T) {
	const over = 2
	all := make([]entryClass, 0, maxAgedRecords+over+1)
	versions := make(map[string]string, cap(all))
	var gone []ack.Resource
	var goneNames []string
	for i := range cap(all) {
		name := "c" + strconv.Itoa(i)
		all = append(all, entryClass{name: name, class: pinClassPinned})
		versions[name] = "v"
		if i > 0 {
			gone = append(gone, ack.Resource{Name: name, Version: "v"})
			goneNames = append(goneNames, name)
		}
	}
	for _, tc := range []struct {
		name string
		// keep makes something hold every record; end takes that away.
		keep, end func(h *ackedPins)
	}{
		{
			name: "removals the proxy accepts",
			keep: func(h *ackedPins) { h.accept(ack.Accepted{Added: gone}) },
			end:  func(h *ackedPins) { h.accept(ack.Accepted{Removed: goneNames}) },
		},
		{
			name: "a stream opened without the clusters",
			keep: func(h *ackedPins) { h.accept(ack.Accepted{Added: gone}) },
			end: func(h *ackedPins) {
				h.accept(ack.Accepted{Opening: true, Rejected: true, Stated: map[string]string{"c0": "v"}})
			},
		},
		{
			name: "responses that are never acknowledged",
			keep: func(h *ackedPins) { h.deliver(ack.Delivery{Resources: gone}) },
			end:  func(h *ackedPins) { h.deliver(ack.Delivery{Resources: gone, Ended: true}) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newStepClock()
			h := ackedPins{now: clock.now}
			h.publish(all, versions, false)
			tc.keep(&h)
			for range goneBuilds {
				h.publish(all[:1], versions, false)
			}
			require.Len(t, h.clusters, len(all), "fixture: held or in flight, every record is kept")
			tc.end(&h)
			assert.Len(t, h.clusters, 1+maxAgedRecords)
		})
	}
}

// TestAckedPinGaugeIsNotWithdrawnForAPlaintextUDPFloor: a UDP floor cluster
// has no transport socket, so no version of it, known to this agent process or
// not, can be in a pin series. A proxy that states one at a version from the
// agent process before this one, and rejects the update, must not cost the
// node its acknowledged pin gauge: there is nothing about a pin that the agent
// does not know.
func TestAckedPinGaugeIsNotWithdrawnForAPlaintextUDPFloor(t *testing.T) {
	c, rec, reader, tracker := ackedPinFixture(t)
	c.SetCaptureEnabled(true)
	ctx := context.Background()
	const udpService = "aether-test/udponly"
	udpName := proxy.UDPClusterName(udpService, c.meshDomain)
	c.SetUDPServiceRoutes(map[string][]proxy.L4Backend{udpService: {{Service: udpService, Cluster: udpName, Weight: 1}}})
	declareDeps(c, udpService)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", udpOnlyRegistry(udpService, "10.0.0.40", 9001)))
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	stated := clusterVersions(t, c)
	require.Contains(t, stated, udpName, "fixture: the snapshot publishes the UDP floor cluster")
	stated[udpName] = "a-version-the-previous-agent-process-published"
	p := connectCDSProxy(t, c, tracker, 1, stated)
	update := p.next()
	require.Equal(t, []string{udpName}, deltaNames(update), "fixture: the proxy is owed this process's version of it and nothing else")
	p.nack(update)

	acked, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok, "the pin state of every TLS cluster the proxy holds is known")
	assert.Equal(t, int64(1), acked.pinned)
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
	assert.Empty(t, rec.with(ackedClusterPinsUnknownMsg))
	assert.NotContains(t, c.acked.clusters, udpName, "a plaintext entry has no record")
}

// TestAckedPinsKnowASnapshotWhoseSetReturnedAnError: why the classes are
// recorded before SetSnapshot without waiting to see whether it succeeds. The
// pinned go-control-plane installs the snapshot first and can fail only
// afterwards, while it answers the watches that were open (here: a watch on a
// channel nobody reads, which the build gives up on after its bound). The
// snapshot is then the one every later request is answered from, so a proxy can hold its clusters
// and acknowledge them: their classes have to be on record. There is no
// "rejected and never published" outcome to keep out of the record.
func TestAckedPinsKnowASnapshotWhoseSetReturnedAnError(t *testing.T) {
	c, _, reader, tracker := ackedPinFixture(t)
	ctx := context.Background()
	c.watchAnswerTimeout = 50 * time.Millisecond
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	// An open watch nobody reads: SetSnapshot blocks answering it until the
	// build's bound for that passes, and returns an error.
	cancel, err := c.CreateDeltaWatch(&discoveryv3.DeltaDiscoveryRequest{
		Node: &corev3.Node{Id: c.nodeName}, TypeUrl: resourcev3.ClusterType, ResponseNonce: "n1",
	}, streamv3.NewDeltaSubscription(nil, nil, clusterVersions(t, c), true), make(chan cachev3.DeltaResponse))
	require.NoError(t, err)
	require.NotNil(t, cancel, "fixture: the watch must be open")
	defer cancel()

	addOutboundCluster(c, bindingClusterName) // unpinned
	require.ErrorIs(t, c.generateSnapshot(ctx), ErrWatchNotAnswered, "fixture: SetSnapshot must fail")
	require.Contains(t, clusterVersions(t, c), bindingClusterName, "the snapshot is served although setting it returned an error")

	p := connectCDSProxy(t, c, tracker, 1, nil)
	p.ack(p.next())
	acked, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok)
	assert.Equal(t, int64(1), acked.pinned)
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned, "the proxy holds the cluster of that snapshot, and the agent knows its class")
}

// TestASnapshotWhoseSetReturnedAnErrorIsReportedAsTheOneServed is #1549, the
// build's own side of the test above. The snapshot is installed although
// SetSnapshot returned an error, so what the agent says about "the snapshot it
// serves" has to be about that one: the published pin gauge and its WARN, and
// the outbound and inbound identity-binding lines. They used to be skipped,
// and were made up for only by the next build, under the next version.
//
// The error is still returned, and it is told apart from a snapshot that was
// never installed: its own line, its own text, and ErrWatchNotAnswered.
func TestASnapshotWhoseSetReturnedAnErrorIsReportedAsTheOneServed(t *testing.T) {
	c, rec, reader, _ := ackedPinFixture(t)
	ctx := context.Background()
	c.watchAnswerTimeout = 50 * time.Millisecond
	serveSecrets(c, inboundEchoIdentity, inboundTrustBundleSDS)
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	published, ok := readPinGauge(t, reader, tlsClustersGauge)
	require.True(t, ok)
	require.Equal(t, byCause(0, 0, 0), published.unpinned, "fixture: nothing is unpinned yet")

	// An open watch nobody reads: SetSnapshot blocks answering it until the
	// build's bound for that passes, and returns an error.
	cancel, err := c.CreateDeltaWatch(&discoveryv3.DeltaDiscoveryRequest{
		Node: &corev3.Node{Id: c.nodeName}, TypeUrl: resourcev3.ClusterType, ResponseNonce: "n1",
	}, streamv3.NewDeltaSubscription(nil, nil, clusterVersions(t, c), true), make(chan cachev3.DeltaResponse))
	require.NoError(t, err)
	require.NotNil(t, cancel, "fixture: the watch must be open")
	defer cancel()

	// One build that changes all three: an unpinned cluster, and a pod, which
	// binds a client certificate to that cluster and a server certificate to
	// its own inbound chains.
	addOutboundCluster(c, bindingClusterName)
	rec.reset()
	err = c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain)
	require.ErrorIs(t, err, ErrWatchNotAnswered, "fixture: SetSnapshot must fail")
	served := snapshotVersion(t, c)
	require.Contains(t, clusterVersions(t, c), bindingClusterName, "the snapshot is served although setting it returned an error")

	published, ok = readPinGauge(t, reader, tlsClustersGauge)
	require.True(t, ok)
	assert.Equal(t, byCause(0, 1, 0), published.unpinned, "the published gauge is of the snapshot the cache serves")
	unpinned := rec.with(unpinnedClusterMsg)
	require.Len(t, unpinned, 1, "and so is the line that names the unpinned cluster")
	assert.Equal(t, served, unpinned[0].attrs["snapshot_version"])

	outbound := rec.with(bindingLineMsg)
	require.NotEmpty(t, outbound, "the outbound bindings of the served snapshot are named")
	for _, l := range outbound {
		assert.Equal(t, served, l.attrs["snapshot_version"])
	}
	inbound := rec.with(inboundLineMsg)
	require.NotEmpty(t, inbound, "and the inbound ones")
	for _, l := range inbound {
		assert.Equal(t, served, l.attrs["snapshot_version"])
	}

	// Installed, with a watch left unanswered: not "failed to set".
	assert.NotContains(t, err.Error(), "failed to set snapshot")
	assert.ErrorContains(t, err, "is installed")
	lines := rec.with(snapshotWatchUnansweredMsg)
	require.Len(t, lines, 1)
	assert.Equal(t, slog.LevelWarn, lines[0].level)
	assert.Equal(t, served, lines[0].attrs["snapshot_version"])

	// The next build has nothing to make up for. (The watch is closed first:
	// it is still open, and a build would wait on it again.)
	cancel()
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Empty(t, rec.with(bindingLineMsg))
	assert.Empty(t, rec.with(inboundLineMsg))
	assert.Empty(t, rec.with(snapshotWatchUnansweredMsg))
}

// TestAckedPinsForgetAVersionThatIsNoLongerInFlight: memory. A version is kept
// while a response that carried it waits for its answer, and no longer: an
// ACK, a NACK and the end of the stream each release it.
func TestAckedPinsForgetAVersionThatIsNoLongerInFlight(t *testing.T) {
	c, _, _, tracker := ackedPinFixture(t)
	ctx := context.Background()
	addClusterVariant(c, bindingClusterName, 0, true)
	require.NoError(t, c.generateSnapshot(ctx))
	inFlight := func() int {
		c.acked.mu.Lock()
		defer c.acked.mu.Unlock()
		return len(c.acked.clusters[bindingClusterName].sent)
	}

	proxy := connectCDSProxy(t, c, tracker, 1, nil)
	first := proxy.next()
	assert.Equal(t, 1, inFlight(), "sent and not answered")
	proxy.ack(first)
	assert.Zero(t, inFlight(), "acknowledged")

	addClusterVariant(c, bindingClusterName, 1, true)
	require.NoError(t, c.generateSnapshot(ctx))
	rejected := proxy.next()
	assert.Equal(t, 1, inFlight())
	proxy.nack(rejected)
	assert.Zero(t, inFlight(), "rejected")

	addClusterVariant(c, bindingClusterName, 2, true)
	require.NoError(t, c.generateSnapshot(ctx))
	require.NotNil(t, proxy.next())
	// A second generation is sent the same version while the first has not
	// answered: one version, in flight twice.
	other := connectCDSProxy(t, c, tracker, 2, nil)
	second := other.next()
	assert.Equal(t, 1, inFlight(), "one version, however many streams carry it")
	proxy.callbacks.OnDeltaStreamClosed(1, nil)
	assert.Equal(t, 1, inFlight(), "still in flight on the other stream")
	other.ack(second)
	assert.Zero(t, inFlight())

	// A stream that ends with a response unanswered.
	addClusterVariant(c, bindingClusterName, 3, true)
	require.NoError(t, c.generateSnapshot(ctx))
	require.NotNil(t, other.next())
	assert.Equal(t, 1, inFlight())
	other.callbacks.OnDeltaStreamClosed(2, nil)
	assert.Zero(t, inFlight(), "the stream ended: its answer will never come")
}

// TestAckedPinsUnderConcurrentBuildsAndAnswers: answers arrive on the xDS
// stream's goroutine while snapshots are built on others. Run under the race
// detector; and whatever the interleaving, a proxy that then acknowledges the
// whole newest snapshot is counted as holding what is published.
func TestAckedPinsUnderConcurrentBuildsAndAnswers(t *testing.T) {
	c, _, reader, _ := ackedPinFixture(t)
	ctx := context.Background()
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	const rounds = 100
	built := make(chan ack.Accepted, rounds)
	go func() {
		defer close(built)
		for i := range rounds {
			if i%2 == 0 {
				addOutboundCluster(c, bindingClusterName)
			} else {
				addPinnedCluster(c, bindingClusterName)
			}
			if !assert.NoError(t, c.generateSnapshot(ctx)) {
				return
			}
			built <- wholeSnapshotAccepted(t, c)
		}
	}()
	for accepted := range built {
		// Some as a new stream, some as a plain ACK of the same clusters.
		accepted.Opening = len(accepted.SystemVersion)%2 == 0
		c.ResponseAccepted(ctx, accepted)
	}

	c.ResponseAccepted(ctx, wholeSnapshotAccepted(t, c))
	acked, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok)
	published, _ := readPinGauge(t, reader, tlsClustersGauge)
	assert.Equal(t, published, acked)
	assert.Len(t, c.acked.clusters, 2)
}

// trackedPinReport is the pin report of the cluster map as a snapshot build
// takes it: with every entry's class, collected into buf.
func (c *SnapshotCache) trackedPinReport(buf []entryClass) pinReport {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()

	r := pinReport{track: true, classes: buf[:0]}
	for name, entry := range c.clusters {
		r.add(name, &entry)
	}
	r.sortNames()
	return r
}

// TestPinReportCostsNoAllocationPerCluster: the report runs on every snapshot
// build, over every cluster entry. On a node where every cluster is pinned it
// allocates nothing, whatever the number of clusters, and recording it in the
// acknowledged pin state allocates nothing either once every entry has its
// record: the classes go into a buffer the build reuses, the records are
// updated in place, and names are collected only for the unpinned.
func TestPinReportCostsNoAllocationPerCluster(t *testing.T) {
	c, _, _ := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	const n = 500
	versions := make(map[string]string, n)
	for i := range n {
		name := fmt.Sprintf("svc-%03d.aether-test.aether.internal", i)
		addPinnedCluster(c, name)
		versions[name] = "version-of-" + name
	}

	buf := make([]entryClass, 0, n)
	report := c.trackedPinReport(buf)
	c.acked.publish(report.classes, versions, false)
	allocs := testing.AllocsPerRun(20, func() {
		report = c.trackedPinReport(buf)
		c.acked.publish(report.classes, versions, false)
	})
	require.Equal(t, n, report.counts.Pinned, "fixture: every entry is a pinned TLS cluster")
	require.Len(t, c.acked.clusters, n, "fixture: every entry has its record")
	assert.Zero(t, allocs, "allocations per report over %d pinned clusters", n)
}

// TestPinReportIsOfTheClustersTheBuildRead: a snapshot's pin report is filed
// under the versions of that snapshot's clusters, so it has to describe the
// cluster entries the build collected its clusters from, not the cluster map
// as it is some time later. The build therefore takes both in one read. Here
// the map changes right after that read, as a registry reload landing
// mid-build would change it: the report still counts what the resources were
// built from.
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
	assert.Equal(t, []entryClass{{name: bindingClusterName, class: pinClassPinned}}, pins.classes, "class by class too")
	assert.Empty(t, pins.unpinned)
	later := c.clusterPinReport()
	assert.Equal(t, 1, later.counts.UnpinnedTotal(), "fixture: a later read of the map does see the new entry")
}

// TestSnapshotPinClassesAreRecordedBeforeItCanBeAcked: the pin classes of a
// snapshot's clusters are recorded before SetSnapshot, which is what makes the
// snapshot visible to a proxy. A watch that is already open is answered from
// inside SetSnapshot; by then the versions must be known, or a fast ACK finds
// nothing.
func TestSnapshotPinClassesAreRecordedBeforeItCanBeAcked(t *testing.T) {
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
		var raw cachev3.DeltaResponse
		other := watches[1]
		select {
		case raw = <-watches[0]:
		case raw = <-watches[1]:
			other = watches[0]
		}
		version := raw.GetResponseVersion()
		accepted := ack.Accepted{TypeURL: resourcev3.ClusterType, SystemVersion: version}
		if resp, err := raw.GetDeltaDiscoveryResponse(); err == nil {
			for _, r := range resp.GetResources() {
				accepted.Added = append(accepted.Added, ack.Resource{Name: r.GetName(), Version: r.GetVersion()})
			}
		}
		c.ResponseAccepted(ctx, accepted)
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
	require.True(t, ok, "the ACK found its cluster's version on record")
	assert.Equal(t, int64(1), got.pinned)
	assert.Equal(t, byCause(0, 0, 0), got.unpinned)
}

// TestAckTrackerFeedsTheAckedPinGauge wires the two as the agent does
// (ack.Tracker.SetAckObserver(cache.ResponseAccepted)) and drives the tracker
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
	tracker.SetAckObserver(c.ResponseAccepted)
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

// TestAckedPinGaugeAfterAnAgentRestartAgainstAnInSyncProxy is #1483: an agent
// that restarts finds a proxy holding exactly the clusters it publishes. The
// proxy says so in its first request, the cache answers with an empty response
// naming the snapshot, the proxy ACKs it, and that is the acknowledged gauge's
// first sample. Before, the gauge had none until a cluster next changed.
func TestAckedPinGaugeAfterAnAgentRestartAgainstAnInSyncProxy(t *testing.T) {
	c, _, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addOutboundCluster(c, bindingClusterName) // no namespace: unpinned
	require.NoError(t, c.generateSnapshot(ctx))

	// The proxy's first request on the new stream: it holds every cluster the
	// restarted agent publishes, at the version it publishes it.
	proxy := connectCDSProxy(t, c, tracker, 1, clusterVersions(t, c))
	opening := proxy.next()
	require.NotNil(t, opening, "the first wildcard request of a stream is answered even when nothing is owed")
	require.Empty(t, opening.GetResources())
	require.Empty(t, opening.GetRemovedResources())
	require.Equal(t, snapshotVersion(t, c), opening.GetSystemVersionInfo())

	_, ok := ackedGauge()
	require.False(t, ok, "sent is not acknowledged")
	proxy.ack(opening)

	acked, ok := ackedGauge()
	require.True(t, ok, "the proxy acknowledged the answer to what it stated it holds: the gauge has its sample")
	published, _ := readPinGauge(t, reader, tlsClustersGauge)
	assert.Equal(t, published, acked, "an in-sync proxy holds what is published")
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned)
}

// TestAckedPinGaugeMakesNoClaimForAProxyThatIsNotInSync is the other side: a
// reconnecting proxy whose stated clusters are not the snapshot's is sent the
// difference, and only its ACK of that moves a cluster to the new version. And
// an empty response later on the stream moves nothing, whatever it names.
//
// The proxy rejects the difference here. What it holds is then what it STATED:
// the unpinned cluster, at a version this agent published earlier, so the
// gauge says exactly that (before #1508 nothing was read from a rejected
// opening exchange and the gauge had no sample). The second half runs the
// pinned go-control-plane cache through the case that makes the empty-response
// rule necessary: after the rejection, a request without a nonce (an on-demand
// subscription) is answered with an empty response that names the rejected
// snapshot, and the proxy ACKs it.
func TestAckedPinGaugeMakesNoClaimForAProxyThatIsNotInSync(t *testing.T) {
	c, _, reader, tracker := ackedPinFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	ctx := context.Background()
	addOutboundCluster(c, bindingClusterName) // no namespace: unpinned
	require.NoError(t, c.generateSnapshot(ctx))
	unpinned := clusterVersions(t, c)

	// The agent pins the cluster. The proxy still holds the unpinned one.
	addPinnedCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	pinnedVersion := snapshotVersion(t, c)

	proxy := connectCDSProxy(t, c, tracker, 1, unpinned)
	opening := proxy.next()
	require.NotNil(t, opening)
	require.NotEmpty(t, opening.GetResources(), "fixture: the proxy is owed the pinned cluster")
	_, ok := ackedGauge()
	require.False(t, ok, "a statement is not read before the proxy has answered")

	// The proxy rejects it.
	proxy.nack(opening)
	acked, ok := ackedGauge()
	require.True(t, ok, "the proxy holds what it stated")
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned, "and that is the unpinned cluster: a NACK acknowledges nothing")
	assert.Zero(t, acked.pinned)

	// The NACK is also the next request: nothing changed since, so the watch
	// stays open and nothing is re-sent. The cache takes the rejected clusters
	// as delivered.
	require.Nil(t, proxy.next(), "go-control-plane does not re-send what a proxy rejected")

	// An on-demand subscription: a request with no nonce. It is answered,
	// empty, in the name of the snapshot the proxy rejected, and ACKed.
	later := proxy.onDemand()
	require.NotNil(t, later, "fixture: a wildcard request without a nonce is answered")
	require.Empty(t, later.GetResources())
	require.Empty(t, later.GetRemovedResources())
	require.Equal(t, pinnedVersion, later.GetSystemVersionInfo(), "fixture: the empty response names the rejected snapshot")
	proxy.ack(later)
	acked, _ = ackedGauge()
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned,
		"the proxy rejected the pinned cluster: an empty response later on the stream must not be read as it holding that snapshot")
	assert.Zero(t, acked.pinned)
}

// lostAckFixture is a proxy that accepted an unpinned cluster this agent sent
// it while the agent never read the answer: the stream ended first. The agent
// then dropped the cluster. It returns the cache, what the proxy would state
// on its next stream (the cluster included), and the tracker.
func lostAckFixture(t *testing.T) (*SnapshotCache, *recorder, *sdkmetric.ManualReader, *ack.Tracker, map[string]string) {
	t.Helper()
	c, rec, reader, tracker := ackedPinFixture(t)
	ctx := context.Background()
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	first := connectCDSProxy(t, c, tracker, 1, nil)
	first.ack(first.next())

	addOutboundCluster(c, bindingClusterName) // unpinned
	require.NoError(t, c.generateSnapshot(ctx))
	sent := first.next()
	require.Equal(t, []string{bindingClusterName}, deltaNames(sent))
	holds := maps.Clone(first.accepted)
	holds[bindingClusterName] = sent.GetResources()[0].GetVersion()
	first.callbacks.OnDeltaStreamClosed(1, nil)
	require.NoError(t, c.RemoveCluster(ctx, bindingClusterName))
	return c, rec, reader, tracker, holds
}

// TestAckedPinGaugeIsNotCompleteWithAStatedClusterItHasNoRecordOf: a proxy can
// hold a mesh cluster this agent has no record of. Here the agent sent it, the
// stream ended before the answer was read, the cluster was removed, and the
// builds a record outlives its entry by went past before the proxy came back.
// Its opening statement names the cluster; it rejects the response that
// removes it, so it goes on holding it. The agent does not know that
// cluster's pin class any more. It must not report a count that leaves it
// out as if it were the whole of what the proxy holds.
func TestAckedPinGaugeIsNotCompleteWithAStatedClusterItHasNoRecordOf(t *testing.T) {
	c, rec, reader, tracker, holds := lostAckFixture(t)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	buildPastTheGoneWindow(t, c)
	require.NotContains(t, c.acked.clusters, bindingClusterName, "fixture: nothing kept the record")

	again := connectCDSProxy(t, c, tracker, 2, holds)
	removal := again.next()
	require.Equal(t, []string{bindingClusterName}, removal.GetRemovedResources(), "fixture: the proxy is told to drop it")
	again.nack(removal)

	_, ok := ackedGauge()
	assert.False(t, ok, "the proxy holds a cluster whose pin state is not known: no count is the whole of it")
	require.Len(t, rec.with(ackedClusterPinsUnknownMsg), 1)
	assert.Equal(t, "1", rec.with(ackedClusterPinsUnknownMsg)[0].attrs["clusters"])

	// It lasts until the proxy accepts the removal.
	third := connectCDSProxy(t, c, tracker, 3, again.accepted)
	third.ack(third.next())
	acked, ok := ackedGauge()
	require.True(t, ok)
	assert.Equal(t, int64(1), acked.pinned)
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
	assert.Len(t, rec.with(ackedClusterPinsKnownMsg), 1)
	buildPastTheGoneWindow(t, c)
	assert.NotContains(t, c.acked.clusters, bindingClusterName, "and nothing is kept for it afterwards")
}

// TestAckedPinGaugeWhenARecordIsDroppedWhileTheOpeningAnswerIsAwaited: the same
// proxy comes back while the record of the removed cluster is still kept, and
// is slow to answer the response that removes it. A removal carries no version
// and is not in flight as far as the record goes, so the builds meanwhile drop
// it. The proxy then rejects the removal. What it holds is what it stated,
// that cluster included, and the gauge must not say otherwise.
func TestAckedPinGaugeWhenARecordIsDroppedWhileTheOpeningAnswerIsAwaited(t *testing.T) {
	c, rec, reader, tracker, holds := lostAckFixture(t)
	require.Contains(t, c.acked.clusters, bindingClusterName, "fixture: the record is still kept when the proxy states the cluster")

	again := connectCDSProxy(t, c, tracker, 2, holds)
	removal := again.next()
	require.Equal(t, []string{bindingClusterName}, removal.GetRemovedResources())
	buildPastTheGoneWindow(t, c)
	again.nack(removal)

	_, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	assert.False(t, ok, "the proxy still holds the cluster it stated; a count without it is not what it holds")
	assert.Len(t, rec.with(ackedClusterPinsUnknownMsg), 1)
}

// TestAckedPinGaugeCountsAStatedClusterWhoseRecordIsStillKept: the same again
// with the answer inside the window. The record is there, with the class the
// cluster was published with, and the cluster is counted, not unknown.
func TestAckedPinGaugeCountsAStatedClusterWhoseRecordIsStillKept(t *testing.T) {
	c, rec, reader, tracker, holds := lostAckFixture(t)
	again := connectCDSProxy(t, c, tracker, 2, holds)
	again.nack(again.next())

	acked, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok)
	assert.Equal(t, int64(1), acked.pinned)
	assert.Equal(t, byCause(0, 1, 0), acked.unpinned)
	assert.Empty(t, rec.with(ackedClusterPinsUnknownMsg))
}

// TestAckedPinsStatedClustersWithNoRecord: which stated names with no record
// make the state unknown. Not a cluster the newest snapshot publishes (it is
// not a cluster entry, or it would have a record). Not a name of a family
// that never carries a pin of its own. Any other is a cluster this agent does
// not publish and has no class for: unknown, until the proxy accepts its
// removal or stops stating it.
func TestAckedPinsStatedClustersWithNoRecord(t *testing.T) {
	const domain = "aether.internal"
	gone := proxy.ServiceClusterName("demo/gone", domain)
	goneFloor := proxy.TCPClusterName("demo/gone", domain)
	stated := map[string]string{
		"a":                      "ha",
		"published_not_an_entry": "hp", // of no family the agent knows, and published: not an entry
		// Stale: of a family the pin gauges do not count, and no longer published.
		"app_pod-0_8080":     "x1",
		"health_pod-0":       "x2",
		"inboundready_pod-0": "x3",
		proxy.QUICClusterName("demo/gone", domain, "demo/client"): "x4",
		proxy.UDPClusterName("demo/gone", domain):                 "x5",
		proxy.PassthroughClusterName:                              "x6",
		proxy.BlackholeClusterName:                                "x7",
		proxy.WaypointIngressClusterName(gone):                    "x8",
		proxy.EdgeK8sClusterName("demo", "gone", 8080):            "x9",
	}
	publish := func(h *ackedPins) {
		h.publish([]entryClass{{name: "a", class: pinClassPinned}}, map[string]string{"a": "ha", "published_not_an_entry": "hp"}, false)
	}

	clock := newStepClock()
	h := ackedPins{now: clock.now}
	publish(&h)
	u := h.accept(ack.Accepted{Opening: true, Rejected: true, Stated: stated})
	require.True(t, u.report, "none of these is a cluster whose pin state could be unknown")
	assert.Equal(t, cachemetrics.PinCounts{Pinned: 1}, u.counts)
	assert.Len(t, h.clusters, 1)

	stated[gone], stated[goneFloor] = "hg", "hf"
	u = h.accept(ack.Accepted{Opening: true, Rejected: true, Stated: stated})
	assert.False(t, u.report)
	assert.Equal(t, 2, u.unclassified, "an HTTP cluster and a TCP floor cluster this agent does not publish")

	// An acknowledged opening response removes what the snapshot does not
	// have: stated, removed, held no more.
	u = h.accept(ack.Accepted{Opening: true, Stated: stated, Removed: []string{gone, goneFloor}})
	require.True(t, u.report)
	assert.Equal(t, cachemetrics.PinCounts{Pinned: 1}, u.counts)

	// A statement that no longer names it releases it too.
	h.accept(ack.Accepted{Opening: true, Rejected: true, Stated: stated})
	delete(stated, gone)
	delete(stated, goneFloor)
	u = h.accept(ack.Accepted{Opening: true, Rejected: true, Stated: stated})
	require.True(t, u.report)
	for range goneBuilds {
		clock.advance(goneAge)
		publish(&h)
	}
	assert.Len(t, h.clusters, 1, "and its record goes")

	// The agent publishes the version the proxy stated: known from then on.
	stated[gone] = "hg"
	h.accept(ack.Accepted{Opening: true, Rejected: true, Stated: stated})
	u = h.publish([]entryClass{{name: "a", class: pinClassPinned}, {name: gone, class: unpinnedClass(cachemetrics.CauseNoNamespaceMetadata)}},
		map[string]string{"a": "ha", gone: "hg"}, false)
	require.True(t, u.report)
	assert.Equal(t, 1, u.counts.UnpinnedTotal())

	// Every name a cluster entry is published under is one the rule can see.
	for _, name := range []string{
		gone, goneFloor, proxy.PortClusterName("demo/gone", domain, 8080), proxy.TCPPortClusterName(goneFloor, 9000),
	} {
		assert.False(t, proxy.ClusterNameOutsidePinGauge(name), name)
	}
}

// TestAckedPinGaugeIsNotWithdrawnForAStaleClusterItDoesNotCount: the stale
// statement and the rejected opening removal, for a cluster that is not a mesh
// cluster entry. The agent stops publishing the ORIGINAL_DST passthrough (the
// capture mode changed while the proxy was away); the proxy states it, and
// rejects the response that removes it. It holds a cluster the agent no
// longer publishes and has no record of, and nothing about a pin is unknown:
// that cluster never carries one.
func TestAckedPinGaugeIsNotWithdrawnForAStaleClusterItDoesNotCount(t *testing.T) {
	c, rec, reader, tracker := ackedPinFixture(t)
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	ctx := context.Background()
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))
	first := connectCDSProxy(t, c, tracker, 1, nil)
	first.ack(first.next())
	require.Contains(t, first.accepted, proxy.PassthroughClusterName, "fixture: the proxy holds the passthrough cluster")
	first.callbacks.OnDeltaStreamClosed(1, nil)

	c.SetCaptureRedirectAll(false)
	require.NoError(t, c.generateSnapshot(ctx))
	require.NotContains(t, clusterVersions(t, c), proxy.PassthroughClusterName, "fixture: the agent no longer publishes it")

	again := connectCDSProxy(t, c, tracker, 2, first.accepted)
	opening := again.next()
	require.Contains(t, opening.GetRemovedResources(), proxy.PassthroughClusterName)
	again.nack(opening)

	acked, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok, "the proxy holds no cluster whose pin state is unknown")
	assert.Equal(t, int64(1), acked.pinned)
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
	assert.Empty(t, rec.with(ackedClusterPinsUnknownMsg))
	assert.NotContains(t, c.acked.clusters, proxy.PassthroughClusterName)
}

// publishedClusterNames is the names of the clusters the cache's snapshot
// publishes.
func publishedClusterNames(t *testing.T, c *SnapshotCache) []string {
	t.Helper()
	snap, err := c.GetSnapshot(c.nodeName)
	require.NoError(t, err)
	return slices.Sorted(maps.Keys(snap.GetResources(resourcev3.ClusterType)))
}

// TestEveryPublishedClusterIsACountedEntryOrOutsideThePinGauges holds the two
// definitions the acknowledged pin state reads a proxy's statement with to
// what the node agent and the edge really publish. A published cluster is
// either a cluster entry the pin report tracks (it has a record, and its name
// is a mesh entry's) or of a family the pin gauges do not count
// (proxy.ClusterNameOutsidePinGauge). Never both, never neither: a cluster that
// is neither would be taken for a mesh cluster of unknown pin state when a
// proxy states it after the agent dropped it, and one that is both would be
// left out of the count when the proxy holds it.
//
// The fixtures publish every family a snapshot can carry; the list at the end
// fails when one of them stops appearing, so the test cannot go vacuous. A
// family added to the generators is caught where it is built, by
// TestEveryClusterConstructorIsOfAClassifiedFamily in the proxy package.
func TestEveryPublishedClusterIsACountedEntryOrOutsideThePinGauges(t *testing.T) {
	ctx := context.Background()
	seen := map[string]bool{}
	check := func(t *testing.T, c *SnapshotCache) {
		t.Helper()
		names := publishedClusterNames(t, c)
		require.NotEmpty(t, names)
		for _, name := range names {
			_, tracked := c.acked.clusters[name]
			outside := proxy.ClusterNameOutsidePinGauge(name)
			assert.NotEqual(t, tracked, outside, "%s: tracked as a cluster entry = %v, of a family outside the pin gauges = %v", name, tracked, outside)
			assert.Equal(t, tracked, proxy.IsMeshEntryClusterName(name, c.meshDomain), "%s: a tracked entry is published under a mesh entry's name, and nothing else is", name)
			seen[clusterFamilyForTest(name)] = true
		}
	}

	t.Run("node agent, scoped capture", func(t *testing.T) {
		c, _, _ := pinFixture(t, true)
		c.SetWaypointConfig(true, proxy.DefaultEastWestTunnelPort)
		udpFloor := proxy.UDPClusterName("demo/dns", c.meshDomain)
		c.SetUDPServiceRoutes(map[string][]proxy.L4Backend{"demo/dns": {{Service: "demo/dns", Cluster: udpFloor, Weight: 1}}})
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
			Name: "web-0", Namespace: "demo", ServiceAccount: "web",
			NetworkNamespace: "/var/run/netns/cni-web-0", Ips: []string{"10.244.0.9"},
		}, raceTrustDomain))
		require.NoError(t, c.generateSnapshot(ctx))
		check(t, c)
	})
	t.Run("node agent, redirect-all capture", func(t *testing.T) {
		c, _, _ := pinFixture(t, true)
		c.SetCaptureRedirectAll(true)
		require.NoError(t, c.generateSnapshot(ctx))
		check(t, c)
	})
	t.Run("edge", func(t *testing.T) {
		c, _, _ := newBindingTestCache(t)
		c.SetEdgeMode(8080)
		require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
		addPinnedCluster(c, bindingClusterName)
		c.SetVirtualHosts([]VirtualHost{{
			Hosts: []string{"plain.example.com"},
			Routes: []Route{{
				Prefix: "/", Service: "plain", BackendNamespace: "legacy", Port: 8080,
				Backends: []RouteBackend{{Service: "plain", BackendNamespace: "legacy", Port: 8080, Weight: 1}},
			}},
		}})
		require.NoError(t, c.generateSnapshot(ctx))
		check(t, c)
	})

	for _, family := range []string{
		"service", "tcp floor", "udp floor", "quic twin", "per-pod app", "per-pod health", "per-pod inbound readiness",
		"waypoint ingress", "edge k8s", "passthrough", "blackhole",
	} {
		assert.True(t, seen[family], "no fixture publishes a cluster of the %q family any more: the test above no longer covers it", family)
	}
	delete(seen, "")
	assert.Len(t, seen, 11, "a fixture publishes a cluster of a family this test does not name: %v", seen)
}

// clusterFamilyForTest names the family of a published cluster for the
// coverage list of TestEveryPublishedClusterIsACountedEntryOrOutsideThePinGauges,
// from the spelling of the name alone (deliberately not through the
// predicates under test).
func clusterFamilyForTest(name string) string {
	for prefix, family := range map[string]string{
		"tcp:": "tcp floor", "udp:": "udp floor", "quic:": "quic twin", "app_": "per-pod app", "health_": "per-pod health",
		"inboundready_": "per-pod inbound readiness", "ew_ingress_": "waypoint ingress", "edge_k8s_": "edge k8s",
	} {
		if strings.HasPrefix(name, prefix) {
			return family
		}
	}
	switch {
	case name == "passthrough_original_dst":
		return "passthrough"
	case name == "blackhole":
		return "blackhole"
	case strings.Contains(name, ".aether-test.") || strings.Contains(name, ".demo."):
		return "service"
	}
	return "unnamed: " + name
}

// TestAckedPinGaugeKeepsAFloorWhoseRemovalWasRejected: "an entry with no
// cluster in the snapshot is not in the acknowledged gauge" holds for an entry
// whose cluster was never published, and for one whose removal the proxy
// accepted. It does not hold for a TCP floor that WAS published, left the
// capture set, and whose removal the proxy rejected: the entry has no cluster
// in the newest snapshot, the proxy still holds the floor cluster at the
// version it accepted, and the gauge goes on counting it there.
func TestAckedPinGaugeKeepsAFloorWhoseRemovalWasRejected(t *testing.T) {
	c, _, reader := pinFixture(t, true)
	ackedGauge := func() (pinSeries, bool) { return readPinGauge(t, reader, ackedTLSClustersGauge) }
	tracker := ack.NewTracker(c.log)
	tracker.SetAckObserver(c.ResponseAccepted)
	tracker.SetDeliveryObserver(c.ResponseDelivery)
	ctx := context.Background()
	p := connectCDSProxy(t, c, tracker, 1, nil)
	p.ack(p.next())
	before, ok := ackedGauge()
	require.True(t, ok)

	// demo/db leaves the capture TCP set: its floor entries stay in the
	// cluster map, pinned, and their clusters leave the snapshot.
	floor := proxy.TCPClusterName("demo/db", c.meshDomain)
	require.Contains(t, p.accepted, floor, "fixture: the proxy holds the floor cluster")
	c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "demo/rawnons", ClusterIP: "10.96.0.31", PrimaryIsTCP: true}})
	require.NoError(t, c.generateSnapshot(ctx))
	require.NotContains(t, clusterVersions(t, c), floor, "fixture: the entry has no cluster in the newest snapshot")
	c.clusterMu.RLock()
	_, stillAnEntry := c.clusters[floor]
	c.clusterMu.RUnlock()
	require.True(t, stillAnEntry, "fixture: and it is still a cluster entry")

	removal := p.next()
	require.Contains(t, removal.GetRemovedResources(), floor)
	gone := int64(0)
	for _, name := range removal.GetRemovedResources() {
		if strings.HasPrefix(name, floor) {
			gone++
		}
	}
	p.nack(removal)
	acked, ok := ackedGauge()
	require.True(t, ok)
	assert.Equal(t, before, acked, "the proxy rejected the removal: it holds the floor clusters at the versions it accepted, and they are counted")

	// Until it accepts the removal.
	again := connectCDSProxy(t, c, tracker, 2, p.accepted)
	again.ack(again.next())
	acked, ok = ackedGauge()
	require.True(t, ok)
	assert.Equal(t, before.pinned-gone, acked.pinned, "accepted: the entry is in the published gauge and no longer in this one")
	assert.Equal(t, before.unpinned, acked.unpinned)
}

// TestAckedPinsReadALateAckWithTheLastClassItsVersionWasPublishedWith: the
// class kept with a version in flight is not the class it had when it was
// FIRST sent. A build can count the same bytes under another reason while they
// are in flight (the tls_not_published promotion), and the version can then
// fall out of the published ones before the proxy answers. The answer is read
// with the class the version was last published with, whichever response
// carried it: with two proxy generations, one sent it before the
// reclassification and one after, and they hold the same bytes.
func TestAckedPinsReadALateAckWithTheLastClassItsVersionWasPublishedWith(t *testing.T) {
	notPublished := unpinnedClass(cachemetrics.CauseTLSNotPublished)
	entries := []entryClass{{name: "a", class: notPublished}}
	versions := map[string]string{"a": "ha"}
	sent := []ack.Resource{{Name: "a", Version: "ha"}}
	gap := cachemetrics.PinCounts{Unpinned: [cachemetrics.NumUnpinnedCauses]int{0, 0, 1, 0}}

	for _, tc := range []struct {
		name         string
		secondStream bool
	}{
		{name: "one response, sent before the reclassification"},
		{name: "two responses, one on each side of it", secondStream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h ackedPins
			h.publish(entries, versions, false)
			h.deliver(ack.Delivery{Resources: sent})
			h.publish(entries, versions, true) // the same bytes, the gap from now on
			if tc.secondStream {
				h.deliver(ack.Delivery{Resources: sent})
			}
			// The cluster is rebuilt until the version is no longer among the
			// published ones.
			for v := range offeredVersions {
				h.publish([]entryClass{{name: "a", class: pinClassPinned}}, map[string]string{"a": fmt.Sprintf("h%d", v)}, false)
			}
			_, offered := h.clusters["a"].classOfOffered("ha")
			require.False(t, offered, "fixture: the version fell out of the published ones")

			u := h.accept(ack.Accepted{Added: sent})
			h.deliver(ack.Delivery{Resources: sent, Ended: true})
			require.True(t, u.report)
			assert.Equal(t, gap, u.counts, "the first proxy's answer")
			if tc.secondStream {
				u = h.accept(ack.Accepted{Added: sent})
				h.deliver(ack.Delivery{Resources: sent, Ended: true})
				require.True(t, u.report)
				assert.Equal(t, gap, u.counts, "and the second's")
			}
		})
	}
}

// blockOnce is a log handler that stops the first record with msg until
// release is closed, and closes entered when it has it.
type blockOnce struct {
	slog.Handler
	msg     string
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (h *blockOnce) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.once.Do(func() {
			close(h.entered)
			<-h.release
		})
	}
	return h.Handler.Handle(ctx, r)
}

// reportInOrder runs first, which must log msg while it reports a change of
// the acknowledged pin state, stops it there, and runs second, a later change,
// while it is stopped. A report is the gauge write and the log lines; msg is
// one that is logged before the gauge is written. If reports can overtake one
// another, second writes the gauge and first then overwrites it with the
// older state.
func reportInOrder(t *testing.T, c *SnapshotCache, msg string, first, second func()) {
	t.Helper()
	block := &blockOnce{Handler: c.log.Handler(), msg: msg, entered: make(chan struct{}), release: make(chan struct{})}
	c.log = slog.New(block)
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(firstDone); first() }()
	select {
	case <-block.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("fixture: the first change did not log " + msg)
	}
	go func() { defer close(secondDone); second() }()
	// The second change either completes while the first is stopped (reports
	// are not ordered: it has now written the gauge) or waits for the first.
	select {
	case <-secondDone:
	case <-time.After(200 * time.Millisecond):
	}
	close(block.release)
	for _, done := range []chan struct{}{firstDone, secondDone} {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("a change of the acknowledged pin state did not return")
		}
	}
}

// TestAckedPinGaugeIsWrittenInTheOrderTheStateChanged: two proxy generations
// answer on two stream goroutines during a hot restart. The state changes one
// answer after the other, and the gauge has to end on the later one: an
// earlier change whose report is slow must not be written over a later one.
func TestAckedPinGaugeIsWrittenInTheOrderTheStateChanged(t *testing.T) {
	// The first change in each case: the proxy accepts, at a version this
	// agent knows, the cluster it held at one it does not. The state is known
	// again, which is logged before the gauge is written: pinned 1, unpinned 1.
	for _, tc := range []struct {
		name   string
		second ack.Accepted
		check  func(t *testing.T, acked pinSeries, ok bool)
	}{
		{
			name:   "a later count",
			second: ack.Accepted{TypeURL: resourcev3.ClusterType, Removed: []string{bindingClusterName}},
			check: func(t *testing.T, acked pinSeries, ok bool) {
				require.True(t, ok)
				assert.Equal(t, int64(1), acked.pinned)
				assert.Equal(t, byCause(0, 0, 0), acked.unpinned, "the proxy dropped the unpinned cluster after it accepted it")
			},
		},
		{
			name:   "a later withdrawal",
			second: ack.Accepted{TypeURL: resourcev3.ClusterType, Added: []ack.Resource{{Name: otherClusterName, Version: "a-version-this-agent-has-no-class-for"}}},
			check: func(t *testing.T, _ pinSeries, ok bool) {
				assert.False(t, ok, "the state became unknown after it was known: the gauge must stay withdrawn")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, reader, _ := ackedPinFixture(t)
			ctx := context.Background()
			addOutboundCluster(c, bindingClusterName) // unpinned
			addPinnedCluster(c, otherClusterName)
			require.NoError(t, c.generateSnapshot(ctx))
			versions := clusterVersions(t, c)
			stated := maps.Clone(versions)
			stated[bindingClusterName] = "a-version-this-agent-has-no-class-for"
			c.ResponseAccepted(ctx, ack.Accepted{TypeURL: resourcev3.ClusterType, Opening: true, Rejected: true, Stated: stated})
			_, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
			require.False(t, ok, "fixture: unknown")

			known := ack.Accepted{TypeURL: resourcev3.ClusterType, Added: []ack.Resource{{Name: bindingClusterName, Version: versions[bindingClusterName]}}}
			reportInOrder(t, c, ackedClusterPinsKnownMsg,
				func() { c.ResponseAccepted(ctx, known) },
				func() { c.ResponseAccepted(ctx, tc.second) })

			acked, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
			tc.check(t, acked, ok)
		})
	}
}

// TestAckedPinGaugeIsWrittenInOrderAcrossABuildAndAnAnswer: the other pair of
// goroutines. A snapshot build can move the acknowledged gauge (it publishes
// the version a proxy stated, which makes that cluster's class known), and an
// answer arrives on the stream's goroutine meanwhile. The later change wins
// here too.
func TestAckedPinGaugeIsWrittenInOrderAcrossABuildAndAnAnswer(t *testing.T) {
	c, _, reader, _ := ackedPinFixture(t)
	ctx := context.Background()
	addPinnedCluster(c, otherClusterName)
	// The proxy holds the cluster at a version this agent published too long
	// ago to have a class for.
	addClusterVariant(c, bindingClusterName, 0, true)
	require.NoError(t, c.generateSnapshot(ctx))
	stated := clusterVersions(t, c)
	for i := range offeredVersions {
		addClusterVariant(c, bindingClusterName, 1+i, false)
		require.NoError(t, c.generateSnapshot(ctx))
	}
	c.ResponseAccepted(ctx, ack.Accepted{TypeURL: resourcev3.ClusterType, Opening: true, Rejected: true, Stated: stated})
	_, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.False(t, ok, "fixture: unknown")

	reportInOrder(t, c, ackedClusterPinsKnownMsg,
		func() {
			// The agent publishes that version again: pinned 2.
			addClusterVariant(c, bindingClusterName, 0, true)
			assert.NoError(t, c.generateSnapshot(ctx))
		},
		func() {
			// The proxy accepts the removal of the cluster: pinned 1.
			c.ResponseAccepted(ctx, ack.Accepted{TypeURL: resourcev3.ClusterType, Removed: []string{bindingClusterName}})
		})

	acked, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok)
	assert.Equal(t, int64(1), acked.pinned, "the answer came after the build's change")
	assert.Equal(t, byCause(0, 0, 0), acked.unpinned)
}

// BenchmarkAckedPinsAccept is what one cluster ACK costs on the xDS stream's
// goroutine. accept recounts every record (settleLocked), so the cost grows
// with the number of cluster entries on the node and not with the response.
// Measure with this before making the count incremental.
func BenchmarkAckedPinsAccept(b *testing.B) {
	for _, n := range []int{200, 2000, 20000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			var h ackedPins
			entries := make([]entryClass, 0, n)
			versions := make(map[string]string, n)
			opening := ack.Accepted{Opening: true, Stated: map[string]string{}}
			for i := range n {
				name := fmt.Sprintf("svc-%05d.aether-test.aether.internal", i)
				class := pinClassPinned
				if i%10 == 0 {
					class = unpinnedClass(cachemetrics.CauseNoNamespaceMetadata)
				}
				entries = append(entries, entryClass{name: name, class: class})
				versions[name] = "v-" + name
				opening.Added = append(opening.Added, ack.Resource{Name: name, Version: versions[name]})
			}
			h.publish(entries, versions, false)
			h.accept(opening)
			one := ack.Accepted{Added: opening.Added[:1]}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				h.accept(one)
			}
		})
	}
}
