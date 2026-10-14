package gamma

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// reconciledSink says when the reconciler has projected.
type reconciledSink struct {
	fakeSink
	done chan struct{}
}

func (s *reconciledSink) SetServiceRoutes(map[string][]proxy.GammaRoute) {
	select {
	case s.done <- struct{}{}:
	default:
	}
}

// lockedInformers serializes FakeInformers.GetInformer: the fake keeps its
// informers in a map with no lock, and a controller starts its sources
// concurrently.
type lockedInformers struct {
	*informertest.FakeInformers
	mu sync.Mutex
}

func (l *lockedInformers) GetInformer(ctx context.Context, obj client.Object, opts ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A metadata-only watch asks with a PartialObjectMetadata, which the fake
	// cannot find a kind for in the scheme: its kind is the one it carries.
	if meta, ok := obj.(*metav1.PartialObjectMetadata); ok {
		return l.FakeInformers.GetInformerForKind(ctx, meta.GroupVersionKind(), opts...)
	}
	return l.FakeInformers.GetInformer(ctx, obj, opts...)
}

// sliceEvents is the fake informer the reconciler's EndpointSlice watch reads.
func (l *lockedInformers) sliceEvents(t *testing.T) *controllertest.FakeInformer {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	inf, err := l.FakeInformers.FakeInformerForKind(context.Background(), discoveryv1.SchemeGroupVersion.WithKind("EndpointSlice"))
	require.NoError(t, err)
	return inf
}

// TestSetupWithManager_APodPortsChangeIsAReconcile: a route's status depends
// on what the pods of a user's Service declare, which is no API object. The
// reconciler used to look again every 30 s; it is told now, on
// Reconciler.PodPortsChanged, and that channel is the ONLY thing that makes
// it look again. A reconciler that is not wired to it leaves "Not checked" in
// a status for good.
//
// A real controller-runtime manager over informers that never deliver an
// object: the only event there can be is the one sent here.
func TestSetupWithManager_APodPortsChangeIsAReconcile(t *testing.T) {
	scheme := newScheme(t)
	informers := &lockedInformers{FakeInformers: &informertest.FakeInformers{Scheme: scheme}}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Service"), meta.RESTScopeNamespace)
	mapper.Add(discoveryv1.SchemeGroupVersion.WithKind("EndpointSlice"), meta.RESTScopeNamespace)
	skip := true
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme: scheme,
		NewCache: func(*rest.Config, ctrlcache.Options) (ctrlcache.Cache, error) {
			return informers, nil
		},
		MapperProvider:         func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil },
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: &skip},
	})
	require.NoError(t, err)

	changed := make(chan event.GenericEvent, 1)
	sink := &reconciledSink{done: make(chan struct{}, 1)}
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		Sink:   sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler),
		PodPortsChanged: changed,
	}
	require.NoError(t, r.SetupWithManager(mgr))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(ctx) }()

	select {
	case <-sink.done:
		t.Fatal("control: a reconcile ran with no event at all, so the one below would prove nothing")
	case <-time.After(300 * time.Millisecond):
	}
	changed <- event.GenericEvent{}
	select {
	case <-sink.done:
	case err := <-stopped:
		t.Fatalf("the manager stopped: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the cache said its pods' ports changed and no reconcile ran")
	}

	// The reconciler's own timer is the other event that is no API object:
	// it keeps the wait of a held-back status when a reconcile ends in an
	// error (wakeIn). The controller watches it too.
	select {
	case <-sink.done:
		t.Fatal("control: a second reconcile ran with no second event")
	case <-time.After(300 * time.Millisecond):
	}
	r.After = func(_ time.Duration, f func()) { go f() } // a timer fires on its own goroutine
	r.wakeIn(time.Second)
	select {
	case <-sink.done:
	case err := <-stopped:
		t.Fatalf("the manager stopped: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the reconciler's timer fired and no reconcile ran")
	}

	// The third: an EndpointSlice of a Service a route names that has a
	// named targetPort. The name resolves (or stops resolving) when the slice
	// appears, changes or goes, and nothing else the reconciler watches
	// changes then: without this the port stayed refused, or resolved to a
	// number the pods had left, until some other event came by.
	sliceOf := func(svc string) *metav1.PartialObjectMetadata {
		m := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: svc + "-abc", Labels: map[string]string{discoveryv1.LabelServiceName: svc}}}
		m.SetGroupVersionKind(discoveryv1.SchemeGroupVersion.WithKind("EndpointSlice"))
		return m
	}
	r.setNamedTargets(map[string]struct{}{"ns/web": {}})
	slicesInformer := informers.sliceEvents(t)
	slicesInformer.Add(sliceOf("db"))
	select {
	case <-sink.done:
		t.Fatal("control: a slice of a Service no route names, or with no named targetPort, was a reconcile")
	case <-time.After(300 * time.Millisecond):
	}
	slicesInformer.Add(sliceOf("web"))
	select {
	case <-sink.done:
	case err := <-stopped:
		t.Fatalf("the manager stopped: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the EndpointSlice of a Service with a named targetPort appeared and no reconcile ran")
	}
	cancel()
	<-stopped
}

// TestSetupWithManager_AFailedReconcileIsRetriedWithinTheCap: the cap on the
// retry delay is the one the CONTROLLER is registered with. A reconciler
// whose reconcile always fails, under a running manager, with the cap set to
// 50 ms: capped, the retries come 5, 10, 20, 40 and then 50 ms apart, some
// forty in two seconds; with controller-runtime's default back-off (which
// doubles to 1000 s) there are nine.
func TestSetupWithManager_AFailedReconcileIsRetriedWithinTheCap(t *testing.T) {
	was := maxRetryDelay
	maxRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() { maxRetryDelay = was })

	scheme := newScheme(t)
	informers := &lockedInformers{FakeInformers: &informertest.FakeInformers{Scheme: scheme}}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Service"), meta.RESTScopeNamespace)
	mapper.Add(discoveryv1.SchemeGroupVersion.WithKind("EndpointSlice"), meta.RESTScopeNamespace)
	skip := true
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme:                 scheme,
		NewCache:               func(*rest.Config, ctrlcache.Options) (ctrlcache.Cache, error) { return informers, nil },
		MapperProvider:         func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil },
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: &skip},
	})
	require.NoError(t, err)

	var calls atomic.Int64
	failing := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			calls.Add(1)
			return errors.New("the API server is away")
		},
	}).Build()
	changed := make(chan event.GenericEvent, 1)
	r := &Reconciler{Client: failing, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), PodPortsChanged: changed}
	require.NoError(t, r.SetupWithManager(mgr))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(ctx) }()

	changed <- event.GenericEvent{}
	require.Eventually(t, func() bool { return calls.Load() > 0 }, 10*time.Second, 5*time.Millisecond, "the reconcile never ran")
	time.Sleep(2 * time.Second)
	got := calls.Load()
	cancel()
	<-stopped
	assert.GreaterOrEqualf(t, got, int64(20), "%d reconciles in two seconds of a reconcile that always fails, with the cap at 50 ms: the controller is registered with another back-off than the capped one", got)
}
