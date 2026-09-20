package proxy

import (
	"aethermesh.dev/agent/internal/xds/config"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	filter_state_overridev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_mappers/filter_state_override/v3"
	on_demand_secretv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_selectors/on_demand_secret/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const (
	// tlsTransportSocketName is the Envoy TLS transport socket name
	tlsTransportSocketName = "envoy.transport_sockets.tls"

	// onDemandCertSelectorName is the upstream TLS certificate SELECTOR: it
	// resolves the client certificate per connection, fetching the named secret
	// over SDS on demand rather than requiring it to be listed in the context.
	// Registered for the upstream side as
	// Ssl::UpstreamTlsCertificateSelectorConfigFactory (Envoy
	// source/extensions/transport_sockets/tls/cert_selectors/on_demand/config.cc).
	onDemandCertSelectorName = "envoy.tls.certificate_selectors.on_demand_secret"

	// filterStateCertMapperName is the upstream certificate MAPPER the selector
	// calls to turn a connection into a secret name: it reads the
	// SourceIdentityCertMapperFilterStateKey object off the downstream-shared
	// filter state and returns its string, or default_value when absent.
	filterStateCertMapperName = "envoy.tls.upstream_certificate_mappers.filter_state_override"

	// AgentXDSClusterName is the bootstrap-defined static cluster the node proxy
	// reaches its agent on (the /run/aether/xds.sock pipe). It is the cluster
	// dynamic_resources.ads_config already names; it MUST stay spelled the way
	// charts/aether/templates/agent-proxy-configmap.yaml spells it.
	//
	// The on-demand certificate selector points at it through its OWN
	// api_config_source rather than through `ads: {}` — see
	// meshCertSelectorSDSSource, where the reason is the whole of issue #842's
	// rev228 outage.
	AgentXDSClusterName = "agent_xds"
)

// meshCertSelectorSDSSource is the config source the per-connection certificate
// selector fetches client certificates over, and it is DELIBERATELY NOT the
// shared ADS stream every other SDS reference on this proxy uses.
//
// # Why (issue #842, the rev228 outage)
//
// Envoy's SecretManagerImpl keys a secret provider on
// `hash(ConfigSource) + "." + name + warm` (secret_manager_impl.h). The
// on-demand selector always asks with warm=false (it must not hold its cluster
// in warming); every statically named SDS reference asks with warm=true. So for
// one secret name Envoy builds TWO independent SdsApi objects with TWO
// independent watches.
//
// On a node proxy those two watches are not hypothetical. EVERY local pod's
// SVID is already named statically by that pod's own inbound listener
// (DownstreamTransportSocket), and the node SVID — the selector's default_value
// AND its prefetch — is already named statically by every inboundready_<pod>
// probe cluster. The selector is therefore always the SECOND subscriber.
//
// Put both watches on one DELTA_GRPC mux and the second one starves. Envoy's
// delta WatchMap deduplicates subscription interest per (type_url, resource
// name) across watches, so the second watch contributes nothing to
// resource_names_subscribe; no request goes out; and a delta control plane —
// correctly — sends only what changed, which is nothing. The second watch never
// receives the resource. It burns its 15 s initial_fetch_timeout
// (`sds.<name>.init_fetch_timeout`, the one stat that moved on the fleet) and
// then does nothing else, because SdsApi::onConfigUpdateFailed only calls
// init_target_.ready() — it does NOT notify the parked certificate-selection
// callback.
//
// The result is the worst shape a mesh has: the upstream handshake is SUSPENDED,
// not failed. No ssl_connection_error, no ssl_fail_verify_san, no
// upstream_transport_failure_reason — the request hangs until the cluster's
// connect_timeout or, sooner, until the downstream client gives up (DC).
//
// # Why a separate api_config_source fixes it
//
// A distinct ConfigSource gets its own gRPC mux with its own WatchMap, so the
// selector's subscription can no longer be deduplicated against a static one.
// It also changes `hash(ConfigSource)`, but that alone would NOT be enough: the
// provider key would differ while the mux — and therefore the watch map that
// does the deduplication — stayed shared. Adding an initial_fetch_timeout to
// `ads: {}` to perturb the hash is NOT a fix. The stream has to be separate.
//
// SotW (ApiConfigSource_GRPC, what SDSConfigSourceFromCluster emits) rather than
// DELTA_GRPC is chosen deliberately: Envoy's SotW GrpcMuxImpl::addWatch queues a
// discovery request unconditionally and a SotW response carries every requested
// resource, so the failure above is structurally impossible on this stream even
// if some future reference does land on it.
//
// # What it costs, measured
//
// Envoy builds a mux PER non-ADS subscription, so this is not one extra stream
// but ONE STREAM PER SECRET NAME the selector resolves — i.e. per identity
// actually originating traffic on this node, appearing lazily on first use.
// Observed directly in //test/mtlspool (one gRPC stream per name, each with its
// own version/nonce sequence). Each carries exactly one resource, so the SotW
// re-send on a secret version bump is one certificate, not the node's set.
//
// Nothing caps that below the agent's own server-side limit: the chart's
// `max_concurrent_streams: 10` on agent_xds is what the proxy ADVERTISES to the
// peer for peer-initiated streams, not a limit on streams Envoy opens. The
// governing value is the agent's grpc.MaxConcurrentStreams(1000)
// (common/xds/xds.go).
//
// The agent already serves SecretDiscoveryService on the same socket as ADS
// (common/xds/xds.go registers both), so this needs no new listener, no new
// bootstrap cluster and no chart change beyond a comment.
func meshCertSelectorSDSSource() *corev3.ConfigSource {
	return config.SDSConfigSourceFromCluster(AgentXDSClusterName)
}

// upstreamCertSelector builds the per-connection client-certificate selector
// every mesh upstream socket carries since issue #842.
//
// This is the whole of the per-source mTLS mechanism now. It replaced a
// per-identity `transport_socket_matches` list plus a `transport_socket_matcher`
// whose exact_match_map named every local ServiceAccount — the structure #815
// spent three releases making cheap to rebuild, and which this deletes outright.
// One socket per cluster; the certificate is chosen per connection from filter
// state.
//
// Three things make it work, and all three must hold together:
//
//   - The source pod's SPIFFE ID reaches the upstream connection as a SHARED
//     filter-state object under the mapper's hardcoded name
//     (buildCertMapperIdentityFilterState).
//   - OUR SDS SECRET NAMES ARE SPIFFE IDs. The mapper returns the filter-state
//     string verbatim AS THE SECRET NAME, so this only works because the two are
//     the same string. They are: the SPIRE bridge publishes one secret per local
//     workload named by proxy.SpiffeIDFromPod, which is the same function
//     SourceIdentityForPod stamps into the listener. Check both ends before
//     changing either — a mismatch is a silent fallback to default_value.
//   - The object is HASHABLE, so the identity is in the upstream pool key and a
//     pool can never hand a connection carrying one identity's certificate to
//     another source. Without that this configuration is the #831 leak.
//
// defaultSecretName is what a connection with NO filter state gets. Required
// (min_len: 1) and deliberately the NODE identity — see meshUpstreamCertSelector.
//
// sdsSource MUST be a config source no statically named secret reference uses —
// see meshCertSelectorSDSSource, which is the whole of the rev228 outage. Do not
// "simplify" this back to config.XDSConfigSourceADS().
//
// The secret is fetched over sdsSource on first use; the cluster initializes
// without waiting for it ("allowing the parent cluster or listener to accept
// connections without warming"), and the FIRST handshake per (secret name,
// worker) is paused until the SDS response lands. prefetchSecretNames starts
// those fetches at config load instead; we prefetch exactly the default so the
// no-filter-state path never pauses, and deliberately NOT the workload
// identities — listing them would put the per-node identity SET back into every
// cluster's bytes, which is the churn #815 removed.
func upstreamCertSelector(defaultSecretName string, sdsSource *corev3.ConfigSource) *corev3.TypedExtensionConfig {
	return &corev3.TypedExtensionConfig{
		Name: onDemandCertSelectorName,
		TypedConfig: config.TypedConfig(&on_demand_secretv3.Config{
			ConfigSource: sdsSource,
			CertificateMapper: &corev3.TypedExtensionConfig{
				Name: filterStateCertMapperName,
				TypedConfig: config.TypedConfig(&filter_state_overridev3.Config{
					DefaultValue: defaultSecretName,
				}),
			},
			PrefetchSecretNames: []string{defaultSecretName},
		}),
	}
}

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
	return upstreamTransportSocket(tlsCertificateSecretName, validationContextName, sanURIs, sni, config.XDSConfigSourceADS(), nil)
}

