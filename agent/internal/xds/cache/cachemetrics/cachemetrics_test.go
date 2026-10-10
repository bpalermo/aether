package cachemetrics

import (
	"context"
	_ "embed"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"aethermesh.dev/common/udspath"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// cachemetricsSource is this package's own source, so the seed-policy test can
// derive the set of registered counters from the registrations themselves
// rather than from a hand-maintained list that the next counter would be
// forgotten from — which is exactly how #882 happened.
//
//go:embed cachemetrics.go
var cachemetricsSource string

func newTestMetrics(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := New(provider.Meter("test"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return m, reader
}

func metricValue(t *testing.T, reader *sdkmetric.ManualReader, name string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				var total int64
				for _, dp := range data.DataPoints {
					total += dp.Value
				}
				return total, true
			case metricdata.Gauge[int64]:
				if len(data.DataPoints) == 0 {
					return 0, false
				}
				return data.DataPoints[len(data.DataPoints)-1].Value, true
			default:
				t.Fatalf("metric %s is %T, want Sum[int64] or Gauge[int64]", name, m.Data)
			}
		}
	}
	return 0, false
}

func TestCacheMetrics_NilReceiverSafe(t *testing.T) {
	var m *Metrics
	m.Generated(context.Background(), 0.01, 1, nil)
	m.Generated(context.Background(), 0.01, 1, errors.New("boom"))
	m.UpstreamTTLRefreshed(context.Background(), 3)
	m.UpstreamsRestored(context.Background(), 3)
	m.ClusterUnpinned(context.Background(), CauseNoNamespaceMetadata, 3)
	m.TLSClusterPins(context.Background(), PinCounts{Pinned: 4})
	m.TLSClusterPinsAcked(context.Background(), PinCounts{Pinned: 4})
	m.TLSClusterPinsAckedUnknown(1)
	m.UDSResolveFailure(context.Background(), "not_csi")
}

// TestCacheMetrics_UpstreamsRestored verifies the restore counter records the
// restored count and nothing on a cold start.
func TestCacheMetrics_UpstreamsRestored(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.UpstreamsRestored(ctx, 0)
	if _, found := metricValue(t, reader, "aether.agent.upstreams.restored"); found {
		t.Error("a cold start must record nothing")
	}

	m.UpstreamsRestored(ctx, 2)
	if got, _ := metricValue(t, reader, "aether.agent.upstreams.restored"); got != 2 {
		t.Errorf("restored = %d, want 2", got)
	}
}

// TestCacheMetrics_UpstreamTTLRefreshed verifies the in-use exemption counter
// sums across prune passes and records nothing when no entry was exempted.
func TestCacheMetrics_UpstreamTTLRefreshed(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.UpstreamTTLRefreshed(ctx, 0)
	if _, found := metricValue(t, reader, "aether.agent.upstreams.ttl_refreshed"); found {
		t.Error("a prune pass that exempted nothing must record nothing")
	}

	m.UpstreamTTLRefreshed(ctx, 2)
	m.UpstreamTTLRefreshed(ctx, 3)
	if got, _ := metricValue(t, reader, "aether.agent.upstreams.ttl_refreshed"); got != 5 {
		t.Errorf("ttl_refreshed = %d, want 5", got)
	}
}

func TestCacheMetrics_GeneratedSuccess(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.Generated(ctx, 0.01, 5, nil)
	m.Generated(ctx, 0.02, 6, nil)

	if got, _ := metricValue(t, reader, "aether.agent.snapshot.builds"); got != 2 {
		t.Errorf("builds = %d, want 2", got)
	}
	if got, _ := metricValue(t, reader, "aether.agent.snapshot.version"); got != 6 {
		t.Errorf("version = %d, want 6", got)
	}
	if _, found := metricValue(t, reader, "aether.agent.snapshot.errors"); found {
		t.Error("errors recorded on success")
	}
}

