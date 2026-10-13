package gamma

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	assert.Equal(t, []string{"*v1.Service", "*v1.EndpointSlice"}, kinds(&Reconciler{}), "no optional CRD installed: the Service and EndpointSlice watches are still there")
	assert.Equal(t,
		[]string{"*v1.GRPCRoute", "*v1beta1.ReferenceGrant", "*v1.HTTPFilter", "*v1.Service", "*v1.EndpointSlice"},
		kinds(&Reconciler{grpcRouteEnabled: true, referenceGrantEnabled: true, httpFilterEnabled: true}))

	// The EndpointSlice watch holds metadata only (every slice of the cluster
	// in every node agent is what a full informer would cost), and passes the
	// slices of the Services a route names that have a named targetPort, and
	// no other: those are the ones whose slices the reconciler reads.
	r := &Reconciler{}
	var sliceWatch *secondaryWatch
	for _, w := range r.secondaryWatches() {
		if _, ok := w.object.(*discoveryv1.EndpointSlice); ok {
			sliceWatch = &w
		}
	}
	require.NotNil(t, sliceWatch)
	require.True(t, sliceWatch.metadataOnly, "the EndpointSlice informer would hold every slice of the cluster")
	require.Len(t, sliceWatch.predicates, 1)
	sliceOf := func(ns, svc string) *metav1.PartialObjectMetadata {
		m := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: svc + "-abc"}}
		if svc != "" {
			m.Labels = map[string]string{discoveryv1.LabelServiceName: svc}
		}
		return m
	}
	passes := func(ns, svc string) bool {
		return sliceWatch.predicates[0].Create(event.CreateEvent{Object: sliceOf(ns, svc)})
	}
	assert.False(t, passes("ns", "web"), "before any reconcile no Service is concerned")
	r.setNamedTargets(map[string]struct{}{"ns/web": {}})
	assert.True(t, passes("ns", "web"), "a slice of a Service a route names that has a named targetPort")
	assert.False(t, passes("other", "web"), "the same name in another namespace")
	assert.False(t, passes("ns", "db"), "a Service with no named targetPort, or that no route names")
	assert.False(t, passes("ns", ""), "a slice of no Service")
	assert.True(t, sliceWatch.predicates[0].Delete(event.DeleteEvent{Object: sliceOf("ns", "web")}), "a slice that goes un-resolves the name")

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
