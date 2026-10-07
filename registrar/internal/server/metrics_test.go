package server

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/snapshotversion"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectSum returns the summed int64 counter value for the named metric, or 0
// if the metric was never recorded.
func collectSum(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	var total int64
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
				total += dp.Value
			}
		}
	}
	return total
}

// collectGauge returns the last recorded int64 gauge value for the named
// metric. Fails the test if the metric was never recorded.
func collectGauge(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
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
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("metric %s is %T, want Gauge[int64]", name, m.Data)
			}
			if len(gauge.DataPoints) == 0 {
				t.Fatalf("metric %s has no data points", name)
			}
			return gauge.DataPoints[len(gauge.DataPoints)-1].Value
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func newTestMetrics(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := NewMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatalf("NewMetrics() error = %v", err)
	}
	return m, reader
}

func TestMetrics_NilReceiverSafe(t *testing.T) {
	var m *Metrics
	ctx := context.Background()
	m.watcherSubscribed(ctx)
	m.watcherUnsubscribed(ctx)
	m.eventsBroadcast(ctx, map[registrarv1.WatchEndpointsResponse_EventType]int64{
		registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED: 1,
	})
	m.eventDropped(ctx, "EVENT_TYPE_ENDPOINT_ADDED")
	m.syncCompleted(ctx, 0.1, State{Generation: 1}, map[string]int{"EVENT_TYPE_ENDPOINT_ADDED": 1})
	m.syncFailed(ctx, 0.1)
	m.snapshotState(ctx, State{Generation: 7})
	m.watchStarted(ctx, ResumeResend, false)
	m.watchStarted(ctx, ResumeCurrent, true)
	if err := m.ObserveSnapshot(NewSnapshot(), nil); err != nil {
		t.Errorf("ObserveSnapshot on nil metrics = %v, want nil", err)
	}
}

func TestBroadcaster_DropIncrementsCounter(t *testing.T) {
	m, reader := newTestMetrics(t)
	b := NewBroadcaster(slog.New(slog.DiscardHandler), m)

	ch := b.Subscribe("slow-watcher", nil)
	_ = ch // never drained: fills to capacity, then drops

	// Fill the buffer, then overflow by 3.
	overflow := 3
	events := make([]*registrarv1.WatchEndpointsResponse, 0, defaultChannelBuffer+overflow)
	for i := 0; i < defaultChannelBuffer+overflow; i++ {
		events = append(events, &registrarv1.WatchEndpointsResponse{
			Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
			ServiceName: fmt.Sprintf("svc-%d", i),
		})
	}
	b.Broadcast(events)

	if got := collectSum(t, reader, "aether.registrar.broadcast.dropped_events"); got != int64(overflow) {
		t.Errorf("dropped_events = %d, want %d", got, overflow)
	}
	if got := collectSum(t, reader, "aether.registrar.broadcast.events"); got != int64(defaultChannelBuffer) {
		t.Errorf("broadcast.events = %d, want %d", got, defaultChannelBuffer)
	}
}

func TestBroadcaster_WatcherCountMetric(t *testing.T) {
	m, reader := newTestMetrics(t)
	b := NewBroadcaster(slog.New(slog.DiscardHandler), m)

	ch1 := b.Subscribe("a", nil)
	b.Subscribe("b", nil)
	if got := collectSum(t, reader, "aether.registrar.watchers"); got != 2 {
		t.Errorf("watchers after subscribes = %d, want 2", got)
	}

	// Reconnect with the same ID: count must not grow.
	b.Subscribe("a", nil)
	if got := collectSum(t, reader, "aether.registrar.watchers"); got != 2 {
		t.Errorf("watchers after reconnect = %d, want 2", got)
	}

	// A stale unsubscribe (old channel) must not decrement.
	b.Unsubscribe("a", ch1)
	if got := collectSum(t, reader, "aether.registrar.watchers"); got != 2 {
		t.Errorf("watchers after stale unsubscribe = %d, want 2", got)
	}
}

func TestMetrics_SyncCompletedRecordsVersionAndEvents(t *testing.T) {
	m, reader := newTestMetrics(t)
	ctx := context.Background()

	m.syncCompleted(ctx, 0.05, State{Generation: 42}, map[string]int{
		"EVENT_TYPE_ENDPOINT_ADDED":   2,
		"EVENT_TYPE_ENDPOINT_REMOVED": 1,
	})

	if got := collectGauge(t, reader, "aether.registrar.snapshot.version"); got != 42 {
		t.Errorf("snapshot.version = %d, want 42", got)
	}
	if got := collectSum(t, reader, "aether.registrar.sync.events"); got != 3 {
		t.Errorf("sync.events = %d, want 3", got)
	}
}