func TestCacheMetrics_GeneratedFailure(t *testing.T) {
	m, reader := newTestMetrics(t)

	m.Generated(context.Background(), 0.01, 5, errors.New("snapshot rejected"))

	if got, _ := metricValue(t, reader, "aether.agent.snapshot.errors"); got != 1 {
		t.Errorf("errors = %d, want 1", got)
	}
	if _, found := metricValue(t, reader, "aether.agent.snapshot.builds"); found {
		t.Error("builds recorded on failure")
	}
	// The version gauge must not advance on failure: Envoy is still on the
	// previous snapshot.
	if _, found := metricValue(t, reader, "aether.agent.snapshot.version"); found {
		t.Error("version recorded on failure")
	}
}

// seededCounters are the anomaly counters whose healthy value is zero forever.
// Each must exist (at zero) before anything is ever counted; otherwise its
// absence in Prometheus reads as a false zero.
var seededCounters = []string{
	// #1105: a published proto mutated in place, caught by a version-memo
	// audit. Zero forever when every builder honours the rule.
	"aether.agent.snapshot.version_memo_mismatch",
	"aether.agent.identity.outbound_binding_mismatch",
	"aether.agent.identity.inbound_binding_mismatch",
	// #832: a cluster shipped with no SAN pin. Its healthy value is zero
	// forever, which is exactly the value that would never be exported.
	"aether.agent.identity.cluster_unpinned",
	// #796/#717: listener entries dropped for a vanished network namespace.
	"aether.agent.snapshot.stale_netns_skipped",
	// #873/#882: UDPRoute inputs the capture listener cannot represent. This
	// one shipped unseeded and had NO Prometheus series on talos-main rev231,
	// so a dashboard or alert built on it could never fire.
	"aether.agent.l4route.udp_unsupported",
	// #931: a published udp: cluster in which NO endpoint is routable, so
	// udp_proxy discards every datagram for that service. Its healthy value is
	// zero forever, and it is the only signal for that state — the config is
	// valid so there is no NACK, nothing was discarded at projection so
	// udp_unsupported stays quiet, and udp_proxy's own rx counter is per-session
	// and a session needs a host, so it does not move either.
	"aether.agent.l4route.udp_no_healthy_backend",
	// Proposal 039 Phase 2: a local pod whose UDS request resolves to no
	// csi.aether.io socket. Seeded once PER REASON (see
	// TestCacheMetrics_UDSResolveFailuresSeededPerReason).
	"aether.agent.uds.resolve_failures",
}

// countersDeliberatelyNotSeeded are the registered counters that are NOT seeded
// at zero, each with the reason. They count ordinary activity rather than an
// anomaly, so their first increment arrives on its own in a healthy process and
// a pre-increment zero would say nothing a later sample does not.
var countersDeliberatelyNotSeeded = map[string]string{
	"aether.agent.snapshot.builds":            "increments on the first snapshot generation, which every live agent performs",
	"aether.agent.snapshot.errors":            "paired with builds; TestCacheMetrics_GeneratedFailure asserts the absence of a build alongside it",
	"aether.agent.upstreams.miss":             "ODCDS misses are traffic-driven, not an always-on invariant",
	"aether.agent.upstreams.ttl_refreshed":    "prune-pass activity; a zero before the first prune pass is not a health signal",
	"aether.agent.upstreams.restored":         "a cold start legitimately restores nothing, and TestCacheMetrics_UpstreamsRestored asserts that absence",
	"aether.agent.snapshot.resource_versions": "increments on the first snapshot generation (every resource is hashed), which every live agent performs",
}

func TestCacheMetrics_AnomalyCountersSeededAtZero(t *testing.T) {
	_, reader := newTestMetrics(t)
	for _, name := range seededCounters {
		v, ok := metricValue(t, reader, name)
		if !ok {
			t.Errorf("%s not exported before first increment", name)
			continue
		}
		if v != 0 {
			t.Errorf("%s = %d, want 0", name, v)
		}
	}
}

