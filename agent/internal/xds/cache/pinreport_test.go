package cache

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	"aethermesh.dev/agent/internal/xds/proxy"
	envoyvalidate "aethermesh.dev/agent/test/envoy_validate"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The #1425 gauges: mesh cluster entries meant to be mTLS, by whether their
// server identity is pinned and, when it is not, why. One is what the current
// snapshot publishes, the other what the proxy last acknowledged.
const (
	tlsClustersGauge      = "aether.agent.snapshot.tls_clusters"
	ackedTLSClustersGauge = "aether.agent.xds.acked_tls_clusters"
)

// pinSeries is one reading of a TLS-cluster gauge: the pinned series and one
// unpinned series per cause.
type pinSeries struct {
	pinned   int64
	unpinned map[cachemetrics.UnpinnedCause]int64
}

func (s pinSeries) unpinnedTotal() int64 {
	var n int64
	for _, v := range s.unpinned {
		n += v
	}
	return n
}

// readPinGauge reads every series of one TLS-cluster gauge and holds the
// series set to its contract: exactly one pin=pinned series and one
// pin=unpinned series per cause of the closed set, each carrying `pin` (and,
// when unpinned, `reason`) and nothing else. Nothing a cluster is named by can
// be an attribute, so the series count cannot grow with the mesh. ok is false
// while the gauge has never been recorded.
func readPinGauge(t *testing.T, reader *sdkmetric.ManualReader, name string) (pinSeries, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := pinSeries{unpinned: map[cachemetrics.UnpinnedCause]int64{}}
	points, pinnedSeries := 0, 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			gauge, isGauge := m.Data.(metricdata.Gauge[int64])
			require.True(t, isGauge, "%s is %T, want Gauge[int64]", name, m.Data)
			for _, dp := range gauge.DataPoints {
				points++
				pin, _ := dp.Attributes.Value("pin")
				reason, hasReason := dp.Attributes.Value("reason")
				switch pin.AsString() {
				case cachemetrics.PinPinned:
					pinnedSeries++
					out.pinned = dp.Value
					assert.Equal(t, 1, dp.Attributes.Len(), "%s: the pinned series carries `pin` alone: %v", name, dp.Attributes)
				case cachemetrics.PinUnpinned:
					require.True(t, hasReason, "%s: an unpinned series has a reason: %v", name, dp.Attributes)
					cause := cachemetrics.UnpinnedCause(reason.AsString())
					require.Contains(t, cachemetrics.UnpinnedCauses, cause, "%s: reason outside the closed set", name)
					_, dup := out.unpinned[cause]
					require.False(t, dup, "%s: two series for reason %q", name, cause)
					out.unpinned[cause] = dp.Value
					assert.Equal(t, 2, dp.Attributes.Len(), "%s: an unpinned series carries `pin` and `reason` alone: %v", name, dp.Attributes)
				default:
					t.Fatalf("%s: unexpected pin attribute in %v", name, dp.Attributes)
				}
			}
		}
	}
	if points == 0 {
		return out, false
	}
	require.Equal(t, 1, pinnedSeries, "%s: exactly one pinned series", name)
	require.Len(t, out.unpinned, cachemetrics.NumUnpinnedCauses,
		"%s: one unpinned series per cause, zeros included, whatever the number of clusters", name)
	return out, true
}

// pinGauge reads the published gauge as its two totals. ok is false until a
// snapshot has recorded it.
func pinGauge(t *testing.T, reader *sdkmetric.ManualReader) (pinned, unpinned int64, ok bool) {
	t.Helper()
	s, ok := readPinGauge(t, reader, tlsClustersGauge)
	return s.pinned, s.unpinnedTotal(), ok
}

// byCause spells a per-cause expectation with the zeros filled in: the gauge
// records every cause on every snapshot.
func byCause(tdUnknown, noNamespace, notRendered int64) map[cachemetrics.UnpinnedCause]int64 {
	return map[cachemetrics.UnpinnedCause]int64{
		cachemetrics.CauseTrustDomainUnknown:  tdUnknown,
		cachemetrics.CauseNoNamespaceMetadata: noNamespace,
		cachemetrics.CausePinNotRendered:      notRendered,
	}
}

// unpinnedByCause reads the #832 counter per `reason` attribute.
func unpinnedByCause(t *testing.T, reader *sdkmetric.ManualReader) map[cachemetrics.UnpinnedCause]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[cachemetrics.UnpinnedCause]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != clusterUnpinnedCtr {
				continue
			}
			sum, isSum := m.Data.(metricdata.Sum[int64])
			require.True(t, isSum)
			for _, dp := range sum.DataPoints {
				v, _ := dp.Attributes.Value("reason")
				out[cachemetrics.UnpinnedCause(v.AsString())] = dp.Value
			}
		}
	}
	return out
}

// warnsByCause indexes the unpinned WARN lines of one snapshot by their
// `reason`, failing if a cause is logged twice.
func warnsByCause(t *testing.T, rec *recorder) map[cachemetrics.UnpinnedCause]capturedRecord {
	t.Helper()
	out := map[cachemetrics.UnpinnedCause]capturedRecord{}
	for _, w := range rec.with(unpinnedClusterMsg) {
		cause := cachemetrics.UnpinnedCause(w.attrs["reason"])
		_, dup := out[cause]
		require.False(t, dup, "cause %q logged twice in one snapshot", cause)
		out[cause] = w
	}
	return out
}

