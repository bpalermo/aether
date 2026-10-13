package gamma

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	ppHTTP = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	ppTCP  = registryv1.PortProtocol_PORT_PROTOCOL_TCP
	ppUDP  = registryv1.PortProtocol_PORT_PROTOCOL_UDP
)

// rowsRegistry lists the rows it holds, per service and protocol.
type rowsRegistry struct {
	registry.Registry // nil-embedded; only ListEndpoints is used
	rows              map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
	err               error
}

func (r rowsRegistry) ListEndpoints(_ context.Context, svc string, p registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	return r.rows[svc][p], r.err
}

func ppRow(ip string, classes map[uint32]registryv1.PortProtocol) *registryv1.ServiceEndpoint {
	ep := &registryv1.ServiceEndpoint{Ip: ip, PortProtocols: classes}
	for port := range classes {
		ep.Ports = append(ep.Ports, port)
	}
	return ep
}

// userFacts is what the status is judged by for a user's Service whose pods
// declare classes.
func userFacts(t *testing.T, svc *corev1.Service, pods PodPorts) portFacts {
	t.Helper()
	f := servicePortFacts(svc)
	m, _ := servicePortMap(svc)
	if classes, known := pods.PodPortClasses(context.Background(), "ns/"+svc.Name); known {
		f = f.withPodPorts(m, classes)
	}
	return f
}

// TestStatus_UserServicePortIsJudgedByWhatItsPodsDeclare: the data plane
// answers 500 for a backendRef to a port of a user's Service whose target port
// the registered pods declare raw TCP or UDP, or do not declare. The status
// said ResolvedRefs=True for it ("not checked here"): a table and a status
// built from different facts. It is now judged by the same registry rows, and
// says False whenever the table answers 500; where nothing is known (no pod
// registered) it says it did not check, and the table answers no 500 either.
func TestStatus_UserServicePortIsJudgedByWhatItsPodsDeclare(t *testing.T) {
	svc := plainService("db",
		corev1.ServicePort{Name: "web", Port: 8000, TargetPort: intstr.FromInt32(8080)},
		corev1.ServicePort{Name: "sql", Port: 5432, TargetPort: intstr.FromInt32(5432)},
		corev1.ServicePort{Name: "dns", Port: 53, TargetPort: intstr.FromInt32(5353)},
		corev1.ServicePort{Name: "gone", Port: 7000, TargetPort: intstr.FromInt32(7070)},
		corev1.ServicePort{Name: "same", Port: 9090},
		corev1.ServicePort{Name: "named", Port: 9999, TargetPort: intstr.FromString("metrics")},
	)
	row := ppRow("10.0.0.1", map[uint32]registryv1.PortProtocol{8080: ppHTTP, 5432: ppTCP, 5353: ppUDP, 9090: ppHTTP, 18081: ppHTTP})
	pods := RegistryPodPorts(rowsRegistry{rows: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		"ns/db": {registryv1.Service_PROTOCOL_HTTP: {row}, registryv1.Service_PROTOCOL_TCP: {row}, registryv1.Service_PROTOCOL_UDP: {row}},
	}})
	facts := map[string]portFacts{"ns/db": userFacts(t, svc, pods)}
	resolve := func(port int32) (bool, string, string) {
		p := gatewayv1.PortNumber(port)
		return resolveRefs(facts, "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{{Name: "db", Port: &p}}, nil)
	}

	ok, reason, msg := resolve(8000)
	assert.True(t, ok)
	assert.Equal(t, "All backend references resolved", msg, "a port whose pod port is HTTP is checked, and says so")
	assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), reason)
	ok, _, msg = resolve(9090)
	assert.True(t, ok, "no targetPort: the pod port is the Service port")
	assert.Equal(t, "All backend references resolved", msg)

	for port, pod := range map[int32]string{5432: "raw TCP", 53: "UDP"} {
		ok, reason, msg = resolve(port)
		assert.Falsef(t, ok, "port %d targets a %s pod port: the table answers 500 and the status says True", port, pod)
		assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedProtocol), reason)
		assert.Contains(t, msg, "answered 500")
	}

	ok, reason, msg = resolve(7000)
	assert.False(t, ok, "port 7000 targets a pod port no pod declares: the table answers 500 and the status says True")
	assert.Equal(t, string(gatewayv1.RouteReasonBackendNotFound), reason)
	assert.Contains(t, msg, "targets pod port 7070, which no registered pod of the service declares")
	assert.Contains(t, msg, "answered 500")

	// A named targetPort that was not resolved: not judged, and said so.
	ok, _, msg = resolve(9999)
	assert.True(t, ok)
	assert.Contains(t, msg, "Not checked: whether the pods serve ns/db:9999 as HTTP")

	// A parentRef is judged by the same facts.
	sql := gatewayv1.PortNumber(5432)
	assert.Contains(t, parentPortProblem(facts, gatewayv1.ParentReference{Name: "db", Port: &sql}, "ns"), "is not an HTTP port")
	web := gatewayv1.PortNumber(8000)
	assert.Empty(t, parentPortProblem(facts, gatewayv1.ParentReference{Name: "db", Port: &web}, "ns"))
}

