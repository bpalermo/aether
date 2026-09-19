package proxy

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestDownstreamTransportSocket(t *testing.T) {
	ts := DownstreamTransportSocket("spiffe://example.org/ns/default/sa/my-sa", "spiffe://example.org")

	require.NotNil(t, ts)
	assert.Equal(t, tlsTransportSocketName, ts.GetName())
	require.NotNil(t, ts.GetTypedConfig())

	var ctx transport_sockets_v3.DownstreamTlsContext
	err := proto.Unmarshal(ts.GetTypedConfig().GetValue(), &ctx)
	require.NoError(t, err)

	assert.True(t, ctx.GetRequireClientCertificate().GetValue())
	require.Len(t, ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs(), 1)
	assert.Equal(t, "spiffe://example.org/ns/default/sa/my-sa", ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs()[0].GetName())
	assert.Equal(t, "spiffe://example.org", ctx.GetCommonTlsContext().GetValidationContextSdsSecretConfig().GetName())
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