// installEntry stores one cluster entry with its pin rendered from st, the way
// a recompute renders every entry, but for this entry alone.
func installEntry(c *SnapshotCache, name string, entry clusterEntry, st localMTLSState) {
	c.clusterMu.Lock()
	defer c.clusterMu.Unlock()
	c.refreshEntryMTLSLocked(&entry, st)
	c.clusters[name] = entry
}

// TestUnpinnedClusterReportGivesEachClusterItsOwnCause is #1424. The report
// used to pick ONE reason for the whole line from the trust domain at report
// time, so every cluster it named was said to have endpoints without namespace
// metadata whenever the trust domain was known, whatever had actually emptied
// its pin. The cause now belongs to the cluster: it is recorded by the render
// that left the pin empty.
func TestUnpinnedClusterReportGivesEachClusterItsOwnCause(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))

	known := c.localMTLSSnapshot()
	unknown := known
	unknown.trustDomain, unknown.validationContextName = "", ""

	base := func(name string) clusterEntry {
		return clusterEntry{cluster: &clusterv3.Cluster{Name: name}, service: "aether-test/echo"}
	}
	// Rendered before the trust domain was known, and not re-rendered since.
	early := base("early.aether-test.aether.internal")
	early.sanNamespaces = []string{"aether-test"}
	installEntry(c, "early.aether-test.aether.internal", early, unknown)
	// Endpoints with no namespace: nothing to build the SPIFFE ID from.
	installEntry(c, "nons-a.aether-test.aether.internal", base("nons-a.aether-test.aether.internal"), known)
	installEntry(c, "nons-b.aether-test.aether.internal", base("nons-b.aether-test.aether.internal"), known)
	// Reached the snapshot without a render at all.
	c.clusterMu.Lock()
	c.clusters["raw.aether-test.aether.internal"] = base("raw.aether-test.aether.internal")
	c.clusterMu.Unlock()
	// And one that is pinned, which must not be named anywhere.
	pinned := base("ok.aether-test.aether.internal")
	pinned.sanNamespaces = []string{"aether-test"}
	installEntry(c, "ok.aether-test.aether.internal", pinned, known)

	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))

	warns := warnsByCause(t, rec)
	require.Len(t, warns, 3, "one line per cause present, so a `stats by (reason)` groups them")

	assert.Equal(t, "early.aether-test.aether.internal", warns[cachemetrics.CauseTrustDomainUnknown].attrs["clusters"])
	assert.Equal(t, "1", warns[cachemetrics.CauseTrustDomainUnknown].attrs["count"])
	assert.Equal(t, "nons-a.aether-test.aether.internal nons-b.aether-test.aether.internal",
		warns[cachemetrics.CauseNoNamespaceMetadata].attrs["clusters"])
	assert.Equal(t, "2", warns[cachemetrics.CauseNoNamespaceMetadata].attrs["count"])
	assert.Equal(t, "raw.aether-test.aether.internal", warns[cachemetrics.CausePinNotRendered].attrs["clusters"])
	assert.Equal(t, "1", warns[cachemetrics.CausePinNotRendered].attrs["count"])

	for cause, w := range warns {
		assert.NotContains(t, w.attrs["clusters"], "ok.aether-test", "%s: a pinned cluster is never named", cause)
		assert.Equal(t, "4", w.attrs["unpinned"], "%s: every line carries the snapshot's totals", cause)
		assert.Equal(t, "1", w.attrs["pinned"], "%s", cause)
		assert.Equal(t, raceTrustDomain, w.attrs["trust_domain"], "%s", cause)
		assert.NotEmpty(t, w.attrs["snapshot_version"], "%s", cause)
	}

	// The counter splits the same way, and the gauge holds the same totals.
	assert.Equal(t, map[cachemetrics.UnpinnedCause]int64{
		cachemetrics.CauseTrustDomainUnknown:  1,
		cachemetrics.CauseNoNamespaceMetadata: 2,
		cachemetrics.CausePinNotRendered:      1,
	}, unpinnedByCause(t, reader))
	gauge, ok := readPinGauge(t, reader, tlsClustersGauge)
	require.True(t, ok)
	assert.Equal(t, int64(1), gauge.pinned)
	assert.Equal(t, byCause(1, 2, 1), gauge.unpinned, "the gauge splits by the cause the line names each cluster under")
}

// TestUnpinnedCauseIsTheRendersNotTheReports: the trust domain became known
// after the pins were rendered and nothing has re-rendered them yet. The
// clusters are unpinned because the trust domain WAS unknown; reading the
// trust domain at report time called that "no namespace metadata" (#1424) and
// sent the reader to the registry.
func TestUnpinnedCauseIsTheRendersNotTheReports(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	addPinnedCluster(c, bindingClusterName) // rendered with no trust domain

	c.trustDomain.Store(raceTrustDomain) // learned; no recompute has run yet
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))

	warns := rec.with(unpinnedClusterMsg)
	require.Len(t, warns, 1)
	assert.Equal(t, string(cachemetrics.CauseTrustDomainUnknown), warns[0].attrs["reason"],
		"the endpoints do carry a namespace; the pin is empty because it was rendered without a trust domain")
	assert.Equal(t, raceTrustDomain, warns[0].attrs["trust_domain"], "the trust domain in force when the snapshot was set")
	assert.Equal(t, int64(1), unpinnedByCause(t, reader)[cachemetrics.CauseTrustDomainUnknown])
	assert.Zero(t, unpinnedByCause(t, reader)[cachemetrics.CauseNoNamespaceMetadata])

	// The next recompute renders the pin, and the report goes quiet.
	c.recomputeMTLSClusters()
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Empty(t, rec.with(unpinnedClusterMsg))
	gotPinned, gotUnpinned, _ := pinGauge(t, reader)
	assert.Equal(t, int64(1), gotPinned)
	assert.Zero(t, gotUnpinned)
}

