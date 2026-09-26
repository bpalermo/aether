package proxy

import (
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func quicTestPod() *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "web-0",
		Namespace:        "shop",
		ServiceAccount:   "web",
		NetworkNamespace: "/var/run/netns/cni-web",
		// Primary 8080 (HTTP), a second HTTP port 8081, and a raw-TCP port 9000
		// (proposal 037) which must get NO QUIC chain.
		Annotations: map[string]string{
			"endpoint.aether.io/port":  "8080",
			"endpoint.aether.io/ports": "8080,8081,9000=tcp",
		},
	}
}

func quicTransportOf(t *testing.T, fc *listenerv3.FilterChain) *quicv3.QuicDownstreamTransport {
	t.Helper()
	require.Equal(t, "envoy.transport_sockets.quic", fc.GetTransportSocket().GetName())
	q := &quicv3.QuicDownstreamTransport{}
	require.NoError(t, fc.GetTransportSocket().GetTypedConfig().UnmarshalTo(q))
	return q
}

func hcmOf(t *testing.T, fc *listenerv3.FilterChain) *http_connection_managerv3.HttpConnectionManager {
	t.Helper()
	require.Len(t, fc.GetFilters(), 1)
	h := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, fc.GetFilters()[0].GetTypedConfig().UnmarshalTo(h))
	return h
}

// TestNewInboundQUICListener pins the shape of the per-pod HTTP/3 inbound
// (proposal 038 Phase 4, R2/R3/R4): UDP on the TCP inbound's port in the pod
// netns, reuse_port + quic_options, one default chain for the primary port and
// a server_names chain per non-primary HTTP port, NO chain for a raw-TCP port,
// HTTP3 codec, and a QUIC transport that requires the client certificate,
// offers only h3, and explicitly disables resumption and 0-RTT.
func TestNewInboundQUICListener(t *testing.T) {
	pod := quicTestPod()
	l, err := NewInboundQUICListener(pod, "example.org", false, false, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, l)

	assert.Equal(t, "inbound_web-0_h3", l.GetName())
	sa := l.GetAddress().GetSocketAddress()
	assert.Equal(t, corev3.SocketAddress_UDP, sa.GetProtocol())
	assert.Equal(t, uint32(defaultInboundPort), sa.GetPortValue(), "R3: QUIC shares the TCP inbound's port number")
	assert.Equal(t, pod.GetNetworkNamespace(), sa.GetNetworkNamespaceFilepath())
	assert.True(t, l.GetEnableReusePort().GetValue())
	assert.NotNil(t, l.GetUdpListenerConfig().GetQuicOptions())
	assert.Equal(t, corev3.TrafficDirection_INBOUND, l.GetTrafficDirection())
	assert.Empty(t, l.GetListenerFilters(), "no tls_inspector on a QUIC listener; SNI comes from the CHLO")

	// Chains: default (8080) + SNI "8081"; 9000 is raw TCP and gets none.
	require.Len(t, l.GetFilterChains(), 2)
	names := map[string]*listenerv3.FilterChain{}
	for _, fc := range l.GetFilterChains() {
		names[fc.GetName()] = fc
	}
	def, ok := names["in_h3_web-0"]
	require.True(t, ok, "primary-port default chain missing: %v", names)
	assert.Nil(t, def.GetFilterChainMatch(), "the primary port is the DEFAULT chain (no h2 ALPN to key on)")
	port8081, ok := names["in_h3_web-0_8081"]
	require.True(t, ok)
	assert.Equal(t, []string{"8081"}, port8081.GetFilterChainMatch().GetServerNames())
	_, tcpChain := names["in_h3_web-0_9000"]
	assert.False(t, tcpChain, "a raw-TCP port must have no QUIC chain: QUIC carries HTTP/3 only")

	for name, fc := range names {
		q := quicTransportOf(t, fc)
		ctx := q.GetDownstreamTlsContext()
		assert.True(t, ctx.GetRequireClientCertificate().GetValue(), "%s: client certificate must be required", name)
		assert.Equal(t, []string{"h3"}, ctx.GetCommonTlsContext().GetAlpnProtocols(), "%s: ALPN must be exactly h3", name)
		require.NotNil(t, q.EnableResumption, "%s: R4 enable_resumption must be EXPLICIT", name)
		assert.False(t, q.EnableResumption.GetValue(), "%s: R4 resumption off", name)
		require.NotNil(t, q.EnableEarlyData, "%s: R4 enable_early_data must be EXPLICIT", name)
		assert.False(t, q.EnableEarlyData.GetValue(), "%s: R4 0-RTT off", name)
		assert.NotEmpty(t, ctx.GetCommonTlsContext().GetCombinedValidationContext().GetDefaultValidationContext().GetMatchTypedSubjectAltNames(),
			"%s: the workload SAN pin (#843) must be present on the QUIC chain too", name)

		h := hcmOf(t, fc)
		assert.Equal(t, http_connection_managerv3.HttpConnectionManager_HTTP3, h.GetCodecType(), "%s", name)
		assert.NotNil(t, h.GetHttp3ProtocolOptions(), "%s", name)
		assert.Equal(t, http_connection_managerv3.HttpConnectionManager_SANITIZE_SET, h.GetForwardClientCertDetails(), "%s: XFCC from the verified peer", name)
		assert.True(t, h.GetSetCurrentClientCertDetails().GetUri(), "%s: the SPIFFE URI SAN must be in XFCC", name)
	}
	// The default chain routes to the primary port's app cluster, the 8081 chain to its own.
	assert.Contains(t, hcmOf(t, def).GetRouteConfig().String(), AppClusterName(pod, 8080))
	assert.Contains(t, hcmOf(t, port8081).GetRouteConfig().String(), AppClusterName(pod, 8081))
}

