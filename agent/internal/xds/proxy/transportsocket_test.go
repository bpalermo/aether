package proxy

import (
	"testing"

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
	ts := UpstreamTransportSocket("spiffe://example.org/ns/default/sa/my-sa", "spiffe://example.org")

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
