package gamma

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"aethermesh.dev/agent/internal/gatewaystatus"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// generatedService is a mesh Service as the registrar writes it: its real
// ports named p<port>-tcp with an appProtocol, beside the fixed placeholders.
func generatedService(name string, real map[int32]string) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{aetherlabels.LabelMeshService: "true"}},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "mesh", Port: 18081, TargetPort: intstr.FromInt32(18081)},
			{Name: "mesh-tcp", Port: 18082, TargetPort: intstr.FromInt32(18082)},
		}},
	}
	_, declares80 := real[80]
	if !declares80 {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80)})
	}
	for port, app := range real {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name: fmt.Sprintf("p%d-tcp", port), Port: port, TargetPort: intstr.FromInt32(port), AppProtocol: ptr(app),
		})
	}
	return svc
}

func plainService(name string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}, Spec: corev1.ServiceSpec{Ports: ports}}
}

// TestRouteStatus_PortsAreRealPorts: a parentRef and a backendRef name a REAL
// port of a Service. One that does not is said in the route's status, with the
// reasons the Gateway API defines, under aether's own controller name:
//
//   - parentRef: Accepted=False, NoMatchingParent (the route attaches to
//     nothing);
//   - backendRef to a port the Service does not have: ResolvedRefs=False,
//     BackendNotFound; to a port that is not HTTP: UnsupportedProtocol.
//
// The mesh HTTP port (18081) and an undeclared 80 of a generated Service are
// placeholders, not ports: until chart 3.0.0 a route that named one was read
// as "the port the pods declare", and its status said nothing.
func TestRouteStatus_PortsAreRealPorts(t *testing.T) {
	multi := func() *corev1.Service {
		return generatedService("multi", map[int32]string{8080: "http", 9090: "http", 9000: "tcp"})
	}
	user := func() *corev1.Service {
		return plainService("web",
			corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(8080)},
			corev1.ServicePort{Port: 81, TargetPort: intstr.FromString("admin")},
			corev1.ServicePort{Port: 53, Protocol: corev1.ProtocolUDP},
		)
	}
	type want struct {
		accepted, acceptedReason string
		resolved, resolvedReason string
	}
	ok := want{"True", "Accepted", "True", "ResolvedRefs"}
	for name, tc := range map[string]struct {
		parent      string
		parentPort  int32 // 0: none
		backend     string
		backendPort int32
		objs        []client.Object
		want        want
	}{
		"generated: real HTTP ports":                 {"multi", 9090, "multi", 8080, []client.Object{multi()}, ok},
		"generated: no parent port":                  {"multi", 0, "multi", 8080, []client.Object{multi()}, ok},
		"generated: parentRef 18081":                 {"multi", 18081, "multi", 8080, []client.Object{multi()}, want{"False", "NoMatchingParent", "True", "ResolvedRefs"}},
		"generated: parentRef to an undeclared 80":   {"multi", 80, "multi", 8080, []client.Object{multi()}, want{"False", "NoMatchingParent", "True", "ResolvedRefs"}},
		"generated: parentRef to an undeclared port": {"multi", 7777, "multi", 8080, []client.Object{multi()}, want{"False", "NoMatchingParent", "True", "ResolvedRefs"}},
		"generated: parentRef to a raw-TCP port":     {"multi", 9000, "multi", 8080, []client.Object{multi()}, want{"False", "NoMatchingParent", "True", "ResolvedRefs"}},
		"generated: backendRef 18081":                {"multi", 0, "multi", 18081, []client.Object{multi()}, want{"True", "Accepted", "False", "BackendNotFound"}},
		"generated: backendRef to an undeclared 80":  {"multi", 0, "multi", 80, []client.Object{multi()}, want{"True", "Accepted", "False", "BackendNotFound"}},
		"generated: backendRef to a raw-TCP port":    {"multi", 0, "multi", 9000, []client.Object{multi()}, want{"True", "Accepted", "False", "UnsupportedProtocol"}},
		"generated: a declared port 80 is a port": {
			"eighty", 80, "eighty", 80, []client.Object{generatedService("eighty", map[int32]string{80: "http", 8080: "http"})}, ok,
		},
		"user Service: its ports": {"web", 80, "web", 80, []client.Object{user()}, ok},
		// A named targetPort that does not resolve (no EndpointSlice of the
		// Service) is refused until it does (owner's ruling, 2026-10-13). It
		// used to be "still a port": True, and delivered to the port the pods
		// declare.
		"user Service: a named targetPort that does not resolve": {"web", 81, "web", 81, []client.Object{user()}, want{"False", "NoMatchingParent", "False", "BackendNotFound"}},
		"user Service: parentRef to a pod port it does not expose": {
			"web", 8080, "web", 80, []client.Object{user()}, want{"False", "NoMatchingParent", "True", "ResolvedRefs"},
		},
		"user Service: backendRef 18081":         {"web", 0, "web", 18081, []client.Object{user()}, want{"True", "Accepted", "False", "BackendNotFound"}},
		"user Service: backendRef to a UDP port": {"web", 0, "web", 53, []client.Object{user()}, want{"True", "Accepted", "False", "BackendNotFound"}},
		// No Service object: the mesh resolves the name through the registry,
		// and nothing is known here to judge the port by.
		"no Service object: not judged": {"ghost", 18081, "ghost", 18081, nil, ok},
	} {
		t.Run(name, func(t *testing.T) {
			hr := httpRoute("r1", "ns", tc.parent, tc.backend)
			if tc.parentPort != 0 {
				hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(tc.parentPort))
			}
			hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(tc.backendPort))
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithObjects(append([]client.Object{hr}, tc.objs...)...).
				WithStatusSubresource(&gatewayv1.HTTPRoute{}).
				Build()
			r := &Reconciler{Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true}
			_, err := r.Reconcile(context.Background(), reconcile.Request{})
			require.NoError(t, err)

			got := &gatewayv1.HTTPRoute{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
			require.Len(t, got.Status.Parents, 1)
			assert.Equal(t, gatewaystatus.MeshControllerName, got.Status.Parents[0].ControllerName, "only aether's own entry is written")
			acc := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
			res := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
			require.NotNil(t, acc)
			require.NotNil(t, res)
			assert.Equal(t, tc.want, want{string(acc.Status), acc.Reason, string(res.Status), res.Reason}, "Accepted: %s | ResolvedRefs: %s", acc.Message, res.Message)
			if acc.Status == metav1.ConditionFalse {
				assert.Contains(t, acc.Message, "attached to nothing")
			}
			if res.Status == metav1.ConditionFalse {
				assert.Contains(t, res.Message, "500")
			}
		})
	}
}

// TestRouteStatus_AnotherControllersEntryIsKept: the port verdict is written
// to aether's own RouteParentStatus and nothing else of the route's status.
func TestRouteStatus_AnotherControllersEntryIsKept(t *testing.T) {
	hr := httpRoute("r1", "ns", "multi", "multi")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(18081))
	hr.Status.Parents = []gatewayv1.RouteParentStatus{{
		ParentRef:      hr.Spec.ParentRefs[0],
		ControllerName: "example.com/other",
		Conditions:     []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted", LastTransitionTime: metav1.Now()}},
	}}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, generatedService("multi", map[int32]string{8080: "http"})).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true}
	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	require.Len(t, got.Status.Parents, 2)
	for _, p := range got.Status.Parents {
		acc := meta.FindStatusCondition(p.Conditions, "Accepted")
		require.NotNil(t, acc)
		if p.ControllerName == "example.com/other" {
			assert.Equal(t, metav1.ConditionTrue, acc.Status, "another controller's entry is not touched")
		} else {
			assert.Equal(t, metav1.ConditionFalse, acc.Status)
		}
	}
}
