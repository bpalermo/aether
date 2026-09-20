package proxy

import (
	"aethermesh.dev/agent/internal/xds/config"
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
//
// It also pins the CLIENT identity to a workload SVID SHAPE: the peer
// certificate must carry a URI SAN with the
// "spiffe://<trust-domain>/ns/" prefix (issue #843). RequireClientCertificate
// alone proves only that SOME certificate this trust bundle signs was
// presented; the inbound HCM then stamps that certificate's URI SAN into XFCC
// with SANITIZE_SET (ingress.go), and downstream RBAC/ext_authz rules key on
// that value. Requiring the SPIFFE path to be /ns/<ns>/sa/<sa> rejects a
// non-workload certificate and is what makes the XFCC value structurally
// trustworthy. It deliberately does NOT pin a specific ServiceAccount: in a
// mesh any workload may legitimately call, so identity-level authorization
// belongs in RBAC/ext_authz, which this pin is the precondition for.
//
// trustDomain == "" emits NO matcher — byte-identical to the pre-#843 shape —
// rather than the unservable "spiffe:///ns/" (see WorkloadSANPrefix). That
// branch is defence in depth, not a live path: NewInboundListener already
// refuses with ErrNoTrustDomain before reaching here on the mTLS path, so the
// only reachable "no trust domain" outcome is a skipped pod with a WARN naming
// it, never a silently unpinned listener. //test/envoy_validate asserts the
// shape over the generated bootstrap bytes so a future caller that bypasses
// that guard fails the build instead of shipping an unpinned inbound.
func DownstreamTransportSocket(tlsCertificateSecretName, validationContextName, trustDomain string) *corev3.TransportSocket {
	common := &transport_sockets_v3.CommonTlsContext{
		TlsCertificateSdsSecretConfigs: []*transport_sockets_v3.SdsSecretConfig{
			sdsSecretConfig(tlsCertificateSecretName),
		},
	}

	if prefix := WorkloadSANPrefix(trustDomain); prefix == "" {
		common.ValidationContextType = &transport_sockets_v3.CommonTlsContext_ValidationContextSdsSecretConfig{
			ValidationContextSdsSecretConfig: sdsSecretConfig(validationContextName),
		}
	} else {
		common.ValidationContextType = combinedValidationContextFromMatchers(
			validationContextName,
			[]*transport_sockets_v3.SubjectAltNameMatcher{uriSANPrefixMatcher(prefix)},
			config.XDSConfigSourceADS(),
		)
	}

	return transportSocket(&transport_sockets_v3.DownstreamTlsContext{
		RequireClientCertificate: wrapperspb.Bool(true),
		CommonTlsContext:         common,
	})
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
		common.ValidationContextType = combinedValidationContext(validationContextName, sanURIs, sdsSource)
	}

	// SNI carries the destination PORT (multi-port routing): the destination
	// inbound listener demuxes filter chains by server_names = the port. Empty
	// for single-port/default callers, which hit the destination's default
	// chain. SNI is routing only — identity is the validated SVID/SAN.
	//
	// MaxSessionKeys: 0 disables the client-side TLS session cache, and it is
	// load-bearing for identity, not a performance knob (#829). Envoy defaults
	// max_session_keys to 1, and a resumed session carries the peer certificate
	// of the server that CREATED it — which Envoy then re-checks against this
	// context's match_typed_subject_alt_names. Resume a session made against one
	// server while dialling another and the handshake fails reporting a peer SVID
	// that never came off the connection, or (worse) succeeds against a stale
	// identity. Upstream envoy#45982 scopes that cache by SNI, but the SNI here
	// is a PORT NUMBER, so every aether context collapses onto the same key
	// ("8080", or "" for the floor) and the scoping is inert for us. We lose
	// nothing by switching resumption off: upstream connections are long-lived
	// pooled h2, so full handshakes are rare and amortised, and the mesh's whole
	// premise is that the SVID on this connection is this peer's.
	return transportSocket(&transport_sockets_v3.UpstreamTlsContext{
		CommonTlsContext: common,
		Sni:              sni,
		MaxSessionKeys:   wrapperspb.UInt32(0),
	})
}

// combinedValidationContext builds the SERVER-identity pin every upstream mTLS
// context in the mesh shares: the peer's presented SVID must carry a URI SAN
// exactly matching one of sanURIs, layered over the SDS-rotated trust bundle.
// Callers must not pass an empty sanURIs — an empty matcher list renders a
// validation context that pins nothing (see issue #832); the unpinned form is
// the explicit `len(sanURIs) == 0` branch above, never a by-product here.
func combinedValidationContext(validationContextName string, sanURIs []string, sdsSource *corev3.ConfigSource) *transport_sockets_v3.CommonTlsContext_CombinedValidationContext {
	matchers := make([]*transport_sockets_v3.SubjectAltNameMatcher, 0, len(sanURIs))
	for _, uri := range sanURIs {
		matchers = append(matchers, &transport_sockets_v3.SubjectAltNameMatcher{
			SanType: transport_sockets_v3.SubjectAltNameMatcher_URI,
			Matcher: &matcherv3.StringMatcher{
				MatchPattern: &matcherv3.StringMatcher_Exact{Exact: uri},
			},
		})
	}
	return combinedValidationContextFromMatchers(validationContextName, matchers, sdsSource)
}

