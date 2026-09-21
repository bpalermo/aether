package l4project

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

func ptr[T any](v T) *T { return &v }

// tcpCluster / udpCluster mirror proxy.TCPClusterName / proxy.UDPClusterName without
// importing agent internals (common must not): "<proto>:<svc>.<ns>.<domain>".
func tcpCluster(domain string) ClusterNameFunc {
	return func(key string, port uint32) string {
		base := "tcp:" + fqdn(key, domain)
		if port == 0 {
			return base
		}
		return fmt.Sprintf("%s:%d", base, port)
	}
}

func udpCluster(domain string) ClusterNameFunc {
	return func(key string, port uint32) string {
		base := "udp:" + fqdn(key, domain)
		if port == 0 {
			return base
		}
		return fmt.Sprintf("%s:%d", base, port)
	}
}

func fqdn(key, domain string) string {
	for i := range key {
		if key[i] == '/' {
			return key[i+1:] + "." + key[:i] + "." + domain
		}
	}
	return key + "." + domain
}

func backendRef(name string) gatewayv1.BackendRef {
	return gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(name)},
	}
}

func weighted(name string, weight int32) gatewayv1.BackendRef {
	ref := backendRef(name)
	ref.Weight = ptr(weight)
	return ref
}

// crossNSGrant is a ReferenceGrant in namespace "other" permitting routes of kind
// routeKind in namespace "ns" to reference any core Service.
func crossNSGrant(routeKind string) gatewayv1beta1.ReferenceGrant {
	return gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{
				Group: gatewayv1.GroupName, Kind: gatewayv1.Kind(routeKind), Namespace: "ns",
			}},
			To: []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}
}

