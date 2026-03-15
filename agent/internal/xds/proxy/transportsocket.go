package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// tlsTransportSocketName is the Envoy TLS transport socket name
	tlsTransportSocketName = "envoy.transport_sockets.tls"
)

// sdsSecretConfig creates an SDS secret config that fetches secrets via ADS.
func sdsSecretConfig(secretName string) *transport_sockets_v3.SdsSecretConfig {
	return &transport_sockets_v3.SdsSecretConfig{
		Name:      secretName,
		SdsConfig: config.XDSConfigSourceADS(),
	}
}

// DownstreamTransportSocket creates a TLS transport socket for downstream (inbound) connections.
// It requires mutual TLS and retrieves certificates and validation context via ADS-served SDS.
func DownstreamTransportSocket(tlsCertificateSecretName string, validationContextName string) *corev3.TransportSocket {
	downstreamTlsContext := &transport_sockets_v3.DownstreamTlsContext{
		RequireClientCertificate: wrapperspb.Bool(true),
		CommonTlsContext: &transport_sockets_v3.CommonTlsContext{
			TlsCertificateSdsSecretConfigs: []*transport_sockets_v3.SdsSecretConfig{
				sdsSecretConfig(tlsCertificateSecretName),
			},
			ValidationContextType: &transport_sockets_v3.CommonTlsContext_ValidationContextSdsSecretConfig{
				ValidationContextSdsSecretConfig: sdsSecretConfig(validationContextName),
			},
		},
	}

	return transportSocket(downstreamTlsContext)
}

// UpstreamTransportSocket creates a TLS transport socket for upstream (outbound) connections.
// It uses mTLS with both a client certificate and validation context fetched via ADS-served SDS.
func UpstreamTransportSocket(tlsCertificateSecretName string, validationContextName string) *corev3.TransportSocket {
	upstreamTlsContext := &transport_sockets_v3.UpstreamTlsContext{
		CommonTlsContext: &transport_sockets_v3.CommonTlsContext{
			TlsCertificateSdsSecretConfigs: []*transport_sockets_v3.SdsSecretConfig{
				sdsSecretConfig(tlsCertificateSecretName),
			},
			ValidationContextType: &transport_sockets_v3.CommonTlsContext_ValidationContextSdsSecretConfig{
				ValidationContextSdsSecretConfig: sdsSecretConfig(validationContextName),
			},
		},
	}

	return transportSocket(upstreamTlsContext)
}

// transportSocket creates a TLS transport socket from the given TLS context message.
func transportSocket(msg proto.Message) *corev3.TransportSocket {
	return &corev3.TransportSocket{
		Name: tlsTransportSocketName,
		ConfigType: &corev3.TransportSocket_TypedConfig{
			TypedConfig: config.TypedConfig(msg),
		},
	}
}