// TestTLSClusterGaugeFollowsTheTrustDomainWindow is #1425 over the #832
// window: the gauge is written on every snapshot, a healthy snapshot is a
// positive sample (pinned > 0, unpinned == 0) rather than silence, and the
// unpinned count is exactly what the WARN names and the counter adds.
func TestTLSClusterGaugeFollowsTheTrustDomainWindow(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()

	_, _, ok := pinGauge(t, reader)
	require.False(t, ok, "nothing is recorded before the first snapshot")

	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	addPinnedCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	gotPinned, gotUnpinned, ok := pinGauge(t, reader)
	require.True(t, ok, "a healthy snapshot records the gauge: the zero has to be a sample")
	assert.Equal(t, int64(1), gotPinned)
	assert.Zero(t, gotUnpinned)

	// The trust domain is unknown: the pin cannot be rendered.
	c.trustDomain.Store("")
	c.recomputeMTLSClusters()
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))

	warns := rec.with(unpinnedClusterMsg)
	require.Len(t, warns, 1)
	assert.Equal(t, string(cachemetrics.CauseTrustDomainUnknown), warns[0].attrs["reason"])
	assert.Equal(t, bindingClusterName, warns[0].attrs["clusters"])
	gotPinned, gotUnpinned, _ = pinGauge(t, reader)
	assert.Zero(t, gotPinned)
	assert.Equal(t, int64(1), gotUnpinned)
	assert.Equal(t, warns[0].attrs["count"], fmt.Sprint(gotUnpinned), "the gauge is the count the line names")
	assert.Equal(t, int64(1), unpinnedByCause(t, reader)[cachemetrics.CauseTrustDomainUnknown])

	// Recovery is a sample too.
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	c.recomputeMTLSClusters()
	require.NoError(t, c.generateSnapshot(ctx))
	gotPinned, gotUnpinned, _ = pinGauge(t, reader)
	assert.Equal(t, int64(1), gotPinned)
	assert.Zero(t, gotUnpinned)
}

// TestTLSClusterGaugeLeavesOutThePlaintextUDPFloor: a UDP floor entry has no
// handshake, so it is neither a pinned TLS cluster nor an unpinned one (#1393
// for the report, the same rule for the gauge). A TLS cluster that loses its
// pin beside it is counted once and named alone.
func TestTLSClusterGaugeLeavesOutThePlaintextUDPFloor(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	declareDeps(c, "aether-test/udponly")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", udpOnlyRegistry("aether-test/udponly", "10.0.0.40", 9001)))

	udpName := proxy.UDPClusterName("aether-test/udponly", c.meshDomain)
	c.clusterMu.RLock()
	udpEntry, hasUDP := c.clusters[udpName]
	entries := len(c.clusters)
	c.clusterMu.RUnlock()
	require.True(t, hasUDP, "the fixture must produce the udp: entry this test is about")
	require.Equal(t, 1, entries)
	kind, cause := udpEntry.pinState()
	assert.Equal(t, pinNotApplicable, kind, "a plaintext entry has no pin to carry or to lose")
	assert.Empty(t, cause)

	require.NoError(t, c.generateSnapshot(ctx))
	gotPinned, gotUnpinned, ok := pinGauge(t, reader)
	require.True(t, ok)
	assert.Zero(t, gotPinned, "the UDP floor is not a pinned TLS cluster")
	assert.Zero(t, gotUnpinned, "and not an unpinned one")

	// A TLS cluster with no pin, next to the UDP entry.
	addOutboundCluster(c, bindingClusterName)
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	warns := rec.with(unpinnedClusterMsg)
	require.Len(t, warns, 1)
	assert.Equal(t, bindingClusterName, warns[0].attrs["clusters"], "only the TLS cluster is named")
	assert.Equal(t, string(cachemetrics.CauseNoNamespaceMetadata), warns[0].attrs["reason"])
	gotPinned, gotUnpinned, _ = pinGauge(t, reader)
	assert.Zero(t, gotPinned)
	assert.Equal(t, int64(1), gotUnpinned)

	// And a pinned one beside both: one pinned, one unpinned, the UDP entry in
	// neither.
	addPinnedCluster(c, "pinned.aether-test.aether.internal")
	require.NoError(t, c.generateSnapshot(ctx))
	gotPinned, gotUnpinned, _ = pinGauge(t, reader)
	assert.Equal(t, int64(1), gotPinned)
	assert.Equal(t, int64(1), gotUnpinned)
}

