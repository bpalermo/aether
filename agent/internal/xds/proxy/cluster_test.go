package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSocketPath = "/var/lib/kubelet/pods/uid-1/volumes/kubernetes.io~empty-dir/uds/app.sock"

// TestNewAppCluster_TCPDelivery pins the default delivery shape: loopback on the
// app port, dialed from inside the pod's network namespace.
func TestNewAppCluster_TCPDelivery(t *testing.T) {
	c := NewAppCluster("app_p_8080", AppAddress{Netns: "/var/run/netns/x"}, 8080, false)

	ep := c.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress()
	require.NotNil(t, ep.GetSocketAddress(), "TCP delivery uses a socket address")
	assert.Equal(t, appLoopbackAddress, ep.GetSocketAddress().GetAddress())
	assert.Equal(t, uint32(8080), ep.GetSocketAddress().GetPortValue())
	assert.Nil(t, ep.GetPipe())
	assert.Equal(t, "/var/run/netns/x", c.GetUpstreamBindConfig().GetSourceAddress().GetNetworkNamespaceFilepath(),
		"the loopback dial must be bound into the pod's netns or it reaches the agent")
}

// TestNewAppCluster_PipeDelivery covers UDS delivery (proposal 034 Phase 1): the
// endpoint is a pipe at the socket's host path and there is NO upstream bind
// config — a pathname socket is reached through the mount namespace, and an
// AF_UNIX connect has no source address to bind.
func TestNewAppCluster_PipeDelivery(t *testing.T) {
	c := NewAppCluster("app_p_8080", AppAddress{Netns: "/var/run/netns/x", Pipe: testSocketPath}, 8080, false)

	ep := c.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress()
	require.NotNil(t, ep.GetPipe(), "UDS delivery uses a pipe address")
	assert.Equal(t, testSocketPath, ep.GetPipe().GetPath())
	assert.Nil(t, ep.GetSocketAddress())
	assert.Nil(t, c.GetUpstreamBindConfig(), "a pipe upstream must carry no bind config (UDS is mount-ns-scoped)")
	assert.Equal(t, "app", c.GetAltStatName(), "the stats collapse is unchanged by the delivery transport")
}

// TestNewAppCluster_PipeDeliveryHTTP2 verifies the per-port app protocol still
// applies over a pipe (a gRPC app on a socket is the motivating case).
func TestNewAppCluster_PipeDeliveryHTTP2(t *testing.T) {
	c := NewAppCluster("app_p_9090", AppAddress{Pipe: testSocketPath}, 9090, true)

	assert.Contains(t, c.GetTypedExtensionProtocolOptions(), "envoy.extensions.upstreams.http.v3.HttpProtocolOptions")
	assert.Nil(t, c.GetUpstreamBindConfig())
}

// TestNewAppHealthProbeCluster_PipeDelivery verifies the delegated-liveness probe
// follows delivery onto the socket in both probe modes: the HTTP check keeps its
// Host/path machinery (HTTP/1.1 over a pipe upstream), and the TCP variant stays
// connect-only — on a pipe that degrades to "the socket exists and accepts".
func TestNewAppHealthProbeCluster_PipeDelivery(t *testing.T) {
	httpC := NewAppHealthProbeCluster("health_p", AppAddress{Pipe: testSocketPath}, 8080, "/healthz", false)
	ep := httpC.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress()
	require.NotNil(t, ep.GetPipe())
	assert.Equal(t, testSocketPath, ep.GetPipe().GetPath())
	assert.Nil(t, httpC.GetUpstreamBindConfig())
	require.Len(t, httpC.GetHealthChecks(), 1)
	hhc := httpC.GetHealthChecks()[0].GetHttpHealthCheck()
	require.NotNil(t, hhc, "HTTP service: HTTP health check over the pipe")
	assert.Equal(t, appHealthCheckHost, hhc.GetHost())
	assert.Equal(t, "/healthz", hhc.GetPath())
	assert.Empty(t, httpC.GetAltStatName(), "the probe cluster's stats stay per-pod")

	tcpC := NewAppHealthProbeCluster("health_p", AppAddress{Pipe: testSocketPath}, 9000, "/healthz", true)
	require.NotNil(t, tcpC.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetPipe())
	assert.Nil(t, tcpC.GetUpstreamBindConfig())
	require.Len(t, tcpC.GetHealthChecks(), 1)
	require.NotNil(t, tcpC.GetHealthChecks()[0].GetTcpHealthCheck())
	assert.Empty(t, tcpC.GetHealthChecks()[0].GetTcpHealthCheck().GetSend())
	assert.Empty(t, tcpC.GetHealthChecks()[0].GetTcpHealthCheck().GetReceive())
}
