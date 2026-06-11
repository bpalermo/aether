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
			expectedInboundName:  "inbound_test-pod",
			expectedOutboundName: "outbound_http_test-pod",
			expectedError:        false,
		},
		{
			name: "empty network namespace",
			cniPod: &cniv1.CNIPod{
				Name:             "test-pod",
				NetworkNamespace: "",
			},
			expectedError: true,
		},
		{
			name:          "nil pod",
			cniPod:        nil,
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inbound, outbound, appCluster, healthCluster, err := GenerateListenersFromRegistryPod(tt.cniPod, "example.org")

			if tt.expectedError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, inbound)
			require.NotNil(t, outbound)
			require.NotNil(t, appCluster)
			require.NotNil(t, healthCluster)
			assert.Equal(t, HealthProbeClusterName(tt.cniPod), healthCluster.GetName())
			require.Len(t, healthCluster.GetHealthChecks(), 1, "probe cluster carries the HC")
			require.Empty(t, appCluster.GetHealthChecks(), "delivery cluster must NOT carry the HC")
			assert.Equal(t, tt.expectedInboundName, inbound.GetName())
			assert.Equal(t, tt.expectedOutboundName, outbound.GetName())
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
			expectedError: true,
		},
		{
			name:          "nil pod",
			cniPod:        nil,
			expectedError: true,
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
			assert.Equal(t, "outbound_http_"+tt.cniPod.GetName(), listener.GetName())
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

// TestPerConnectionBufferLimits verifies every generated listener and cluster
// caps per-connection buffering at 32 KiB (Envoy default is 1 MiB/connection/
// direction, which turns connection-count incidents into memory incidents on
// a node proxy carrying thousands of connections).
func TestPerConnectionBufferLimits(t *testing.T) {
	pod := &cniv1.CNIPod{
		Name:             "buf-pod",
		NetworkNamespace: "/var/run/netns/buf",
	}
	inbound, outbound, appCluster, healthCluster, err := GenerateListenersFromRegistryPod(pod, "aether.internal")
	require.NoError(t, err)

	want := uint32(perConnectionBufferLimitBytes)
	assert.Equal(t, want, inbound.GetPerConnectionBufferLimitBytes().GetValue(), "inbound listener")
	assert.Equal(t, want, outbound.GetPerConnectionBufferLimitBytes().GetValue(), "outbound listener")
	assert.Equal(t, want, appCluster.GetPerConnectionBufferLimitBytes().GetValue(), "app cluster")
	assert.Equal(t, want, healthCluster.GetPerConnectionBufferLimitBytes().GetValue(), "health cluster")
	assert.Equal(t, want, NewServiceCluster("svc-x").GetPerConnectionBufferLimitBytes().GetValue(), "service cluster")
	assert.Equal(t, want, BuildHealthGatewayListener("/run/aether/health.sock", nil).GetPerConnectionBufferLimitBytes().GetValue(), "health gateway")
}
