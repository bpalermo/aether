package gatewayapi

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
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
