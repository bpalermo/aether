package proxy

import (
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
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

// TestUDPLoadAssignment_DropsWaypointedEndpoints is the regression test for #932.
//
// The source assignment is the service's shared bare-name one. For a
// remote-cluster endpoint the HTTP path rewrote its address to the destination
// NODE's IP plus the east/west tunnel port, and marked it with waypoint subset
// metadata. The old clone-and-patch rewrote only the PORT, so the node IP
// survived and the datagram went to a host that was never the backend.
func TestUDPLoadAssignment_DropsWaypointedEndpoints(t *testing.T) {
	waypointed := &endpointv3.LbEndpoint{
		HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
			Address: sockAddr("10.0.0.99", 18009), // node IP + tunnel port
		}},
		Metadata: &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
			"envoy.lb": {Fields: map[string]*structpb.Value{
				"waypoint": structpb.NewStringValue("true"),
			}},
		}},
		HealthStatus: corev3.HealthStatus_HEALTHY,
	}
	local := &endpointv3.LbEndpoint{
		HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
			Address:           sockAddr("10.0.0.10", 18008), // pod IP + mesh inbound
			HealthCheckConfig: &endpointv3.Endpoint_HealthCheckConfig{DisableActiveHealthCheck: true},
		}},
		Metadata: &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
			"envoy.lb": {Fields: map[string]*structpb.Value{
				"tenant": structpb.NewStringValue("a"),
			}},
		}},
		HealthStatus: corev3.HealthStatus_HEALTHY,
	}

	src := &endpointv3.ClusterLoadAssignment{
		ClusterName: "aether-test/svc",
		Endpoints: []*endpointv3.LocalityLbEndpoints{{
			LbEndpoints: []*endpointv3.LbEndpoint{waypointed, local},
			Priority:    3,
		}},
	}

	got := UDPLoadAssignment(src, "udp:svc.aether-test.aether.internal", 9001)
	require.NotNil(t, got)
	require.Len(t, got.GetEndpoints(), 1)
	lbs := got.GetEndpoints()[0].GetLbEndpoints()

	require.Len(t, lbs, 1, "the waypointed endpoint must be dropped, not rewritten")
	sa := lbs[0].GetEndpoint().GetAddress().GetSocketAddress()
	assert.Equal(t, "10.0.0.10", sa.GetAddress(), "only the pod-addressed endpoint survives")
	assert.EqualValues(t, 9001, sa.GetPortValue())
	assert.Equal(t, corev3.SocketAddress_UDP, sa.GetProtocol())

	// The node IP must not appear anywhere: that was the #932 failure.
	assert.NotEqual(t, "10.0.0.99", sa.GetAddress())

	// Health status DOES survive -- post-#936 it is a real signal (the probe
	// targets the mesh inbound port, which a UDP pod really does answer).
	assert.Equal(t, corev3.HealthStatus_HEALTHY, lbs[0].GetHealthStatus())

	// Metadata and HealthCheckConfig do not: this cluster has no subset LB and
	// no active health check, so both describe mechanisms that do not exist here.
	assert.Nil(t, lbs[0].GetMetadata(), "subset metadata is inert on the udp: cluster")
	assert.Nil(t, lbs[0].GetEndpoint().GetHealthCheckConfig(),
		"there is no active health check to opt out of")

	// Priority is flattened: a lone endpoint left in a non-zero tier sits in an
	// empty fallback tier Envoy will not use.
	assert.EqualValues(t, 0, got.GetEndpoints()[0].GetPriority())

	// And the source is untouched -- it is the SHARED bare-name assignment.
	assert.Len(t, src.GetEndpoints()[0].GetLbEndpoints(), 2, "UDPLoadAssignment must not mutate src")
	assert.EqualValues(t, 3, src.GetEndpoints()[0].GetPriority())
}

// TestUDPLoadAssignment_AllWaypointedYieldsNoEndpoints pins the deliberate
// consequence: a service reachable ONLY through the waypoint has no UDP path at
// all, and that shows up as an empty endpoint set -- which udp_no_healthy_backend
// counts and logs (#931) rather than being delivered to the wrong host.
func TestUDPLoadAssignment_AllWaypointedYieldsNoEndpoints(t *testing.T) {
	wp := func(ip string) *endpointv3.LbEndpoint {
		return &endpointv3.LbEndpoint{
			HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
				Address: sockAddr(ip, 18009),
			}},
			Metadata: &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
				"envoy.lb": {Fields: map[string]*structpb.Value{
					"waypoint": structpb.NewStringValue("true"),
				}},
			}},
		}
	}
	src := &endpointv3.ClusterLoadAssignment{
		ClusterName: "aether-test/remote",
		Endpoints: []*endpointv3.LocalityLbEndpoints{{
			LbEndpoints: []*endpointv3.LbEndpoint{wp("10.0.0.98"), wp("10.0.0.99")},
		}},
	}

	got := UDPLoadAssignment(src, "udp:remote.aether-test.aether.internal", 5353)
	require.NotNil(t, got)
	require.Len(t, got.GetEndpoints(), 1)
	assert.Empty(t, got.GetEndpoints()[0].GetLbEndpoints(),
		"a waypoint-only service is genuinely unreachable over the plaintext UDP floor")
}

func sockAddr(ip string, port uint32) *corev3.Address {
	return &corev3.Address{Address: &corev3.Address_SocketAddress{
		SocketAddress: &corev3.SocketAddress{
			Protocol:      corev3.SocketAddress_TCP,
			Address:       ip,
			PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: port},
		},
	}}
}
