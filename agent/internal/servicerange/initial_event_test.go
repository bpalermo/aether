package servicerange

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// signalSink records deliveries and says when one arrives.
type signalSink struct {
	mu    sync.Mutex
	calls [][]netip.Prefix
	got   chan struct{}
}

func (s *signalSink) SetServiceRanges(r []netip.Prefix) {
	s.mu.Lock()
	s.calls = append(s.calls, r)
	s.mu.Unlock()
	select {
	case s.got <- struct{}{}:
	default:
	}
}

// TestSetup_NoServiceCIDRObjectStillDelivers: where the ServiceCIDR API is
// served but holds no object, no watch event ever comes. The xDS server holds
// its first serve until the sink's first delivery (and a surge standby waits
// for it with no bound), so the controller must reconcile once on its own
// and decide the range: none, with the WARN.
//
// A real controller-runtime manager over informers that never deliver an
// object, wired by the public Setup.
func TestSetup_NoServiceCIDRObjectStillDelivers(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	mapper := servedMapper()
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
	require.NoError(t, err)

	sink := &signalSink{got: make(chan struct{}, 1)}
	logs := &bytes.Buffer{}
	reader := sdkmetric.NewManualReader()
	require.NoError(t, Setup(mgr, Options{
		Sink:  sink,
		Log:   slog.New(slog.NewTextHandler(logs, nil)),
		Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"),
	}))
	sink.mu.Lock()
	require.Empty(t, sink.calls, "the range comes from the reconciler, not from Setup")
	sink.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped })

	select {
	case <-sink.got:
	case err := <-stopped:
		t.Fatalf("the manager stopped: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no ServiceCIDR object and the range was never delivered")
	}
	sink.mu.Lock()
	assert.Equal(t, [][]netip.Prefix{nil}, sink.calls, "one delivery: no range")
	sink.mu.Unlock()
	v, ok := gauge(t, reader)
	require.True(t, ok)
	assert.Equal(t, int64(0), v)
}
