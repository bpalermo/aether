package proxy

import (
	"fmt"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	quicv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/quic/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// quicTransportSocketName is the well-known Envoy name for the QUIC downstream transport socket.
	quicTransportSocketName = "envoy.transport_sockets.quic"
)

// EdgeGatewayH3ListenerName returns the unique name for a per-Gateway HTTP/3 (QUIC) UDP listener.
// The "_h3" suffix distinguishes it from the TCP HTTPS listener (edge_gw_<ns>_<gwname>_<port>)
// so both coexist in LDS without name collision.
func EdgeGatewayH3ListenerName(namespace, gatewayName string, internalPort uint32) string {
	return fmt.Sprintf("edge_gw_%s_%s_%d_h3", namespace, gatewayName, internalPort)
}

// BuildEdgeGatewayHTTP3Listener builds the UDP/QUIC listener that serves HTTP/3 traffic on the
// same internalPort as the Gateway's co-located HTTPS (TCP) listener. Returns nil when
// tlsSecretNames is empty — QUIC mandates TLS, so a plain-HTTP Gateway cannot use HTTP/3.
//
// The listener:
//   - Binds UDP on defaultEdgeAddress:internalPort with enable_reuse_port=true (required for QUIC).
//   - Sets udp_listener_config.quic_options (empty = Envoy's built-in QUIC defaults).
//   - Uses envoy.transport_sockets.quic wrapping the same SDS-backed DownstreamTlsContext as the
//     HTTPS listener so cert rotation is automatic and no extra secret wiring is needed.
//   - HCM: codec_type=HTTP3, Http3ProtocolOptions{} (empty = QUIC defaults), geo+readiness filter
//     prefix, RDS pointing at the per-Gateway route name (EdgeGatewayRouteName), and
//     ApplyEdgeHardening (use_remote_address, timeouts, h2 caps — same policy as HTTPS).
func BuildEdgeGatewayHTTP3Listener(namespace, gatewayName string, internalPort uint32, tlsSecretNames []string, geoFilters []*http_connection_managerv3.HttpFilter, edgeCfg *configv1.EdgeConfigSpec) *listenerv3.Listener {
	if len(tlsSecretNames) == 0 {
		return nil
	}
	name := EdgeGatewayH3ListenerName(namespace, gatewayName, internalPort)
	routeName := EdgeGatewayRouteName(namespace, gatewayName)

	hcm := buildHTTPConnectionManager(name, ReporterSource, "", "", nil)
	hcm.CodecType = http_connection_managerv3.HttpConnectionManager_HTTP3
	hcm.Http3ProtocolOptions = &corev3.Http3ProtocolOptions{}
	prefix := append([]*http_connection_managerv3.HttpFilter{readinessHttpFilter()}, geoFilters...)
	hcm.HttpFilters = append(prefix, hcm.HttpFilters...)
	ApplyEdgeHardening(hcm, nil, edgeCfg)
	hcm.RouteSpecifier = &http_connection_managerv3.HttpConnectionManager_Rds{
		Rds: &http_connection_managerv3.Rds{
			RouteConfigName: routeName,
			ConfigSource:    config.XDSConfigSourceADS(),
		},
	}

	fc := &listenerv3.FilterChain{
		Name:            name,
		Filters:         []*listenerv3.Filter{buildHTTPConnectionManagerFilter(hcm)},
		TransportSocket: edgeQuicDownstreamTransport(tlsSecretNames),
	}

	return &listenerv3.Listener{
		Name: name,
		Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Protocol: corev3.SocketAddress_UDP,
					Address:  defaultEdgeAddress,
					PortSpecifier: &corev3.SocketAddress_PortValue{
						PortValue: internalPort,
					},
				},
			},
		},
		EnableReusePort: wrapperspb.Bool(true),
		UdpListenerConfig: &listenerv3.UdpListenerConfig{
			QuicOptions: &listenerv3.QuicProtocolOptions{},
		},
		TrafficDirection: corev3.TrafficDirection_INBOUND,
		FilterChains:     []*listenerv3.FilterChain{fc},
	}
}

// edgeQuicDownstreamTransport builds the envoy.transport_sockets.quic transport socket for the
// H3 listener. It wraps the same SDS-served DownstreamTlsContext as edgeDownstreamTLSFromSDS
// inside a QuicDownstreamTransport so Envoy registers the socket under the QUIC extension name.
func edgeQuicDownstreamTransport(sdsSecretNames []string) *corev3.TransportSocket {
	certs := make([]*transport_sockets_v3.SdsSecretConfig, 0, len(sdsSecretNames))
	for _, name := range sdsSecretNames {
		certs = append(certs, sdsSecretConfig(name))
	}
	downstream := &transport_sockets_v3.DownstreamTlsContext{
		CommonTlsContext: &transport_sockets_v3.CommonTlsContext{
			TlsCertificateSdsSecretConfigs: certs,
			TlsParams: &transport_sockets_v3.TlsParameters{
				// QUIC mandates TLS 1.3 at the crypto layer regardless of this floor.
				TlsMinimumProtocolVersion: transport_sockets_v3.TlsParameters_TLSv1_2,
			},
			// QUIC carries HTTP/3 ONLY: the ALPN must offer "h3", not the TCP ALPNs.
			// With "h2"/"http/1.1" here a h3 client's handshake is rejected with TLS
			// alert 120 no_application_protocol (found in the 029 M4 e2e).
			AlpnProtocols: []string{"h3"},
		},
	}
	return &corev3.TransportSocket{
		Name: quicTransportSocketName,
		ConfigType: &corev3.TransportSocket_TypedConfig{
			TypedConfig: config.TypedConfig(&quicv3.QuicDownstreamTransport{
				DownstreamTlsContext: downstream,
			}),
		},
	}
}
