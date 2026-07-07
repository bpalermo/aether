package proxy_test

import (
	"fmt"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestBuildEdgeGatewayHTTP3Listener_NilOnNoTLS(t *testing.T) {
	ln := proxy.BuildEdgeGatewayHTTP3Listener("ns", "gw", 18443, nil, nil, nil)
	assert.Nil(t, ln, "no TLS → no QUIC listener")
	ln = proxy.BuildEdgeGatewayHTTP3Listener("ns", "gw", 18443, []string{}, nil, nil)
	assert.Nil(t, ln, "empty TLS names → no QUIC listener")
}

func TestBuildEdgeGatewayHTTP3Listener_Structure(t *testing.T) {
	tls := []string{"spiffe://aether.internal/edge-cert"}
	ln := proxy.BuildEdgeGatewayHTTP3Listener("ns-a", "gw1", 18443, tls, nil, nil)
	require.NotNil(t, ln)

	// Unique name includes "_h3" suffix.
	assert.Equal(t, proxy.EdgeGatewayH3ListenerName("ns-a", "gw1", 18443), ln.GetName())
	assert.Contains(t, ln.GetName(), "_h3")

	// UDP address.
	sa := ln.GetAddress().GetSocketAddress()
	require.NotNil(t, sa)
	assert.Equal(t, corev3.SocketAddress_UDP, sa.GetProtocol(), "QUIC listener must use UDP")
	assert.Equal(t, uint32(18443), sa.GetPortValue())

	// enable_reuse_port (required for QUIC multi-worker).
	assert.True(t, ln.GetEnableReusePort().GetValue(), "enable_reuse_port must be true")

	// udp_listener_config.quic_options present.
	require.NotNil(t, ln.GetUdpListenerConfig(), "UdpListenerConfig must be set")
	require.NotNil(t, ln.GetUdpListenerConfig().GetQuicOptions(), "quic_options must be set")

	// Exactly one filter chain.
	require.Len(t, ln.GetFilterChains(), 1)
	fc := ln.GetFilterChains()[0]

	// Transport socket uses the QUIC extension name.
	ts := fc.GetTransportSocket()
	require.NotNil(t, ts)
	assert.Equal(t, "envoy.transport_sockets.quic", ts.GetName(), "transport socket must be the QUIC extension")

	// QUIC ALPN must be exactly ["h3"] — TCP ALPNs (h2/http-1.1) make a h3 client's
	// handshake fail with TLS alert 120 no_application_protocol (029 M4 e2e bug).
	var quic quicv3.QuicDownstreamTransport
	require.NoError(t, proto.Unmarshal(ts.GetTypedConfig().GetValue(), &quic))
	assert.Equal(t, []string{"h3"}, quic.GetDownstreamTlsContext().GetCommonTlsContext().GetAlpnProtocols())

	// HCM in the filter chain uses HTTP3 codec.
	require.Len(t, fc.GetFilters(), 1)
	hcmAny := fc.GetFilters()[0].GetTypedConfig()
	require.NotNil(t, hcmAny)
	var hcm http_connection_managerv3.HttpConnectionManager
	require.NoError(t, proto.Unmarshal(hcmAny.GetValue(), &hcm))
	assert.Equal(t, http_connection_managerv3.HttpConnectionManager_HTTP3, hcm.GetCodecType(), "HCM must use HTTP3 codec")
	assert.NotNil(t, hcm.GetHttp3ProtocolOptions(), "Http3ProtocolOptions must be set")

	// RDS route config points at the same per-Gateway route name as the HTTPS listener.
	rds := hcm.GetRds()
	require.NotNil(t, rds)
	assert.Equal(t, proxy.EdgeGatewayRouteName("ns-a", "gw1"), rds.GetRouteConfigName())

	// INBOUND traffic direction.
	assert.Equal(t, corev3.TrafficDirection_INBOUND, ln.GetTrafficDirection())
}

func TestBuildEdgeGatewayHTTP3Listener_EnableReusePort(t *testing.T) {
	ln := proxy.BuildEdgeGatewayHTTP3Listener("ns", "gw", 9443, []string{"cert-a"}, nil, nil)
	require.NotNil(t, ln)
	require.NotNil(t, ln.GetEnableReusePort())
	assert.True(t, ln.GetEnableReusePort().GetValue())
}

func TestBuildEdgeGatewayRouteConfiguration_AltSvcPresent(t *testing.T) {
	vh := proxy.BuildEdgeVirtualHost("api.example.com", []string{"api.example.com"}, nil)

	// With http3 enabled (external port 443): alt-svc header present.
	rc := proxy.BuildEdgeGatewayRouteConfiguration("ns", "gw", []*routev3.VirtualHost{vh}, 443)
	assert.NotEmpty(t, rc.GetResponseHeadersToAdd(), "alt-svc header must be present when http3 port is set")
	found := false
	for _, h := range rc.GetResponseHeadersToAdd() {
		if h.GetHeader().GetKey() == "alt-svc" {
			assert.Equal(t, `h3=":443"; ma=86400`, h.GetHeader().GetValue())
			found = true
		}
	}
	assert.True(t, found, "alt-svc header must reference the external port")
}

func TestBuildEdgeGatewayRouteConfiguration_AltSvcAbsent(t *testing.T) {
	vh := proxy.BuildEdgeVirtualHost("api.example.com", []string{"api.example.com"}, nil)

	// Without http3 (port 0): no alt-svc header.
	rc := proxy.BuildEdgeGatewayRouteConfiguration("ns", "gw", []*routev3.VirtualHost{vh}, 0)
	for _, h := range rc.GetResponseHeadersToAdd() {
		assert.NotEqual(t, "alt-svc", h.GetHeader().GetKey(), "alt-svc must not be added when http3 is disabled")
	}
}

func TestBuildEdgeGatewayRouteConfiguration_AltSvcPort(t *testing.T) {
	for _, port := range []uint32{443, 8443, 1234} {
		rc := proxy.BuildEdgeGatewayRouteConfiguration("ns", "gw", nil, port)
		want := fmt.Sprintf(`h3=":%d"; ma=86400`, port)
		found := false
		for _, h := range rc.GetResponseHeadersToAdd() {
			if h.GetHeader().GetKey() == "alt-svc" {
				assert.Equal(t, want, h.GetHeader().GetValue())
				found = true
			}
		}
		assert.True(t, found, "alt-svc must reference port %d", port)
	}
}

func TestEdgeGatewayH3ListenerName(t *testing.T) {
	assert.Equal(t, "edge_gw_ns_gw_443_h3", proxy.EdgeGatewayH3ListenerName("ns", "gw", 443))
	// Must not collide with the TCP HTTPS listener name.
	h3 := proxy.EdgeGatewayH3ListenerName("ns", "gw", 18443)
	tcp := proxy.EdgeGatewayListenerName("ns", "gw", 18443)
	assert.NotEqual(t, h3, tcp, "H3 and HTTPS listener names must be distinct")
}

// TestBuildEdgeGatewayHTTP3Listener_WithEdgeConfig exercises the EdgeConfig path:
// ApplyEdgeHardening must be applied (non-nil HCM fields). The http3.enabled flag is
// consumed by the cache layer — the builder itself always emits the QUIC listener when
// called with non-empty tlsSecretNames.
func TestBuildEdgeGatewayHTTP3Listener_WithEdgeConfig(t *testing.T) {
	cfg := configv1.EdgeConfigSpec_builder{}.Build()
	cfg.SetXffNumTrustedHops(wrapperspb.UInt32(2))

	ln := proxy.BuildEdgeGatewayHTTP3Listener("ns", "gw", 18443, []string{"cert"}, nil, cfg)
	require.NotNil(t, ln)

	require.Len(t, ln.GetFilterChains(), 1)
	var hcm http_connection_managerv3.HttpConnectionManager
	require.NoError(t, proto.Unmarshal(ln.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().GetValue(), &hcm))
	// ApplyEdgeHardening sets use_remote_address (default true).
	assert.NotNil(t, hcm.GetUseRemoteAddress(), "use_remote_address must be set by ApplyEdgeHardening")
	assert.True(t, hcm.GetUseRemoteAddress().GetValue())
}