// reasonValues returns the per-reason data points of the named counter.
func reasonValues(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %s is %T, want Sum[int64]", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				v, _ := dp.Attributes.Value(attrReason)
				out[v.AsString()] = dp.Value
			}
		}
	}
	return out
}

// TestCacheMetrics_UDSResolveFailuresSeededPerReason: every reason exists at
// zero before the first failure, so reason="not_csi" == 0 is a live answer
// rather than an absent series, and a failure increments only its own reason.
func TestCacheMetrics_UDSResolveFailuresSeededPerReason(t *testing.T) {
	m, reader := newTestMetrics(t)
	got := reasonValues(t, reader, "aether.agent.uds.resolve_failures")
	if len(got) != len(udspath.Reasons) {
		t.Fatalf("seeded %d reasons (%v), want %d", len(got), got, len(udspath.Reasons))
	}
	for _, r := range udspath.Reasons {
		if v, ok := got[string(r)]; !ok || v != 0 {
			t.Errorf("reason %q = %d (present %v), want a seeded 0", r, v, ok)
		}
	}

	m.UDSResolveFailure(context.Background(), string(udspath.ReasonNotCSI))
	got = reasonValues(t, reader, "aether.agent.uds.resolve_failures")
	if got[string(udspath.ReasonNotCSI)] != 1 || got[string(udspath.ReasonVolumeNotDeclared)] != 0 {
		t.Errorf("after one not_csi failure: %v", got)
	}
}

// TestCacheMetrics_ClusterUnpinnedSeededPerCause: every cause exists at zero
// before anything is counted, so reason="trust_domain_unknown" == 0 is a live
// answer, and an unpinned cluster increments only its own cause (#1424).
func TestCacheMetrics_ClusterUnpinnedSeededPerCause(t *testing.T) {
	const name = "aether.agent.identity.cluster_unpinned"
	m, reader := newTestMetrics(t)
	got := reasonValues(t, reader, name)
	if len(got) != len(UnpinnedCauses) {
		t.Fatalf("seeded %d causes (%v), want %d", len(got), got, len(UnpinnedCauses))
	}
	for _, c := range UnpinnedCauses {
		if v, ok := got[string(c)]; !ok || v != 0 {
			t.Errorf("reason %q = %d (present %v), want a seeded 0", c, v, ok)
		}
	}

	m.ClusterUnpinned(context.Background(), CauseNoNamespaceMetadata, 2)
	m.ClusterUnpinned(context.Background(), CauseTrustDomainUnknown, 0)
	got = reasonValues(t, reader, name)
	if got[string(CauseNoNamespaceMetadata)] != 2 || got[string(CauseTrustDomainUnknown)] != 0 || got[string(CausePinNotRendered)] != 0 {
		t.Errorf("after two no_namespace_metadata clusters: %v", got)
	}
	if len(got) != len(UnpinnedCauses) {
		t.Errorf("the label set grew beyond the closed set: %v", got)
	}
}

// pinValues returns one TLS-cluster gauge's series, keyed "pinned" or
// "unpinned/<reason>", and fails on any attribute other than pin and reason.
func pinValues(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("%s is %T, want Gauge[int64]", name, m.Data)
			}
			for _, dp := range gauge.DataPoints {
				pin, _ := dp.Attributes.Value(attrPin)
				key, want := pin.AsString(), 1
				if reason, ok := dp.Attributes.Value(attrReason); ok {
					key, want = key+"/"+reason.AsString(), 2
				}
				if dp.Attributes.Len() != want {
					t.Errorf("%s data point carries attributes beyond pin and reason: %v", name, dp.Attributes)
				}
				if _, dup := out[key]; dup {
					t.Errorf("%s has two series for %s", name, key)
				}
				out[key] = dp.Value
			}
		}
	}
	return out
}

