package cache

import (
	"context"
	"fmt"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// requireOnlyClosedReasonSeries holds the unpinned counter to its contract:
// every series carries `reason`, from the closed set, and nothing else. No
// attribute can name a cluster, so the series count cannot grow with the mesh.
// (readPinGauge holds the two gauges to the same contract on every read.)
func requireOnlyClosedReasonSeries(t *testing.T, reader *sdkmetric.ManualReader) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	series := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != clusterUnpinnedCtr {
				continue
			}
			sum, isSum := m.Data.(metricdata.Sum[int64])
			require.True(t, isSum)
			for _, dp := range sum.DataPoints {
				series++
				reason, has := dp.Attributes.Value("reason")
				require.True(t, has, "a counter series without a reason: %v", dp.Attributes)
				require.Contains(t, cachemetrics.UnpinnedCauses, cachemetrics.UnpinnedCause(reason.AsString()))
				require.Equal(t, 1, dp.Attributes.Len(), "the counter carries `reason` alone: %v", dp.Attributes)
			}
		}
	}
	require.Equal(t, cachemetrics.NumUnpinnedCauses, series, "one counter series per cause, whatever the number of clusters")
}

// entriesWithoutNamespace is the number of cluster entries of the pinFixture
// that are meant to be mTLS and whose endpoints carry no namespace: the ones
// no pin can be rendered for while the trust domain is known.
func entriesWithoutNamespace(c *SnapshotCache) int {
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	n := 0
	for _, e := range c.clusters {
		if !e.plaintext && len(e.sanNamespaces) == 0 {
			n++
		}
	}
	return n
}

// TestNoNamespaceIsNotTheValidationGapUntilTLSIsPublished is #1482.
//
// Before the node agent has its SVID nothing it publishes carries TLS: an HTTP
// cluster goes out bare and the TCP floor cluster is withheld. An entry whose
// endpoints carry no namespace was counted and logged there under
// no_namespace_metadata, the reason that otherwise means "TLS is served and
// checks no server identity", and a reader of the line could not tell which.
//
// It has a reason of its own while no TLS is published, and it becomes
// no_namespace_metadata in the very snapshot that first publishes TLS for it:
// the gap is never hidden, and it is never announced before it exists.
func TestNoNamespaceIsNotTheValidationGapUntilTLSIsPublished(t *testing.T) {
	c, rec, reader := pinFixture(t, false) // trust domain known, node SVID not served
	ctx := context.Background()
	n := entriesWithoutNamespace(c)
	require.Positive(t, n, "fixture: entries with no namespace metadata")

	// One more snapshot in that state, so the counter's step is this one's.
	before := unpinnedByCause(t, reader)
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))

	// What is published: no TLS at all.
	gate := gateOnSnapshot(t, c)
	require.Empty(t, gate.pinned, "no node SVID: no TLS cluster is published")
	require.Empty(t, gate.unpinned, "no node SVID: no TLS cluster is published")

	// What is reported: the entries are named, under the reason that says so.
	warns := warnsByCause(t, rec)
	require.Len(t, warns, 1, "one line, one reason: %v", warns)
	line, ok := warns[cachemetrics.CauseTLSNotPublished]
	require.True(t, ok, "no TLS is published, so the line must not say no_namespace_metadata: %v", warns)
	assert.Equal(t, fmt.Sprint(n), line.attrs["count"])
	assert.Contains(t, line.attrs["clusters"], proxy.TCPClusterName("demo/rawnons", c.meshDomain))

	gauge, ok := readPinGauge(t, reader, tlsClustersGauge)
	require.True(t, ok)
	assert.Zero(t, gauge.pinned)
	assert.Equal(t, withTLSNotPublished(byCause(0, 0, 0), int64(n)), gauge.unpinned,
		"the gauge must not show a validation gap that is not being served")
	counter := unpinnedByCause(t, reader)
	assert.Equal(t, int64(n), counter[cachemetrics.CauseTLSNotPublished]-before[cachemetrics.CauseTLSNotPublished])
	assert.Zero(t, counter[cachemetrics.CauseNoNamespaceMetadata], "nothing was ever served without a pin on this node")
	requireOnlyClosedReasonSeries(t, reader)

	// The node SVID arrives. SetNodeIdentity publishes the snapshot that first
	// carries TLS, and THAT snapshot is loud about the missing pin.
	before = counter
	rec.reset()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	gate = gateOnSnapshot(t, c)
	require.NotEmpty(t, gate.unpinned, "fixture: TLS is now published without a pin for the entries with no namespace")
	runtime := runtimeReading(c)
	for name := range gate.unpinned {
		if proxy.IsQUICClusterName(name) {
			continue
		}
		assert.Equal(t, cachemetrics.CauseNoNamespaceMetadata, runtime.unpinned[name],
			"%s is published with an unpinned TLS context: it must be reported as the validation gap", name)
	}

	warns = warnsByCause(t, rec)
	require.Len(t, warns, 1, "%v", warns)
	line, ok = warns[cachemetrics.CauseNoNamespaceMetadata]
	require.True(t, ok, "the snapshot that publishes TLS without a pin must say so: %v", warns)
	assert.Equal(t, fmt.Sprint(n), line.attrs["count"])

	gauge, _ = readPinGauge(t, reader, tlsClustersGauge)
	assert.Positive(t, gauge.pinned)
	assert.Equal(t, byCause(0, int64(n), 0), gauge.unpinned, "tls_not_published reads zero once TLS is published")
	counter = unpinnedByCause(t, reader)
	assert.Equal(t, int64(n), counter[cachemetrics.CauseNoNamespaceMetadata], "counted in the snapshot that first publishes TLS")
	assert.Equal(t, before[cachemetrics.CauseTLSNotPublished], counter[cachemetrics.CauseTLSNotPublished], "and no longer under the other reason")
	requireOnlyClosedReasonSeries(t, reader)
}

