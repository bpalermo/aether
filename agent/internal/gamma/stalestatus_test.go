package gamma

import (
	"context"
	"log/slog"
	"testing"

	"aethermesh.dev/agent/internal/gatewaystatus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestRouteStatus_EntryOfAParentRefThatLeftTheSpecIsRemoved: aether's status
// entry is per parentRef. When a parentRef is edited (its port changed), the
// entry of the old one is aether's to remove: left behind, the route shows a
// verdict about a parent it no longer names. Another controller's entry is
// not aether's to remove.
func TestRouteStatus_EntryOfAParentRefThatLeftTheSpecIsRemoved(t *testing.T) {
	hr := httpRoute("r1", "ns", "multi", "multi")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(9090))
	old := hr.Spec.ParentRefs[0]
	old.Port = ptr(gatewayv1.PortNumber(18081))
	cond := []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "NoMatchingParent", LastTransitionTime: metav1.Now()}}
	hr.Status.Parents = []gatewayv1.RouteParentStatus{
		{ParentRef: old, ControllerName: gatewaystatus.MeshControllerName, Conditions: cond},
		{ParentRef: old, ControllerName: "example.com/other", Conditions: cond},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, generatedService("multi", map[int32]string{8080: "http", 9090: "http"})).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true}
	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	var mine, others int
	for _, p := range got.Status.Parents {
		if p.ControllerName != gatewaystatus.MeshControllerName {
			others++
			continue
		}
		mine++
		require.NotNil(t, p.ParentRef.Port)
		assert.Equal(t, gatewayv1.PortNumber(9090), *p.ParentRef.Port, "aether's entry is for the parentRef the route names now")
		acc := meta.FindStatusCondition(p.Conditions, "Accepted")
		require.NotNil(t, acc)
		assert.Equal(t, metav1.ConditionTrue, acc.Status)
	}
	assert.Equal(t, 1, mine, "one entry of aether's, for the one parentRef")
	assert.Equal(t, 1, others, "another controller's entry stays")
}