// TestMetrics_VersionAdvanced: the RPC write path records the snapshot
// generation (the content-change count since #1193), not a parsed version.
func TestMetrics_VersionAdvanced(t *testing.T) {
	m, reader := newTestMetrics(t)
	m.snapshotState(context.Background(), State{Generation: 17, Version: "hash:0123456789abcdef"})
	if got := collectGauge(t, reader, "aether.registrar.snapshot.version"); got != 17 {
		t.Errorf("snapshot.version = %d, want 17", got)
	}
	// A version that is not a number is irrelevant: the generation is recorded.
	m.snapshotState(context.Background(), State{Generation: 17, Version: "41+0123456789abcdef"})
	if got := collectGauge(t, reader, "aether.registrar.snapshot.version"); got != 17 {
		t.Errorf("snapshot.version after a dirty version = %d, want 17", got)
	}
}

// fakeRevisioned is a registry.RevisionedLister that only reports a store
// revision.
type fakeRevisioned struct{ rev int64 }

func (f *fakeRevisioned) ListAllEndpointsRevisioned(context.Context, []registryv1.Service_Protocol) (map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, int64, error) {
	return nil, f.rev, nil
}
func (f *fakeRevisioned) StoreRevision() int64 { return f.rev }

// collectGaugePoints returns every data point of the named int64 gauge.
func collectGaugePoints(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.DataPoint[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m.Data.(metricdata.Gauge[int64]).DataPoints
			}
		}
	}
	return nil
}

// TestMetrics_ObserveSnapshot: the revision gauges report the store head and
// the served listing's revision, and the content info gauge exports exactly one
// series -- the current hash -- however often the contents change (#1193).
func TestMetrics_ObserveSnapshot(t *testing.T) {
	m, reader := newTestMetrics(t)
	snap := NewSnapshot()
	store := &fakeRevisioned{rev: 12}
	if err := m.ObserveSnapshot(snap, store); err != nil {
		t.Fatalf("ObserveSnapshot() = %v", err)
	}

	snap.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {"10.0.0.1"}}), Origin{Revision: 10})
	if got := collectGauge(t, reader, "aether.registrar.store_revision"); got != 12 {
		t.Errorf("store_revision = %d, want 12", got)
	}
	if got := collectGauge(t, reader, "aether.registrar.snapshot_revision"); got != 10 {
		t.Errorf("snapshot_revision = %d, want 10", got)
	}

	for i, ip := range []string{"10.0.0.2", "10.0.0.3", "10.0.0.4"} {
		snap.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {ip}}), Origin{Revision: int64(11 + i)})
	}
	points := collectGaugePoints(t, reader, "aether.registrar.snapshot.content")
	if len(points) != 1 {
		t.Fatalf("snapshot.content series = %d, want exactly 1", len(points))
	}
	hash, _ := points[0].Attributes.Value(attrContentHashLabel)
	if got, want := hash.AsString(), snap.State().ContentHash; got != want {
		t.Errorf("content_hash = %q, want the current %q", got, want)
	}
}

const contentHashGauge = "aether.registrar.snapshot.content_hash"

// wantHashValue is the value the content-hash gauge must report for st: the
// first 13 hex digits of the hash, checked here against an independent
// rendering (%013x) rather than against the function under test.
func wantHashValue(t *testing.T, st State) int64 {
	t.Helper()
	v, ok := snapshotversion.ContentHashValue(st.ContentHash)
	if !ok {
		t.Fatalf("ContentHashValue(%q) not ok", st.ContentHash)
	}
	if got, want := fmt.Sprintf("%013x", v), st.ContentHash[:13]; got != want {
		t.Fatalf("value %d renders as %s, want the hash prefix %s", v, got, want)
	}
	if v < 0 || v >= 1<<52 || int64(float64(v)) != v {
		t.Fatalf("value %d is not a non-negative 52-bit integer exact in a float64", v)
	}
	return v
}