// TestBackends is the single source of truth for L4 backendRef projection semantics,
// shared by the node agent's capture-path L4 reconciler (TCPRoute/TLSRoute/UDPRoute)
// and the edge gateway reconciler. It is the union of the two test sets those
// reconcilers used to carry separately.
func TestBackends(t *testing.T) {
	otherNS := gatewayv1.Namespace("other")

	tests := []struct {
		name         string
		refs         []gatewayv1.BackendRef
		routeNS      string
		routeKind    string
		grants       []gatewayv1beta1.ReferenceGrant
		clusterName  ClusterNameFunc
		wantBackends []Backend
	}{
		{
			name:        "single backend resolves to a TCP cluster",
			refs:        []gatewayv1.BackendRef{weighted("svc-a", 1)},
			routeNS:     "ns",
			routeKind:   "TCPRoute",
			clusterName: tcpCluster("aether.internal"),
			wantBackends: []Backend{
				{Service: "ns/svc-a", Cluster: "tcp:svc-a.ns.aether.internal", Weight: 1},
			},
		},
		{
			name:        "weighted backends keep their order and weights",
			refs:        []gatewayv1.BackendRef{weighted("svc-v1", 90), weighted("svc-v2", 10)},
			routeNS:     "ns",
			routeKind:   "TCPRoute",
			clusterName: tcpCluster("mesh"),
			wantBackends: []Backend{
				{Service: "ns/svc-v1", Cluster: "tcp:svc-v1.ns.mesh", Weight: 90},
				{Service: "ns/svc-v2", Cluster: "tcp:svc-v2.ns.mesh", Weight: 10},
			},
		},
		{
			// The edge's TestBuildL4Backends case: two backends, TCPClusterName shape.
			name:        "edge-style multi-backend split",
			refs:        []gatewayv1.BackendRef{weighted("pg", 1), weighted("cache", 2)},
			routeNS:     "ns",
			routeKind:   "TCPRoute",
			clusterName: tcpCluster("aether.internal"),
			wantBackends: []Backend{
				{Service: "ns/pg", Cluster: "tcp:pg.ns.aether.internal", Weight: 1},
				{Service: "ns/cache", Cluster: "tcp:cache.ns.aether.internal", Weight: 2},
			},
		},
		{
			name: "foreign group and non-Service kind are skipped",
			refs: []gatewayv1.BackendRef{
				{BackendObjectReference: gatewayv1.BackendObjectReference{
					Group: ptr(gatewayv1.Group("apps")), Name: "should-skip",
				}},
				{BackendObjectReference: gatewayv1.BackendObjectReference{
					Kind: ptr(gatewayv1.Kind("ServiceImport")), Name: "also-skip",
				}},
				backendRef("keep-me"),
			},
			routeNS:     "ns",
			routeKind:   "TCPRoute",
			clusterName: tcpCluster("aether.internal"),
			wantBackends: []Backend{
				{Service: "ns/keep-me", Cluster: "tcp:keep-me.ns.aether.internal", Weight: 1},
			},
		},
		{
			name: "explicit core group and Service kind are kept",
			refs: []gatewayv1.BackendRef{
				{BackendObjectReference: gatewayv1.BackendObjectReference{
					Group: ptr(gatewayv1.Group("")), Kind: ptr(gatewayv1.Kind("Service")), Name: "explicit",
				}},
			},
			routeNS:     "ns",
			routeKind:   "TCPRoute",
			clusterName: tcpCluster("aether.internal"),
			wantBackends: []Backend{
				{Service: "ns/explicit", Cluster: "tcp:explicit.ns.aether.internal", Weight: 1},
			},
		},
		{
			name:        "empty backendRef name is skipped",
			refs:        []gatewayv1.BackendRef{backendRef(""), backendRef("real")},
			routeNS:     "ns",
			routeKind:   "TCPRoute",
			clusterName: tcpCluster("aether.internal"),
			wantBackends: []Backend{
				{Service: "ns/real", Cluster: "tcp:real.ns.aether.internal", Weight: 1},
			},
		},
		{
			name:        "nil weight defaults to 1",
			refs:        []gatewayv1.BackendRef{backendRef("svc-d")},
			routeNS:     "ns",
			routeKind:   "TCPRoute",
			clusterName: tcpCluster("aether.internal"),
			wantBackends: []Backend{
				{Service: "ns/svc-d", Cluster: "tcp:svc-d.ns.aether.internal", Weight: 1},
			},
		},
		{
			// #492: an explicit 0 is DRAIN and must reach the proxy layer as 0, not be
			// normalised to 1. The Envoy builders omit zero-weight backends.
			name:        "explicit weight 0 is preserved as drain",
			refs:        []gatewayv1.BackendRef{weighted("draining", 0), weighted("live", 100)},
			routeNS:     "ns",
			routeKind:   "TCPRoute",
			clusterName: tcpCluster("aether.internal"),
			wantBackends: []Backend{
				{Service: "ns/draining", Cluster: "tcp:draining.ns.aether.internal", Weight: 0},
				{Service: "ns/live", Cluster: "tcp:live.ns.aether.internal", Weight: 100},
			},
		},
		{
			name: "ungranted cross-namespace backendRef is dropped",
			refs: []gatewayv1.BackendRef{
				backendRef("local"),
				{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "remote", Namespace: &otherNS}},
			},
			routeNS:     "ns",
			routeKind:   "TCPRoute",
			clusterName: tcpCluster("aether.internal"),
			wantBackends: []Backend{
				{Service: "ns/local", Cluster: "tcp:local.ns.aether.internal", Weight: 1},
			},
		},
		{
			// A granted cross-namespace backend resolves to its OWN "other/remote" key
			// (backendRef namespace), not the route's "ns" (020 Part 1).
			name: "granted cross-namespace backendRef keeps its own namespace key",
			refs: []gatewayv1.BackendRef{
				backendRef("local"),
				{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "remote", Namespace: &otherNS}},
			},
			routeNS:     "ns",
			routeKind:   "TCPRoute",
			grants:      []gatewayv1beta1.ReferenceGrant{crossNSGrant("TCPRoute")},
			clusterName: tcpCluster("aether.internal"),
			wantBackends: []Backend{
				{Service: "ns/local", Cluster: "tcp:local.ns.aether.internal", Weight: 1},
				{Service: "other/remote", Cluster: "tcp:remote.other.aether.internal", Weight: 1},
			},
		},
		{
			// The grant's "from" kind must match the referring route's kind.
			name: "grant for another route kind does not permit the ref",
			refs: []gatewayv1.BackendRef{
				{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "remote", Namespace: &otherNS}},
			},
			routeNS:      "ns",
			routeKind:    "TLSRoute",
			grants:       []gatewayv1beta1.ReferenceGrant{crossNSGrant("TCPRoute")},
			clusterName:  tcpCluster("aether.internal"),
			wantBackends: []Backend{},
		},
		{
			// UDP backends resolve to "udp:" clusters (plain EDS, no mTLS).
			name:        "UDP cluster naming",
			refs:        []gatewayv1.BackendRef{weighted("svc-c", 5)},
			routeNS:     "ns",
			routeKind:   "UDPRoute",
			clusterName: udpCluster("aether.internal"),
			wantBackends: []Backend{
				{Service: "ns/svc-c", Cluster: "udp:svc-c.ns.aether.internal", Weight: 5},
			},
		},
		{
			name:         "no refs yields an empty, non-nil slice",
			refs:         nil,
			routeNS:      "ns",
			routeKind:    "TCPRoute",
			clusterName:  tcpCluster("aether.internal"),
			wantBackends: []Backend{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Backends(tt.refs, tt.routeNS, tt.routeKind, tt.grants, tt.clusterName)
			require.NotNil(t, got, "Backends must never return nil")
			assert.Equal(t, tt.wantBackends, got)
		})
	}
}