// TestTLSClusterGaugeDoesNotCountAClusterPublishedWithoutTLS: before the node
// SVID is served (and for the whole life of a mesh with SPIRE off) an HTTP
// entry's cluster goes out bare, with no transport socket. Its pin is rendered
// but nothing carries it, so calling it a pinned TLS cluster would make the
// gauge claim a check that is not being made. It is in neither count.
func TestTLSClusterGaugeDoesNotCountAClusterPublishedWithoutTLS(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	addPinnedCluster(c, bindingClusterName) // no SetNodeIdentity: no mTLS cluster is built

	c.clusterMu.RLock()
	entry := c.clusters[bindingClusterName]
	c.clusterMu.RUnlock()
	require.NotEmpty(t, entry.sanURIs, "the pin is rendered")
	require.Nil(t, entry.mtlsCluster, "but the cluster that would carry it is not built")

	require.NoError(t, c.generateSnapshot(ctx))
	assert.Empty(t, rec.with(unpinnedClusterMsg), "no pin was lost: there is no TLS to pin")
	gotPinned, gotUnpinned, ok := pinGauge(t, reader)
	require.True(t, ok)
	assert.Zero(t, gotPinned, "a cluster published with no transport socket is not a pinned TLS cluster")
	assert.Zero(t, gotUnpinned)

	// The node SVID arrives: the same entry is now a pinned TLS cluster.
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	gotPinned, gotUnpinned, _ = pinGauge(t, reader)
	assert.Equal(t, int64(1), gotPinned)
	assert.Zero(t, gotUnpinned)
}

// TestUnpinnedClusterReportBoundsTheNamesPerCause: the names are the
// diagnostic part and stay bounded, 20 per cause and one line per cause, so a
// snapshot's report cannot grow with the number of unpinned clusters; the
// count, the counter and the gauge stay exact.
func TestUnpinnedClusterReportBoundsTheNamesPerCause(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	const n = maxUnpinnedClusterNames + 5
	for i := range n {
		addOutboundCluster(c, fmt.Sprintf("svc-%02d.aether-test.aether.internal", i))
	}
	// And more than the bound under a second cause, in the same snapshot.
	const early = maxUnpinnedClusterNames + 2
	unknown := c.localMTLSSnapshot()
	unknown.trustDomain, unknown.validationContextName = "", ""
	for i := range early {
		name := fmt.Sprintf("early-%02d.aether-test.aether.internal", i)
		installEntry(c, name, clusterEntry{
			cluster: &clusterv3.Cluster{Name: name}, service: "aether-test/echo", sanNamespaces: []string{"aether-test"},
		}, unknown)
	}

	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	warns := warnsByCause(t, rec)
	require.Len(t, warns, 2, "one line per cause, never one per cluster")

	nons := warns[cachemetrics.CauseNoNamespaceMetadata]
	shown := strings.Fields(nons.attrs["clusters"])
	require.Len(t, shown, maxUnpinnedClusterNames+1)
	assert.Equal(t, "...", shown[maxUnpinnedClusterNames], "a truncated list says so")
	assert.Equal(t, "svc-00.aether-test.aether.internal", shown[0], "sorted, so the same clusters are shown every snapshot")
	assert.Equal(t, fmt.Sprint(n), nons.attrs["count"], "the count is exact when the names are cut")

	tdu := warns[cachemetrics.CauseTrustDomainUnknown]
	shown = strings.Fields(tdu.attrs["clusters"])
	require.Len(t, shown, maxUnpinnedClusterNames+1)
	assert.Equal(t, "early-00.aether-test.aether.internal", shown[0])
	assert.Equal(t, fmt.Sprint(early), tdu.attrs["count"])
	for cause, w := range warns {
		for _, name := range strings.Fields(w.attrs["clusters"]) {
			if cause == cachemetrics.CauseTrustDomainUnknown {
				assert.False(t, strings.HasPrefix(name, "svc-"), "%s is named under a cause that is not its own", name)
			} else {
				assert.False(t, strings.HasPrefix(name, "early-"), "%s is named under a cause that is not its own", name)
			}
		}
		assert.Equal(t, fmt.Sprint(n+early), w.attrs["unpinned"], "%s: the snapshot's total", cause)
	}

	gauge, ok := readPinGauge(t, reader, tlsClustersGauge)
	require.True(t, ok, "and still four series, with %d clusters unpinned", n+early)
	assert.Equal(t, byCause(early, n, 0), gauge.unpinned)
	assert.Equal(t, int64(n), unpinnedByCause(t, reader)[cachemetrics.CauseNoNamespaceMetadata])
	assert.Equal(t, int64(early), unpinnedByCause(t, reader)[cachemetrics.CauseTrustDomainUnknown])
}

// pinRegistry is a registry whose HTTP services a test rewrites between loads.
type pinRegistry struct {
	http map[string][]*registryv1.ServiceEndpoint
}

func (r *pinRegistry) registry() *mockRegistry {
	return &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol != registryv1.Service_PROTOCOL_HTTP {
				return map[string][]*registryv1.ServiceEndpoint{}, nil
			}
			return r.http, nil
		},
	}
}

func nsEndpoint(ip, namespace string) *registryv1.ServiceEndpoint {
	e := makeEndpoint(ip, "cluster-1", "node-2", 8080)
	e.KubernetesMetadata.Namespace = namespace
	return e
}

