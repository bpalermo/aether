package proxy

import (
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestDownstreamTransportSocket(t *testing.T) {
	ts := DownstreamTransportSocket("spiffe://example.org/ns/default/sa/my-sa", "spiffe://example.org", "example.org")

	require.NotNil(t, ts)
	assert.Equal(t, tlsTransportSocketName, ts.GetName())
	require.NotNil(t, ts.GetTypedConfig())

	var ctx transport_sockets_v3.DownstreamTlsContext
	err := proto.Unmarshal(ts.GetTypedConfig().GetValue(), &ctx)
	require.NoError(t, err)

	assert.True(t, ctx.GetRequireClientCertificate().GetValue())
	require.Len(t, ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs(), 1)
	assert.Equal(t, "spiffe://example.org/ns/default/sa/my-sa", ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs()[0].GetName())
	// The trust bundle still rotates over SDS; it just lives one level down now
	// that inline SAN matchers sit alongside it.
	assert.Equal(t, "spiffe://example.org",
		ctx.GetCommonTlsContext().GetCombinedValidationContext().GetValidationContextSdsSecretConfig().GetName())
}

// TestDownstreamTransportSocket_ClientSANPin is issue #843: RequireClientCertificate
// alone proves only that SOME certificate the trust bundle signs was presented,
// and the inbound HCM then stamps that certificate's URI SAN into XFCC with
// SANITIZE_SET for RBAC/ext_authz to key on. The pin requires the SPIFFE path
// to be a workload one (/ns/<ns>/sa/<sa>), which is what makes the XFCC value
// structurally trustworthy.
func TestDownstreamTransportSocket_ClientSANPin(t *testing.T) {
	ts := DownstreamTransportSocket("spiffe://aether.internal/ns/default/sa/echo", "spiffe://aether.internal", "aether.internal")

	var ctx transport_sockets_v3.DownstreamTlsContext
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(&ctx))

	combined := ctx.GetCommonTlsContext().GetCombinedValidationContext()
	require.NotNil(t, combined, "the client pin uses the combined validation context")
	assert.Equal(t, "spiffe://aether.internal", combined.GetValidationContextSdsSecretConfig().GetName(),
		"the SDS-rotated trust bundle must survive the pin")

	matchers := combined.GetDefaultValidationContext().GetMatchTypedSubjectAltNames()
	require.Len(t, matchers, 1)
	assert.Equal(t, transport_sockets_v3.SubjectAltNameMatcher_URI, matchers[0].GetSanType())
	assert.Equal(t, "spiffe://aether.internal/ns/", matchers[0].GetMatcher().GetPrefix(),
		"a PREFIX, not an exact match: the destination cannot know which ServiceAccount may call it")
	assert.Empty(t, matchers[0].GetMatcher().GetExact(),
		"an exact matcher here would pin one ServiceAccount and break every other legitimate caller")
}

