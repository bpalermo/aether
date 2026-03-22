package proxy

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildDefaultInboundHTTPFilterChain(t *testing.T) {
	tests := []struct {
		name                     string
		filterChainName          string
		tlsCertificateSecretName string
		validationContextName    string
		wantName                 string
	}{
		{
			name:                     "creates inbound filter chain with standard pod name",
			filterChainName:          "my-pod",
			tlsCertificateSecretName: "spiffe://example.org/ns/default/sa/my-svc",
			validationContextName:    "spiffe://example.org",
			wantName:                 "in_http_my-pod",
		},
		{
			name:                     "creates inbound filter chain with hyphenated name",
			filterChainName:          "backend-api-pod",
			tlsCertificateSecretName: "spiffe://example.org/ns/default/sa/backend",
			validationContextName:    "spiffe://example.org",
			wantName:                 "in_http_backend-api-pod",
		},
		{
			name:                     "creates inbound filter chain with empty names",
			filterChainName:          "",
			tlsCertificateSecretName: "",
			validationContextName:    "",
			wantName:                 "in_http_",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := buildDefaultInboundHTTPFilterChain(tt.filterChainName, tt.tlsCertificateSecretName, tt.validationContextName)

			require.NotNil(t, fc)
			assert.Equal(t, tt.wantName, fc.GetName())
			assert.NotEmpty(t, fc.GetFilters(), "inbound filter chain should have network filters")
			assert.NotNil(t, fc.GetTransportSocket(), "inbound filter chain should have a TLS transport socket")
		})
	}
}

func TestBuildDefaultInboundHTTPFilterChain_NameFormat(t *testing.T) {
	podName := "test-pod"
	fc := buildDefaultInboundHTTPFilterChain(podName, "cert", "ca")

	assert.Equal(t, fmt.Sprintf("in_http_%s", podName), fc.GetName())
}

func TestBuildDefaultInboundHTTPFilterChain_HasTLSTransportSocket(t *testing.T) {
	fc := buildDefaultInboundHTTPFilterChain("my-pod", "my-cert", "my-ca")

	require.NotNil(t, fc)
	ts := fc.GetTransportSocket()
	require.NotNil(t, ts)
	assert.Equal(t, tlsTransportSocketName, ts.GetName())
}

func TestBuildDefaultOutboundHTTPFilterChain(t *testing.T) {
	tests := []struct {
		name            string
		filterChainName string
		wantName        string
	}{
		{
			name:            "creates outbound filter chain with standard pod name",
			filterChainName: "my-pod",
			wantName:        "out_http_my-pod",
		},
		{
			name:            "creates outbound filter chain with hyphenated name",
			filterChainName: "frontend-pod",
			wantName:        "out_http_frontend-pod",
		},
		{
			name:            "creates outbound filter chain with empty name",
			filterChainName: "",
			wantName:        "out_http_",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := buildDefaultOutboundHTTPFilterChain(tt.filterChainName)

			require.NotNil(t, fc)
			assert.Equal(t, tt.wantName, fc.GetName())
			assert.NotEmpty(t, fc.GetFilters(), "outbound filter chain should have network filters")
			assert.Nil(t, fc.GetTransportSocket(), "outbound filter chain should not have a transport socket")
		})
	}
}

func TestBuildDefaultOutboundHTTPFilterChain_NameFormat(t *testing.T) {
	podName := "outbound-test-pod"
	fc := buildDefaultOutboundHTTPFilterChain(podName)

	assert.Equal(t, fmt.Sprintf("out_http_%s", podName), fc.GetName())
}

func TestBuildDefaultOutboundHTTPFilterChain_HasMultipleFilters(t *testing.T) {
	// The outbound filter chain includes both the network namespace filter state
	// and the HTTP connection manager filter.
	fc := buildDefaultOutboundHTTPFilterChain("pod")

	require.NotNil(t, fc)
	// Should have at least 2 filters: network namespace filter + HTTP connection manager.
	assert.GreaterOrEqual(t, len(fc.GetFilters()), 2,
		"outbound filter chain should have network namespace filter and HTTP connection manager")
}