// TestStatus_UserServiceWithNoRegisteredPodIsNotJudged: nothing is known of
// the pods (none registered, or the registry could not be read). The data
// plane holds no row either and answers no 500; the status says it did not
// check, and what will happen when it can.
func TestStatus_UserServiceWithNoRegisteredPodIsNotJudged(t *testing.T) {
	svc := plainService("db", corev1.ServicePort{Port: 5432, TargetPort: intstr.FromInt32(5432)})
	for name, pods := range map[string]PodPorts{
		"no row":       RegistryPodPorts(rowsRegistry{}),
		"read failure": RegistryPodPorts(rowsRegistry{err: errors.New("unavailable")}),
		"only a row without port_protocols": RegistryPodPorts(rowsRegistry{rows: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
			"ns/db": {registryv1.Service_PROTOCOL_TCP: {{Ip: "10.0.0.1", Port: 5432}}},
		}}),
	} {
		t.Run(name, func(t *testing.T) {
			facts := map[string]portFacts{"ns/db": userFacts(t, svc, pods)}
			port := gatewayv1.PortNumber(5432)
			ok, reason, msg := resolveRefs(facts, "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{{Name: "db", Port: &port}}, nil)
			assert.True(t, ok)
			assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), reason)
			assert.Contains(t, msg, "Not checked: whether the pods serve ns/db:5432 as HTTP")
			assert.Contains(t, msg, "turns False")
		})
	}
}

// TestRegistryPodPorts: every protocol's listing is read, a pod in two
// listings is one pod, the mesh's own ports are no port of a service, and a
// port the pods disagree about has no class.
func TestRegistryPodPorts(t *testing.T) {
	a := ppRow("10.0.0.1", map[uint32]registryv1.PortProtocol{8080: ppHTTP, 9000: ppTCP, 18082: ppTCP})
	b := ppRow("10.0.0.2", map[uint32]registryv1.PortProtocol{8080: ppTCP, 5353: ppUDP})
	pods := RegistryPodPorts(rowsRegistry{rows: map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
		"ns/x": {
			registryv1.Service_PROTOCOL_HTTP: {a},
			registryv1.Service_PROTOCOL_TCP:  {a, b},
			registryv1.Service_PROTOCOL_UDP:  {b},
		},
	}})
	classes, known := pods.PodPortClasses(context.Background(), "ns/x")
	require.True(t, known)
	assert.Equal(t, map[uint32]registryv1.PortProtocol{
		8080: registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED, 9000: ppTCP, 5353: ppUDP,
	}, classes)
	_, known = pods.PodPortClasses(context.Background(), "ns/none")
	assert.False(t, known)

	// A port the pods disagree about stays unverified.
	svc := plainService("x", corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(8080)})
	f := servicePortFacts(svc)
	m, _ := servicePortMap(svc)
	f = f.withPodPorts(m, classes)
	assert.True(t, f.unverified[80])
	assert.True(t, f.ports[80])
}

