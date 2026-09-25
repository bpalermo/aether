package endpointmeta

import (
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func anno(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

const (
	http = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	tcp  = registryv1.PortProtocol_PORT_PROTOCOL_TCP
	udp  = registryv1.PortProtocol_PORT_PROTOCOL_UDP
)

func TestPortProtocols(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        map[uint32]registryv1.PortProtocol
		wantErr     string
	}{
		{
			name:        "no annotations: the default port is HTTP",
			annotations: anno(),
			want:        map[uint32]registryv1.PortProtocol{8080: http},
		},
		{
			name: "pod-level tcp applies to the primary",
			annotations: anno(
				aetherannotations.AnnotationEndpointProtocol, "tcp",
				aetherannotations.AnnotationEndpointPort, "9000"),
			want: map[uint32]registryv1.PortProtocol{9000: tcp},
		},
		{
			name: "unsuffixed ports inherit the pod-level protocol",
			annotations: anno(
				aetherannotations.AnnotationEndpointProtocol, "tcp",
				aetherannotations.AnnotationEndpointPort, "9000",
				aetherannotations.AnnotationEndpointPorts, "9000,5432"),
			want: map[uint32]registryv1.PortProtocol{9000: tcp, 5432: tcp},
		},
		{
			name: "a mixed pod: HTTP primary, raw TCP secondary",
			annotations: anno(
				aetherannotations.AnnotationEndpointPort, "8080",
				aetherannotations.AnnotationEndpointPorts, "8080,9000=tcp"),
			want: map[uint32]registryv1.PortProtocol{8080: http, 9000: tcp},
		},
		{
			name: "h1 and h2 are both HTTP: the codec is not the L4 class",
			annotations: anno(
				aetherannotations.AnnotationEndpointPort, "8080",
				aetherannotations.AnnotationEndpointPorts, "8080=h1,9090=h2,7070=http2"),
			want: map[uint32]registryv1.PortProtocol{8080: http, 9090: http, 7070: http},
		},
		{
			name: "a suffix overrides the pod-level default in BOTH directions",
			annotations: anno(
				aetherannotations.AnnotationEndpointProtocol, "tcp",
				aetherannotations.AnnotationEndpointPort, "9000",
				aetherannotations.AnnotationEndpointPorts, "9000,8080=h1"),
			want: map[uint32]registryv1.PortProtocol{9000: tcp, 8080: http},
		},
		{
			name: "the primary is always a member even if ports omits it",
			annotations: anno(
				aetherannotations.AnnotationEndpointPort, "8080",
				aetherannotations.AnnotationEndpointPorts, "9000=tcp"),
			want: map[uint32]registryv1.PortProtocol{8080: http, 9000: tcp},
		},
		{
			name: "pod-level udp applies to the primary",
			annotations: anno(
				aetherannotations.AnnotationEndpointProtocol, "udp",
				aetherannotations.AnnotationEndpointPort, "9001"),
			want: map[uint32]registryv1.PortProtocol{9001: udp},
		},
		{
			// The shape #931 could not express: a workload serving datagrams on
			// its own port alongside an HTTP port. Before "udp" existed, the
			// UDP port had to be declared "tcp", which made aether TCP-probe it
			// and mark the whole endpoint permanently UNHEALTHY.
			name: "a mixed pod: HTTP primary, datagram secondary",
			annotations: anno(
				aetherannotations.AnnotationEndpointPort, "8080",
				aetherannotations.AnnotationEndpointPorts, "8080,9001=udp"),
			want: map[uint32]registryv1.PortProtocol{8080: http, 9001: udp},
		},
		{
			name: "all three classes on one pod",
			annotations: anno(
				aetherannotations.AnnotationEndpointPort, "8080",
				aetherannotations.AnnotationEndpointPorts, "8080,9000=tcp,9001=udp"),
			want: map[uint32]registryv1.PortProtocol{8080: http, 9000: tcp, 9001: udp},
		},
		{
			// TCP and UDP are separate socket families, so the same port number
			// can legitimately carry both. The registry key is
			// (service, protocol), not (service, port) — so this is two
			// registrations, not a collision. The map cannot represent it:
			// last-suffix-wins, and that is a known limit, not a silent one.
			name: "the same port number declared twice: the later suffix wins",
			annotations: anno(
				aetherannotations.AnnotationEndpointPort, "8080",
				aetherannotations.AnnotationEndpointPorts, "9001=tcp,9001=udp"),
			want: map[uint32]registryv1.PortProtocol{8080: http, 9001: udp},
		},
		{
			name: "an unknown suffix is an error, never a default",
			annotations: anno(
				aetherannotations.AnnotationEndpointPorts, "9000=quic"),
			wantErr: "unknown port protocol suffix",
		},
		{
			name: "grpc is NOT accepted: it would advertise a distinction the data plane does not make",
			annotations: anno(
				aetherannotations.AnnotationEndpointPorts, "9000=grpc"),
			wantErr: "unknown port protocol suffix",
		},
		{
			name: "an invalid pod-level protocol propagates",
			annotations: anno(
				aetherannotations.AnnotationEndpointProtocol, "HTTP/2"),
			wantErr: "invalid protocol annotation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PortProtocols(tt.annotations)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestProtocolsServed covers the registry-key consequence: a pod serving both
// classes must be registered under BOTH keys, and one serving a single class
// under exactly one — the pre-037 behaviour, unchanged for every existing pod.
func TestProtocolsServed(t *testing.T) {
	tests := []struct {
		name string
		in   map[uint32]registryv1.PortProtocol
		want []registryv1.Service_Protocol
	}{
		{
			name: "HTTP only: one key, exactly as before",
			in:   map[uint32]registryv1.PortProtocol{8080: http, 9090: http},
			want: []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP},
		},
		{
			name: "TCP only: one key",
			in:   map[uint32]registryv1.PortProtocol{9000: tcp},
			want: []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_TCP},
		},
		{
			name: "mixed: two keys, sorted",
			in:   map[uint32]registryv1.PortProtocol{8080: http, 9000: tcp},
			want: []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP, registryv1.Service_PROTOCOL_TCP},
		},
		{
			name: "empty: no keys",
			in:   map[uint32]registryv1.PortProtocol{},
			want: []registryv1.Service_Protocol{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ProtocolsServed(tt.in))
		})
	}
}
