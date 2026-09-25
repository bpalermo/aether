package proxy

import (
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewAppHealthProbeCluster_UDPProbesTheInboundPort is the regression test
// for #931.
//
// A UDP workload listens on a UDP socket and nothing else. Probing its
// application port with a TCP connect can never succeed, so the pod was marked
// UNHEALTHY forever; that verdict was cloned into the plaintext udp: cluster,
// whose HealthyPanicThreshold is 0, leaving udp_proxy with an empty healthy set
// and dropping every datagram with no log, no NACK and no stat.
//
// The probe therefore targets the mesh inbound port, which really does answer a
// TCP connect.
func TestNewAppHealthProbeCluster_UDPProbesTheInboundPort(t *testing.T) {
	const appUDPPort = 9001

	udpC := NewAppHealthProbeCluster(
		"health_p", AppAddress{Netns: "/var/run/netns/x"}, appUDPPort, "/-/-/ready",
		registryv1.Service_PROTOCOL_UDP)

	require.Len(t, udpC.GetHealthChecks(), 1)
	hc := udpC.GetHealthChecks()[0]
	require.NotNil(t, hc.GetTcpHealthCheck(), "a UDP service has no HTTP readiness surface")
	assert.Nil(t, hc.GetHttpHealthCheck())
	assert.Empty(t, hc.GetTcpHealthCheck().GetSend().GetText(), "connect-only: no payload")
	assert.Empty(t, hc.GetTcpHealthCheck().GetReceive())

	addr := udpC.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].
		GetEndpoint().GetAddress().GetSocketAddress()
	require.NotNil(t, addr)
	assert.EqualValues(t, defaultInboundPort, addr.GetPortValue(),
		"the probe must NOT target the application UDP port: a TCP connect there can never succeed")
	assert.NotEqualValues(t, appUDPPort, addr.GetPortValue())

	// The probe stays a TCP socket even though the service is UDP: it is
	// checking the mesh inbound listener, which is TCP.
	assert.Equal(t, "TCP", addr.GetProtocol().String())
}

// TestNewAppHealthProbeCluster_NonUDPStillProbesTheAppPort pins the other side,
// so the #931 fix cannot quietly redirect every workload's liveness probe away
// from the application it is supposed to be checking.
func TestNewAppHealthProbeCluster_NonUDPStillProbesTheAppPort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol registryv1.Service_Protocol
	}{
		{"http", registryv1.Service_PROTOCOL_HTTP},
		{"tcp", registryv1.Service_PROTOCOL_TCP},
		{"unspecified", registryv1.Service_PROTOCOL_UNSPECIFIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewAppHealthProbeCluster(
				"health_p", AppAddress{Netns: "/var/run/netns/x"}, 8080, "/-/-/ready", tc.protocol)
			addr := c.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].
				GetEndpoint().GetAddress().GetSocketAddress()
			assert.EqualValues(t, 8080, addr.GetPortValue(),
				"only UDP moves off the application port")
		})
	}
}

// TestNewAppDeliveryClusters_ProtocolComesFromEndpointmeta pins that the probe
// shape is decided by the registry's OWN annotation parser, not by a second
// open-coded comparison in this package.
//
// The two disagreeing is how #931 stayed invisible: the pod registered one way
// and was probed another, and nothing compared the two.
func TestNewAppDeliveryClusters_ProtocolComesFromEndpointmeta(t *testing.T) {
	pod := func(protocol string) *cniv1.CNIPod {
		return &cniv1.CNIPod{
			Name:             "p-0",
			Namespace:        "aether-test",
			NetworkNamespace: "/var/run/netns/x",
			Annotations: map[string]string{
				aetherannotations.AnnotationEndpointProtocol: protocol,
				aetherannotations.AnnotationEndpointPort:     "9001",
			},
		}
	}

	_, udpHealth := NewAppDeliveryClusters(pod(aetherannotations.ProtocolUDP), "")
	require.NotNil(t, udpHealth)
	udpAddr := udpHealth.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].
		GetEndpoint().GetAddress().GetSocketAddress()
	assert.EqualValues(t, defaultInboundPort, udpAddr.GetPortValue(),
		`a pod annotated protocol: "udp" must be probed at the inbound port`)

	_, tcpHealth := NewAppDeliveryClusters(pod(aetherannotations.ProtocolTCP), "")
	require.NotNil(t, tcpHealth)
	tcpAddr := tcpHealth.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].
		GetEndpoint().GetAddress().GetSocketAddress()
	assert.EqualValues(t, 9001, tcpAddr.GetPortValue())

	// An unparseable value degrades to HTTP, reproducing what the previous
	// equality check did with anything other than "tcp". Registration rejects
	// it separately and loudly, so this path is not the place to fail.
	_, junkHealth := NewAppDeliveryClusters(pod("sctp"), "")
	require.NotNil(t, junkHealth)
	require.NotNil(t, junkHealth.GetHealthChecks()[0].GetHttpHealthCheck(),
		"an unrecognised protocol annotation degrades to the HTTP probe")
}
