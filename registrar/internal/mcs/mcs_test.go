package mcs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/registry/export"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

// fakeExporter is an in-memory registry.ServiceExporter for the projection tests.
// Keyed by service; records (namespace, cluster) for the local cluster only —
// remote exports are seeded directly via the exports slice.
type fakeExporter struct {
	mu      sync.Mutex
	cluster string
	exports map[string]export.ServiceExport // local exports, keyed by service
	seeded  []export.ServiceExport          // remote (or extra) exports returned verbatim
	listErr error
}

func newFakeExporter(cluster string) *fakeExporter {
	return &fakeExporter{cluster: cluster, exports: map[string]export.ServiceExport{}}
}

func (f *fakeExporter) SetExport(_ context.Context, service, namespace string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exports[service] = export.ServiceExport{Service: service, Namespace: namespace, Cluster: f.cluster}
	return nil
}

func (f *fakeExporter) UnsetExport(_ context.Context, service string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.exports, service)
	return nil
}

func (f *fakeExporter) ListExports(_ context.Context) ([]export.ServiceExport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := append([]export.ServiceExport{}, f.seeded...)
	for _, e := range f.exports {
		out = append(out, e)
	}
	return out, nil
}

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, mcsv1alpha1.AddToScheme(s))
	return s
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&mcsv1alpha1.ServiceExport{}, &mcsv1alpha1.ServiceImport{}).
		Build()
}

// newIPAllocatingClient mimics the apiserver's ClusterIP allocator: it assigns a
// deterministic ClusterIP to every ClusterIP Service on Create (the fake client
// otherwise leaves spec.ClusterIP empty), so the import generator observes a VIP.
func newIPAllocatingClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	var n int
	return fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&mcsv1alpha1.ServiceExport{}, &mcsv1alpha1.ServiceImport{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if svc, ok := obj.(*corev1.Service); ok &&
					svc.Spec.ClusterIP == "" && svc.Spec.Type == corev1.ServiceTypeClusterIP {
					n++
					svc.Spec.ClusterIP = fmt.Sprintf("10.96.0.%d", n)
					svc.Spec.ClusterIPs = []string{svc.Spec.ClusterIP}
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
}

// --- ExportController: ServiceExport -> registry ---------------------------

func TestExportController_ExportsBackedService(t *testing.T) {
	ctx := context.Background()
	se := &mcsv1alpha1.ServiceExport{ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "team-a"}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "team-a"}}
	c := newClient(t, se, svc)
	exp := newFakeExporter("cluster-1")
	ctl := &ExportController{Client: c, Exporter: exp, Log: slog.New(slog.DiscardHandler)}

	_, err := ctl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "svc-1", Namespace: "team-a"}})
	require.NoError(t, err)

	exports, _ := exp.ListExports(ctx)
	require.Len(t, exports, 1)
	assert.Equal(t, "svc-1", exports[0].Service)
	assert.Equal(t, "team-a", exports[0].Namespace)

	got := &mcsv1alpha1.ServiceExport{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "svc-1", Namespace: "team-a"}, got))
	assert.True(t, hasTrueCondition(got.Status.Conditions, string(mcsv1alpha1.ServiceExportConditionValid)))
	assert.True(t, hasTrueCondition(got.Status.Conditions, string(mcsv1alpha1.ServiceExportConditionReady)))
}

func TestExportController_NoBackingServiceIsInvalid(t *testing.T) {
	ctx := context.Background()
	se := &mcsv1alpha1.ServiceExport{ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "team-a"}}
	c := newClient(t, se) // no backing Service
	exp := newFakeExporter("cluster-1")
	ctl := &ExportController{Client: c, Exporter: exp, Log: slog.New(slog.DiscardHandler)}

	_, err := ctl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "svc-1", Namespace: "team-a"}})
	require.NoError(t, err)

	exports, _ := exp.ListExports(ctx)
	assert.Empty(t, exports, "no registry export without a backing Service")

	got := &mcsv1alpha1.ServiceExport{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "svc-1", Namespace: "team-a"}, got))
	assert.False(t, hasTrueCondition(got.Status.Conditions, string(mcsv1alpha1.ServiceExportConditionValid)))
}