// TestTLSClusterGaugeByReasonThroughAPinAndARemoval is #1425 through the two
// refreshes that change the answer: one that pins a cluster that was unpinned,
// and one that removes a cluster. The gauge is "N clusters unpinned for reason
// R on this node", so it must come DOWN in both, per reason, and a reason
// nothing is unpinned for any more must read zero rather than keep its last
// value. A counter cannot say either.
//
// Each HTTP service is three cluster entries (the default cluster, and its
// mesh-port and default-port aliases), all rendered from one pin.
func TestTLSClusterGaugeByReasonThroughAPinAndARemoval(t *testing.T) {
	const perService = 3
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))

	reg := &pinRegistry{http: map[string][]*registryv1.ServiceEndpoint{
		"demo/web": {nsEndpoint("10.0.0.10", "demo")},
		"demo/a":   {nsEndpoint("10.0.0.20", "")},
		"demo/b":   {nsEndpoint("10.0.0.30", "")},
	}}
	declareDeps(c, "demo/web", "demo/a", "demo/b")
	// One refresh: a registry reload, which sets exactly one snapshot.
	load := func() pinSeries {
		t.Helper()
		rec.reset()
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg.registry()))
		gauge, ok := readPinGauge(t, reader, tlsClustersGauge)
		require.True(t, ok)
		return gauge
	}

	// Two services whose endpoints carry no namespace, one that is pinned.
	gauge := load()
	c.clusterMu.RLock()
	entries := len(c.clusters)
	c.clusterMu.RUnlock()
	require.Equal(t, 3*perService, entries, "fixture: three entries per HTTP service")
	assert.Equal(t, int64(perService), gauge.pinned)
	assert.Equal(t, byCause(0, 2*perService, 0), gauge.unpinned)
	require.Len(t, rec.with(unpinnedClusterMsg), 1)

	// A refresh that PINS one of them: its endpoints now carry a namespace.
	reg.http["demo/a"] = []*registryv1.ServiceEndpoint{nsEndpoint("10.0.0.20", "demo")}
	gauge = load()
	assert.Equal(t, int64(2*perService), gauge.pinned, "the cluster that gained its pin is counted as pinned")
	assert.Equal(t, byCause(0, perService, 0), gauge.unpinned, "and no longer as unpinned")
	warns := rec.with(unpinnedClusterMsg)
	require.Len(t, warns, 1)
	assert.NotContains(t, warns[0].attrs["clusters"], "a.demo.", "a cluster that is pinned now is no longer named")
	assert.Contains(t, warns[0].attrs["clusters"], "b.demo.")

	// The other service's endpoints leave the registry while a pod still
	// declares it. Its cluster is NOT removed: it is retained, empty, for the
	// retention grace, and still published as it was, without a pin. The gauge
	// counts what is published.
	delete(reg.http, "demo/b")
	gauge = load()
	assert.Equal(t, byCause(0, perService, 0), gauge.unpinned, "a retained cluster is still published unpinned")
	require.Len(t, rec.with(unpinnedClusterMsg), 1)

	// A refresh that REMOVES it: nothing on the node depends on it any more.
	declareDeps(c, "demo/web", "demo/a")
	gauge = load()
	c.clusterMu.RLock()
	entries = len(c.clusters)
	c.clusterMu.RUnlock()
	require.Equal(t, 2*perService, entries, "fixture: the service's entries are gone")
	assert.Equal(t, int64(2*perService), gauge.pinned)
	assert.Equal(t, byCause(0, 0, 0), gauge.unpinned,
		"a removed cluster is not unpinned any more, and the reason reads zero: the series stays, at 0")
	assert.Empty(t, rec.with(unpinnedClusterMsg), "nothing is unpinned: no line")

	// The counter, by contrast, only ever went up: 6 + 3 + 3 over the first
	// three snapshots. It says a snapshot went out unpinned, never what is
	// unpinned now.
	assert.Equal(t, int64(4*perService), unpinnedByCause(t, reader)[cachemetrics.CauseNoNamespaceMetadata])
}

// pinFixture is a node with every kind of mesh cluster entry the cache builds,
// each in a pinned and an unpinned form where the kind has a pin at all:
//
//	demo/web      HTTP, endpoints in namespace demo       -> pinned
//	demo/nons     HTTP, endpoints with no namespace        -> unpinned
//	demo/db       TCP (two raw-TCP ports), namespace demo  -> pinned
//	demo/rawnons  TCP, endpoints with no namespace         -> unpinned
//	demo/dns      UDP                                       -> plaintext, no pin
//
// Both TCP services are in the capture TCP set, so their floor clusters are
// published; a local pod of demo/client has dialled both HTTP services over
// QUIC, so each has a `quic:` twin.
func pinFixture(t *testing.T, nodeIdentityServed bool) (*SnapshotCache, *recorder, *sdkmetric.ManualReader) {
	t.Helper()
	c, rec, reader := newBindingTestCache(t)
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "client-0", Namespace: "demo", ServiceAccount: "client",
		NetworkNamespace: "/var/run/netns/cni-client-0",
	}, raceTrustDomain))
	if nodeIdentityServed {
		require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	}

	ep := func(ip, namespace string, port uint32) *registryv1.ServiceEndpoint {
		e := makeEndpoint(ip, "cluster-1", "node-2", port)
		e.KubernetesMetadata.Namespace = namespace
		return e
	}
	db := ep("10.0.0.30", "demo", 9000)
	db.Ports = []uint32{9000, 5432}
	db.PortProtocols = map[uint32]registryv1.PortProtocol{
		9000: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
		5432: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
	}
	byProtocol := map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint{
		registryv1.Service_PROTOCOL_HTTP: {
			"demo/web":  {ep("10.0.0.10", "demo", 8080)},
			"demo/nons": {ep("10.0.0.20", "", 8080)},
		},
		registryv1.Service_PROTOCOL_TCP: {
			"demo/db":      {db},
			"demo/rawnons": {ep("10.0.0.31", "", 9100)},
		},
		registryv1.Service_PROTOCOL_UDP: {
			"demo/dns": {ep("10.0.0.40", "demo", 5353)},
		},
	}
	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if eps, ok := byProtocol[protocol]; ok {
				return eps, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}
	c.SetCaptureAuthorities(map[string]string{
		"demo/web":  "web.demo.svc.cluster.local",
		"demo/nons": "nons.demo.svc.cluster.local",
	})
	c.SetCaptureTCPServices([]capture.CaptureTCPService{
		{ServiceName: "demo/db", ClusterIP: "10.96.0.30", PrimaryIsTCP: true},
		{ServiceName: "demo/rawnons", ClusterIP: "10.96.0.31", PrimaryIsTCP: true},
	})
	declareDeps(c, "demo/web", "demo/nons", "demo/db", "demo/rawnons", "demo/dns")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	if nodeIdentityServed {
		observeQUIC(t, c, "demo/web", "demo/client")
		observeQUIC(t, c, "demo/nons", "demo/client")
	}
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	return c, rec, reader
}

