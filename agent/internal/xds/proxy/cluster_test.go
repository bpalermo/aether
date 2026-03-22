package proxy

import (
	"testing"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestDefaultClusterConfig(t *testing.T) {
	cc := DefaultClusterConfig()

	assert.Equal(t, "127.0.0.1", cc.LocalClusterBindAddress)
	assert.Equal(t, uint32(1), cc.HealthCheckHealthyThreshold)
	assert.Equal(t, uint32(1), cc.HealthCheckUnhealthyThreshold)
	assert.Equal(t, 5*time.Second, cc.HealthCheckInterval)
	assert.Equal(t, 1*time.Second, cc.HealthCheckTimeout)
	assert.Equal(t, UpstreamProtocolH2, cc.UpstreamProtocol)
}

func TestNewClusterForService(t *testing.T) {
	tests := []struct {
		name             string
		serviceName      string
		cc               *ClusterConfig
		wantH2           bool
		wantProtocolOpts bool
	}{
		{
			name:        "default config produces HTTP/2 cluster",
			serviceName: "my-service",
			cc:          DefaultClusterConfig(),
			wantH2:      true,
		},
		{
			name:        "http1 config produces HTTP/1.1 cluster",
			serviceName: "legacy-service",
			cc: &ClusterConfig{
				UpstreamProtocol: UpstreamProtocolHTTP1,
			},
			wantH2: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := NewClusterForService(tt.serviceName, tt.cc)

			require.NotNil(t, cluster)
			assert.Equal(t, tt.serviceName, cluster.GetName())

			// Verify protocol options key is present.
			opts, ok := cluster.GetTypedExtensionProtocolOptions()[config.UpstreamHTTPProtocolOptionsKey]
			require.True(t, ok, "expected protocol options key")
			require.NotNil(t, opts)

			if tt.wantH2 {
				assertProtocolOptionsH2(t, opts)
			} else {
				assertProtocolOptionsHTTP1(t, opts)
			}
		})
	}
}

func TestNewLocalClusterForService(t *testing.T) {
	endpoint := &registryv1.ServiceEndpoint{
		Ip:   "10.0.0.1",
		Port: 8080,
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			PodName: "test-pod",
		},
		ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
			NetworkNamespace: "/proc/123/ns/net",
		},
	}

	tests := []struct {
		name                       string
		cc                         *ClusterConfig
		wantBindAddress            string
		wantHealthyThreshold       uint32
		wantUnhealthyThreshold     uint32
		wantHealthCheckIntervalSec int64
		wantHealthCheckTimeoutSec  int64
		wantCodecClientType        typev3.CodecClientType
	}{
		{
			name:                       "default config matches previous hardcoded values",
			cc:                         DefaultClusterConfig(),
			wantBindAddress:            "127.0.0.1",
			wantHealthyThreshold:       1,
			wantUnhealthyThreshold:     1,
			wantHealthCheckIntervalSec: 5,
			wantHealthCheckTimeoutSec:  1,
			wantCodecClientType:        typev3.CodecClientType_HTTP2,
		},
		{
			name: "custom health check thresholds",
			cc: &ClusterConfig{
				LocalClusterBindAddress:       "127.0.0.1",
				HealthCheckHealthyThreshold:   3,
				HealthCheckUnhealthyThreshold: 5,
				HealthCheckInterval:           10 * time.Second,
				HealthCheckTimeout:            2 * time.Second,
				UpstreamProtocol:              UpstreamProtocolH2,
			},
			wantBindAddress:            "127.0.0.1",
			wantHealthyThreshold:       3,
			wantUnhealthyThreshold:     5,
			wantHealthCheckIntervalSec: 10,
			wantHealthCheckTimeoutSec:  2,
			wantCodecClientType:        typev3.CodecClientType_HTTP2,
		},
		{
			name: "custom bind address",
			cc: &ClusterConfig{
				LocalClusterBindAddress:       "0.0.0.0",
				HealthCheckHealthyThreshold:   1,
				HealthCheckUnhealthyThreshold: 1,
				HealthCheckInterval:           5 * time.Second,
				HealthCheckTimeout:            1 * time.Second,
				UpstreamProtocol:              UpstreamProtocolH2,
			},
			wantBindAddress:            "0.0.0.0",
			wantHealthyThreshold:       1,
			wantUnhealthyThreshold:     1,
			wantHealthCheckIntervalSec: 5,
			wantHealthCheckTimeoutSec:  1,
			wantCodecClientType:        typev3.CodecClientType_HTTP2,
		},
		{
			name: "http1 upstream protocol",
			cc: &ClusterConfig{
				LocalClusterBindAddress:       "127.0.0.1",
				HealthCheckHealthyThreshold:   1,
				HealthCheckUnhealthyThreshold: 1,
				HealthCheckInterval:           5 * time.Second,
				HealthCheckTimeout:            1 * time.Second,
				UpstreamProtocol:              UpstreamProtocolHTTP1,
			},
			wantBindAddress:            "127.0.0.1",
			wantHealthyThreshold:       1,
			wantUnhealthyThreshold:     1,
			wantHealthCheckIntervalSec: 5,
			wantHealthCheckTimeoutSec:  1,
			wantCodecClientType:        typev3.CodecClientType_HTTP1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := NewLocalClusterForService("local_my-service", endpoint, tt.cc)

			require.NotNil(t, cluster)
			assert.Equal(t, "local_my-service", cluster.GetName())

			// Verify bind address
			bindCfg := cluster.GetUpstreamBindConfig()
			require.NotNil(t, bindCfg)
			assert.Equal(t, tt.wantBindAddress, bindCfg.GetSourceAddress().GetAddress())
			assert.Equal(t, "/proc/123/ns/net", bindCfg.GetSourceAddress().GetNetworkNamespaceFilepath())

			// Verify health check settings
			require.Len(t, cluster.GetHealthChecks(), 1)
			hc := cluster.GetHealthChecks()[0]
			assert.Equal(t, tt.wantHealthyThreshold, hc.GetHealthyThreshold().GetValue())
			assert.Equal(t, tt.wantUnhealthyThreshold, hc.GetUnhealthyThreshold().GetValue())
			assert.Equal(t, tt.wantHealthCheckIntervalSec, hc.GetInterval().GetSeconds())
			assert.Equal(t, tt.wantHealthCheckTimeoutSec, hc.GetTimeout().GetSeconds())

			// Verify codec client type in health check
			httpHC := hc.GetHttpHealthCheck()
			require.NotNil(t, httpHC)
			assert.Equal(t, tt.wantCodecClientType, httpHC.GetCodecClientType())
			assert.Equal(t, "test-pod", httpHC.GetHost())
		})
	}
}

// assertProtocolOptionsH2 verifies that the Any-wrapped protocol options contain HTTP/2 config.
func assertProtocolOptionsH2(t *testing.T, opts *anypb.Any) {
	t.Helper()
	// The type URL should reference HttpProtocolOptions
	assert.Contains(t, opts.GetTypeUrl(), "HttpProtocolOptions")
}

// assertProtocolOptionsHTTP1 verifies that the Any-wrapped protocol options contain HTTP/1.1 config.
func assertProtocolOptionsHTTP1(t *testing.T, opts *anypb.Any) {
	t.Helper()
	assert.Contains(t, opts.GetTypeUrl(), "HttpProtocolOptions")
}