// TestCacheMetrics_TLSClusterPins: each of the two gauges has exactly five
// series (pinned, and unpinned per cause), all written on every record (a zero
// is a sample, not an absence), and holds the last values rather than a sum
// (#1425). Recording one does not touch the other.
func TestCacheMetrics_TLSClusterPins(t *testing.T) {
	const published, acked = "aether.agent.snapshot.tls_clusters", "aether.agent.xds.acked_tls_clusters"
	m, reader := newTestMetrics(t)
	for _, name := range []string{published, acked} {
		if got := pinValues(t, reader, name); len(got) != 0 {
			t.Fatalf("nothing recorded yet, but %s has %v", name, got)
		}
	}
	want := func(name string, pinned, tdUnknown, noNamespace, notRendered int64) {
		t.Helper()
		got := pinValues(t, reader, name)
		exp := map[string]int64{
			PinPinned: pinned,
			PinUnpinned + "/" + string(CauseTrustDomainUnknown):  tdUnknown,
			PinUnpinned + "/" + string(CauseTLSNotPublished):     0,
			PinUnpinned + "/" + string(CauseNoNamespaceMetadata): noNamespace,
			PinUnpinned + "/" + string(CausePinNotRendered):      notRendered,
		}
		if len(got) != len(exp) {
			t.Fatalf("%s: %d series %v, want exactly %d (zeros are data points)", name, len(got), got, len(exp))
		}
		for k, v := range exp {
			if got[k] != v {
				t.Errorf("%s{%s} = %d, want %d (all: %v)", name, k, got[k], v, got)
			}
		}
	}

	var healthy PinCounts
	healthy.Pinned = 7
	m.TLSClusterPins(context.Background(), healthy)
	want(published, 7, 0, 0, 0)
	if got := pinValues(t, reader, acked); len(got) != 0 {
		t.Fatalf("published is not acknowledged, but %s has %v", acked, got)
	}

	var mixed PinCounts
	mixed.Pinned = 5
	mixed.AddUnpinned(CauseNoNamespaceMetadata)
	mixed.AddUnpinned(CauseNoNamespaceMetadata)
	mixed.AddUnpinned(CauseTrustDomainUnknown)
	if mixed.UnpinnedTotal() != 3 {
		t.Fatalf("UnpinnedTotal() = %d, want 3", mixed.UnpinnedTotal())
	}
	m.TLSClusterPins(context.Background(), mixed)
	want(published, 5, 1, 2, 0)

	m.TLSClusterPinsAcked(context.Background(), mixed)
	want(acked, 5, 1, 2, 0)

	// Back to healthy: every reason reads zero, none keeps its last value.
	m.TLSClusterPins(context.Background(), healthy)
	want(published, 7, 0, 0, 0)
	want(acked, 5, 1, 2, 0)
}

// TestPinCounts_AClosedSet: a cause outside the closed set cannot open a new
// series. It is counted, so the total stays exact, under pin_not_rendered.
func TestPinCounts_AClosedSet(t *testing.T) {
	var p PinCounts
	p.AddUnpinned(UnpinnedCause("some.cluster.name"))
	p.AddUnpinned("")
	if p.UnpinnedTotal() != 2 || p.Unpinned[NumUnpinnedCauses-1] != 2 {
		t.Fatalf("counts = %+v, want both under %s", p, CausePinNotRendered)
	}
	if UnpinnedCauses[NumUnpinnedCauses-1] != CausePinNotRendered {
		t.Fatalf("the fallback slot must be %s", CausePinNotRendered)
	}
}

