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

// sdsSecretConfig creates an SDS secret config that fetches secrets via ADS
// (the agent-served snapshot secrets used by the node proxy).
func sdsSecretConfig(secretName string) *transport_sockets_v3.SdsSecretConfig {
	return sdsSecretConfigFrom(secretName, config.XDSConfigSourceADS())
}

// sdsSecretConfigFrom creates an SDS secret config that fetches the named
// secret over the given config source. The edge proxy passes a source pointing
// at the static spire_agent SDS cluster (config.SDSConfigSourceFromCluster) so
// secrets come straight from SPIRE instead of the agent's ADS stream.
func sdsSecretConfigFrom(secretName string, source *corev3.ConfigSource) *transport_sockets_v3.SdsSecretConfig {
	return &transport_sockets_v3.SdsSecretConfig{
		Name:      secretName,
		SdsConfig: source,
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
func UpstreamTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string, sni string) *corev3.TransportSocket {
	return upstreamTransportSocket(tlsCertificateSecretName, validationContextName, sanURIs, sni, config.XDSConfigSourceADS())
}

// EdgeUpstreamTransportSocket is UpstreamTransportSocket for the edge proxy: it
// fetches the (single) edge SVID and trust bundle over the SPIRE Agent's native
// SDS API (the static spire_agent cluster) instead of the agent's ADS stream.
// SAN pinning is unchanged — match_typed_subject_alt_names is inline static
// config; only the validation-context bundle comes over SDS.
func EdgeUpstreamTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string, sni string) *corev3.TransportSocket {
	return upstreamTransportSocket(tlsCertificateSecretName, validationContextName, sanURIs, sni, config.SDSConfigSourceFromCluster(SpireAgentSDSClusterName))
}

func upstreamTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string, sni string, sdsSource *corev3.ConfigSource) *corev3.TransportSocket {
	common := &transport_sockets_v3.CommonTlsContext{
		// Clusters speak HTTP/2 upstream; advertise it so the mTLS handshake
		// negotiates h2 (matches the inbound listener's codec).
		AlpnProtocols: []string{"h2"},
		TlsCertificateSdsSecretConfigs: []*transport_sockets_v3.SdsSecretConfig{
			sdsSecretConfigFrom(tlsCertificateSecretName, sdsSource),
		},
	}

	if len(sanURIs) == 0 {
		common.ValidationContextType = &transport_sockets_v3.CommonTlsContext_ValidationContextSdsSecretConfig{
			ValidationContextSdsSecretConfig: sdsSecretConfigFrom(validationContextName, sdsSource),
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
				ValidationContextSdsSecretConfig: sdsSecretConfigFrom(validationContextName, sdsSource),
			},
		}
	}

	// SNI carries the destination PORT (multi-port routing): the destination
	// inbound listener demuxes filter chains by server_names = the port. Empty
	// for single-port/default callers, which hit the destination's default
	// chain. SNI is routing only — identity is the validated SVID/SAN.
	return transportSocket(&transport_sockets_v3.UpstreamTlsContext{CommonTlsContext: common, Sni: sni})
}

// edgeDownstreamTLS terminates external (north-south) TLS at the edge listener
// from a mounted certificate/key (a Kubernetes TLS Secret). It does NOT require a
// client certificate — external callers have no mesh identity; the edge presents
// its own SVID on the separate upstream (edge -> pod) mTLS hop. The cert/key are
// inline file data sources; rotating the Secret takes effect on the next edge pod
// roll (no SDS dependency for the downstream cert).
func edgeDownstreamTLS(certPath, keyPath string) *corev3.TransportSocket {
	return transportSocket(&transport_sockets_v3.DownstreamTlsContext{
		CommonTlsContext: &transport_sockets_v3.CommonTlsContext{
			TlsCertificates: []*transport_sockets_v3.TlsCertificate{
				{
					CertificateChain: &corev3.DataSource{
						Specifier: &corev3.DataSource_Filename{Filename: certPath},
					},
					PrivateKey: &corev3.DataSource{
						Specifier: &corev3.DataSource_Filename{Filename: keyPath},
					},
				},
			},
		},
	})
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
