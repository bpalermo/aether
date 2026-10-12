package gamma

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestReconcile_DroppedBackendRefsAreRecorded: a backendRef the projection
// drops (another kind, another group, a Service in another namespace with no
// ReferenceGrant) is an invalid backendRef. The rule that reaches the cache
// says that it had some and what weight they held, so that the proxy answers
// their share 500 instead of handing it to the rule's other backends.
func TestReconcile_DroppedBackendRefsAreRecorded(t *testing.T) {
	weighted := func(ref gatewayv1.HTTPBackendRef, w int32) gatewayv1.HTTPBackendRef {
		ref.Weight = ptr(w)
		return ref
	}
	hr := httpRoute("r1", "ns", "echo", "echo-v1")
	hr.Spec.Rules = []gatewayv1.HTTPRouteRule{
		{BackendRefs: []gatewayv1.HTTPBackendRef{
			weighted(backendRef("echo-v1", 8080), 5),
			weighted(backendRef("not-a-service", 7070, func(r *gatewayv1.BackendObjectReference) { r.Kind = ptr(gatewayv1.Kind("ConfigMap")) }), 2),
			backendRef("ungranted", 6060, func(r *gatewayv1.BackendObjectReference) { r.Namespace = ptr(gatewayv1.Namespace("other")) }), // weight defaults to 1
			weighted(backendRef("other-group", 5050, func(r *gatewayv1.BackendObjectReference) { r.Group = ptr(gatewayv1.Group("example.com")) }), 0),
		}},
		{BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("echo-v1", 8080)}},
		{BackendRefs: []gatewayv1.HTTPBackendRef{
			weighted(backendRef("not-a-service", 7070, func(r *gatewayv1.BackendObjectReference) { r.Kind = ptr(gatewayv1.Kind("ConfigMap")) }), 0),
		}},
	}
	sink := reconcileObjects(t, hr)
	rules := sink.routes["ns/echo"]
	require.Len(t, rules, 3)

	assert.True(t, rules[0].DroppedBackends)
	assert.Equal(t, uint32(3), rules[0].DroppedBackendWeight, "2 + the default 1 + 0")
	require.Len(t, rules[0].Backends, 1, "the dropped ones are still not backends")

	assert.False(t, rules[1].DroppedBackends, "a rule whose backendRefs are all projected")
	assert.Zero(t, rules[1].DroppedBackendWeight)

	assert.True(t, rules[2].DroppedBackends, "weight 0 holds no share, and the rule still had a backendRef: it is not a rule without backends")
	assert.Zero(t, rules[2].DroppedBackendWeight)
	assert.Empty(t, rules[2].Backends)
}