// TestPinCounts_Promote: the entries counted under one cause move to another
// and the total does not change (#1482: a snapshot that turns out to carry TLS
// reports its tls_not_published entries as the validation gap they are).
func TestPinCounts_Promote(t *testing.T) {
	var p PinCounts
	p.Pinned = 3
	p.AddUnpinned(CauseTLSNotPublished)
	p.AddUnpinned(CauseTLSNotPublished)
	p.AddUnpinned(CauseNoNamespaceMetadata)
	p.AddUnpinned(CauseTrustDomainUnknown)

	p.Promote(CauseTLSNotPublished, CauseNoNamespaceMetadata)

	var want PinCounts
	want.Pinned = 3
	for range 3 {
		want.AddUnpinned(CauseNoNamespaceMetadata)
	}
	want.AddUnpinned(CauseTrustDomainUnknown)
	if p != want {
		t.Fatalf("after Promote: %+v, want %+v", p, want)
	}
	if p.UnpinnedTotal() != 4 {
		t.Fatalf("Promote changed the total: %d, want 4", p.UnpinnedTotal())
	}

	// Move takes some of them back, and never more than there are.
	p.Move(CauseNoNamespaceMetadata, CauseTLSNotPublished, 2)
	if p.Unpinned != [NumUnpinnedCauses]int{1, 2, 1, 0} {
		t.Fatalf("after Move(2): %+v", p)
	}
	p.Move(CauseTLSNotPublished, CauseNoNamespaceMetadata, 5)
	if p != want {
		t.Fatalf("after Move(5) of 2: %+v, want %+v", p, want)
	}

	// A cause outside the closed set moves nothing.
	p.Promote("some.cluster.name", CauseNoNamespaceMetadata)
	p.Promote(CauseTrustDomainUnknown, "some.cluster.name")
	if p != want {
		t.Fatalf("a Promote naming an unknown cause changed the counts: %+v", p)
	}
}

// registeredCounterNames parses this package's own source and returns the
// metric name of every meter.Int64Counter registration in it.
func registeredCounterNames(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "cachemetrics.go", cachemetricsSource, 0)
	if err != nil {
		t.Fatalf("parsing cachemetrics.go: %v", err)
	}
	var names []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Int64Counter" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("Int64Counter call with a non-literal name: %#v", call.Args[0])
			return true
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("unquoting %s: %v", lit.Value, err)
		}
		names = append(names, name)
		return true
	})
	return names
}

// TestEveryRegisteredCounterDeclaresItsSeedPolicy is the gate #882 asks for: it
// derives the registered counters from the source rather than from a list, so a
// counter added to New() without either a zero seed or a written reason not to
// seed it fails here. Two counters have now been registered without a seed
// (#638 on rev200, #873 on rev231) and both were only caught in production, by
// querying Prometheus and finding no series.
func TestEveryRegisteredCounterDeclaresItsSeedPolicy(t *testing.T) {
	registered := registeredCounterNames(t)

	// Neither list may name a counter that is no longer registered: a stale
	// entry would offset the count check below and let a real omission through.
	isRegistered := make(map[string]bool, len(registered))
	for _, name := range registered {
		isRegistered[name] = true
	}
	for _, name := range seededCounters {
		if !isRegistered[name] {
			t.Errorf("seededCounters names %s, which New() does not register", name)
		}
	}
	for name := range countersDeliberatelyNotSeeded {
		if !isRegistered[name] {
			t.Errorf("countersDeliberatelyNotSeeded names %s, which New() does not register", name)
		}
	}

	_, reader := newTestMetrics(t)
	for _, name := range registered {
		_, exported := metricValue(t, reader, name)
		reason, unseeded := countersDeliberatelyNotSeeded[name]
		switch {
		case exported && unseeded:
			t.Errorf("%s is exported before its first increment but is listed as deliberately not seeded (%q); pick one", name, reason)
		case !exported && !unseeded:
			t.Errorf("%s is registered but has no data point before its first increment: seed it at zero in New(), "+
				"or add it to countersDeliberatelyNotSeeded with the reason", name)
		}
	}

	// Anti-vacuity, checked last so the per-counter diagnosis above comes
	// first: a parse that matched nothing — or that stopped matching because
	// the registration shape changed — would satisfy every assertion above
	// without checking anything. That is the #853 failure mode, and this
	// assertion is why the green run proves the parse really reads the
	// registrations.
	if want := len(seededCounters) + len(countersDeliberatelyNotSeeded); len(registered) != want {
		t.Fatalf("parsed %d Int64Counter registrations (%v), want %d; update seededCounters or countersDeliberatelyNotSeeded",
			len(registered), registered, want)
	}
}

