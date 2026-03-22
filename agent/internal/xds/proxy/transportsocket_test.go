package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDownstreamTransportSocket(t *testing.T) {
	tests := []struct {
		name                     string
		tlsCertificateSecretName string
		validationContextName    string
	}{
		{
			name:                     "creates downstream TLS socket with SPIFFE IDs",
			tlsCertificateSecretName: "spiffe://example.org/ns/default/sa/my-service",
			validationContextName:    "spiffe://example.org",
		},
		{
			name:                     "creates downstream TLS socket with custom names",
			tlsCertificateSecretName: "my-cert",
			validationContextName:    "my-ca",
		},
		{
			name:                     "creates downstream TLS socket with empty names",
			tlsCertificateSecretName: "",
			validationContextName:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := DownstreamTransportSocket(tt.tlsCertificateSecretName, tt.validationContextName)

			require.NotNil(t, ts)
			assert.Equal(t, tlsTransportSocketName, ts.GetName())
			require.NotNil(t, ts.GetTypedConfig())
		})
	}
}

func TestUpstreamTransportSocket(t *testing.T) {
	tests := []struct {
		name                     string
		tlsCertificateSecretName string
		validationContextName    string
	}{
		{
			name:                     "creates upstream TLS socket with SPIFFE IDs",
			tlsCertificateSecretName: "spiffe://example.org/ns/default/sa/my-service",
			validationContextName:    "spiffe://example.org",
		},
		{
			name:                     "creates upstream TLS socket with custom names",
			tlsCertificateSecretName: "client-cert",
			validationContextName:    "root-ca",
		},
		{
			name:                     "creates upstream TLS socket with empty names",
			tlsCertificateSecretName: "",
			validationContextName:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := UpstreamTransportSocket(tt.tlsCertificateSecretName, tt.validationContextName)

			require.NotNil(t, ts)
			assert.Equal(t, tlsTransportSocketName, ts.GetName())
			require.NotNil(t, ts.GetTypedConfig())
		})
	}
}

func TestTransportSocketName(t *testing.T) {
	// Both downstream and upstream sockets should use the TLS transport socket name.
	downstream := DownstreamTransportSocket("cert", "ca")
	upstream := UpstreamTransportSocket("cert", "ca")

	assert.Equal(t, tlsTransportSocketName, downstream.GetName())
	assert.Equal(t, tlsTransportSocketName, upstream.GetName())
	assert.Equal(t, "envoy.transport_sockets.tls", downstream.GetName())
}