// TestInboundQUICSharesTheTCPInboundTLSContext is the "one builder, two
// wrappers" pin: strip the QUIC-only ALPN and the QUIC chain's inner
// DownstreamTlsContext must be byte-identical to the TCP inbound chain's. If
// the two ever diverged (a pin on one transport but not the other), a caller
// could pick the weaker transport.
func TestInboundQUICSharesTheTCPInboundTLSContext(t *testing.T) {
	pod := quicTestPod()
	tcp, err := NewInboundListener(pod, "example.org", false, false, nil, nil)
	require.NoError(t, err)
	quic, err := NewInboundQUICListener(pod, "example.org", false, false, nil, nil)
	require.NoError(t, err)

	var tcpCtx *tlsv3.DownstreamTlsContext
	for _, fc := range tcp.GetFilterChains() {
		if fc.GetName() == "in_web-0" {
			tcpCtx = &tlsv3.DownstreamTlsContext{}
			require.NoError(t, fc.GetTransportSocket().GetTypedConfig().UnmarshalTo(tcpCtx))
		}
	}
	require.NotNil(t, tcpCtx)
	var quicCtx *tlsv3.DownstreamTlsContext
	for _, fc := range quic.GetFilterChains() {
		if fc.GetName() == "in_h3_web-0" {
			quicCtx = quicTransportOf(t, fc).GetDownstreamTlsContext()
		}
	}
	require.NotNil(t, quicCtx)
	quicCtx.CommonTlsContext.AlpnProtocols = nil
	assert.True(t, proto.Equal(tcpCtx, quicCtx), "the QUIC inbound's TLS context must be the TCP inbound's, ALPN aside:\ntcp:  %s\nquic: %s", tcpCtx, quicCtx)
}

// TestNewInboundQUICListener_Nil: no QUIC without TLS (SPIRE off), and the
// same identity refusal as the TCP inbound when there is no trust domain.
func TestNewInboundQUICListener_Nil(t *testing.T) {
	pod := quicTestPod()
	l, err := NewInboundQUICListener(pod, "example.org", false, true, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, l, "cleartext (SPIRE off) has no SVID to present; QUIC mandates TLS")

	_, err = NewInboundQUICListener(pod, "", false, false, nil, nil)
	require.ErrorIs(t, err, ErrNoTrustDomain)

	_, err = NewInboundQUICListener(&cniv1.CNIPod{Name: "no-netns"}, "example.org", false, false, nil, nil)
	require.Error(t, err)
}