// TestDownstreamTransportSocket_AcceptsAndRejects walks concrete SPIFFE IDs
// through the prefix the socket ships, so the pin is described by what it lets
// in rather than by its own spelling. Envoy evaluates match_typed_subject_alt_names
// as a plain string prefix for san_type: URI (StringSanMatcher(GEN_URI, …) in
// source/common/tls/cert_validator/san_matcher.cc), so strings.HasPrefix is the
// same predicate the proxy applies.
func TestDownstreamTransportSocket_AcceptsAndRejects(t *testing.T) {
	ts := DownstreamTransportSocket("spiffe://aether.internal/ns/default/sa/echo", "spiffe://aether.internal", "aether.internal")
	var ctx transport_sockets_v3.DownstreamTlsContext
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(&ctx))
	prefix := ctx.GetCommonTlsContext().GetCombinedValidationContext().
		GetDefaultValidationContext().GetMatchTypedSubjectAltNames()[0].GetMatcher().GetPrefix()

	accepted := []string{
		// An ordinary mesh workload.
		"spiffe://aether.internal/ns/default/sa/echo",
		// A workload in another namespace: any mesh peer may legitimately call.
		"spiffe://aether.internal/ns/other-team/sa/api",
		// The node agent's own SVID — the inbound-readiness probe presents this
		// on every local pod every 5s. Rejecting it would demote endpoints.
		"spiffe://aether.internal/ns/aether-system/sa/aether-agent",
		// The edge gateway's SVID (edge-clusterspiffeid.yaml).
		"spiffe://aether.internal/ns/aether-ingress/sa/aether-edge",
	}
	for _, id := range accepted {
		assert.True(t, strings.HasPrefix(id, prefix), "%s must be accepted", id)
	}

	rejected := []string{
		// SPIRE's own agent/node identities: signed by the same bundle, not a
		// workload. This is the class the pin exists to exclude.
		"spiffe://aether.internal/spire/agent/k8s_psat/talos-main/abc123",
		"spiffe://aether.internal/node/main-worker-01",
		// Another trust domain. Already unreachable in the client direction
		// (the upstream pin is exact on the LOCAL trust domain); this closes
		// the same hole on the server side.
		"spiffe://evil.example/ns/default/sa/echo",
		// A near-miss path that is not the /ns/ workload shape.
		"spiffe://aether.internal/nsfoo/sa/echo",
	}
	for _, id := range rejected {
		assert.False(t, strings.HasPrefix(id, prefix), "%s must be rejected", id)
	}
}

// TestDownstreamTransportSocket_NoTrustDomainEmitsNoMatcher is the #815/#819
// guard rail, and the reason this branch exists at all.
//
// With an empty trust domain the prefix would render "spiffe:///ns/", which
// matches no certificate any CA in the mesh has ever issued — so EVERY inbound
// handshake on that pod would fail, permanently, because the malformed config
// is already published. That is the rev222 failure mode with a whole node's
// blast radius. Emitting no matcher is a real (if brief) authentication
// downgrade, and it is still strictly the lesser evil: the listener keeps
// working and the next rebuild pins it.
//
// In production this branch is unreachable — NewInboundListener returns
// ErrNoTrustDomain first (see TestNewInboundListener_NoTrustDomainRefuses) — so
// it is defence in depth for a future caller, not a live path.
func TestDownstreamTransportSocket_NoTrustDomainEmitsNoMatcher(t *testing.T) {
	ts := DownstreamTransportSocket("", "", "")

	var ctx transport_sockets_v3.DownstreamTlsContext
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(&ctx))

	assert.Nil(t, ctx.GetCommonTlsContext().GetCombinedValidationContext(),
		"no trust domain must emit NO matcher, never the unservable spiffe:///ns/ prefix")
	require.NotNil(t, ctx.GetCommonTlsContext().GetValidationContextSdsSecretConfig(),
		"the bundle-only validation context is the fallback shape")
	assert.True(t, ctx.GetRequireClientCertificate().GetValue(),
		"a client certificate is still required; only the shape check is deferred")

	assert.Empty(t, WorkloadSANPrefix(""), "the malformed prefix must be unrepresentable at the source")
	assert.Equal(t, "spiffe://aether.internal/ns/", WorkloadSANPrefix("aether.internal"))
}

func TestUpstreamTransportSocket(t *testing.T) {
	ts := UpstreamTransportSocket("spiffe://example.org/ns/default/sa/my-sa", "spiffe://example.org", nil, "")

	require.NotNil(t, ts)
	assert.Equal(t, tlsTransportSocketName, ts.GetName())
	require.NotNil(t, ts.GetTypedConfig())

	var ctx transport_sockets_v3.UpstreamTlsContext
	err := proto.Unmarshal(ts.GetTypedConfig().GetValue(), &ctx)
	require.NoError(t, err)

	require.Len(t, ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs(), 1)
	assert.Equal(t, "spiffe://example.org/ns/default/sa/my-sa", ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs()[0].GetName())
	assert.Equal(t, "spiffe://example.org", ctx.GetCommonTlsContext().GetValidationContextSdsSecretConfig().GetName())
	assert.Equal(t, []string{"h2"}, ctx.GetCommonTlsContext().GetAlpnProtocols())
}