func TestExportController_ExternalNameIsInvalid(t *testing.T) {
	ctx := context.Background()
	se := &mcsv1alpha1.ServiceExport{ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "team-a"}}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "team-a"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "example.com"},
	}
	c := newClient(t, se, svc)
	exp := newFakeExporter("cluster-1")
	ctl := &ExportController{Client: c, Exporter: exp, Log: slog.New(slog.DiscardHandler)}

	_, err := ctl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "svc-1", Namespace: "team-a"}})
	require.NoError(t, err)

	exports, _ := exp.ListExports(ctx)
	assert.Empty(t, exports, "ExternalName Services are not exportable")
}

func TestExportController_DeletedServiceExportClearsMark(t *testing.T) {
	ctx := context.Background()
	c := newClient(t) // ServiceExport absent => treated as deleted
	exp := newFakeExporter("cluster-1")
	require.NoError(t, exp.SetExport(ctx, "svc-1", "team-a"))
	ctl := &ExportController{Client: c, Exporter: exp, Log: slog.New(slog.DiscardHandler)}

	_, err := ctl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "svc-1", Namespace: "team-a"}})
	require.NoError(t, err)

	exports, _ := exp.ListExports(ctx)
	assert.Empty(t, exports, "deleting the ServiceExport clears the registry mark")
}

// --- ImportGenerator: registry -> ServiceImport + clusterset VIP -----------

func newImportGen(c client.Client, exp *fakeExporter) *ImportGenerator {
	return &ImportGenerator{Client: c, Exporter: exp, MeshPort: 18081, Log: slog.New(slog.DiscardHandler)}
}

func TestImportGenerator_MaterializesImportAndVIP(t *testing.T) {
	ctx := context.Background()
	c := newIPAllocatingClient(t)
	exp := newFakeExporter("cluster-1")
	exp.seeded = []export.ServiceExport{
		{Service: "svc-1", Namespace: "team-a", Cluster: "cluster-2"},
	}
	g := newImportGen(c, exp)
	g.reconcile(ctx)

	// The clusterset VIP Service has a DISTINCT name (<svc>-clusterset) so it
	// coexists with the same-named per-cluster mesh Service.
	svc := &corev1.Service{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "svc-1-clusterset", Namespace: "team-a"}, svc))
	assert.Equal(t, "true", svc.Labels[constants.LabelClustersetService])
	assert.Empty(t, svc.Spec.Selector, "selectorless clusterset VIP: endpoints stay in the registry")
	assert.Equal(t, "svc-1", svc.Annotations[constants.AnnotationMeshService])
	assert.Equal(t, "svc-1", svc.Annotations[constants.AnnotationClustersetImport])
	assert.Equal(t, "18081", svc.Annotations[constants.AnnotationMeshPort])
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, int32(18081), svc.Spec.Ports[0].Port)
	require.NotEmpty(t, svc.Spec.ClusterIP, "the clusterset VIP gets its own ClusterIP")

	// No same-named clusterset Service collides with the mesh Service name.
	notClusterset := &corev1.Service{}
	err := c.Get(ctx, types.NamespacedName{Name: "svc-1", Namespace: "team-a"}, notClusterset)
	assert.True(t, apierrors.IsNotFound(err), "the clusterset VIP does NOT take the mesh Service name")

	si := &mcsv1alpha1.ServiceImport{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "svc-1", Namespace: "team-a"}, si))
	assert.Equal(t, "true", si.Labels[constants.LabelManagedServiceImport])
	assert.Equal(t, mcsv1alpha1.ClusterSetIP, si.Spec.Type)
	require.Len(t, si.Spec.Ports, 1)
	assert.Equal(t, int32(18081), si.Spec.Ports[0].Port)
	require.Len(t, si.Spec.IPs, 1, "the ServiceImport VIP is the clusterset Service ClusterIP")
	assert.Equal(t, svc.Spec.ClusterIP, si.Spec.IPs[0])
	require.Len(t, si.Status.Clusters, 1)
	assert.Equal(t, "cluster-2", si.Status.Clusters[0].Cluster)
}

func TestImportGenerator_SingleClusterExportGetsVIP(t *testing.T) {
	ctx := context.Background()
	c := newIPAllocatingClient(t)
	// Only a LOCAL export (this cluster) — single-cluster MCS must still produce a
	// ServiceImport with a real clusterset VIP, distinct from the mesh VIP.
	exp := newFakeExporter("cluster-1")
	require.NoError(t, exp.SetExport(ctx, "svc-1", "team-a"))
	g := newImportGen(c, exp)
	g.reconcile(ctx)

	svc := &corev1.Service{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "svc-1-clusterset", Namespace: "team-a"}, svc))
	require.NotEmpty(t, svc.Spec.ClusterIP)

	si := &mcsv1alpha1.ServiceImport{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "svc-1", Namespace: "team-a"}, si))
	require.Len(t, si.Spec.IPs, 1)
	assert.Equal(t, svc.Spec.ClusterIP, si.Spec.IPs[0], "single-cluster ServiceImport carries the clusterset VIP")
	require.Len(t, si.Status.Clusters, 1)
	assert.Equal(t, "cluster-1", si.Status.Clusters[0].Cluster)
}

