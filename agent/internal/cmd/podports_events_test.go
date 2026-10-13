package cmd

import (
	"log/slog"
	"net/http"
	"testing"

	"aethermesh.dev/agent/internal/gamma"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type hookHolder struct{ fn func() }

// fakeGammaCache is a snapshot cache as the GAMMA reconciler sees it.
type fakeGammaCache struct {
	gamma.RouteSink
	gamma.PodPorts
	hookHolder
}

// TestNewGammaReconciler_IsGivenTheCachesEvents: the reconciler the agent
// runs is built over the cache in every role, the events included.
func TestNewGammaReconciler_IsGivenTheCachesEvents(t *testing.T) {
	c := &fakeGammaCache{}
	r := newGammaReconciler(nil, nil, c, "mesh.example")
	if r.Sink != gamma.RouteSink(c) || r.PodPorts != gamma.PodPorts(c) {
		t.Fatal("the reconciler's sink and its pod-port answers are not the cache")
	}
	if r.MeshDomain != "mesh.example" {
		t.Fatalf("mesh domain %q", r.MeshDomain)
	}
	if r.PodPortsChanged == nil || c.fn == nil {
		t.Fatal("the reconciler is not given the cache's pod-port events: nothing would make it judge a status again when no API object changed")
	}
	c.fn()
	select {
	case <-r.PodPortsChanged:
	default:
		t.Fatal("the cache said its pods' ports changed and the reconciler's channel carries no event")
	}
}

func (h *hookHolder) OnPodPortsChanged(fn func()) { h.fn = fn }

// TestPodPortsEvents: the cache's "my pods' ports changed" reaches the route
// reconciler as an event, and the hook, which the cache calls with its lock
// held, never blocks: changes that pile up before the reconciler runs are one
// event, because one reconcile judges every route.
func TestPodPortsEvents(t *testing.T) {
	h := &hookHolder{}
	events := podPortsEvents(h)
	if h.fn == nil {
		t.Fatal("no hook was registered with the cache")
	}
	select {
	case <-events:
		t.Fatal("an event before any change")
	default:
	}
	for i := 0; i < 5; i++ {
		h.fn() // must not block
	}
	select {
	case <-events:
	default:
		t.Fatal("the cache said its pods' ports changed and no event was queued")
	}
	select {
	case <-events:
		t.Fatal("five changes before one reconcile are one event")
	default:
	}
	h.fn()
	select {
	case <-events:
	default:
		t.Fatal("a change after the event was taken queued nothing")
	}
}

// TestWireGAMMA_ReturnsTheReconcilerItBuiltWithTheCachesEvents: the same, of
// the reconciler wireGAMMA hands to SetupWithManager and returns: the channel
// is not cleared between building it and registering it. What the test does
// NOT see is the manager's side: that the controller the manager runs is this
// reconciler and watches the channel is held by gamma's own
// TestSetupWithManager_APodPortsChangeIsAReconcile, with a running manager.
func TestWireGAMMA_ReturnsTheReconcilerItBuiltWithTheCachesEvents(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Service"), meta.RESTScopeNamespace)
	mapper.Add(discoveryv1.SchemeGroupVersion.WithKind("EndpointSlice"), meta.RESTScopeNamespace)
	skip := true
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme: scheme,
		NewCache: func(*rest.Config, ctrlcache.Options) (ctrlcache.Cache, error) {
			return &informertest.FakeInformers{Scheme: scheme}, nil
		},
		MapperProvider:         func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return mapper, nil },
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: &skip},
	})
	if err != nil {
		t.Fatal(err)
	}
	was, logger := cfg.Gamma, l
	t.Cleanup(func() { cfg.Gamma, l = was, logger })
	l = slog.New(slog.DiscardHandler)

	cfg.Gamma = false
	if r, err := wireGAMMA(mgr, &fakeGammaCache{}); r != nil || err != nil {
		t.Fatalf("GAMMA off: wireGAMMA returned (%v, %v)", r, err)
	}

	cfg.Gamma = true
	c := &fakeGammaCache{}
	r, err := wireGAMMA(mgr, c)
	if err != nil || r == nil {
		t.Fatalf("wireGAMMA returned (%v, %v)", r, err)
	}
	if r.Sink != gamma.RouteSink(c) || r.PodPorts != gamma.PodPorts(c) {
		t.Fatal("the registered reconciler's sink and pod-port answers are not the cache")
	}
	if r.PodPortsChanged == nil || c.fn == nil {
		t.Fatal("the registered reconciler is not given the cache's pod-port events")
	}
	c.fn()
	select {
	case <-r.PodPortsChanged:
	default:
		t.Fatal("the cache said its pods' ports changed and the registered reconciler's channel carries no event")
	}
}
