package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDefaultInboundHTTPFilterChain(t *testing.T) {
	tests := []struct {
		name                     string
		podName                  string
		tlsCertificateSecretName string
		validationContextName    string
		expectedChainName        string
	}{
		{
			name:                     "standard inbound chain",
			podName:                  "my-pod",
			tlsCertificateSecretName: "spiffe://example.org/ns/default/sa/my-sa",
			validationContextName:    "spiffe://example.org",
			expectedChainName:        "in_http_my-pod",
		},
		{
			name:                     "empty names",
			podName:                  "",
			tlsCertificateSecretName: "",
			validationContextName:    "",
			expectedChainName:        "in_http_",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := buildDefaultInboundHTTPFilterChain(tt.podName, tt.tlsCertificateSecretName, tt.validationContextName)

			require.NotNil(t, fc)
			assert.Equal(t, tt.expectedChainName, fc.GetName())
			assert.NotEmpty(t, fc.GetFilters())
			assert.NotNil(t, fc.GetTransportSocket(), "inbound filter chain should have TLS transport socket")
		})
	}
}

func TestBuildDefaultOutboundHTTPFilterChain(t *testing.T) {
	tests := []struct {
		name              string
		podName           string
		expectedChainName string
	}{
		{
			name:              "standard outbound chain",
			podName:           "my-pod",
			expectedChainName: "out_http_my-pod",
		},
		{
			name:              "empty name",
			podName:           "",
			expectedChainName: "out_http_",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := buildDefaultOutboundHTTPFilterChain(tt.podName)

			require.NotNil(t, fc)
			assert.Equal(t, tt.expectedChainName, fc.GetName())
			// Outbound has 2 filters: set_filter_state + http_connection_manager
			assert.Len(t, fc.GetFilters(), 2)
			assert.Nil(t, fc.GetTransportSocket(), "outbound filter chain should not have TLS transport socket")
		})
	}
}
