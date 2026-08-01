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
			inbound, outbound, appClusters, healthCluster, err := GenerateListenersFromRegistryPod(tt.cniPod, "example.org", "example.org", false, false, nil, nil)

			if tt.expectedError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, inbound)
			require.NotNil(t, outbound)
			require.NotEmpty(t, appClusters)
			appCluster := appClusters[0]
			require.NotNil(t, healthCluster)
			assert.Equal(t, HealthProbeClusterName(tt.cniPod), healthCluster.GetName())
			require.Len(t, healthCluster.GetHealthChecks(), 1, "probe cluster carries the HC")
			assert.Equal(t, "app", appCluster.GetAltStatName(),
				"app clusters collapse into one cluster.app.* stats block (cardinality round 2)")
			assert.Empty(t, healthCluster.GetAltStatName(),
				"health probe cluster stats MUST stay per-pod: the health_check filter reads THIS cluster's membership gauges (2026-06-11 outage) — a shared alt_stat_name merges every pod's membership")
			hc := healthCluster.GetHealthChecks()[0]
			assert.Equal(t, hc.GetInterval().AsDuration(), hc.GetNoTrafficInterval().AsDuration(),
				"probe cluster never carries routed traffic; the no-traffic interval (default 60s) would govern every check after the first")
			assert.Equal(t, hc.GetInterval().AsDuration(), hc.GetNoTrafficHealthyInterval().AsDuration(),
				"healthy no-traffic interval must match too, or app-death demotion waits up to 60s")
			require.Empty(t, appCluster.GetHealthChecks(), "delivery cluster must NOT carry the HC")
			assert.Equal(t, tt.expectedInboundName, inbound.GetName())
			assert.Equal(t, tt.expectedOutboundName, outbound.GetName())
		})
	}
}

// TestNewAppHealthProbeCluster_CheckerType verifies the active health checker is a
// raw TCP connect for TCP-floor services and an HTTP GET otherwise (proposal 018
// Phase 3b TCP liveness).
func TestNewAppHealthProbeCluster_CheckerType(t *testing.T) {
	httpC := NewAppHealthProbeCluster("health_p", "/var/run/netns/x", 8080, "/-/-/ready", false)
	require.Len(t, httpC.GetHealthChecks(), 1)
	require.NotNil(t, httpC.GetHealthChecks()[0].GetHttpHealthCheck(), "HTTP service: HTTP health check")
	assert.Nil(t, httpC.GetHealthChecks()[0].GetTcpHealthCheck())
	assert.Equal(t, "/-/-/ready", httpC.GetHealthChecks()[0].GetHttpHealthCheck().GetPath())

	tcpC := NewAppHealthProbeCluster("health_p", "/var/run/netns/x", 9000, "/-/-/ready", true)
	require.Len(t, tcpC.GetHealthChecks(), 1)
	require.NotNil(t, tcpC.GetHealthChecks()[0].GetTcpHealthCheck(), "TCP service: connect-only TCP health check")
	assert.Nil(t, tcpC.GetHealthChecks()[0].GetHttpHealthCheck())
	// Connect-only: no send/receive payloads.
	assert.Empty(t, tcpC.GetHealthChecks()[0].GetTcpHealthCheck().GetSend())
	assert.Empty(t, tcpC.GetHealthChecks()[0].GetTcpHealthCheck().GetReceive())
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
			listener, err := GenerateOutboundHTTPListener(tt.cniPod, "aether.internal", false, nil)

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
	inbound, outbound, appClusters, healthCluster, err := GenerateListenersFromRegistryPod(pod, "aether.internal", "aether.internal", false, false, nil, nil)
	require.NotEmpty(t, appClusters)
	appCluster := appClusters[0]
	require.NoError(t, err)

	want := uint32(perConnectionBufferLimitBytes)
	assert.Equal(t, want, inbound.GetPerConnectionBufferLimitBytes().GetValue(), "inbound listener")
	assert.Equal(t, want, outbound.GetPerConnectionBufferLimitBytes().GetValue(), "outbound listener")
	assert.Equal(t, want, appCluster.GetPerConnectionBufferLimitBytes().GetValue(), "app cluster")
	assert.Equal(t, want, healthCluster.GetPerConnectionBufferLimitBytes().GetValue(), "health cluster")
	assert.Equal(t, want, NewServiceCluster("svc-x.aether.internal", "svc-x", "svc-x", nil, true).GetPerConnectionBufferLimitBytes().GetValue(), "service cluster")
	assert.Equal(t, want, BuildHealthGatewayListener("/run/aether/health.sock", nil).GetPerConnectionBufferLimitBytes().GetValue(), "health gateway")
}