// TestTLSNotPublishedIsNeverTheReasonOfASnapshotThatCarriesTLS is the fail-loud
// half of #1482. The reason is recorded when the pin is rendered, and the TCP
// floor cluster is built at snapshot time from the node identity in force
// THEN. If the identity lands between the two (SetNodeIdentity stores it, then
// recomputes), a snapshot can carry a TLS floor cluster with no pin while the
// entry still says tls_not_published. Such a snapshot must report the
// validation gap, not the benign reason.
func TestTLSNotPublishedIsNeverTheReasonOfASnapshotThatCarriesTLS(t *testing.T) {
	c, rec, reader := pinFixture(t, false)
	ctx := context.Background()
	n := entriesWithoutNamespace(c)

	// The identity is stored; the recompute that follows it has not run yet.
	c.localMu.Lock()
	c.nodeSpiffeID = nodeIdentity
	c.localMu.Unlock()
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))

	gate := gateOnSnapshot(t, c)
	rawnons := proxy.TCPClusterName("demo/rawnons", c.meshDomain)
	require.Contains(t, gate.unpinned, rawnons, "fixture: this snapshot carries the TCP floor with TLS and no pin")

	warns := warnsByCause(t, rec)
	_, benign := warns[cachemetrics.CauseTLSNotPublished]
	assert.False(t, benign, "a snapshot that carries TLS without a pin must not call it tls_not_published: %v", warns)
	line, ok := warns[cachemetrics.CauseNoNamespaceMetadata]
	require.True(t, ok, "%v", warns)
	assert.Contains(t, line.attrs["clusters"], rawnons)
	assert.Equal(t, fmt.Sprint(n), line.attrs["count"])

	gauge, _ := readPinGauge(t, reader, tlsClustersGauge)
	assert.Equal(t, byCause(0, int64(n), 0), gauge.unpinned)

	// And the acknowledged gauge is of the same, corrected, counts.
	c.ClusterPinsAcked(ctx, snapshotVersion(t, c))
	acked, ok := readPinGauge(t, reader, ackedTLSClustersGauge)
	require.True(t, ok)
	assert.Equal(t, gauge.unpinned, acked.unpinned)
}

// TestTLSNotPublishedIsNotTheTrustDomainWindow: with no trust domain every
// entry has the one cause it had before (#832), whether or not its endpoints
// carry a namespace. The new reason does not take entries from it.
func TestTLSNotPublishedIsNotTheTrustDomainWindow(t *testing.T) {
	c, rec, reader := pinFixture(t, false)
	c.trustDomain.Store("")
	c.recomputeMTLSClusters()
	rec.reset()
	require.NoError(t, c.generateSnapshot(context.Background()))

	warns := warnsByCause(t, rec)
	require.Len(t, warns, 1, "%v", warns)
	require.Contains(t, warns, cachemetrics.CauseTrustDomainUnknown)
	gauge, _ := readPinGauge(t, reader, tlsClustersGauge)
	assert.Zero(t, gauge.unpinned[cachemetrics.CauseTLSNotPublished])
	assert.Zero(t, gauge.unpinned[cachemetrics.CauseNoNamespaceMetadata])
	assert.Positive(t, gauge.unpinned[cachemetrics.CauseTrustDomainUnknown])
}