// uriSANPrefixMatcher matches any URI SAN starting with prefix.
//
// Envoy builds a generic StringSanMatcher(GEN_URI, matcher) for san_type: URI
// (source/common/tls/cert_validator/san_matcher.cc), so the full StringMatcher
// surface — prefix included — applies. Only DNS gets special treatment there
// (an exact matcher becomes RFC 6125 wildcard matching); URI is plain string
// matching, which is what a SPIFFE path prefix needs.
func uriSANPrefixMatcher(prefix string) *transport_sockets_v3.SubjectAltNameMatcher {
	return &transport_sockets_v3.SubjectAltNameMatcher{
		SanType: transport_sockets_v3.SubjectAltNameMatcher_URI,
		Matcher: &matcherv3.StringMatcher{
			MatchPattern: &matcherv3.StringMatcher_Prefix{Prefix: prefix},
		},
	}
}

// combinedValidationContextFromMatchers layers inline SAN matchers over the
// SDS-rotated trust bundle. It is the one shape both directions of the mesh use
// — the upstream SERVER pin (exact per-service SPIFFE IDs) and the inbound
// CLIENT pin (a workload-shape URI prefix) — so the trust bundle keeps
// rotating over SDS while the matchers stay inline static config.
//
// Callers must not pass an empty matcher list: an empty
// match_typed_subject_alt_names renders a validation context that pins nothing
// (issue #832), and the unpinned form must always be an explicit branch at the
// call site rather than a by-product here.
func combinedValidationContextFromMatchers(validationContextName string, matchers []*transport_sockets_v3.SubjectAltNameMatcher, sdsSource *corev3.ConfigSource) *transport_sockets_v3.CommonTlsContext_CombinedValidationContext {
	return &transport_sockets_v3.CommonTlsContext_CombinedValidationContext{
		CombinedValidationContext: &transport_sockets_v3.CommonTlsContext_CombinedCertificateValidationContext{
			DefaultValidationContext: &transport_sockets_v3.CertificateValidationContext{
				MatchTypedSubjectAltNames: matchers,
			},
			ValidationContextSdsSecretConfig: sdsSecretConfigFrom(validationContextName, sdsSource),
		},
	}
}

// InboundReadyProbeTransportSocket builds the upstream TLS context for the
// per-pod inbound-readiness probe cluster (issue #815) and for NOTHING else.
//
// THE INVARIANT: nothing this cluster does may be observable by application
// traffic (issue #836). The probe dials each local pod's mesh inbound INSIDE
// that pod's network namespace every 5 s, so its TLS peer is always a pod on
// this node presenting that pod's own SVID — the one certificate that must
// never turn up in an unrelated cluster's handshake. In #829 a probe-sourced
// TLS session did exactly that: mesh clusters resumed from a session the probe
// had created and were handed a local pod's certificate.
//
// That is why this context is written out here in full rather than calling the
// mesh helper (UpstreamTransportSocket): the probe carries its OWN
// MaxSessionKeys: 0, so it can never deposit resumable session state whatever
// the mesh path does. #834 set max_session_keys to 0 on the mesh builders too
// and that should stay — but it is a global switch someone may reverse for a
// good reason (upstream envoy#45982 scopes the cache by SNI, which would make
// resumption defensible again), and this boundary has to hold when they do.
//
// Everything else is load-bearing and explained at NewInboundReadyProbeCluster:
// ALPN "h2" selects the inbound listener's always-present no-SNI HCM chain, the
// SAN pin (the pod's own SPIFFE ID) is what makes a pass mean "THIS pod
// answered" rather than "something answered on :18008", and the SNI stays EMPTY
// because a non-empty SNI is a destination PORT and would select a per-port
// chain instead.
func InboundReadyProbeTransportSocket(nodeSpiffeID, validationContextName, podSpiffeID string) *corev3.TransportSocket {
	return transportSocket(&transport_sockets_v3.UpstreamTlsContext{
		CommonTlsContext: &transport_sockets_v3.CommonTlsContext{
			AlpnProtocols: []string{"h2"},
			TlsCertificateSdsSecretConfigs: []*transport_sockets_v3.SdsSecretConfig{
				sdsSecretConfig(nodeSpiffeID),
			},
			ValidationContextType: combinedValidationContext(validationContextName, []string{podSpiffeID}, config.XDSConfigSourceADS()),
		},
		// No SNI: the no-SNI h2 chain (see above).
		Sni: "",
		// The probe's own resumption switch, independent of the mesh helper's.
		// A health check gains nothing from resumption — it runs every 5 s
		// against one fixed local peer and re-proving the certificate on a fresh
		// handshake is the entire point of the probe.
		MaxSessionKeys: wrapperspb.UInt32(0),
	})
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
