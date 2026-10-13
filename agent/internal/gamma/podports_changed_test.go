package gamma

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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
	return l.FakeInformers.GetInformer(ctx, obj, opts...)
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
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Service"), meta.RESTScopeNamespace)
	skip := true
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme: scheme,
		NewCache: func(*rest.Config, ctrlcache.Options) (ctrlcache.Cache, error) {
			return &lockedInformers{FakeInformers: &informertest.FakeInformers{Scheme: scheme}}, nil
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
	cancel()
	<-stopped
}