// publishedPins is the build-time gate's reading of one snapshot: the snapshot's
// published clusters, wrapped as a bootstrap, through //agent/test/envoy_validate.
type publishedPins struct {
	all      map[string]*clusterv3.Cluster
	pinned   map[string]struct{} // carries an upstream TLS context, every one pinned
	unpinned map[string]struct{} // carries an upstream TLS context, one not pinned
}

func gateOnSnapshot(t *testing.T, c *SnapshotCache) publishedPins {
	t.Helper()
	snap, err := c.GetSnapshot(c.nodeName)
	require.NoError(t, err)
	out := publishedPins{all: map[string]*clusterv3.Cluster{}, pinned: map[string]struct{}{}, unpinned: map[string]struct{}{}}
	var clusters []*clusterv3.Cluster
	for name, res := range snap.GetResources(resourcev3.ClusterType) {
		cl, ok := res.(*clusterv3.Cluster)
		require.True(t, ok)
		out.all[name] = cl
		clusters = append(clusters, cl)
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].GetName() < clusters[j].GetName() })
	data, err := envoyvalidate.ClustersBootstrapJSON(clusters)
	require.NoError(t, err)

	pinned, unpinned, err := envoyvalidate.MeshClusterPins(data)
	require.NoError(t, err)
	for _, n := range pinned {
		out.pinned[n] = struct{}{}
	}
	for _, n := range unpinned {
		out.unpinned[n] = struct{}{}
	}

	// MeshClusterPins and the gate the validate test runs are one walk; hold
	// that here too, so this test cannot pass against a second definition.
	gate, err := envoyvalidate.UnpinnedMeshClusters(data)
	require.NoError(t, err)
	gateClusters := map[string]struct{}{}
	for _, socket := range gate {
		// The gate names a socket "<cluster>" or "<cluster>/<match>". A cluster
		// name can hold a "/" of its own (a QUIC twin ends in
		// "@<ns>/<service account>"), so the cluster is found among the
		// published names, the longest one the socket starts with.
		name := ""
		for candidate := range out.all {
			if (socket == candidate || strings.HasPrefix(socket, candidate+"/")) && len(candidate) > len(name) {
				name = candidate
			}
		}
		require.NotEmpty(t, name, "the gate names socket %q, which belongs to no published cluster", socket)
		gateClusters[name] = struct{}{}
	}
	require.Equal(t, out.unpinned, gateClusters, "MeshClusterPins disagrees with UnpinnedMeshClusters")
	return out
}

// runtimePins is the runtime report's reading of the same cache, with every
// entry key turned into the name its cluster is published under.
type runtimePins struct {
	pinned    map[string]struct{}
	unpinned  map[string]cachemetrics.UnpinnedCause
	plaintext map[string]struct{}
}

func runtimeReading(c *SnapshotCache) runtimePins {
	out := runtimePins{pinned: map[string]struct{}{}, unpinned: map[string]cachemetrics.UnpinnedCause{}, plaintext: map[string]struct{}{}}
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	for key, entry := range c.clusters {
		name := key
		if entry.cluster != nil {
			name = entry.cluster.GetName() // a default entry is keyed by "<ns>/<svc>"
		}
		switch kind, cause := entry.pinState(); kind {
		case pinPresent:
			out.pinned[name] = struct{}{}
		case pinMissing:
			out.unpinned[name] = cause
		default:
			out.plaintext[name] = struct{}{}
		}
	}
	return out
}

func pinKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRuntimePinReportAgreesWithTheBuildTimeGate holds the two definitions of
// "unpinned" to one answer. #1393 was the two drifting apart: the build-time
// gate (//agent/test/envoy_validate, UnpinnedMeshClusters) reads the published
// protos and was right, the runtime report read the cache entries and named a
// cluster with no TLS at all.
//
// One snapshot, generated by the real cache with every kind of mesh cluster in
// it, is read both ways. Every published cluster with an upstream TLS context
// must be accounted for by the runtime report in the same state the gate finds
// it in, and nothing the gate leaves out may be in the runtime counts.
func TestRuntimePinReportAgreesWithTheBuildTimeGate(t *testing.T) {
	c, rec, reader := pinFixture(t, true)
	gate := gateOnSnapshot(t, c)
	runtime := runtimeReading(c)

	web := proxy.ServiceClusterName("demo/web", c.meshDomain)
	nons := proxy.ServiceClusterName("demo/nons", c.meshDomain)
	db := proxy.TCPClusterName("demo/db", c.meshDomain)
	rawnons := proxy.TCPClusterName("demo/rawnons", c.meshDomain)

	// The fixture has to contain what the test claims to cover.
	for _, name := range []string{web, proxy.PortClusterName("demo/web", c.meshDomain, 8080), db, proxy.TCPPortClusterName(db, 9000), proxy.TCPPortClusterName(db, 5432)} {
		require.Contains(t, gate.pinned, name, "fixture: %s must be published with a pinned TLS context", name)
	}
	for _, name := range []string{nons, proxy.PortClusterName("demo/nons", c.meshDomain, 8080), rawnons, proxy.TCPPortClusterName(rawnons, 9100)} {
		require.Contains(t, gate.unpinned, name, "fixture: %s must be published with an UNPINNED TLS context", name)
	}

	// Fold what the gate sees that is not a cache entry of its own.
	entryPinned, entryUnpinned := map[string]struct{}{}, map[string]struct{}{}
	twins, probes := 0, 0
	fold := func(names map[string]struct{}, into map[string]struct{}, isPinned bool) {
		for name := range names {
			switch {
			case proxy.IsQUICClusterName(name):
				// A QUIC twin has no entry: it is built from its h2 entry's
				// own pin, so it must be in the state its base is in.
				service, _, ok := proxy.ParseQUICClusterName(name, c.meshDomain)
				require.True(t, ok, "twin %s", name)
				base := proxy.ServiceClusterName(service, c.meshDomain)
				if isPinned {
					assert.Contains(t, gate.pinned, base, "twin %s is pinned but its base %s is not", name, base)
				} else {
					assert.Contains(t, gate.unpinned, base, "twin %s is unpinned but its base %s is not", name, base)
				}
				twins++
			case strings.HasPrefix(name, "inboundready_"):
				// A pod's own inbound-readiness probe (#815/#836): its pin is
				// the pod's SPIFFE ID, built in the same call as the cluster.
				assert.True(t, isPinned, "probe cluster %s must be pinned", name)
				probes++
			default:
				into[name] = struct{}{}
			}
		}
	}
	fold(gate.pinned, entryPinned, true)
	fold(gate.unpinned, entryUnpinned, false)
	require.Equal(t, 2, twins, "fixture: one pinned and one unpinned twin")
	require.Equal(t, 1, probes, "fixture: the local pod's probe cluster")

	// The same answer, by name.
	assert.Equal(t, pinKeys(entryUnpinned), pinKeys(runtime.unpinned),
		"the runtime report and the build-time gate disagree on which published TLS clusters have no pin")
	assert.Equal(t, pinKeys(entryPinned), pinKeys(runtime.pinned),
		"the runtime report and the build-time gate disagree on which published TLS clusters are pinned")
	for name, cause := range runtime.unpinned {
		assert.Equal(t, cachemetrics.CauseNoNamespaceMetadata, cause, "%s", name)
	}

	// What has no TLS is in neither runtime count, and the gate leaves it out.
	require.Equal(t, []string{proxy.UDPClusterName("demo/dns", c.meshDomain)}, pinKeys(runtime.plaintext))
	for name := range gate.all {
		_, tlsPinned := gate.pinned[name]
		_, tlsUnpinned := gate.unpinned[name]
		if !tlsPinned && !tlsUnpinned {
			assert.NotContains(t, runtime.pinned, name, "%s carries no upstream TLS context", name)
			assert.NotContains(t, runtime.unpinned, name, "%s carries no upstream TLS context", name)
		}
	}

	// And the three signals carry that one answer: the report, the gauge, the
	// counter and the lines.
	report := c.clusterPinReport()
	assert.Equal(t, len(runtime.pinned), report.counts.Pinned)
	assert.Equal(t, len(runtime.unpinned), report.counts.UnpinnedTotal())
	gotPinned, gotUnpinned, ok := pinGauge(t, reader)
	require.True(t, ok)
	assert.Equal(t, int64(len(entryPinned)), gotPinned)
	assert.Equal(t, int64(len(entryUnpinned)), gotUnpinned)
	warns := warnsByCause(t, rec)
	require.Len(t, warns, 1)
	assert.Equal(t, fmt.Sprint(len(entryUnpinned)), warns[cachemetrics.CauseNoNamespaceMetadata].attrs["count"])
}

