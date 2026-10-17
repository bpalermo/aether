package servicerange

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// fakeSink records every range it is handed.
type fakeSink struct {
	calls [][]netip.Prefix
}

func (s *fakeSink) SetServiceRanges(r []netip.Prefix) { s.calls = append(s.calls, r) }

func (s *fakeSink) last(t *testing.T) []netip.Prefix {
	t.Helper()
	require.NotEmpty(t, s.calls, "the sink was never called")
	return s.calls[len(s.calls)-1]
}

func prefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func serviceCIDR(name string, cidrs ...string) *networkingv1.ServiceCIDR {
	return &networkingv1.ServiceCIDR{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       networkingv1.ServiceCIDRSpec{CIDRs: cidrs},
	}
}

// harness is a reconciler over a fake client, with its log and gauge captured.
type harness struct {
	r      *Reconciler
	sink   *fakeSink
	logs   *bytes.Buffer
	reader *sdkmetric.ManualReader
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	h := &harness{sink: &fakeSink{}, logs: &bytes.Buffer{}, reader: sdkmetric.NewManualReader()}
	st, err := newState(slog.New(slog.NewTextHandler(h.logs, nil)),
		sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader)).Meter("test"))
	require.NoError(t, err)
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	h.r = &Reconciler{Client: c, Sink: h.sink, state: st}
	return h
}

func (h *harness) reconcile(t *testing.T) []netip.Prefix {
	t.Helper()
	_, err := h.r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	return h.sink.last(t)
}

func (h *harness) warns() int {
	return strings.Count(h.logs.String(), "level=WARN")
}

// gauge reads aether.agent.service_range.known; ok is false when it was not exported.
func gauge(t *testing.T, reader *sdkmetric.ManualReader) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aether.agent.service_range.known" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			require.True(t, ok, "known is not an int64 gauge: %T", m.Data)
			require.Len(t, g.DataPoints, 1)
			return g.DataPoints[0].Value, true
		}
	}
	return 0, false
}

func TestCanonicalize(t *testing.T) {
	got := Canonicalize(prefixes(t,
		"10.96.0.1/12", // host bits set: masked
		"10.0.0.0/16",
		"fd00::/108",   // IPv6: dropped
		"10.96.0.0/12", // duplicate once masked
		"10.0.0.0/8",
	))
	assert.Equal(t, prefixes(t, "10.0.0.0/8", "10.0.0.0/16", "10.96.0.0/12"), got)

	assert.Nil(t, Canonicalize(nil))
	assert.Nil(t, Canonicalize(prefixes(t, "fd00::/108")), "an IPv6-only range is no range")
	assert.Nil(t, Canonicalize([]netip.Prefix{{}}), "the zero prefix is not a range")
}

func TestParseCIDRs(t *testing.T) {
	got, v6, err := ParseCIDRs([]string{" 10.96.0.0/12", "fd00::/108", "10.200.0.0/16"})
	require.NoError(t, err)
	assert.Equal(t, prefixes(t, "10.96.0.0/12", "10.200.0.0/16"), got)
	assert.Equal(t, 1, v6)

	for _, bad := range []string{"10.96.0.0", "10.96.0.0/33", "not-a-cidr", ""} {
		_, _, err := ParseCIDRs([]string{"10.96.0.0/12", bad})
		assert.Error(t, err, "%q must be rejected", bad)
	}
}

func TestReconcile_OneServiceCIDR(t *testing.T) {
	h := newHarness(t, serviceCIDR("kubernetes", "10.96.0.0/12"))
	assert.Equal(t, prefixes(t, "10.96.0.0/12"), h.reconcile(t))
	v, ok := gauge(t, h.reader)
	require.True(t, ok)
	assert.Equal(t, int64(1), v)
	assert.Zero(t, h.warns())
}

func TestReconcile_TwoServiceCIDRsAreMerged(t *testing.T) {
	h := newHarness(t,
		serviceCIDR("kubernetes", "10.96.0.0/12"),
		serviceCIDR("extra", "10.0.0.0/24"),
	)
	assert.Equal(t, prefixes(t, "10.0.0.0/24", "10.96.0.0/12"), h.reconcile(t))
}

func TestReconcile_DualStackKeepsIPv4(t *testing.T) {
	h := newHarness(t, serviceCIDR("kubernetes", "10.96.0.0/12", "fd00:10:96::/108"))
	assert.Equal(t, prefixes(t, "10.96.0.0/12"), h.reconcile(t))
}

func TestReconcile_IPv6OnlyIsNoRange(t *testing.T) {
	h := newHarness(t, serviceCIDR("kubernetes", "fd00:10:96::/108"))
	assert.Nil(t, h.reconcile(t))
	v, ok := gauge(t, h.reader)
	require.True(t, ok)
	assert.Equal(t, int64(0), v)
	assert.Equal(t, 1, h.warns())
}

// The WARN is logged once per loss of the range, not once per reconcile.
func TestReconcile_OneWarnPerLoss(t *testing.T) {
	h := newHarness(t)
	assert.Nil(t, h.reconcile(t))
	assert.Nil(t, h.reconcile(t))
	assert.Equal(t, 1, h.warns(), "a range that stays unknown warns once")

	obj := serviceCIDR("kubernetes", "10.96.0.0/12")
	require.NoError(t, h.r.Create(context.Background(), obj))
	assert.NotNil(t, h.reconcile(t))
	require.NoError(t, h.r.Delete(context.Background(), obj))
	assert.Nil(t, h.reconcile(t))
	assert.Equal(t, 2, h.warns(), "a range lost again warns again")
}

