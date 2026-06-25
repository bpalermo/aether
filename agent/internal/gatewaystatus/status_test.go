package gatewaystatus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T { return &v }

func gatewayParentRef(name string) gatewayv1.ParentReference {
	return gatewayv1.ParentReference{
		Kind: ptr(gatewayv1.Kind("Gateway")),
		Name: gatewayv1.ObjectName(name),
	}
}

// MergeRouteParentStatus on an empty list adds our owned entry with the
// requested conditions and reports changed.
func TestMergeRouteParentStatus_Insert(t *testing.T) {
	parents, changed := MergeRouteParentStatus(nil, EdgeControllerName, gatewayParentRef("edge"), 3,
		Condition{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: string(gatewayv1.RouteReasonAccepted)},
		Condition{Type: string(gatewayv1.RouteConditionResolvedRefs), Status: metav1.ConditionTrue, Reason: string(gatewayv1.RouteReasonResolvedRefs)},
	)
	require.True(t, changed)
	require.Len(t, parents, 1)
	assert.Equal(t, EdgeControllerName, parents[0].ControllerName)
	acc := meta.FindStatusCondition(parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionTrue, acc.Status)
	assert.Equal(t, int64(3), acc.ObservedGeneration)
}

// A second merge with identical conditions is a no-op (changed=false), so the
// reconciler skips the status update — no hot loop.
func TestMergeRouteParentStatus_NoChange(t *testing.T) {
	parents, _ := MergeRouteParentStatus(nil, EdgeControllerName, gatewayParentRef("edge"), 3,
		Condition{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: string(gatewayv1.RouteReasonAccepted)},
	)
	parents2, changed := MergeRouteParentStatus(parents, EdgeControllerName, gatewayParentRef("edge"), 3,
		Condition{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: string(gatewayv1.RouteReasonAccepted)},
	)
	assert.False(t, changed)
	assert.Len(t, parents2, 1)
}

// A foreign controller's entry MUST be preserved untouched; we only update our own.
func TestMergeRouteParentStatus_PreservesForeign(t *testing.T) {
	foreign := gatewayv1.RouteParentStatus{
		ParentRef:      gatewayParentRef("edge"),
		ControllerName: "other.example/controller",
		Conditions: []metav1.Condition{{
			Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted",
		}},
	}
	parents, changed := MergeRouteParentStatus([]gatewayv1.RouteParentStatus{foreign}, EdgeControllerName, gatewayParentRef("edge"), 1,
		Condition{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: string(gatewayv1.RouteReasonAccepted)},
	)
	require.True(t, changed)
	require.Len(t, parents, 2, "ours appended, foreign preserved")
	assert.Equal(t, gatewayv1.GatewayController("other.example/controller"), parents[0].ControllerName)
	assert.Equal(t, EdgeControllerName, parents[1].ControllerName)
}

// MergeConditions sets a condition and detects a status flip as a change.
func TestMergeConditions(t *testing.T) {
	conds, changed := MergeConditions(nil, 5, Condition{
		Type: string(gatewayv1.GatewayConditionProgrammed), Status: metav1.ConditionTrue, Reason: string(gatewayv1.GatewayReasonProgrammed),
	})
	require.True(t, changed)
	prog := meta.FindStatusCondition(conds, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, prog)
	assert.Equal(t, metav1.ConditionTrue, prog.Status)
	assert.Equal(t, int64(5), prog.ObservedGeneration)

	// Re-applying the same condition is a no-op.
	_, changed2 := MergeConditions(conds, 5, Condition{
		Type: string(gatewayv1.GatewayConditionProgrammed), Status: metav1.ConditionTrue, Reason: string(gatewayv1.GatewayReasonProgrammed),
	})
	assert.False(t, changed2)
}
