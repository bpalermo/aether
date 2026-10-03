package referencegrant

import (
	"testing"

	"github.com/stretchr/testify/assert"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// TestBackendKey covers namespace defaulting in isolation: an unset backendRef
// namespace inherits the route's, an explicit one wins.
func TestBackendKey(t *testing.T) {
	otherNS := gatewayv1.Namespace("other")
	emptyNS := gatewayv1.Namespace("")

	assert.Equal(t, "ns/svc", BackendKey(nil, "ns", "svc"))
	assert.Equal(t, "ns/svc", BackendKey(&emptyNS, "ns", "svc"))
	assert.Equal(t, "other/svc", BackendKey(&otherNS, "ns", "svc"))
}

// TestBackendPermitted covers the ReferenceGrant gate in isolation.
func TestBackendPermitted(t *testing.T) {
	otherNS := gatewayv1.Namespace("other")
	sameNS := gatewayv1.Namespace("ns")
	grants := []gatewayv1beta1.ReferenceGrant{grant("other",
		[]gatewayv1.ReferenceGrantFrom{from(gatewayv1.GroupName, "TCPRoute", "ns")},
		[]gatewayv1.ReferenceGrantTo{to("", "Service", nil)},
	)}

	assert.True(t, BackendPermitted(nil, "ns", "TCPRoute", "svc", nil),
		"same-namespace (unset) ref needs no grant")
	assert.True(t, BackendPermitted(&sameNS, "ns", "TCPRoute", "svc", nil),
		"explicit same-namespace ref needs no grant")
	assert.False(t, BackendPermitted(&otherNS, "ns", "TCPRoute", "svc", nil),
		"cross-namespace ref without a grant is not permitted")
	assert.True(t, BackendPermitted(&otherNS, "ns", "TCPRoute", "svc", grants),
		"cross-namespace ref with a matching grant is permitted")
	assert.False(t, BackendPermitted(&otherNS, "ns", "UDPRoute", "svc", grants),
		"grant naming another route kind does not permit the ref")
}

// TestBackendNamespace covers the nil-safe accessor.
func TestBackendNamespace(t *testing.T) {
	ns := gatewayv1.Namespace("other")
	assert.Equal(t, "", BackendNamespace(nil))
	assert.Equal(t, "other", BackendNamespace(&ns))
}