// TestRuntimePinReportNeverMissesWhatTheGateFinds covers the states in which
// the node publishes no TLS cluster for its mesh entries at all. The runtime
// report may still name an entry there (the trust-domain window is reported on
// purpose, #832), so the two readings are not equal, but the direction that
// matters holds in every state: nothing the gate finds unpinned is missing
// from the runtime report, and nothing is counted as a pinned TLS cluster
// unless a pinned TLS cluster was published for it.
func TestRuntimePinReportNeverMissesWhatTheGateFinds(t *testing.T) {
	check := func(t *testing.T, c *SnapshotCache) (publishedPins, runtimePins) {
		t.Helper()
		gate := gateOnSnapshot(t, c)
		runtime := runtimeReading(c)
		for name := range gate.unpinned {
			assert.Contains(t, runtime.unpinned, name, "the gate finds %s unpinned and the runtime report does not name it", name)
		}
		for name := range runtime.pinned {
			assert.Contains(t, gate.pinned, name, "the runtime report counts %s as pinned and no pinned TLS cluster was published for it", name)
		}
		return gate, runtime
	}

	t.Run("trust domain unknown", func(t *testing.T) {
		c, rec, reader := pinFixture(t, true)
		c.trustDomain.Store("")
		c.recomputeMTLSClusters()
		rec.reset()
		require.NoError(t, c.generateSnapshot(context.Background()))

		gate, runtime := check(t, c)
		assert.Empty(t, gate.unpinned, "with no trust domain no mesh cluster is published with TLS at all: HTTP clusters go out bare, the TCP floor is withheld")
		assert.Empty(t, runtime.pinned)
		// Every entry that is meant to be mTLS is named, under the one cause.
		c.clusterMu.RLock()
		entries := len(c.clusters)
		c.clusterMu.RUnlock()
		assert.Len(t, runtime.unpinned, entries-1, "every entry but the UDP floor")
		for name, cause := range runtime.unpinned {
			assert.Equal(t, cachemetrics.CauseTrustDomainUnknown, cause, "%s", name)
		}
		warns := warnsByCause(t, rec)
		require.Len(t, warns, 1)
		assert.Equal(t, fmt.Sprint(entries-1), warns[cachemetrics.CauseTrustDomainUnknown].attrs["count"])
		gotPinned, gotUnpinned, _ := pinGauge(t, reader)
		assert.Zero(t, gotPinned)
		assert.Equal(t, int64(entries-1), gotUnpinned)
	})

	t.Run("node SVID not served", func(t *testing.T) {
		c, _, reader := pinFixture(t, false)
		gate, runtime := check(t, c)
		assert.Empty(t, gate.pinned)
		assert.Empty(t, gate.unpinned)
		assert.Empty(t, runtime.pinned, "nothing is published with TLS, so nothing is a pinned TLS cluster")
		gotPinned, _, ok := pinGauge(t, reader)
		require.True(t, ok)
		assert.Zero(t, gotPinned)
	})
}

// TestPinStateByEntryKind states the rule for every kind of cluster entry the
// cache builds: which are meant to be mTLS, and so either carry a pin or are
// reported for lacking one, and which are not.
func TestPinStateByEntryKind(t *testing.T) {
	c, _, _ := pinFixture(t, true)
	db := proxy.TCPClusterName("demo/db", c.meshDomain)
	rawnons := proxy.TCPClusterName("demo/rawnons", c.meshDomain)

	cases := []struct {
		kind, key string
		want      clusterPinKind
		cause     cachemetrics.UnpinnedCause
	}{
		{"HTTP default", "demo/web", pinPresent, ""},
		{"HTTP mesh-port alias", proxy.PortClusterName("demo/web", c.meshDomain, 18081), pinPresent, ""},
		{"HTTP default-port alias", proxy.PortClusterName("demo/web", c.meshDomain, 8080), pinPresent, ""},
		{"HTTP default, no namespace", "demo/nons", pinMissing, cachemetrics.CauseNoNamespaceMetadata},
		{"HTTP alias, no namespace", proxy.PortClusterName("demo/nons", c.meshDomain, 8080), pinMissing, cachemetrics.CauseNoNamespaceMetadata},
		{"TCP floor", db, pinPresent, ""},
		{"TCP primary-port alias", proxy.TCPPortClusterName(db, 9000), pinPresent, ""},
		{"TCP per-port", proxy.TCPPortClusterName(db, 5432), pinPresent, ""},
		{"TCP floor, no namespace", rawnons, pinMissing, cachemetrics.CauseNoNamespaceMetadata},
		{"TCP alias, no namespace", proxy.TCPPortClusterName(rawnons, 9100), pinMissing, cachemetrics.CauseNoNamespaceMetadata},
		{"UDP floor", proxy.UDPClusterName("demo/dns", c.meshDomain), pinNotApplicable, ""},
	}
	c.clusterMu.RLock()
	defer c.clusterMu.RUnlock()
	covered := map[string]struct{}{}
	for _, tc := range cases {
		entry, ok := c.clusters[tc.key]
		require.True(t, ok, "%s: the fixture has no entry %q (have %v)", tc.kind, tc.key, pinKeys(c.clusters))
		kind, cause := entry.pinState()
		assert.Equal(t, tc.want, kind, "%s (%s)", tc.kind, tc.key)
		assert.Equal(t, tc.cause, cause, "%s (%s)", tc.kind, tc.key)
		covered[tc.key] = struct{}{}
	}
	// A QUIC twin is not an entry: it is derived from its h2 entry at snapshot
	// time and shares that entry's pin, so the entry stands for both.
	for key := range c.clusters {
		assert.False(t, proxy.IsQUICClusterName(key), "twin %s must not be a cluster entry", key)
		if _, ok := covered[key]; !ok {
			// The remaining alias of demo/nons; every key must be a kind above.
			assert.Equal(t, proxy.PortClusterName("demo/nons", c.meshDomain, 18081), key, "an entry kind this table does not state the rule for")
		}
	}
}
