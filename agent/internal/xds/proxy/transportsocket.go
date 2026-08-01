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

// UpstreamTCPTransportSocket creates a TLS transport socket for TCP-proxy upstream
// connections (proposal 018, Phase 3a TCP floor). It is identical to
// UpstreamTransportSocket except that it advertises NO ALPN (vs HTTP's "h2"). The
// destination inbound listener's HTTP chains match application_protocols:["h2"], so
// a no-ALPN mTLS connection falls through to the inbound TCP floor's DEFAULT chain —
// demultiplexing TCP from HTTP with the standard h2 ALPN instead of a bespoke token.
// sanURIs and sni semantics are unchanged.
func UpstreamTCPTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string, sni string) *corev3.TransportSocket {
	return upstreamTransportSocket(tlsCertificateSecretName, validationContextName, sanURIs, sni, config.XDSConfigSourceADS(), "")
}

// EdgeUpstreamTransportSocket is UpstreamTransportSocket for the edge proxy: it
// fetches the (single) edge SVID and trust bundle over the SPIRE Agent's native
// SDS API (the static spire_agent cluster) instead of the agent's ADS stream.
// SAN pinning is unchanged — match_typed_subject_alt_names is inline static
// config; only the validation-context bundle comes over SDS.
func EdgeUpstreamTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string, sni string) *corev3.TransportSocket {
	return upstreamTransportSocket(tlsCertificateSecretName, validationContextName, sanURIs, sni, config.SDSConfigSourceFromCluster(SpireAgentSDSClusterName))
}

// EdgeUpstreamTCPTransportSocket is EdgeUpstreamTransportSocket for TCP floor
// clusters: it advertises NO ALPN (like the east-west UpstreamTCPTransportSocket)
// and NO SNI so the destination inbound demuxes to the TCP floor's DEFAULT chain,
// while still fetching the edge SVID and trust bundle directly from the SPIRE Agent
// (not the agent's ADS stream). #304 removed the bespoke "aether-tcp" ALPN; #306
// established that the SNI must be empty for the floor — a non-empty SNI would
// hit the destination's per-port HCM chain instead of the floor default.
func EdgeUpstreamTCPTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string) *corev3.TransportSocket {
	return upstreamTransportSocket(tlsCertificateSecretName, validationContextName, sanURIs, "" /* no SNI */, config.SDSConfigSourceFromCluster(SpireAgentSDSClusterName), "" /* no ALPN */)
}

// upstreamTransportSocket builds an upstream TLS context. With no alpnOverride the
// ALPN is "h2" (HTTP/2 mesh transport). An alpnOverride of "" suppresses ALPN
// entirely — the TCP floor path, so the destination inbound demuxes it as the
// default (non-h2) chain; any other override value sets that explicit ALPN list.
func upstreamTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string, sni string, sdsSource *corev3.ConfigSource, alpnOverride ...string) *corev3.TransportSocket {
	alpn := []string{"h2"}
	if len(alpnOverride) > 0 {
		if alpnOverride[0] == "" {
			alpn = nil // no ALPN (TCP floor): falls to the inbound default chain
		} else {
			alpn = alpnOverride
		}
	}
	common := &transport_sockets_v3.CommonTlsContext{
		// Clusters speak HTTP/2 upstream ("h2"); the TCP floor sends no ALPN so the
		// destination inbound demuxes it to the default tcp_proxy chain (HTTP matches "h2").
		AlpnProtocols: alpn,
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

// edgeDownstreamTLSFromSDS terminates external (north-south) TLS at the edge
// listener, presenting one of the named certs (selected by SNI) served over the
// agent's ADS SDS. It does NOT require a client certificate — external callers
// have no mesh identity; the edge presents its own SVID on the separate upstream
// (edge -> pod) mTLS hop. Certs are SDS-served so rotation needs no pod roll.
func edgeDownstreamTLSFromSDS(sdsSecretNames []string) *corev3.TransportSocket {
	certs := make([]*transport_sockets_v3.SdsSecretConfig, 0, len(sdsSecretNames))
	for _, name := range sdsSecretNames {
		certs = append(certs, sdsSecretConfig(name)) // ADS-served (the cache secrets)
	}
	return transportSocket(&transport_sockets_v3.DownstreamTlsContext{
		CommonTlsContext: &transport_sockets_v3.CommonTlsContext{
			TlsCertificateSdsSecretConfigs: certs,
			TlsParams: &transport_sockets_v3.TlsParameters{
				TlsMinimumProtocolVersion: transport_sockets_v3.TlsParameters_TLSv1_2,
			},
			AlpnProtocols: []string{"h2", "http/1.1"},
		},
	})
}

// NewDownstreamTLSSecret builds an Envoy SDS Secret carrying an inline
// certificate/key (read by the edge from a provider-backed source). name is the
// provider-prefixed SDS name the edge listener references.
func NewDownstreamTLSSecret(name string, cert, key []byte) *transport_sockets_v3.Secret {
	return &transport_sockets_v3.Secret{
		Name: name,
		Type: &transport_sockets_v3.Secret_TlsCertificate{
			TlsCertificate: &transport_sockets_v3.TlsCertificate{
				CertificateChain: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineBytes{InlineBytes: cert},
				},
				PrivateKey: &corev3.DataSource{
					Specifier: &corev3.DataSource_InlineBytes{InlineBytes: key},
				},
			},
		},
	}
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