// TestAckedTLSClusters_WithdrawnWhileUnknown: the acknowledged gauge has to be
// able to stop. A recorded gauge exports its last values for the life of the
// process; this one is observed, so that while the acknowledged state is not
// known it has no sample at all, and it has one again when it is set.
func TestAckedTLSClusters_WithdrawnWhileUnknown(t *testing.T) {
	const name = "aether.agent.xds.acked_tls_clusters"
	m, reader := newTestMetrics(t)
	points := func() int {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, metric := range sm.Metrics {
				if metric.Name == name {
					return len(metric.Data.(metricdata.Gauge[int64]).DataPoints)
				}
			}
		}
		return 0
	}

	if n := points(); n != 0 {
		t.Fatalf("before any acknowledgement: %d data points, want none", n)
	}
	m.TLSClusterPinsAcked(context.Background(), PinCounts{Pinned: 4})
	if n := points(); n != 1+NumUnpinnedCauses {
		t.Fatalf("set: %d data points, want %d", n, 1+NumUnpinnedCauses)
	}
	if n := points(); n != 1+NumUnpinnedCauses {
		t.Fatalf("unchanged at the next collection: %d data points, want %d", n, 1+NumUnpinnedCauses)
	}
	m.TLSClusterPinsAckedUnknown(2)
	if n := points(); n != 0 {
		t.Fatalf("withdrawn: %d data points, want none", n)
	}
	m.TLSClusterPinsAcked(context.Background(), PinCounts{})
	if n := points(); n != 1+NumUnpinnedCauses {
		t.Fatalf("set again, zeros included: %d data points, want %d", n, 1+NumUnpinnedCauses)
	}
}

// TestAckedTLSClustersUnknown_IsTheOtherHalfOfTheAckedGauge: a withdrawn
// acknowledged gauge is an absence, and no rule matches an absence (#1509).
// The unknown gauge is the sample for it: absent until the acknowledged state
// was first settled either way, the number of clusters the agent cannot place
// while the acknowledged gauge is withdrawn, and zero, not absent, while it is
// written.
func TestAckedTLSClustersUnknown_IsTheOtherHalfOfTheAckedGauge(t *testing.T) {
	const name = "aether.agent.xds.acked_tls_clusters_unknown"
	m, reader := newTestMetrics(t)
	unknown := func() (int64, bool) {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, metric := range sm.Metrics {
				if metric.Name != name {
					continue
				}
				points := metric.Data.(metricdata.Gauge[int64]).DataPoints
				switch len(points) {
				case 0:
					return 0, false
				case 1:
					if n := points[0].Attributes.Len(); n != 0 {
						t.Fatalf("%s carries %d attributes, want none: %v", name, n, points[0].Attributes)
					}
					return points[0].Value, true
				default:
					t.Fatalf("%s has %d data points, want one", name, len(points))
				}
			}
		}
		return 0, false
	}
	expect := func(step string, want int64, wantOK bool) {
		t.Helper()
		if got, ok := unknown(); got != want || ok != wantOK {
			t.Fatalf("%s: got (%d, %t), want (%d, %t)", step, got, ok, want, wantOK)
		}
	}

	expect("before any answer", 0, false)
	m.TLSClusterPinsAckedUnknown(3)
	expect("withdrawn first", 3, true)
	m.TLSClusterPinsAcked(context.Background(), PinCounts{Pinned: 4})
	expect("known", 0, true)
	m.TLSClusterPinsAckedUnknown(1)
	expect("withdrawn after having been known", 1, true)
	m.TLSClusterPinsAcked(context.Background(), PinCounts{})
	expect("known again", 0, true)
}
