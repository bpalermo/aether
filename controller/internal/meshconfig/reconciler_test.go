package meshconfig

import (
	"context"
	"log/slog"
	"testing"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const fallbackNS = "aether-system"

func meshCfg(ns string, spec *configv1.MeshConfigSpec) *crdv1.MeshConfig {
	return &crdv1.MeshConfig{
		ObjectMeta: metav1.ObjectMeta{Name: SingletonName, Namespace: ns},
		Spec:       spec,
	}
}

func newReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, crdv1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Reconciler{
		Client:            c,
		ConfigMapName:     DefaultMeshConfigMapName,
		FallbackNamespace: fallbackNS,
		Log:               slog.New(slog.DiscardHandler),
	}
}

// TestEffectiveSpec covers the namespaced fallback: a namespace inherits the
// control-plane MeshConfig field-by-field unless it overrides.
func TestEffectiveSpec(t *testing.T) {
	ctx := context.Background()
	fallback := configv1.MeshConfigSpec_builder{Proxy: configv1.ProxyTelemetry_builder{
		AccessLogsEnabled: proto.Bool(true),
		TraceSampleRate:   proto.Float64(0.5),
	}.Build()}.Build()

	t.Run("fallback namespace projects its own spec verbatim", func(t *testing.T) {
		r := newReconciler(t, meshCfg(fallbackNS, fallback))
		got, err := r.effectiveSpec(ctx, meshCfg(fallbackNS, fallback))
		require.NoError(t, err)
		assert.True(t, got.GetProxy().GetAccessLogsEnabled())
		assert.InEpsilon(t, 0.5, got.GetProxy().GetTraceSampleRate(), 1e-9)
	})

	t.Run("override merges over the fallback (set fields win, unset inherit)", func(t *testing.T) {
		override := configv1.MeshConfigSpec_builder{Proxy: configv1.ProxyTelemetry_builder{
			TraceSampleRate: proto.Float64(0.1), // override only the sample rate
		}.Build()}.Build()
		r := newReconciler(t, meshCfg(fallbackNS, fallback), meshCfg("aether-ingress", override))
		got, err := r.effectiveSpec(ctx, meshCfg("aether-ingress", override))
		require.NoError(t, err)
		assert.True(t, got.GetProxy().GetAccessLogsEnabled(), "inherited from the fallback")
		assert.InEpsilon(t, 0.1, got.GetProxy().GetTraceSampleRate(), 1e-9, "override wins")
	})

	t.Run("no fallback present projects the namespace's own spec", func(t *testing.T) {
		own := configv1.MeshConfigSpec_builder{Proxy: configv1.ProxyTelemetry_builder{
			TracingEnabled: proto.Bool(true),
		}.Build()}.Build()
		r := newReconciler(t, meshCfg("aether-ingress", own))
		got, err := r.effectiveSpec(ctx, meshCfg("aether-ingress", own))
		require.NoError(t, err)
		assert.True(t, got.GetProxy().GetTracingEnabled())
	})
}

// TestEnqueueInheritors: a change to the fallback namespace's MeshConfig fans out to
// every other namespace's MeshConfig; a change elsewhere enqueues nothing (For
// handles it).
func TestEnqueueInheritors(t *testing.T) {
	ctx := context.Background()
	r := newReconciler(
		t,
		meshCfg(fallbackNS, nil),
		meshCfg("aether-ingress", nil),
		meshCfg("team-a", nil),
	)

	reqs := r.enqueueInheritors(ctx, meshCfg(fallbackNS, nil))
	got := map[string]bool{}
	for _, rq := range reqs {
		got[rq.Namespace] = true
	}
	assert.True(t, got["aether-ingress"])
	assert.True(t, got["team-a"])
	assert.False(t, got[fallbackNS], "the fallback itself is handled by For()")

	assert.Empty(t, r.enqueueInheritors(ctx, meshCfg("aether-ingress", nil)),
		"a non-fallback change enqueues nothing here")
}