// MeshUpstreamTransportSocket is the ONE upstream socket a node-proxy mesh
// service cluster carries since issue #842. It is UpstreamTransportSocket with
// the statically named client certificate replaced by the per-connection
// on-demand certificate selector (upstreamCertSelector): the certificate is
// chosen at handshake time from the source pod's SPIFFE ID in filter state,
// instead of by a per-identity transport_socket_matches list the control plane
// had to rebuild whenever the node's identity SET changed.
//
// nodeSpiffeID is the selector's default_value — the certificate a connection
// carrying no source identity presents, which is what the pre-#842 matcher's
// on_no_match presented for exactly the same connections. Everything else
// (ALPN h2, the SAN pin, the SNI, MaxSessionKeys 0) is byte-identical to
// UpstreamTransportSocket.
func MeshUpstreamTransportSocket(nodeSpiffeID string, validationContextName string, sanURIs []string, sni string) *corev3.TransportSocket {
	return upstreamTransportSocket("", validationContextName, sanURIs, sni,
		config.XDSConfigSourceADS(),
		upstreamCertSelector(nodeSpiffeID, meshCertSelectorSDSSource()))
}

// UpstreamTCPTransportSocket creates a TLS transport socket for TCP-proxy upstream
// connections (proposal 018, Phase 3a TCP floor). It is identical to
// UpstreamTransportSocket except that it advertises NO ALPN (vs HTTP's "h2"). The
// destination inbound listener's HTTP chains match application_protocols:["h2"], so
// a no-ALPN mTLS connection falls through to the inbound TCP floor's DEFAULT chain —
// demultiplexing TCP from HTTP with the standard h2 ALPN instead of a bespoke token.
// sanURIs and sni semantics are unchanged.
func UpstreamTCPTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string, sni string) *corev3.TransportSocket {
	return upstreamTransportSocket(tlsCertificateSecretName, validationContextName, sanURIs, sni, config.XDSConfigSourceADS(), nil, "")
}

