package servicerange

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"

	aetherlabels "aethermesh.dev/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

func mcsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(s))
	require.NoError(t, mcsv1alpha1.AddToScheme(s))
	return s
}

func serviceImport(ns, name string, typ mcsv1alpha1.ServiceImportType, labels map[string]string, ips ...string) *mcsv1alpha1.ServiceImport {
	return &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: mcsv1alpha1.ServiceImportSpec{
			Type:  typ,
			IPs:   ips,
			Ports: []mcsv1alpha1.ServicePort{{Port: 80}},
		},
	}
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

var managed = map[string]string{aetherlabels.LabelManagedServiceImport: "true"}

func TestClusterSetIPs(t *testing.T) {
	got := ClusterSetIPs([]mcsv1alpha1.ServiceImport{
		*serviceImport("a", "b", mcsv1alpha1.ClusterSetIP, nil, "192.168.7.2", "fd00::7"),
		*serviceImport("a", "c", mcsv1alpha1.ClusterSetIP, nil, "192.168.7.1", "192.168.7.2"),
		*serviceImport("a", "mesh", mcsv1alpha1.ClusterSetIP, managed, "10.96.5.5"),
		*serviceImport("a", "headless", mcsv1alpha1.Headless, nil, "192.168.9.9"),
		*serviceImport("a", "bad", mcsv1alpha1.ClusterSetIP, nil, "not-an-ip", "192.168.8.0/24", "::ffff:192.168.8.8", ""),
		*serviceImport("a", "unallocated", mcsv1alpha1.ClusterSetIP, nil),
	})
	assert.Equal(t, addrs("192.168.7.1", "192.168.7.2"), got,
		"sorted, deduplicated, IPv4 only; aether-managed, Headless and unparseable entries skipped")

	// A managed label set to anything but "true" is not aether's.
	got = ClusterSetIPs([]mcsv1alpha1.ServiceImport{
		*serviceImport("a", "other", mcsv1alpha1.ClusterSetIP, map[string]string{aetherlabels.LabelManagedServiceImport: "false"}, "192.168.7.3"),
	})
	assert.Equal(t, addrs("192.168.7.3"), got)

	assert.Nil(t, ClusterSetIPs(nil))
}

func TestCombine(t *testing.T) {
	cidrs := prefixes(t, "10.96.0.0/12")
	assert.Equal(t, cidrs, Combine(cidrs, addrs("10.96.0.10", "10.111.255.255")),
		"an address inside the range adds nothing")
	assert.Equal(t, prefixes(t, "10.96.0.0/12", "192.168.7.1/32"),
		Combine(cidrs, addrs("192.168.7.1", "10.96.0.10")),
		"an address outside the range adds a /32")
	assert.Equal(t, prefixes(t, "192.168.7.1/32"), Combine(nil, addrs("192.168.7.1")),
		"with no ServiceCIDR part every ClusterSetIP is a /32")
	assert.Nil(t, Combine(nil, nil))
	assert.Equal(t, cidrs, Combine(cidrs, nil))
}

// importHarness runs a ServiceCIDR reconciler and a ServiceImport reconciler
// over one fake client and one shared state, as Setup wires them.
type importHarness struct {
	cidrs   *Reconciler
	imports *ImportReconciler
	c       client.Client
	sink    *fakeSink
	logs    *bytes.Buffer
}

func newImportHarness(t *testing.T, objs ...client.Object) *importHarness {
	t.Helper()
	h := &importHarness{sink: &fakeSink{}, logs: &bytes.Buffer{}}
	st, err := newState(slog.New(slog.NewTextHandler(h.logs, nil)),
		sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader())).Meter("test"))
	require.NoError(t, err)
	h.c = fake.NewClientBuilder().WithScheme(mcsScheme(t)).WithObjects(objs...).Build()
	h.cidrs = &Reconciler{Client: h.c, Sink: h.sink, state: st}
	h.imports = &ImportReconciler{Client: h.c, Sink: h.sink, state: st}
	return h
}

func (h *importHarness) reconcileCIDRs(t *testing.T) {
	t.Helper()
	_, err := h.cidrs.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
}

func (h *importHarness) reconcileImports(t *testing.T) {
	t.Helper()
	_, err := h.imports.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
}

func TestImports_InsideTheRangeAddsNothing(t *testing.T) {
	h := newImportHarness(t,
		serviceCIDR("kubernetes", "10.96.0.0/12"),
		serviceImport("ns", "inside", mcsv1alpha1.ClusterSetIP, nil, "10.96.0.50"),
	)
	h.reconcileCIDRs(t)
	h.reconcileImports(t)
	assert.Equal(t, [][]netip.Prefix{prefixes(t, "10.96.0.0/12")}, h.sink.calls,
		"an import inside the range changes nothing, so the sink is not called again")
}

func TestImports_OutsideTheRangeAddsASlash32(t *testing.T) {
	h := newImportHarness(t,
		serviceCIDR("kubernetes", "10.96.0.0/12"),
		serviceImport("ns", "far", mcsv1alpha1.ClusterSetIP, nil, "192.168.7.1"),
	)
	h.reconcileCIDRs(t)
	h.reconcileImports(t)
	assert.Equal(t, prefixes(t, "10.96.0.0/12", "192.168.7.1/32"), h.sink.last(t))
}

// The imports can reconcile before the ServiceCIDRs: the /32 is delivered on
// its own, and folded away once a block that covers it is known.
func TestImports_BeforeTheServiceCIDRs(t *testing.T) {
	h := newImportHarness(t,
		serviceCIDR("kubernetes", "192.168.0.0/16"),
		serviceImport("ns", "far", mcsv1alpha1.ClusterSetIP, nil, "192.168.7.1"),
	)
	h.reconcileImports(t)
	assert.Equal(t, prefixes(t, "192.168.7.1/32"), h.sink.last(t))
	h.reconcileCIDRs(t)
	assert.Equal(t, prefixes(t, "192.168.0.0/16"), h.sink.last(t))
	assert.Zero(t, strings.Count(h.logs.String(), "level=WARN"),
		"a ClusterSetIP delivered first is not a missing Service range")
}