// TestUpstreamTransportSocket_SANPinning verifies server-identity pinning:
// with expected SPIFFE IDs the validation context becomes a combined context
// (inline exact URI SAN matchers layered over the SDS trust bundle), and
// without them validation stays bundle-only.
func TestUpstreamTransportSocket_SANPinning(t *testing.T) {
	sans := []string{
		"spiffe://aether.internal/ns/aether-test/sa/svc-1",
		"spiffe://aether.internal/ns/other/sa/svc-1",
	}
	ts := UpstreamTransportSocket("spiffe://aether.internal/ns/x/sa/client", "spiffe://aether.internal", sans, "")

	utc := &transport_sockets_v3.UpstreamTlsContext{}
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(utc))
	combined := utc.GetCommonTlsContext().GetCombinedValidationContext()
	require.NotNil(t, combined, "SAN pinning uses the combined validation context")
	assert.Equal(t, "spiffe://aether.internal", combined.GetValidationContextSdsSecretConfig().GetName(),
		"trust bundle still rotates over SDS")

	matchers := combined.GetDefaultValidationContext().GetMatchTypedSubjectAltNames()
	require.Len(t, matchers, 2)
	for i, m := range matchers {
		assert.Equal(t, transport_sockets_v3.SubjectAltNameMatcher_URI, m.GetSanType())
		assert.Equal(t, sans[i], m.GetMatcher().GetExact())
	}

	// No expected identities: bundle-only (legacy shape).
	ts = UpstreamTransportSocket("spiffe://aether.internal/ns/x/sa/client", "spiffe://aether.internal", nil, "")
	utc = &transport_sockets_v3.UpstreamTlsContext{}
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(utc))
	assert.Nil(t, utc.GetCommonTlsContext().GetCombinedValidationContext())
	require.NotNil(t, utc.GetCommonTlsContext().GetValidationContextSdsSecretConfig())
}

// TestUpstreamTransportSocket_NoSessionResumption pins max_session_keys to 0 on
// EVERY upstream builder. Envoy defaults it to 1, and a resumed session carries
// the peer certificate of the server that created it — which is then re-checked
// against this context's SAN matchers, so a session resumed against the wrong
// server reports (or trusts) an identity that never came off the connection
// (#829). Upstream envoy#45982 scopes the cache by SNI, but aether's SNI is a
// port number, so that scoping cannot discriminate here. If this test fails, the
// mesh has silently re-enabled cross-server session reuse.
func TestUpstreamTransportSocket_NoSessionResumption(t *testing.T) {
	sans := []string{"spiffe://aether.internal/ns/aether-test/sa/echo"}

	for name, ts := range map[string]*corev3.TransportSocket{
		"http":     UpstreamTransportSocket("spiffe://aether.internal/ns/x/sa/client", "spiffe://aether.internal", sans, "8080"),
		"tcp":      UpstreamTCPTransportSocket("spiffe://aether.internal/ns/x/sa/client", "spiffe://aether.internal", sans, "8080"),
		"edge":     EdgeUpstreamTransportSocket("spiffe://aether.internal/ns/x/sa/edge", "spiffe://aether.internal", sans, "8080"),
		"edge-tcp": EdgeUpstreamTCPTransportSocket("spiffe://aether.internal/ns/x/sa/edge", "spiffe://aether.internal", sans),
		// The inbound-readiness probe dials each local pod's inbound listener in
		// its own netns, so its peers are this node's own pods — precisely the
		// population whose SVIDs leaked into other clusters' handshakes in #829.
		"inboundready": UpstreamTransportSocket("spiffe://aether.internal/ns/aether-system/sa/aether-agent", "spiffe://aether.internal", sans, ""),
	} {
		t.Run(name, func(t *testing.T) {
			utc := &transport_sockets_v3.UpstreamTlsContext{}
			require.NoError(t, ts.GetTypedConfig().UnmarshalTo(utc))
			require.NotNil(t, utc.GetMaxSessionKeys(), "max_session_keys must be set explicitly; unset means Envoy's default of 1")
			assert.Zero(t, utc.GetMaxSessionKeys().GetValue(), "client-side TLS session resumption must stay disabled")
		})
	}
}