// TestMetrics_ContentHashGauge: the hash is exported as the VALUE of one
// label-free series per replica (#1329). A content change moves the value of
// that series instead of starting another, and returning to earlier contents
// returns to the earlier value.
func TestMetrics_ContentHashGauge(t *testing.T) {
	m, reader := newTestMetrics(t)
	snap := NewSnapshot()
	if err := m.ObserveSnapshot(snap, &fakeRevisioned{rev: 12}); err != nil {
		t.Fatalf("ObserveSnapshot() = %v", err)
	}

	seen := map[int64]string{}
	var first int64
	for i, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.1"} {
		snap.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {ip}}), Origin{Revision: int64(10 + i)})
		points := collectGaugePoints(t, reader, contentHashGauge)
		if len(points) != 1 {
			t.Fatalf("step %d: content_hash series = %d, want exactly 1", i, len(points))
		}
		if n := points[0].Attributes.Len(); n != 0 {
			t.Errorf("step %d: content_hash carries %d attributes, want none (a label would go stale)", i, n)
		}
		got := points[0].Value
		if want := wantHashValue(t, snap.State()); got != want {
			t.Errorf("step %d: content_hash = %d, want %d", i, got, want)
		}
		if i == 0 {
			first = got
		}
		if prev, dup := seen[got]; dup && prev != ip {
			t.Errorf("step %d: contents %s and %s report the same value %d", i, prev, ip, got)
		}
		seen[got] = ip
	}
	if got := collectGauge(t, reader, contentHashGauge); got != first {
		t.Errorf("content_hash after returning to the first contents = %d, want %d", got, first)
	}
	if len(seen) != 3 {
		t.Errorf("distinct values = %d, want 3 (one per distinct endpoint set)", len(seen))
	}
	// The deprecated labelled gauge is still exported next to it for one release.
	if got := collectGaugePoints(t, reader, "aether.registrar.snapshot.content"); len(got) != 1 {
		t.Errorf("deprecated snapshot.content series = %d, want 1", len(got))
	}
}

// TestMetrics_ContentHashGaugeAcrossReplicas: two replicas (two snapshots, two
// meter providers) report the SAME value for the same contents -- whatever
// order they learned them in -- and different values for different contents.
// That equality is what the divergence rule compares.
func TestMetrics_ContentHashGaugeAcrossReplicas(t *testing.T) {
	ma, ra := newTestMetrics(t)
	mb, rb := newTestMetrics(t)
	a, b := NewSnapshot(), NewSnapshot()
	if err := ma.ObserveSnapshot(a, &fakeRevisioned{rev: 9}); err != nil {
		t.Fatalf("ObserveSnapshot(a) = %v", err)
	}
	if err := mb.ObserveSnapshot(b, &fakeRevisioned{rev: 9}); err != nil {
		t.Fatalf("ObserveSnapshot(b) = %v", err)
	}

	// Both empty: equal already.
	if va, vb := collectGauge(t, ra, contentHashGauge), collectGauge(t, rb, contentHashGauge); va != vb {
		t.Errorf("empty snapshots: %d != %d", va, vb)
	}

	a.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {"10.0.0.1", "10.0.0.2"}, "ns/b": {"10.0.1.1"}}), Origin{Revision: 7})
	// b reaches the same contents by another path and another input order.
	b.DiffAndReplaceAt(listing(map[string][]string{"ns/b": {"10.0.1.9"}}), Origin{Revision: 5})
	b.DiffAndReplaceAt(listing(map[string][]string{"ns/b": {"10.0.1.1"}, "ns/a": {"10.0.0.2", "10.0.0.1"}}), Origin{Revision: 7})
	va, vb := collectGauge(t, ra, contentHashGauge), collectGauge(t, rb, contentHashGauge)
	if va != vb {
		t.Errorf("same contents: replica a = %d, replica b = %d", va, vb)
	}
	if want := wantHashValue(t, a.State()); va != want {
		t.Errorf("replica a = %d, want %d", va, want)
	}

	// Same revision, different contents: the divergence the rule fires on.
	b.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {"10.0.0.1"}, "ns/b": {"10.0.1.1"}}), Origin{Revision: 7})
	if got := collectGauge(t, rb, "aether.registrar.snapshot_revision"); got != 7 {
		t.Fatalf("replica b snapshot_revision = %d, want 7", got)
	}
	if vb := collectGauge(t, rb, contentHashGauge); vb == va {
		t.Errorf("different contents at one revision report the same value %d", va)
	}
}