func TestReconcile_DeletedServiceCIDRLeavesTheRange(t *testing.T) {
	extra := serviceCIDR("extra", "10.0.0.0/24")
	h := newHarness(t, serviceCIDR("kubernetes", "10.96.0.0/12"), extra)
	require.Len(t, h.reconcile(t), 2)

	require.NoError(t, h.r.Delete(context.Background(), extra))
	assert.Equal(t, prefixes(t, "10.96.0.0/12"), h.reconcile(t))
}

// A ServiceCIDR being deleted can still hold live ClusterIPs: the apiserver
// keeps its finalizer until none is left uncovered. Its block stays in the
// range until the object is gone.
func TestReconcile_TerminatingServiceCIDRStays(t *testing.T) {
	extra := serviceCIDR("extra", "10.0.0.0/24")
	extra.Finalizers = []string{"networking.k8s.io/service-cidr-finalizer"}
	extra.Status.Conditions = []metav1.Condition{{
		Type: networkingv1.ServiceCIDRConditionReady, Status: metav1.ConditionFalse,
		Reason: networkingv1.ServiceCIDRReasonTerminating, LastTransitionTime: metav1.Now(),
	}}
	h := newHarness(t, serviceCIDR("kubernetes", "10.96.0.0/12"), extra)
	require.NoError(t, h.r.Delete(context.Background(), extra)) // sets deletionTimestamp only

	got := &networkingv1.ServiceCIDR{}
	require.NoError(t, h.r.Get(context.Background(), client.ObjectKeyFromObject(extra), got))
	require.NotNil(t, got.DeletionTimestamp, "the fake client did not mark it terminating")
	assert.Equal(t, prefixes(t, "10.0.0.0/24", "10.96.0.0/12"), h.reconcile(t))
}

// servedMapper serves ServiceCIDR, as Kubernetes >= 1.33 does.
func servedMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(networkingv1.SchemeGroupVersion.WithKind("ServiceCIDR"), meta.RESTScopeRoot)
	return m
}

func setupOpts(sink *fakeSink, logs *bytes.Buffer, reader *sdkmetric.ManualReader, override ...string) Options {
	return Options{
		Override: override,
		Sink:     sink,
		Log:      slog.New(slog.NewTextHandler(logs, nil)),
		Meter:    sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"),
	}
}

// The override wins over discovery: the range is the flag's, and no reconciler
// is registered even where the API is served.
func TestSetup_OverrideWins(t *testing.T) {
	sink, logs, reader := &fakeSink{}, &bytes.Buffer{}, sdkmetric.NewManualReader()
	registered := false
	err := setup(servedMapper(), func(*Reconciler) error { registered = true; return nil },
		setupOpts(sink, logs, reader, "10.96.0.0/12", "fd00::/108"))
	require.NoError(t, err)
	assert.False(t, registered, "the ServiceCIDR reconciler must not run under an override")
	assert.Equal(t, [][]netip.Prefix{prefixes(t, "10.96.0.0/12")}, sink.calls)
	v, ok := gauge(t, reader)
	require.True(t, ok)
	assert.Equal(t, int64(1), v)
}

// The public entry point never touches the manager under an override (nil here).
func TestSetup_OverrideNeedsNoManager(t *testing.T) {
	sink, logs, reader := &fakeSink{}, &bytes.Buffer{}, sdkmetric.NewManualReader()
	require.NoError(t, Setup(nil, setupOpts(sink, logs, reader, "10.96.0.0/12")))
	assert.Equal(t, prefixes(t, "10.96.0.0/12"), sink.last(t))
}

func TestSetup_InvalidOverrideFails(t *testing.T) {
	sink, logs, reader := &fakeSink{}, &bytes.Buffer{}, sdkmetric.NewManualReader()
	err := Setup(nil, setupOpts(sink, logs, reader, "10.96.0.0/12", "10.96.0.0"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--service-cidrs")
	assert.Empty(t, sink.calls)
}

func TestSetup_DiscoveryWhenServed(t *testing.T) {
	sink, logs, reader := &fakeSink{}, &bytes.Buffer{}, sdkmetric.NewManualReader()
	var got *Reconciler
	require.NoError(t, setup(servedMapper(), func(r *Reconciler) error { got = r; return nil },
		setupOpts(sink, logs, reader)))
	require.NotNil(t, got, "the reconciler must be registered where the API is served")
	assert.Same(t, sink, got.Sink)
	assert.Empty(t, sink.calls, "the range comes from the reconciler, not from setup")
}

func TestSetup_APINotServedIsNoRange(t *testing.T) {
	sink, logs, reader := &fakeSink{}, &bytes.Buffer{}, sdkmetric.NewManualReader()
	registered := false
	require.NoError(t, setup(meta.NewDefaultRESTMapper(nil), func(*Reconciler) error { registered = true; return nil },
		setupOpts(sink, logs, reader)))
	assert.False(t, registered, "no watch on an API that is not served")
	assert.Equal(t, [][]netip.Prefix{nil}, sink.calls)
	v, ok := gauge(t, reader)
	require.True(t, ok)
	assert.Equal(t, int64(0), v)
	assert.Equal(t, 1, strings.Count(logs.String(), "level=WARN"))
}
