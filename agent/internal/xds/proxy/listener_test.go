package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateListenersFromRegistryPod(t *testing.T) {
	tests := []struct {
		name                 string
		cniPod               *cniv1.CNIPod
		expectedInboundName  string
		expectedOutboundName string
		expectedError        bool
	}{
		{
			name: "standard pod",
			cniPod: &cniv1.CNIPod{
				Name:             "test-pod",
				NetworkNamespace: "/var/run/netns/test",
			},
			expectedInboundName:  "inbound_http",
			expectedOutboundName: "outbound_http",
			expectedError:        false,
		},
		{
			name: "empty network namespace",
			cniPod: &cniv1.CNIPod{
				Name:             "test-pod",
				NetworkNamespace: "",
			},
			expectedInboundName:  "inbound_http",
			expectedOutboundName: "outbound_http",
			expectedError:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inbound, outbound, err := GenerateListenersFromRegistryPod(tt.cniPod)

			if tt.expectedError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, inbound)
			require.NotNil(t, outbound)
			assert.Equal(t, tt.expectedInboundName, inbound.GetName())
			assert.Equal(t, tt.expectedOutboundName, outbound.GetName())
		})
	}
}

func TestGenerateInboundHTTPListener(t *testing.T) {
	tests := []struct {
		name                     string
		cniPod                   *cniv1.CNIPod
		expectedStatPrefix       string
		expectedNetworkNamespace string
		expectedError            bool
	}{
		{
			name: "standard pod",
			cniPod: &cniv1.CNIPod{
				Name:             "test-pod",
				NetworkNamespace: "/var/run/netns/test",
			},
			expectedStatPrefix:       "in_http_test-pod",
			expectedNetworkNamespace: "/var/run/netns/test",
			expectedError:            false,
		},
		{
			name: "empty network namespace",
			cniPod: &cniv1.CNIPod{
				Name:             "another-pod",
				NetworkNamespace: "",
			},
			expectedStatPrefix:       "in_http_another-pod",
			expectedNetworkNamespace: "",
			expectedError:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := generateInboundHTTPListener(tt.cniPod)

			if tt.expectedError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, listener)
			assert.Equal(t, "inbound_http", listener.GetName())
			assert.Equal(t, tt.expectedStatPrefix, listener.GetStatPrefix())
			assert.Equal(t, corev3.TrafficDirection_INBOUND, listener.GetTrafficDirection())

			address := listener.GetAddress()
			require.NotNil(t, address)

			socketAddr := address.GetSocketAddress()
			require.NotNil(t, socketAddr)
			assert.Equal(t, defaultInboundAddress, socketAddr.GetAddress())
			assert.Equal(t, uint32(defaultHTTPInboundPort), socketAddr.GetPortValue())
			assert.Equal(t, corev3.SocketAddress_TCP, socketAddr.GetProtocol())
			assert.Equal(t, tt.expectedNetworkNamespace, socketAddr.GetNetworkNamespaceFilepath())

			assert.NotEmpty(t, listener.GetListenerFilters())
			assert.Len(t, listener.GetFilterChains(), 1)
		})
	}
}

func TestGenerateOutboundHTTPListener(t *testing.T) {
	tests := []struct {
		name                     string
		cniPod                   *cniv1.CNIPod
		expectedStatPrefix       string
		expectedNetworkNamespace string
		expectedError            bool
	}{
		{
			name: "standard pod",
			cniPod: &cniv1.CNIPod{
				Name:             "test-pod",
				NetworkNamespace: "/var/run/netns/test",
			},
			expectedStatPrefix:       "out_http_test-pod",
			expectedNetworkNamespace: "/var/run/netns/test",
			expectedError:            false,
		},
		{
			name: "empty network namespace",
			cniPod: &cniv1.CNIPod{
				Name:             "another-pod",
				NetworkNamespace: "",
			},
			expectedStatPrefix:       "out_http_another-pod",
			expectedNetworkNamespace: "",
			expectedError:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := generateOutboundHTTPListener(tt.cniPod)

			if tt.expectedError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, listener)
			assert.Equal(t, "outbound_http", listener.GetName())
			assert.Equal(t, tt.expectedStatPrefix, listener.GetStatPrefix())
			assert.Equal(t, corev3.TrafficDirection_OUTBOUND, listener.GetTrafficDirection())

			address := listener.GetAddress()
			require.NotNil(t, address)

			socketAddr := address.GetSocketAddress()
			require.NotNil(t, socketAddr)
			assert.Equal(t, defaultOutboundAddress, socketAddr.GetAddress())
			assert.Equal(t, uint32(defaultHTTPOutboundPort), socketAddr.GetPortValue())
			assert.Equal(t, corev3.SocketAddress_TCP, socketAddr.GetProtocol())
			assert.Equal(t, tt.expectedNetworkNamespace, socketAddr.GetNetworkNamespaceFilepath())

			assert.Len(t, listener.GetFilterChains(), 1)
		})
	}
}