// TestBackendServiceKey covers namespace defaulting in isolation: an unset backendRef
// namespace inherits the route's, an explicit one wins.
func TestBackendServiceKey(t *testing.T) {
	otherNS := gatewayv1.Namespace("other")
	emptyNS := gatewayv1.Namespace("")

	assert.Equal(t, "ns/svc", backendServiceKey(nil, "ns", "svc"))
	assert.Equal(t, "ns/svc", backendServiceKey(&emptyNS, "ns", "svc"))
	assert.Equal(t, "other/svc", backendServiceKey(&otherNS, "ns", "svc"))
}

// TestBackendPermitted covers the ReferenceGrant gate in isolation.
func TestBackendPermitted(t *testing.T) {
	otherNS := gatewayv1.Namespace("other")
	sameNS := gatewayv1.Namespace("ns")
	grants := []gatewayv1beta1.ReferenceGrant{crossNSGrant("TCPRoute")}

	assert.True(t, backendPermitted(nil, "ns", "TCPRoute", "svc", nil),
		"same-namespace (unset) ref needs no grant")
	assert.True(t, backendPermitted(&sameNS, "ns", "TCPRoute", "svc", nil),
		"explicit same-namespace ref needs no grant")
	assert.False(t, backendPermitted(&otherNS, "ns", "TCPRoute", "svc", nil),
		"cross-namespace ref without a grant is not permitted")
	assert.True(t, backendPermitted(&otherNS, "ns", "TCPRoute", "svc", grants),
		"cross-namespace ref with a matching grant is permitted")
	assert.False(t, backendPermitted(&otherNS, "ns", "UDPRoute", "svc", grants),
		"grant naming another route kind does not permit the ref")
}

// TestDerefBackendNamespace covers the nil-safe accessor.
func TestDerefBackendNamespace(t *testing.T) {
	ns := gatewayv1.Namespace("other")
	assert.Equal(t, "", derefBackendNamespace(nil))
	assert.Equal(t, "other", derefBackendNamespace(&ns))
}

// TestBackends_PortQualified covers proposal 037 Phase 3: a backendRef's port
// selects that port's cluster.
//
// Before Phase 3 the port was ignored, because the TCP floor addressed one port
// per service and there was nothing to select. Now that a service can carry
// several raw-TCP ports, ignoring it would silently send a route for :5432 to
// whatever the floor forwards to — a misroute with no error anywhere.
func TestBackends_PortQualified(t *testing.T) {
	port := func(p int32) *gatewayv1.PortNumber {
		pn := gatewayv1.PortNumber(p)
		return &pn
	}

	tests := []struct {
		name string
		refs []gatewayv1.BackendRef
		want string
	}{
		{
			name: "no port: the service's default floor cluster (every pre-037 route)",
			refs: []gatewayv1.BackendRef{{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "echo"}}},
			want: "tcp:echo.default.aether.internal",
		},
		{
			name: "an explicit port selects that port's cluster",
			refs: []gatewayv1.BackendRef{{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "echo", Port: port(5432)}}},
			want: "tcp:echo.default.aether.internal:5432",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Backends(tt.refs, "default", "TCPRoute", nil, tcpCluster("aether.internal"))
			require.Len(t, got, 1)
			assert.Equal(t, tt.want, got[0].Cluster)
		})
	}
}

// TestBackends_UDPIgnoresPort: UDP is deliberately not port-qualified. The UDP
// floor already addresses backends by their registered application port
// (proposal 018 Phase 3b), and proposal 037 is a TCP/HTTP change.
func TestBackends_UDPIgnoresPort(t *testing.T) {
	pn := gatewayv1.PortNumber(5353)
	got := Backends(
		[]gatewayv1.BackendRef{{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "dns", Port: &pn}}},
		"default", "UDPRoute", nil, udpCluster("aether.internal"),
	)
	require.Len(t, got, 1)
	assert.Equal(t, "udp:dns.default.aether.internal:5353", got[0].Cluster,
		"the shared test namer qualifies; the AGENT's UDP namer ignores the port — see buildUDPL4Backends")
}