// TestPodPortsRecheck: a status judged by the registry is judged again on a
// timer, because no watch of the reconciler fires when a pod registers.
func TestPodPortsRecheck(t *testing.T) {
	gen := servicePortFacts(generatedService("multi", map[int32]string{8080: "http"}))
	user := servicePortFacts(plainService("db", corev1.ServicePort{Port: 5432}))
	assert.Zero(t, podPortsRecheck(map[string]portFacts{"ns/multi": gen}))
	assert.Equal(t, podPortsRecheckInterval, podPortsRecheck(map[string]portFacts{"ns/multi": gen, "ns/db": user}))
}

// podPortsFunc is a PodPorts from a function.
type podPortsFunc func(service string) (map[uint32]registryv1.PortProtocol, bool)

func (f podPortsFunc) PodPortClasses(_ context.Context, service string) (map[uint32]registryv1.PortProtocol, bool) {
	return f(service)
}

// TestReconcile_WritesTheStatusOfAUserServicePortFromWhatItsPodsDeclare is the
// whole path: a reconcile of an HTTPRoute whose backendRef names a port of a
// user's Service writes ResolvedRefs by what the registered pods of that
// Service declare, and asks to be run again, since nothing it watches changes
// when such a pod registers.
func TestReconcile_WritesTheStatusOfAUserServicePortFromWhatItsPodsDeclare(t *testing.T) {
	svc := plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	for name, tc := range map[string]struct {
		classes map[uint32]registryv1.PortProtocol
		known   bool
		status  metav1.ConditionStatus
		reason  string
	}{
		"the pods declare the target port HTTP":    {map[uint32]registryv1.PortProtocol{8080: ppHTTP}, true, metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs)},
		"the pods declare the target port raw TCP": {map[uint32]registryv1.PortProtocol{8080: ppTCP}, true, metav1.ConditionFalse, string(gatewayv1.RouteReasonUnsupportedProtocol)},
		"no pod declares the target port":          {map[uint32]registryv1.PortProtocol{9090: ppHTTP}, true, metav1.ConditionFalse, string(gatewayv1.RouteReasonBackendNotFound)},
		"no pod is registered":                     {nil, false, metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs)},
	} {
		t.Run(name, func(t *testing.T) {
			hr := httpRoute("r1", "ns", "web", "web")
			hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithObjects(hr, svc.DeepCopy()).
				WithStatusSubresource(&gatewayv1.HTTPRoute{}).
				Build()
			asked := []string{}
			r := &Reconciler{
				Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true,
				PodPorts: podPortsFunc(func(service string) (map[uint32]registryv1.PortProtocol, bool) {
					asked = append(asked, service)
					return tc.classes, tc.known
				}),
			}
			res, err := r.Reconcile(context.Background(), reconcile.Request{})
			require.NoError(t, err)
			assert.Equal(t, []string{"ns/web"}, asked, "the registry is asked for the user's Service a route names, and for nothing else")
			assert.Equal(t, podPortsRecheckInterval, res.RequeueAfter)

			got := &gatewayv1.HTTPRoute{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
			require.Len(t, got.Status.Parents, 1)
			cond := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
			require.NotNil(t, cond)
			assert.Equal(t, tc.status, cond.Status, cond.Message)
			assert.Equal(t, tc.reason, cond.Reason, cond.Message)
			if !tc.known {
				assert.Contains(t, cond.Message, "Not checked")
			}
		})
	}

	// A route that names generated Services only is not judged by the
	// registry and asks for no recheck.
	hr := httpRoute("r1", "ns", "multi", "multi")
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(8080))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, generatedService("multi", map[int32]string{8080: "http"})).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{
		Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true,
		PodPorts: podPortsFunc(func(service string) (map[uint32]registryv1.PortProtocol, bool) {
			t.Errorf("the registry was asked for %s, a generated Service", service)
			return nil, false
		}),
	}
	res, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
}