func TestImportGenerator_MergesMultiClusterExports(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	exp := newFakeExporter("cluster-1")
	exp.seeded = []export.ServiceExport{
		{Service: "svc-1", Namespace: "team-a", Cluster: "cluster-2"},
		{Service: "svc-1", Namespace: "team-a", Cluster: "cluster-1"},
		{Service: "svc-1", Namespace: "team-a", Cluster: "cluster-1"}, // dup
	}
	g := newImportGen(c, exp)
	g.reconcile(ctx)

	si := &mcsv1alpha1.ServiceImport{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "svc-1", Namespace: "team-a"}, si))
	require.Len(t, si.Status.Clusters, 2, "one ClusterStatus per distinct exporting cluster")
	assert.Equal(t, "cluster-1", si.Status.Clusters[0].Cluster)
	assert.Equal(t, "cluster-2", si.Status.Clusters[1].Cluster)
}

func TestImportGenerator_PrunesStale(t *testing.T) {
	ctx := context.Background()
	staleSI := &mcsv1alpha1.ServiceImport{ObjectMeta: metav1.ObjectMeta{
		Name: "gone", Namespace: "team-a",
		Labels: map[string]string{constants.LabelManagedServiceImport: "true"},
	}}
	staleSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "gone-clusterset", Namespace: "team-a",
		Labels:      map[string]string{constants.LabelClustersetService: "true"},
		Annotations: map[string]string{constants.AnnotationClustersetImport: "gone"},
	}}
	c := newIPAllocatingClient(t, staleSI, staleSvc)
	exp := newFakeExporter("cluster-1")
	exp.seeded = []export.ServiceExport{{Service: "svc-1", Namespace: "team-a", Cluster: "cluster-2"}}
	g := newImportGen(c, exp)
	g.reconcile(ctx)

	err := c.Get(ctx, types.NamespacedName{Name: "gone", Namespace: "team-a"}, &mcsv1alpha1.ServiceImport{})
	assert.True(t, apierrors.IsNotFound(err), "ServiceImport for a vanished export is pruned")
	err = c.Get(ctx, types.NamespacedName{Name: "gone-clusterset", Namespace: "team-a"}, &corev1.Service{})
	assert.True(t, apierrors.IsNotFound(err), "clusterset VIP for a vanished export is pruned")
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "svc-1", Namespace: "team-a"}, &mcsv1alpha1.ServiceImport{}))
}

func TestImportGenerator_DoesNotClobberUserObjects(t *testing.T) {
	ctx := context.Background()
	userSI := &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "team-a"}, // no managed label
		Spec:       mcsv1alpha1.ServiceImportSpec{Type: mcsv1alpha1.Headless},
	}
	// A user's Service occupies the clusterset VIP's DISTINCT name (svc-1-clusterset):
	// the generator must skip it, not clobber it.
	userClustersetName := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-1-clusterset", Namespace: "team-a"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "owned-by-user"}},
	}
	c := newIPAllocatingClient(t, userSI, userClustersetName)
	exp := newFakeExporter("cluster-1")
	exp.seeded = []export.ServiceExport{{Service: "svc-1", Namespace: "team-a", Cluster: "cluster-2"}}
	g := newImportGen(c, exp)
	g.reconcile(ctx)

	si := &mcsv1alpha1.ServiceImport{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "svc-1", Namespace: "team-a"}, si))
	assert.Equal(t, mcsv1alpha1.Headless, si.Spec.Type, "a user's ServiceImport is left untouched")

	svc := &corev1.Service{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "svc-1-clusterset", Namespace: "team-a"}, svc))
	assert.Equal(t, map[string]string{"app": "owned-by-user"}, svc.Spec.Selector, "a user's Service on the clusterset name is left untouched")
}

func hasTrueCondition(conds []metav1.Condition, t string) bool {
	for _, c := range conds {
		if c.Type == t {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}
