package gamma

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// TestSecondaryWatches: what the reconciler watches besides HTTPRoute. Every
// kind Reconcile reads must be here, or a change to it is seen only when
// something else triggers a reconcile.
//
// The Service watch is unconditional (a Service is a port map with or without
// any optional CRD) and carries the ports predicate: a Service whose ports
// change re-projects, one whose status or annotations change does not.
func TestSecondaryWatches(t *testing.T) {
	kinds := func(r *Reconciler) []string {
		var out []string
		for _, w := range r.secondaryWatches() {
			out = append(out, fmt.Sprintf("%T", w.object))
		}
		return out
	}
	assert.Equal(t, []string{"*v1.Service"}, kinds(&Reconciler{}), "no optional CRD installed: the Service watch is still there")
	assert.Equal(t,
		[]string{"*v1.GRPCRoute", "*v1beta1.ReferenceGrant", "*v1.HTTPFilter", "*v1.Service"},
		kinds(&Reconciler{grpcRouteEnabled: true, referenceGrantEnabled: true, httpFilterEnabled: true}))

	var svcWatch *secondaryWatch
	for _, w := range (&Reconciler{}).secondaryWatches() {
		if _, ok := w.object.(*corev1.Service); ok {
			svcWatch = &w
		}
	}
	require.NotNil(t, svcWatch)
	require.Len(t, svcWatch.predicates, 1, "the Service watch is filtered to port changes")
	old := userService("ns", "echo", corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(8080)})
	moved := old.DeepCopy()
	moved.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	noise := old.DeepCopy()
	noise.Annotations = map[string]string{"a": "b"}
	assert.True(t, svcWatch.predicates[0].Update(event.UpdateEvent{ObjectOld: old, ObjectNew: moved}), "a targetPort change is a port-map change")
	assert.False(t, svcWatch.predicates[0].Update(event.UpdateEvent{ObjectOld: old, ObjectNew: noise}))
}
