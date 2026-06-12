package proxy

import (
	"github.com/bpalermo/aether/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
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
// It uses mTLS with both a client certificate and validation context fetched via
// ADS-served SDS. sanURIs, when non-empty, pins the SERVER identity: the
// presented SVID's URI SAN must exactly match one of the expected per-service
// SPIFFE IDs (spiffe://<td>/ns/<ns>/sa/<service>), layered over the
// SDS-rotated trust bundle via a combined validation context. Without it the
// handshake only proves trust-domain membership — any mesh workload — which
// leaves registry poisoning able to impersonate a service (an attacker-
// registered endpoint would present a valid but WRONG identity).
func UpstreamTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string) *corev3.TransportSocket {
	common := &transport_sockets_v3.CommonTlsContext{
		// Clusters speak HTTP/2 upstream; advertise it so the mTLS handshake
		// negotiates h2 (matches the inbound listener's codec).
		AlpnProtocols: []string{"h2"},
		TlsCertificateSdsSecretConfigs: []*transport_sockets_v3.SdsSecretConfig{
			sdsSecretConfig(tlsCertificateSecretName),
		},
	}

	if len(sanURIs) == 0 {
		common.ValidationContextType = &transport_sockets_v3.CommonTlsContext_ValidationContextSdsSecretConfig{
			ValidationContextSdsSecretConfig: sdsSecretConfig(validationContextName),
		}
	} else {
		matchers := make([]*transport_sockets_v3.SubjectAltNameMatcher, 0, len(sanURIs))
		for _, uri := range sanURIs {
			matchers = append(matchers, &transport_sockets_v3.SubjectAltNameMatcher{
				SanType: transport_sockets_v3.SubjectAltNameMatcher_URI,
				Matcher: &matcherv3.StringMatcher{
					MatchPattern: &matcherv3.StringMatcher_Exact{Exact: uri},
				},
			})
		}
		common.ValidationContextType = &transport_sockets_v3.CommonTlsContext_CombinedValidationContext{
			CombinedValidationContext: &transport_sockets_v3.CommonTlsContext_CombinedCertificateValidationContext{
				DefaultValidationContext: &transport_sockets_v3.CertificateValidationContext{
					MatchTypedSubjectAltNames: matchers,
				},
				ValidationContextSdsSecretConfig: sdsSecretConfig(validationContextName),
			},
		}
	}

	return transportSocket(&transport_sockets_v3.UpstreamTlsContext{CommonTlsContext: common})
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
