package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUDPCaptureListenerSelectsPerVIP is the proposal 038 gate for the UDP
// path: with the IP header reaching the socket intact, ONE listener carries
// one matcher arm per UDPRoute parent, keyed on the ClusterIP the pod dialled.
// Before 038 the listener bound a single cluster for the whole node and every
// other parent was dropped (#873).
func TestUDPCaptureListenerSelectsPerVIP(t *testing.T) {
	routes := map[string][]L4Backend{
		"ns/a": {{Service: "ns/a-be", Cluster: "udp:a-be.mesh", Weight: 1}},
		"ns/b": {{Service: "ns/b-be", Cluster: "udp:b-be.mesh", Weight: 1}},
	}
	arms := udpArmsOf(t, "pod-two", routes)
	require.Len(t, arms, 2, "one arm per UDPRoute parent")
	assert.Equal(t, "udp:a-be.mesh", arms[syntheticVIP("ns/a")], "a's VIP must select a's backend")
	assert.Equal(t, "udp:b-be.mesh", arms[syntheticVIP("ns/b")], "b's VIP must select b's backend")
	assert.NotEqual(t, syntheticVIP("ns/a"), syntheticVIP("ns/b"), "the fixture's VIPs must differ or the test proves nothing")
}

// TestUDPCaptureListenerBindsTheDialledPort pins that the listener binds
// ProxyL4OutboundPort as passed AND is transparent. The port is not a free
// variable (038 D1): Envoy replies from the socket's bound port, so it must be
// the port the client dialled, and the CNI diverts udp dport 18082 with no
// rewrite. Transparency is what lets the divert deliver to a non-local VIP.
func TestUDPCaptureListenerBindsTheDialledPort(t *testing.T) {
	routes := map[string][]L4Backend{"ns/a": {{Cluster: "udp:a.mesh", Weight: 1}}}
	l, err := GenerateUDPCaptureListener("pod-port", "/var/run/netns/x", 18082, routes, map[string]string{"ns/a": "10.96.5.5"})
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, uint32(18082), l.GetAddress().GetSocketAddress().GetPortValue())
	assert.True(t, l.GetTransparent().GetValue())
}

// TestUDPCaptureArmsKeyIsCanonical: the reconcile compares arm SETS, so the
// key must be order-independent and must move on any change to membership, a
// VIP or a cluster — the three things the pre-038 one-string compare missed.
func TestUDPCaptureArmsKeyIsCanonical(t *testing.T) {
	a := UDPCaptureArm{Service: "ns/a", ClusterIP: "10.0.0.1", Cluster: "udp:a"}
	b := UDPCaptureArm{Service: "ns/b", ClusterIP: "10.0.0.2", Cluster: "udp:b"}
	ab := UDPCaptureArmsKey([]UDPCaptureArm{a, b})
	assert.Equal(t, ab, UDPCaptureArmsKey([]UDPCaptureArm{b, a}), "order must not matter")
	assert.NotEqual(t, ab, UDPCaptureArmsKey([]UDPCaptureArm{a}), "membership must")
	b2 := b
	b2.ClusterIP = "10.0.0.3"
	assert.NotEqual(t, ab, UDPCaptureArmsKey([]UDPCaptureArm{a, b2}), "a VIP change must")
	b3 := b
	b3.Cluster = "udp:b-v2"
	assert.NotEqual(t, ab, UDPCaptureArmsKey([]UDPCaptureArm{a, b3}), "a cluster change must")
	assert.Equal(t, "", UDPCaptureArmsKey(nil))
}

// TestUDPCaptureArmsNeedAVIP: UDPCaptureArms — what the reconcile keys on —
// must not carry an arm for a parent without a ClusterIP, and must carry one
// the moment it appears (a late VIP is the change the per-push reconcile is
// there to see).
func TestUDPCaptureArmsNeedAVIP(t *testing.T) {
	routes := map[string][]L4Backend{"ns/a": {{Cluster: "udp:a.mesh", Weight: 1}}}
	assert.Empty(t, UDPCaptureArms(routes, nil))
	arms := UDPCaptureArms(routes, map[string]string{"ns/a": "10.96.1.1"})
	require.Len(t, arms, 1)
	assert.Equal(t, UDPCaptureArm{Service: "ns/a", ClusterIP: "10.96.1.1", Cluster: "udp:a.mesh"}, arms[0])
}