// TestUnpublishedTCPFloorIsNotTheValidationGap: a TCP floor entry is kept in
// the cluster cache whether or not its floor cluster is published (only a
// service in the capture TCP set, or on the edge one a route references, gets
// a "tcp:" cluster). An entry with no namespace metadata whose floor cluster
// is NOT in the snapshot has no TLS on the wire either, so it is
// tls_not_published, not the validation gap; and it is the gap in the snapshot
// that first publishes its floor cluster.
func TestUnpublishedTCPFloorIsNotTheValidationGap(t *testing.T) {
	c, rec, reader := pinFixture(t, true) // node SVID served: HTTP clusters carry TLS
	ctx := context.Background()
	rawnons := proxy.TCPClusterName("demo/rawnons", c.meshDomain)
	rawnonsPort := proxy.TCPPortClusterName(rawnons, 9100)
	nons := proxy.ServiceClusterName("demo/nons", c.meshDomain)

	// demo/rawnons leaves the capture TCP set: its floor is no longer published.
	c.SetCaptureTCPServices([]capture.CaptureTCPService{
		{ServiceName: "demo/db", ClusterIP: "10.96.0.30", PrimaryIsTCP: true},
	})
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))

	gate := gateOnSnapshot(t, c)
	require.NotContains(t, gate.all, rawnons, "fixture: the floor cluster is not in the snapshot")
	require.NotContains(t, gate.all, rawnonsPort, "fixture: nor its per-port cluster")
	require.Contains(t, gate.unpinned, nons, "fixture: the HTTP cluster with no namespace is still TLS without a pin")

	warns := warnsByCause(t, rec)
	require.Len(t, warns, 2, "%v", warns)
	gap := warns[cachemetrics.CauseNoNamespaceMetadata].attrs["clusters"]
	pending := warns[cachemetrics.CauseTLSNotPublished].attrs["clusters"]
	assert.NotContains(t, gap, "tcp:", "no floor cluster is published: it must not be reported as TLS without a pin")
	assert.Contains(t, gap, "demo/nons", "the published HTTP gap is still reported as the gap")
	assert.Equal(t, rawnons+" "+rawnonsPort, pending)
	gauge, _ := readPinGauge(t, reader, tlsClustersGauge)
	assert.Equal(t, int64(2), gauge.unpinned[cachemetrics.CauseTLSNotPublished])
	assert.Equal(t, int64(entriesWithoutNamespace(c)-2), gauge.unpinned[cachemetrics.CauseNoNamespaceMetadata])

	// Every published cluster with an unpinned TLS context is reported as the gap.
	for name := range gate.unpinned {
		if proxy.IsQUICClusterName(name) {
			continue
		}
		assert.Contains(t, gap, name, "%s is published with an unpinned TLS context", name)
	}

	// It is captured again: the snapshot that publishes the floor says so.
	c.SetCaptureTCPServices([]capture.CaptureTCPService{
		{ServiceName: "demo/db", ClusterIP: "10.96.0.30", PrimaryIsTCP: true},
		{ServiceName: "demo/rawnons", ClusterIP: "10.96.0.31", PrimaryIsTCP: true},
	})
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	gate = gateOnSnapshot(t, c)
	require.Contains(t, gate.unpinned, rawnons)
	warns = warnsByCause(t, rec)
	require.Len(t, warns, 1, "%v", warns)
	assert.Contains(t, warns[cachemetrics.CauseNoNamespaceMetadata].attrs["clusters"], rawnons)
	gauge, _ = readPinGauge(t, reader, tlsClustersGauge)
	assert.Equal(t, byCause(0, int64(entriesWithoutNamespace(c)), 0), gauge.unpinned)
}

// TestEveryPublishedResourceTypeHasASeededNackSeries holds the closed set of
// type URLs the NACK counter is seeded for (#1480, ack.ServedTypeURLs) to what
// a snapshot actually publishes, for the node proxy and for the edge. A
// resource type added to the snapshot without a seeded series would be counted
// under "other", and its "no NACKs" would be an absent series again.
func TestEveryPublishedResourceTypeHasASeededNackSeries(t *testing.T) {
	check := func(t *testing.T, c *SnapshotCache) {
		t.Helper()
		snap, err := c.GetSnapshot(c.nodeName)
		require.NoError(t, err)
		published := 0
		for rt := types.ResponseType(0); rt < types.UnknownType; rt++ {
			typeURL, err := cachev3.GetResponseTypeURL(rt)
			require.NoError(t, err)
			if len(snap.GetResources(typeURL)) == 0 {
				continue
			}
			published++
			assert.Contains(t, ack.ServedTypeURLs, typeURL, "the snapshot publishes %s and the NACK counter has no seeded series for it", typeURL)
		}
		require.GreaterOrEqual(t, published, 3, "fixture: a snapshot with nothing in it holds nothing to the set")
	}

	t.Run("node proxy", func(t *testing.T) {
		c, _, _ := pinFixture(t, true)
		check(t, c)
	})
	t.Run("edge", func(t *testing.T) {
		c, _, _ := newBindingTestCache(t)
		c.SetEdgeMode(8080)
		require.NoError(t, c.generateSnapshot(context.Background()))
		check(t, c)
	})
}
