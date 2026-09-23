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
	m.ClusterUnpinned(context.Background(), 3)
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
}

// countersDeliberatelyNotSeeded are the registered counters that are NOT seeded
// at zero, each with the reason. They count ordinary activity rather than an
// anomaly, so their first increment arrives on its own in a healthy process and
// a pre-increment zero would say nothing a later sample does not.
var countersDeliberatelyNotSeeded = map[string]string{
	"aether.agent.snapshot.builds":         "increments on the first snapshot generation, which every live agent performs",
	"aether.agent.snapshot.errors":         "paired with builds; TestCacheMetrics_GeneratedFailure asserts the absence of a build alongside it",
	"aether.agent.upstreams.miss":          "ODCDS misses are traffic-driven, not an always-on invariant",
	"aether.agent.upstreams.ttl_refreshed": "prune-pass activity; a zero before the first prune pass is not a health signal",
	"aether.agent.upstreams.restored":      "a cold start legitimately restores nothing, and TestCacheMetrics_UpstreamsRestored asserts that absence",
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
