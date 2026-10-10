package gatewayapi

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"aethermesh.dev/agent/internal/gatewaystatus"
	configv1 "aethermesh.dev/api/aether/config/v1"
	configapisv1 "aethermesh.dev/common/apis/config/v1"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func edgeConfigScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, gatewayv1.Install(s))
	require.NoError(t, configapisv1.AddToScheme(s))
	return s
}

func gwClassWithParams(name, ecName, ecNs string) *gatewayv1.GatewayClass {
	ns := gatewayv1.Namespace(ecNs)
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "gateway.aether.io/edge",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: "config.aether.io", Kind: "EdgeConfig", Name: ecName, Namespace: &ns,
			},
		},
	}
}

// class default supplies request_timeout; the per-Gateway override flips
// use_remote_address — proto.Merge yields both.
func TestResolveEdgeConfig_ClassDefaultPlusGatewayOverride(t *testing.T) {
	def := &configapisv1.EdgeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: "aether-ingress"},
		Spec: configv1.EdgeConfigSpec_builder{
			UseRemoteAddress:  wrapperspb.Bool(true),
			XffNumTrustedHops: wrapperspb.UInt32(2),
		}.Build(),
	}
	override := &configapisv1.EdgeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-override", Namespace: "team-a"},
		Spec: configv1.EdgeConfigSpec_builder{
			UseRemoteAddress: wrapperspb.Bool(false), // wins
		}.Build(),
	}
	ref := gatewayv1.LocalParametersReference{Group: "config.aether.io", Kind: "EdgeConfig", Name: "gw-override"}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"},
		Spec:       gatewayv1.GatewaySpec{Infrastructure: &gatewayv1.GatewayInfrastructure{ParametersRef: &ref}},
	}
	c := fake.NewClientBuilder().WithScheme(edgeConfigScheme(t)).
		WithObjects(gwClassWithParams("aether", "defaults", "aether-ingress"), def, override).Build()
	r := &Reconciler{Client: c, GatewayClassName: "aether"}

	eff := r.resolveEdgeConfig(context.Background(), gw)
	require.NotNil(t, eff)
	assert.False(t, eff.GetUseRemoteAddress().GetValue(), "override wins")
	assert.Equal(t, uint32(2), eff.GetXffNumTrustedHops().GetValue(), "inherited from class default")
}

// no parametersRef anywhere → nil (compiled defaults apply downstream).
func TestResolveEdgeConfig_NoRefs(t *testing.T) {
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "team-a"}}
	c := fake.NewClientBuilder().WithScheme(edgeConfigScheme(t)).
		WithObjects(&gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "aether"}}).Build()
	r := &Reconciler{Client: c, GatewayClassName: "aether"}
	assert.Nil(t, r.resolveEdgeConfig(context.Background(), gw))
}

// TestReconcile_GatewayInvalidParametersRef: a Gateway whose
// infrastructure.parametersRef cannot resolve is rejected with
// Accepted=False/InvalidParameters and is not Programmed; one whose ref names
// an existing EdgeConfig is accepted. The first case is the manifest of the
// upstream conformance test GatewayInvalidParametersRef, which gateway-api
// v1.6.3 gates on the GatewayInfrastructure feature aether does not advertise
// (docs/conformance/gateway-api-features.md), so the suite no longer runs it.
func TestReconcile_GatewayInvalidParametersRef(t *testing.T) {
	scheme := statusScheme(t)
	require.NoError(t, configapisv1.AddToScheme(scheme))

	tests := []struct {
		name         string
		ref          gatewayv1.LocalParametersReference
		wantAccepted metav1.ConditionStatus
		wantReason   gatewayv1.GatewayConditionReason
	}{
		{
			name:         "unsupported group and kind",
			ref:          gatewayv1.LocalParametersReference{Group: "invalid.io", Kind: "InvalidParameters", Name: "invalid"},
			wantAccepted: metav1.ConditionFalse,
			wantReason:   gatewayv1.GatewayReasonInvalidParameters,
		},
		{
			name:         "EdgeConfig that does not exist",
			ref:          gatewayv1.LocalParametersReference{Group: "config.aether.io", Kind: "EdgeConfig", Name: "missing"},
			wantAccepted: metav1.ConditionFalse,
			wantReason:   gatewayv1.GatewayReasonInvalidParameters,
		},
		{
			name:         "EdgeConfig that exists",
			ref:          gatewayv1.LocalParametersReference{Group: "config.aether.io", Kind: "EdgeConfig", Name: "present"},
			wantAccepted: metav1.ConditionTrue,
			wantReason:   gatewayv1.GatewayReasonAccepted,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gc := &gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "aether", Generation: 1},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewaystatus.EdgeControllerName},
			}
			gw := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra", Generation: 1},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "aether",
					Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
					Infrastructure:   &gatewayv1.GatewayInfrastructure{ParametersRef: &tc.ref},
				},
			}
			present := &configapisv1.EdgeConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "present", Namespace: "infra"},
				Spec:       configv1.EdgeConfigSpec_builder{UseRemoteAddress: wrapperspb.Bool(true)}.Build(),
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(gc, gw, present).
				WithStatusSubresource(&gatewayv1.GatewayClass{}, &gatewayv1.Gateway{}).
				Build()
			r := &Reconciler{
				Client: c, APIReader: c, Sink: statusFakeSink{},
				Namespace: "aether-ingress", GatewayClassName: "aether", MeshDomain: "mesh", Log: slog.Default(),
			}

			_, err := r.Reconcile(context.Background(), reconcile.Request{})
			require.NoError(t, err)

			got := &gatewayv1.Gateway{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "infra", Name: "gw"}, got))
			accepted := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
			require.NotNil(t, accepted)
			assert.Equal(t, tc.wantAccepted, accepted.Status)
			assert.Equal(t, string(tc.wantReason), accepted.Reason)
			assert.Equal(t, int64(1), accepted.ObservedGeneration)
			programmed := meta.FindStatusCondition(got.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
			require.NotNil(t, programmed)
			if tc.wantAccepted == metav1.ConditionFalse {
				assert.Equal(t, metav1.ConditionFalse, programmed.Status)
				assert.Equal(t, string(gatewayv1.GatewayReasonInvalid), programmed.Reason)
			}
		})
	}
}
