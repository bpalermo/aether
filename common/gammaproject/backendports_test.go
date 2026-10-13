package gammaproject

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// TestBackendRefPortsAgreesWithTheProjection: BackendRefPorts must keep
// exactly the refs projectBackends keeps, in the same order, so that its
// element i is the port of the projected rule's backend i. The node agent
// zips the two; a disagreement about ONE ref shifts every later port onto the
// wrong backend, which is a request delivered to another service's port.
//
// Every combination of group, kind, namespace and grant is run through both,
// alone and inside lists where it sits between two refs that are always kept.
func TestBackendRefPortsAgreesWithTheProjection(t *testing.T) {
	const routeNS, routeKind, mesh = "ns", "HTTPRoute", "mesh.local"

	groups := []*gatewayv1.Group{nil, ptr(gatewayv1.Group("")), ptr(gatewayv1.Group("example.com"))}
	kinds := []*gatewayv1.Kind{nil, ptr(gatewayv1.Kind("Service")), ptr(gatewayv1.Kind("ConfigMap"))}
	namespaces := []*gatewayv1.Namespace{nil, ptr(gatewayv1.Namespace(routeNS)), ptr(gatewayv1.Namespace("granted")), ptr(gatewayv1.Namespace("ungranted"))}
	ports := []*gatewayv1.PortNumber{nil, ptr(gatewayv1.PortNumber(9090))}
	grants := []gatewayv1beta1.ReferenceGrant{{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "granted"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: routeKind, Namespace: routeNS}},
			To:   []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}}

	first := gatewayv1.BackendObjectReference{Name: "first", Port: ptr(gatewayv1.PortNumber(1111))}
	last := gatewayv1.BackendObjectReference{Name: "last", Port: ptr(gatewayv1.PortNumber(2222))}

	kept, dropped := 0, 0
	for gi, g := range groups {
		for ki, k := range kinds {
			for ni, n := range namespaces {
				for pi, p := range ports {
					mid := gatewayv1.BackendObjectReference{Group: g, Kind: k, Namespace: n, Name: "mid", Port: p}
					name := fmt.Sprintf("group%d/kind%d/ns%d/port%d", gi, ki, ni, pi)
					t.Run(name, func(t *testing.T) {
						for _, grantSet := range [][]gatewayv1beta1.ReferenceGrant{nil, grants} {
							refs := []gatewayv1.BackendObjectReference{first, mid, last}
							httpRefs := make([]gatewayv1.HTTPBackendRef, 0, len(refs))
							for _, r := range refs {
								httpRefs = append(httpRefs, gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{BackendObjectReference: r}})
							}
							projected := projectBackends(httpRefs, routeNS, routeKind, mesh, grantSet)
							got := BackendRefPorts(refs, routeNS, routeKind, grantSet)

							require.Len(t, got, len(projected), "one port per projected backend")
							require.GreaterOrEqual(t, len(projected), 2)
							// The ends are always kept and carry their own ports, wherever the middle went.
							assert.Equal(t, "ns/first", projected[0].GetService())
							assert.Equal(t, uint32(1111), got[0])
							assert.Equal(t, "ns/last", projected[len(projected)-1].GetService())
							assert.Equal(t, uint32(2222), got[len(got)-1], "a dropped ref must not shift the ports after it")
							if len(projected) == 3 {
								kept++
								want := uint32(0)
								if p != nil {
									want = 9090
								}
								assert.Equal(t, want, got[1])
							} else {
								dropped++
							}
						}
					})
				}
			}
		}
	}
	// The table exercised both outcomes; without that the agreement is vacuous.
	assert.Positive(t, kept, "some combination is projected")
	assert.Positive(t, dropped, "some combination is dropped")
}

// TestBackendRefPorts: the plain cases, with what is dropped and why.
func TestBackendRefPorts(t *testing.T) {
	refs := []gatewayv1.BackendObjectReference{
		{Name: "a", Port: ptr(gatewayv1.PortNumber(8080))},
		{Name: "no-port"},
		{Name: "cm", Kind: ptr(gatewayv1.Kind("ConfigMap")), Port: ptr(gatewayv1.PortNumber(1))},
		{Name: "grp", Group: ptr(gatewayv1.Group("example.com")), Port: ptr(gatewayv1.PortNumber(2))},
		{Name: "x-ns", Namespace: ptr(gatewayv1.Namespace("other")), Port: ptr(gatewayv1.PortNumber(3))},
		{Name: "b", Port: ptr(gatewayv1.PortNumber(9090))},
	}
	assert.Equal(t, []uint32{8080, 0, 9090}, BackendRefPorts(refs, "ns", "HTTPRoute", nil))
	assert.Empty(t, BackendRefPorts(nil, "ns", "HTTPRoute", nil))

	// The projected proto does not carry the port (it gains the field with
	// the cross-cluster projection, proposal 043 Phase 2): a peer on the
	// previous chart minor sees exactly what it saw.
	projected := projectBackends([]gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: refs[0]}}}, "ns", "HTTPRoute", "mesh.local", nil)
	require.Len(t, projected, 1)
	assert.Equal(t, "a.ns.mesh.local", projected[0].GetCluster(), "the projected cluster is still the backend's default cluster")
}