// TestMetrics_ContentHashGaugeDirty: a snapshot that deviates from its listing
// (version "<rev>+<hash>": an RPC applied since the last sync) reports the hash
// of what it serves NOW, at the unchanged revision. The rule excludes that
// state by the pending write-behind intent, and `for:` covers the rest.
func TestMetrics_ContentHashGaugeDirty(t *testing.T) {
	m, reader := newTestMetrics(t)
	snap := NewSnapshot()
	if err := m.ObserveSnapshot(snap, &fakeRevisioned{rev: 3}); err != nil {
		t.Fatalf("ObserveSnapshot() = %v", err)
	}
	snap.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {"10.0.0.1"}}), Origin{Revision: 3})
	clean := collectGauge(t, reader, contentHashGauge)

	snap.Apply([]*registrarv1.WatchEndpointsResponse{{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "ns/a",
		Protocol:    registryv1.Service_PROTOCOL_HTTP,
		Endpoint:    &registryv1.ServiceEndpoint{Ip: "10.0.0.2", Port: 8080},
	}})
	st := snap.State()
	if !st.Dirty || st.Version != "3+"+st.ContentHash {
		t.Fatalf("state = %+v, want the dirty form at revision 3", st)
	}
	dirty := collectGauge(t, reader, contentHashGauge)
	if dirty == clean {
		t.Errorf("content_hash did not move with the applied endpoint (%d)", dirty)
	}
	if want := wantHashValue(t, st); dirty != want {
		t.Errorf("dirty content_hash = %d, want %d", dirty, want)
	}
	// The version's hash part, read through the shared parser, is the same name.
	h, ok := snapshotversion.ContentHash(st.Version)
	if v, vok := snapshotversion.ContentHashValue(h); !ok || !vok || v != dirty {
		t.Errorf("ContentHashValue(ContentHash(%q)) = %d, %v; want %d", st.Version, v, ok && vok, dirty)
	}
	if got := collectGauge(t, reader, "aether.registrar.snapshot_revision"); got != 3 {
		t.Errorf("snapshot_revision = %d, want 3", got)
	}
}

// TestMetrics_ObserveSnapshotWithoutRevisions: a backend without revisions
// (kubernetes) reports the content hash and no revision gauges.
func TestMetrics_ObserveSnapshotWithoutRevisions(t *testing.T) {
	m, reader := newTestMetrics(t)
	snap := NewSnapshot()
	if err := m.ObserveSnapshot(snap, nil); err != nil {
		t.Fatalf("ObserveSnapshot() = %v", err)
	}
	snap.DiffAndReplace(listing(map[string][]string{"ns/a": {"10.0.0.1"}}))
	if got := collectGaugePoints(t, reader, "aether.registrar.snapshot_revision"); len(got) != 0 {
		t.Errorf("snapshot_revision reported %d points without a revisioned backend", len(got))
	}
	if got := collectGaugePoints(t, reader, "aether.registrar.snapshot.content"); len(got) != 1 {
		t.Errorf("snapshot.content series = %d, want 1", len(got))
	}
	// The value gauge is reported without a revision too (version "hash:<hash>"):
	// it names the contents. Nothing compares it there, because the divergence
	// rule joins on snapshot_revision, which this backend does not report.
	st := snap.State()
	if st.Version != snapshotversion.HashPrefix+st.ContentHash {
		t.Fatalf("version = %q, want the hash: form", st.Version)
	}
	points := collectGaugePoints(t, reader, contentHashGauge)
	if len(points) != 1 {
		t.Fatalf("content_hash series = %d, want 1", len(points))
	}
	if got, want := points[0].Value, wantHashValue(t, st); got != want {
		t.Errorf("content_hash = %d, want %d", got, want)
	}
	// Two such replicas with the same contents still agree.
	m2, reader2 := newTestMetrics(t)
	snap2 := NewSnapshot()
	if err := m2.ObserveSnapshot(snap2, nil); err != nil {
		t.Fatalf("ObserveSnapshot() = %v", err)
	}
	snap2.DiffAndReplace(listing(map[string][]string{"ns/a": {"10.0.0.1"}}))
	if got := collectGauge(t, reader2, contentHashGauge); got != points[0].Value {
		t.Errorf("second replica content_hash = %d, want %d", got, points[0].Value)
	}
}

// TestMetrics_WatchStarts: every watch start is counted once, labelled by
// whether it resent the snapshot.
func TestMetrics_WatchStarts(t *testing.T) {
	m, reader := newTestMetrics(t)
	snap := NewSnapshot()
	snap.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {"10.0.0.1"}}), Origin{Revision: 4})
	s := NewRegistrarServer(&flakyRegistry{}, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", slog.New(slog.DiscardHandler), m)
	synced := make(chan struct{})
	close(synced)
	s.GateOnSync(synced)

	reconnect(t, s, "")
	reconnect(t, s, snap.Version())
	reconnect(t, s, snap.Version())
	reconnect(t, s, "3."+snap.State().ContentHash) // same contents, older name

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "aether.registrar.watch.starts" {
				continue
			}
			for _, dp := range mt.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attrResume)
				got[v.AsString()] += dp.Value
			}
		}
	}
	if got["resent"] != 1 || got["current"] != 2 || got["renamed"] != 1 {
		t.Errorf("watch.starts = %v, want resent=1 current=2 renamed=1", got)
	}
}
