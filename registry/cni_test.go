package registry

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	aetherannotations "github.com/bpalermo/aether/common/constants/annotations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCNIPod(annotations map[string]string) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "echo-0",
		Namespace:        "default",
		NetworkNamespace: "/var/run/netns/echo-0",
		ContainerId:      "container-0",
		Ips:              []string{"10.0.0.1"},
		ServiceAccount:   "echo",
		Annotations:      annotations,
	}
}

// TestNewServiceEndpointFromCNIPod_Protocol verifies the service protocol is
// chosen from the endpoint.aether.io/protocol annotation: default HTTP, explicit
// "http", explicit "tcp", and a rejected unknown value.
func TestNewServiceEndpointFromCNIPod_Protocol(t *testing.T) {
	tests := []struct {
		name       string
		annotation map[string]string
		want       registryv1.Service_Protocol
		wantErr    bool
	}{
		{
			name:       "unset defaults to HTTP",
			annotation: nil,
			want:       registryv1.Service_PROTOCOL_HTTP,
		},
		{
			name:       "explicit http",
			annotation: map[string]string{aetherannotations.AnnotationEndpointProtocol: "http"},
			want:       registryv1.Service_PROTOCOL_HTTP,
		},
		{
			name:       "tcp annotation selects PROTOCOL_TCP",
			annotation: map[string]string{aetherannotations.AnnotationEndpointProtocol: "tcp"},
			want:       registryv1.Service_PROTOCOL_TCP,
		},
		{
			name:       "unknown value is rejected",
			annotation: map[string]string{aetherannotations.AnnotationEndpointProtocol: "udp"},
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, protocol, ep, err := NewServiceEndpointFromCNIPod(
				"cluster-a", "node-1", "region-1", "zone-a", "192.168.0.10", testCNIPod(tt.annotation),
			)
			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, registryv1.Service_PROTOCOL_UNSPECIFIED, protocol)
				return
			}
			require.NoError(t, err)
			// 020 Part 1: the registry key is namespace-qualified "<ns>/<svc>".
			assert.Equal(t, "default/echo", service)
			assert.Equal(t, tt.want, protocol)
			require.NotNil(t, ep)
			assert.Equal(t, "10.0.0.1", ep.GetIp())
			// proposal 019: the node's routable IP is advertised for the
			// per-node east/west waypoint (cross-cluster dial target).
			assert.Equal(t, "192.168.0.10", ep.GetKubernetesMetadata().GetNodeIp())
		})
	}
}
