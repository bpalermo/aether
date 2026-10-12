package gamma

// Third adversarial pass on the proposal-043 stack: review test, uncommitted.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// A generated Service as a chart 2.6 registrar writes it (the registrar held
// back, or a rollback in progress): the placeholders and no real port. The
// data plane does not read the Service for a service it holds an endpoint of
// (the registry decides), so a backendRef to the pods' real port 8080 is
// delivered and a parentRef that names it is attached. The status used to be
// judged by the Service alone and said the opposite of both. A Service that
// carries no real port says nothing of the service's ports: nothing is
// claimed.
func TestStatus_GeneratedServiceWithoutRealPortsIsNotJudged(t *testing.T) {
	svc := generatedService("multi", nil)
	svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "https", Port: 443, TargetPort: intstr.FromInt32(443)})
	f := servicePortFacts(svc)
	require.True(t, f.unknown)
	facts := map[string]portFacts{"ns/multi": f}
	port := gatewayv1.PortNumber(8080)

	ok, reason, msg := backendPortProblem(facts, "ns", []gatewayv1.BackendObjectReference{{Name: "multi", Port: &port}})
	assert.True(t, ok, "reason=%q message=%q", reason, msg)
	assert.Empty(t, parentPortProblem(facts, gatewayv1.ParentReference{Name: "multi", Port: &port}, "ns"))

	// With one real port it IS judged: the registrar wrote what it knows.
	h := "http"
	svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "p9090-tcp", Port: 9090, AppProtocol: &h})
	facts["ns/multi"] = servicePortFacts(svc)
	require.False(t, facts["ns/multi"].unknown)
	ok, _, _ = backendPortProblem(facts, "ns", []gatewayv1.BackendObjectReference{{Name: "multi", Port: &port}})
	assert.False(t, ok, "8080 is not a port of a Service that carries its real ports")
}

// A user Service: the status knows that the port exists, and nothing of what
// its target port serves (the pods' declaration, which the data plane checks:
// it answers 500 for a backendRef whose target port the pods declare raw TCP,
// or do not declare). ResolvedRefs stays True, because the status cannot know
// better; but it no longer says "All backend references resolved" as if it
// had checked: the condition's message names the port it did not check and
// what happens if the pods disagree.
func TestStatus_UserServicePortIsNamedAsUnchecked(t *testing.T) {
	svc := plainService("db", corev1.ServicePort{Port: 5432, TargetPort: intstr.FromInt32(5432)})
	facts := map[string]portFacts{"ns/db": servicePortFacts(svc), "ns/multi": servicePortFacts(generatedService("multi", map[int32]string{8080: "http"}))}
	port := gatewayv1.PortNumber(5432)
	ok, reason, msg := resolveRefs(facts, "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{{Name: "db", Port: &port}}, nil)
	assert.True(t, ok)
	assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), reason)
	assert.Contains(t, msg, "Not checked here: whether the pods serve ns/db:5432 as HTTP")
	assert.Contains(t, msg, "is answered 500")

	// A generated Service's port IS checked, and says so.
	gen := gatewayv1.PortNumber(8080)
	ok, _, msg = resolveRefs(facts, "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{{Name: "multi", Port: &gen}}, nil)
	assert.True(t, ok)
	assert.Equal(t, "All backend references resolved", msg)
}