func TestImports_ManagedAndHeadlessAreSkipped(t *testing.T) {
	h := newImportHarness(t,
		serviceCIDR("kubernetes", "10.96.0.0/12"),
		serviceImport("ns", "mesh", mcsv1alpha1.ClusterSetIP, managed, "192.168.7.1"),
		serviceImport("ns", "headless", mcsv1alpha1.Headless, nil, "192.168.7.2"),
	)
	h.reconcileCIDRs(t)
	h.reconcileImports(t)
	assert.Equal(t, [][]netip.Prefix{prefixes(t, "10.96.0.0/12")}, h.sink.calls)
}

func TestImports_IPv6AndInvalidAreSkipped(t *testing.T) {
	h := newImportHarness(t,
		serviceCIDR("kubernetes", "10.96.0.0/12"),
		serviceImport("ns", "v6", mcsv1alpha1.ClusterSetIP, nil, "fd00::7"),
		serviceImport("ns", "bad", mcsv1alpha1.ClusterSetIP, nil, "192.168.7"),
	)
	h.reconcileCIDRs(t)
	h.reconcileImports(t)
	assert.Equal(t, [][]netip.Prefix{prefixes(t, "10.96.0.0/12")}, h.sink.calls)
}

func TestImports_DeletionRemovesTheSlash32(t *testing.T) {
	far := serviceImport("ns", "far", mcsv1alpha1.ClusterSetIP, nil, "192.168.7.1")
	h := newImportHarness(t,
		serviceCIDR("kubernetes", "10.96.0.0/12"),
		far,
		serviceImport("ns", "other", mcsv1alpha1.ClusterSetIP, nil, "192.168.7.9"),
	)
	h.reconcileCIDRs(t)
	h.reconcileImports(t)
	require.Equal(t, prefixes(t, "10.96.0.0/12", "192.168.7.1/32", "192.168.7.9/32"), h.sink.last(t))

	require.NoError(t, h.c.Delete(context.Background(), far))
	h.reconcileImports(t)
	assert.Equal(t, prefixes(t, "10.96.0.0/12", "192.168.7.9/32"), h.sink.last(t))
}

// A resync that finds the same set hands the sink nothing, from either side.
func TestImports_AnUnchangedSetIsNotRedelivered(t *testing.T) {
	h := newImportHarness(t,
		serviceCIDR("kubernetes", "10.96.0.0/12"),
		serviceImport("ns", "far", mcsv1alpha1.ClusterSetIP, nil, "192.168.7.1"),
	)
	h.reconcileCIDRs(t)
	h.reconcileImports(t)
	h.reconcileImports(t)
	h.reconcileCIDRs(t)
	assert.Equal(t, [][]netip.Prefix{
		prefixes(t, "10.96.0.0/12"),
		prefixes(t, "10.96.0.0/12", "192.168.7.1/32"),
	}, h.sink.calls)
}

// The two reconcilers run on their own goroutines; the last delivery is
// always the combination of both parts (run with --config=race).
func TestImports_ConcurrentReconcilers(t *testing.T) {
	h := newImportHarness(t,
		serviceCIDR("kubernetes", "10.96.0.0/12"),
		serviceImport("ns", "far", mcsv1alpha1.ClusterSetIP, nil, "192.168.7.1"),
	)
	sink := &lockedSink{}
	h.cidrs.Sink, h.imports.Sink = sink, sink
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := h.cidrs.Reconcile(context.Background(), reconcile.Request{})
			assert.NoError(t, err)
		}()
		go func() {
			defer wg.Done()
			_, err := h.imports.Reconcile(context.Background(), reconcile.Request{})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	assert.Equal(t, prefixes(t, "10.96.0.0/12", "192.168.7.1/32"), sink.calls[len(sink.calls)-1])
}

type lockedSink struct {
	mu    sync.Mutex
	calls [][]netip.Prefix
}

func (s *lockedSink) SetServiceRanges(r []netip.Prefix) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r)
}

// Without the ServiceImport CRD the range is the ServiceCIDR part alone, and
// its absence is not a warning.
func TestSetup_ServiceImportCRDAbsent(t *testing.T) {
	sink, logs, reader := &fakeSink{}, &bytes.Buffer{}, sdkmetric.NewManualReader()
	require.NoError(t, setup(servedMapper(), func(*Reconciler) error { return nil }, noImports(t),
		setupOpts(sink, logs, reader, "10.96.0.0/12")))
	assert.Equal(t, [][]netip.Prefix{prefixes(t, "10.96.0.0/12")}, sink.calls)
	assert.Zero(t, strings.Count(logs.String(), "level=WARN"))
	assert.Contains(t, logs.String(), "ServiceImport CRD is not installed")
}

func TestSetup_ServiceImportCRDPresent(t *testing.T) {
	sink, logs, reader := &fakeSink{}, &bytes.Buffer{}, sdkmetric.NewManualReader()
	var cidrs *Reconciler
	var imports *ImportReconciler
	require.NoError(t, setup(servedMapper(withServiceImport),
		func(r *Reconciler) error { cidrs = r; return nil },
		func(r *ImportReconciler) error { imports = r; return nil },
		setupOpts(sink, logs, reader)))
	require.NotNil(t, cidrs)
	require.NotNil(t, imports)
	assert.Same(t, sink, imports.Sink)
	assert.Same(t, cidrs.state, imports.state, "both parts must combine in one state")
}