// MeshUpstreamTCPTransportSocket is MeshUpstreamTransportSocket for the TCP
// floor: the same per-connection certificate selector, with no ALPN so the
// destination inbound demuxes to its TCP floor default chain.
func MeshUpstreamTCPTransportSocket(nodeSpiffeID string, validationContextName string, sanURIs []string, sni string) *corev3.TransportSocket {
	return upstreamTransportSocket("", validationContextName, sanURIs, sni,
		config.XDSConfigSourceADS(),
		upstreamCertSelector(nodeSpiffeID, meshCertSelectorSDSSource()), "")
}

// EdgeUpstreamTransportSocket is UpstreamTransportSocket for the edge proxy: it
// fetches the (single) edge SVID and trust bundle over the SPIRE Agent's native
// SDS API (the static spire_agent cluster) instead of the agent's ADS stream.
// SAN pinning is unchanged — match_typed_subject_alt_names is inline static
// config; only the validation-context bundle comes over SDS.
func EdgeUpstreamTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string, sni string) *corev3.TransportSocket {
	return upstreamTransportSocket(tlsCertificateSecretName, validationContextName, sanURIs, sni, config.SDSConfigSourceFromCluster(SpireAgentSDSClusterName), nil)
}

// EdgeUpstreamTCPTransportSocket is EdgeUpstreamTransportSocket for TCP floor
// clusters: it advertises NO ALPN (like the east-west UpstreamTCPTransportSocket)
// and NO SNI so the destination inbound demuxes to the TCP floor's DEFAULT chain,
// while still fetching the edge SVID and trust bundle directly from the SPIRE Agent
// (not the agent's ADS stream). #304 removed the bespoke "aether-tcp" ALPN; #306
// established that the SNI must be empty for the floor — a non-empty SNI would
// hit the destination's per-port HCM chain instead of the floor default.
func EdgeUpstreamTCPTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string) *corev3.TransportSocket {
	return upstreamTransportSocket(tlsCertificateSecretName, validationContextName, sanURIs, "" /* no SNI */, config.SDSConfigSourceFromCluster(SpireAgentSDSClusterName), nil, "" /* no ALPN */)
}

// upstreamTransportSocket builds an upstream TLS context. With no alpnOverride the
// ALPN is "h2" (HTTP/2 mesh transport). An alpnOverride of "" suppresses ALPN
// entirely — the TCP floor path, so the destination inbound demuxes it as the
// default (non-h2) chain; any other override value sets that explicit ALPN list.
//
// EXACTLY ONE of tlsCertificateSecretName and certSelector supplies the client
// certificate. A nil certSelector is the static form: one SDS secret named up
// front (the edge, and the pre-#842 mesh shape). A non-nil certSelector is the
// per-connection form, and then NO tls_certificate_sds_secret_configs is
// emitted at all — the selector owns certificate resolution end to end, and a
// statically named certificate beside it would be dead config that the cluster
// still has to resolve before it leaves warming, reintroducing the very
// coupling the selector removes.
func upstreamTransportSocket(tlsCertificateSecretName string, validationContextName string, sanURIs []string, sni string, sdsSource *corev3.ConfigSource, certSelector *corev3.TypedExtensionConfig, alpnOverride ...string) *corev3.TransportSocket {
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
	}
	if certSelector != nil {
		common.CustomTlsCertificateSelector = certSelector
	} else {
		common.TlsCertificateSdsSecretConfigs = []*transport_sockets_v3.SdsSecretConfig{
			sdsSecretConfigFrom(tlsCertificateSecretName, sdsSource),
		}
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
	//
	// SINCE #842 IT IS ALSO A PRECONDITION OF THE CERTIFICATE SELECTOR, not just
	// a defence against #829. tls.proto states that a client context supports
	// more than one TLS certificate only when custom_tls_certificate_selector is
	// explicitly defined AND max_session_keys is 0 — a cached client session is
	// keyed without reference to which on-demand certificate produced it, so
	// resuming one would hand a connection a certificate the selector did not
	// choose for it. That is exactly the cross-source mix-up this change exists
	// to make impossible. Raising max_session_keys here breaks per-source
	// identity, not merely performance. Keep it 0.
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
