package proxy

import (
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testNodeIdentity = "spiffe://" + testTrustDomain + "/ns/aether-system/sa/aether-agent"

func testInboundReadyCluster(t *testing.T) *clusterv3.Cluster {
	t.Helper()
	pod := sourceTestPod()
	return NewInboundReadyProbeCluster(
		InboundReadyClusterName(pod),
		pod.GetNetworkNamespace(),
		testNodeIdentity,
		"spiffe://"+testTrustDomain,
		SpiffeIDFromPod(pod, testTrustDomain),
	)
}

func inboundReadyTLS(t *testing.T, c *clusterv3.Cluster) *tlsv3.UpstreamTlsContext {
	t.Helper()
	require.NotNil(t, c.GetTransportSocket())
	var ctx tlsv3.UpstreamTlsContext
	require.NoError(t, c.GetTransportSocket().GetTypedConfig().UnmarshalTo(&ctx))
	return &ctx
}

// TestInboundReadyProbeClusterAddressing: the probe must dial the pod's OWN
// mesh inbound listener — 127.0.0.1:18008 bound into the pod's netns, exactly
// where NewInboundListener binds 0.0.0.0:18008.
func TestInboundReadyProbeClusterAddressing(t *testing.T) {
	c := testInboundReadyCluster(t)

	assert.Equal(t, "inboundready_"+testSourcePod, c.GetName())
	assert.Equal(t, clusterv3.Cluster_STATIC, c.GetClusterDiscoveryType().(*clusterv3.Cluster_Type).Type)

	endpoints := c.GetLoadAssignment().GetEndpoints()
	require.Len(t, endpoints, 1)
	require.Len(t, endpoints[0].GetLbEndpoints(), 1)
	sa := endpoints[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	require.NotNil(t, sa)
	assert.Equal(t, appLoopbackAddress, sa.GetAddress())
	assert.Equal(t, uint32(defaultInboundPort), sa.GetPortValue())

	bind := c.GetUpstreamBindConfig().GetSourceAddress()
	require.NotNil(t, bind, "the dial must be bound into the pod's netns or it reaches the agent's loopback")
	assert.Equal(t, "/var/run/netns/cni-a", bind.GetNetworkNamespaceFilepath())

	assert.Empty(t, c.GetAltStatName(),
		"stats must stay per-pod: the health gateway's health_check filter reads THIS cluster's membership gauges")
}

// TestInboundReadyProbeClusterTLS pins what a passing probe actually proves:
// the node presents its own SVID, the peer is validated against the mesh trust
// bundle, and the peer's URI SAN must be exactly this pod's SPIFFE ID. ALPN h2
// with no SNI lands the handshake on the inbound listener's no-SNI HCM chain.
func TestInboundReadyProbeClusterTLS(t *testing.T) {
	ctx := inboundReadyTLS(t, testInboundReadyCluster(t))
	common := ctx.GetCommonTlsContext()

	assert.Empty(t, ctx.GetSni(), "no SNI: a port SNI would select a per-port chain instead of the always-present h2 chain")
	assert.Equal(t, []string{"h2"}, common.GetAlpnProtocols(),
		"ALPN h2 selects the no-SNI HCM chain; no ALPN would hit the TCP floor and dial the app on every probe")

	certs := common.GetTlsCertificateSdsSecretConfigs()
	require.Len(t, certs, 1)
	assert.Equal(t, testNodeIdentity, certs[0].GetName(),
		"the probe is node-originated: it presents the node SVID, the same secret the matcher's no-match path uses")

	combined := common.GetCombinedValidationContext()
	require.NotNil(t, combined, "SAN pinning requires the combined validation context")
	assert.Equal(t, "spiffe://"+testTrustDomain, combined.GetValidationContextSdsSecretConfig().GetName())

	sans := combined.GetDefaultValidationContext().GetMatchTypedSubjectAltNames()
	require.Len(t, sans, 1, "exactly one expected server identity: this pod")
	assert.Equal(t, tlsv3.SubjectAltNameMatcher_URI, sans[0].GetSanType())
	assert.Equal(t, testSourceIdentity, sans[0].GetMatcher().GetExact(),
		"a probe that passes because SOMETHING answered on :18008 proves nothing")
}

// TestInboundReadyProbeClusterHealthCheck: TCP with an EMPTY send payload.
// Envoy's TCP health-check session succeeds on the Connected event, and a
// connection with a TLS transport socket does not raise Connected until the
// handshake completes — so connect-only + TLS is exactly "the listener is
// listening, has a certificate, and it verifies as this pod". Any send/receive
// payload here would turn it into an application-level probe and change that
// meaning.
func TestInboundReadyProbeClusterHealthCheck(t *testing.T) {
	c := testInboundReadyCluster(t)
	require.Len(t, c.GetHealthChecks(), 1)
	hc := c.GetHealthChecks()[0]

	tcp, ok := hc.GetHealthChecker().(*corev3.HealthCheck_TcpHealthCheck_)
	require.True(t, ok, "must be a TCP health check, not HTTP")
	assert.Nil(t, tcp.TcpHealthCheck.GetSend(), "connect-only: a send payload would probe the application, not the handshake")
	assert.Empty(t, tcp.TcpHealthCheck.GetReceive())

	assert.Equal(t, float64(5), hc.GetInterval().AsDuration().Seconds())
	assert.Equal(t, float64(1), hc.GetTimeout().AsDuration().Seconds())
	assert.Equal(t, uint32(1), hc.GetHealthyThreshold().GetValue())
	assert.Equal(t, uint32(2), hc.GetUnhealthyThreshold().GetValue())
	// Never-routed clusters otherwise fall to Envoy's 60s no-traffic cadence,
	// which showed up as a 30-62s promotion delay on the app probe.
	assert.Equal(t, hc.GetInterval().AsDuration(), hc.GetNoTrafficInterval().AsDuration())
	assert.Equal(t, hc.GetInterval().AsDuration(), hc.GetNoTrafficHealthyInterval().AsDuration())
	assert.False(t, hc.GetReuseConnection().GetValue(), "a fresh handshake per check is the point")
}

// TestInboundReadyProbeClusterHasNoSourceMatcher: the probe is node-originated,
// so it must carry a PLAIN transport socket. A per-source
// transport_socket_matcher would only ever take its no-match branch and would
// re-embed the local pod set — the exact CDS-churn defect #815 removes.
func TestInboundReadyProbeClusterHasNoSourceMatcher(t *testing.T) {
	c := testInboundReadyCluster(t)
	assert.Nil(t, c.GetTransportSocketMatcher())
	assert.Empty(t, c.GetTransportSocketMatches())
}

// TestInboundReadyProbeIsAcceptedByInboundValidation reads the two halves
// against each other: the inbound listener requires a client certificate and
// validates it against the trust-domain bundle with NO SAN restriction, so the
// node identity the probe presents is accepted without loosening anything.
func TestInboundReadyProbeIsAcceptedByInboundValidation(t *testing.T) {
	ts := DownstreamTransportSocket(testSourceIdentity, "spiffe://"+testTrustDomain)
	var down tlsv3.DownstreamTlsContext
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(&down))

	assert.True(t, down.GetRequireClientCertificate().GetValue())
	assert.Nil(t, down.GetCommonTlsContext().GetCombinedValidationContext(),
		"inbound pins no client SAN, so any trust-domain identity — including the node's — is accepted")
	require.NotNil(t, down.GetCommonTlsContext().GetValidationContextSdsSecretConfig())
	assert.Equal(t, "spiffe://"+testTrustDomain, down.GetCommonTlsContext().GetValidationContextSdsSecretConfig().GetName())
}

// TestInboundReadyChainAlwaysExists: the probe's ALPN/SNI choice targets the
// no-SNI h2 chain, which buildInboundFilterChains emits unconditionally for
// every mTLS pod, including single-port ones.
func TestInboundReadyChainAlwaysExists(t *testing.T) {
	pod := sourceTestPod()
	chains := buildInboundFilterChains(pod, testSourceIdentity, "spiffe://"+testTrustDomain, false, nil, nil)

	var h2Chains int
	for _, fc := range chains {
		m := fc.GetFilterChainMatch()
		if m != nil && len(m.GetServerNames()) == 0 && len(m.GetApplicationProtocols()) == 1 && m.GetApplicationProtocols()[0] == "h2" {
			h2Chains++
			require.NotNil(t, fc.GetTransportSocket(), "the h2 chain must terminate mTLS")
		}
	}
	assert.Equal(t, 1, h2Chains, "exactly one no-SNI h2 chain, which the probe's ALPN selects")
}

// TestHealthGatewayRequiresBothProbes: the pod's gateway path is gated on the
// app probe AND the inbound-readiness probe, while the PATH still names only
// the app probe cluster (so the agent's liveness loop is unchanged).
func TestHealthGatewayRequiresBothProbes(t *testing.T) {
	probe := NewHealthGatewayProbe("health_echo-1", "inboundready_echo-1")
	assert.Equal(t, "health_echo-1", probe.Cluster)
	assert.Equal(t, []string{"health_echo-1", "inboundready_echo-1"}, probe.Requires)

	hcm := gatewayHCM(t, []HealthGatewayProbe{probe})
	filters := hcm.GetHttpFilters()
	require.Len(t, filters, 2, "one health_check filter + router")

	hc := decodeGatewayHealthCheck(t, filters[0])
	assert.Equal(t, HealthGatewayPath("health_echo-1"), hc.GetHeaders()[0].GetStringMatch().GetExact())
	require.Len(t, hc.GetClusterMinHealthyPercentages(), 2)
	for _, name := range []string{"health_echo-1", "inboundready_echo-1"} {
		require.Contains(t, hc.GetClusterMinHealthyPercentages(), name)
		assert.Equal(t, float64(100), hc.GetClusterMinHealthyPercentages()[name].GetValue())
	}
}

// TestHealthGatewayProbeDedupesAndSorts: the requirement set must be a pure
// function of its inputs — empty names dropped, duplicates collapsed, order
// fixed — so an equal pod set never re-hashes the gateway listener.
func TestHealthGatewayProbeDedupesAndSorts(t *testing.T) {
	assert.Equal(t, []string{"health_a"}, NewHealthGatewayProbe("health_a").Requires)
	assert.Equal(t, []string{"health_a"}, NewHealthGatewayProbe("health_a", "").Requires,
		"a pod with no inbound-readiness probe keeps today's single-cluster gate")
	assert.Equal(t, []string{"health_a"}, NewHealthGatewayProbe("health_a", "health_a").Requires)
	assert.Equal(t, []string{"health_a", "inboundready_a"},
		NewHealthGatewayProbe("health_a", "inboundready_a").Requires)
}
