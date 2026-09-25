package registry

import (
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
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
// "http", explicit "tcp", explicit "udp", and a rejected unknown value.
func TestNewServiceEndpointFromCNIPod_Protocol(t *testing.T) {
	tests := []struct {
		name       string
		annotation map[string]string
		// The KEY SET, not a single protocol (proposal 037). A pod whose ports
		// are all one class yields exactly one key -- every pre-037 manifest --
		// so asserting the slice also pins the cardinality: a pod must not
		// silently acquire a second registry key.
		want    []registryv1.Service_Protocol
		wantErr bool
	}{
		{
			name:       "unset defaults to HTTP",
			annotation: nil,
			want:       []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP},
		},
		{
			name:       "explicit http",
			annotation: map[string]string{aetherannotations.AnnotationEndpointProtocol: "http"},
			want:       []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP},
		},
		{
			name:       "tcp annotation selects PROTOCOL_TCP",
			annotation: map[string]string{aetherannotations.AnnotationEndpointProtocol: "tcp"},
			want:       []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_TCP},
		},
		{
			name:       "udp annotation selects PROTOCOL_UDP",
			annotation: map[string]string{aetherannotations.AnnotationEndpointProtocol: "udp"},
			want:       []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_UDP},
		},
		{
			// "udp" used to be this case's unknown value. It is a real protocol
			// now (#931), so the rejection needs a value that is still unknown
			// -- the assertion is "an unrecognised annotation yields no registry
			// keys at all", and it has to keep being tested against something
			// the vocabulary really does not contain.
			name:       "unknown value is rejected",
			annotation: map[string]string{aetherannotations.AnnotationEndpointProtocol: "sctp"},
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, protocols, ep, err := NewServiceEndpointFromCNIPod(
				"cluster-a", "node-1", "region-1", "zone-a", "192.168.0.10", testCNIPod(tt.annotation),
			)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, protocols, "a rejected pod yields no registry keys at all")
				return
			}
			require.NoError(t, err)
			// 020 Part 1: the registry key is namespace-qualified "<ns>/<svc>".
			assert.Equal(t, "default/echo", service)
			assert.Equal(t, tt.want, protocols)
			require.NotNil(t, ep)
			assert.Equal(t, "10.0.0.1", ep.GetIp())
			// proposal 019: the node's routable IP is advertised for the
			// per-node east/west waypoint (cross-cluster dial target).
			assert.Equal(t, "192.168.0.10", ep.GetKubernetesMetadata().GetNodeIp())
		})
	}
}

// TestNewServiceEndpointFromCNIPod_PortProtocols covers the per-port L4 class
// the CNI registration path now carries (proposal 037).
//
// The field is on the ENDPOINT, not in the registry key: the key protocol is
// unchanged, and a reader that predates the field treats its absence as "every
// port is the protocol of my key" — the pre-037 meaning, which is why this is
// additive rather than a migration.
func TestNewServiceEndpointFromCNIPod_PortProtocols(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantKey     []registryv1.Service_Protocol
		wantPorts   map[uint32]registryv1.PortProtocol
		wantErr     string
	}{
		{
			name:        "no port annotations: the default port, HTTP",
			annotations: nil,
			wantKey:     []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP},
			wantPorts: map[uint32]registryv1.PortProtocol{
				8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP,
			},
		},
		{
			name: "a mixed pod: HTTP primary, raw TCP secondary",
			annotations: map[string]string{
				aetherannotations.AnnotationEndpointPort:  "8080",
				aetherannotations.AnnotationEndpointPorts: "8080,9000=tcp",
			},
			// TWO keys: the pod must appear in both listings, or the TCP port
			// is invisible to every client (a cluster with no hosts, not an
			// error). The same endpoint -- full ports and port_protocols --
			// is written under each.
			wantKey: []registryv1.Service_Protocol{
				registryv1.Service_PROTOCOL_HTTP,
				registryv1.Service_PROTOCOL_TCP,
			},
			wantPorts: map[uint32]registryv1.PortProtocol{
				8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP,
				9000: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
			},
		},
		{
			name: "the =h2 codec suffix is still HTTP at L4",
			annotations: map[string]string{
				aetherannotations.AnnotationEndpointPort:  "8080",
				aetherannotations.AnnotationEndpointPorts: "8080,9090=h2",
			},
			wantKey: []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP},
			wantPorts: map[uint32]registryv1.PortProtocol{
				8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP,
				9090: registryv1.PortProtocol_PORT_PROTOCOL_HTTP,
			},
		},
		{
			name: "an unknown suffix fails the registration rather than defaulting",
			annotations: map[string]string{
				aetherannotations.AnnotationEndpointPorts: "9000=quic",
			},
			wantErr: "unknown port protocol suffix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, protocols, ep, err := NewServiceEndpointFromCNIPod(
				"cluster-a", "node-1", "region-1", "zone-a", "192.168.0.10", testCNIPod(tt.annotations),
			)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKey, protocols)
			assert.Equal(t, tt.wantPorts, ep.GetPortProtocols())
		})
	}
}
