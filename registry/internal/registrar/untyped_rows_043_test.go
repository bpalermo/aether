package registrar

import (
	"context"
	"testing"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry/endpointmeta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// registerCounter is a registrar that counts the registrations it is sent.
type registerCounter struct {
	registrarv1.RegistrarServiceClient // nil-embedded; only RegisterEndpoint is used
	sent                               []*registrarv1.RegisterEndpointRequest
}

func (c *registerCounter) RegisterEndpoint(_ context.Context, in *registrarv1.RegisterEndpointRequest, _ ...grpc.CallOption) (*registrarv1.RegisterEndpointResponse, error) {
	c.sent = append(c.sent, in)
	return &registrarv1.RegisterEndpointResponse{}, nil
}

// TestRegisterEndpoint_RefusesARowWithoutPortProtocols: port_protocols is
// required (proposal 043). The registrar refuses such a row; its client
// refuses it first, with the same error every backend returns, and sends
// nothing.
func TestRegisterEndpoint_RefusesARowWithoutPortProtocols(t *testing.T) {
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	for name, row := range map[string]*registryv1.ServiceEndpoint{
		"no map":                     {Ip: "10.0.0.1", Port: 8080},
		"an empty map":               {Ip: "10.0.0.1", Port: 8080, PortProtocols: map[uint32]registryv1.PortProtocol{}},
		"a listed port is not named": {Ip: "10.0.0.1", Port: 8080, Ports: []uint32{8080, 9090}, PortProtocols: map[uint32]registryv1.PortProtocol{8080: h}},
		"a port named with no class": {Ip: "10.0.0.1", Port: 8080, PortProtocols: map[uint32]registryv1.PortProtocol{8080: registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED}},
		"no row":                     nil,
	} {
		t.Run(name, func(t *testing.T) {
			r := newTestRegistry()
			client := &registerCounter{}
			r.client = client
			for _, protocol := range []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP, registryv1.Service_PROTOCOL_TCP, registryv1.Service_PROTOCOL_UDP} {
				err := r.RegisterEndpoint(context.Background(), "ns/svc", protocol, row)
				require.ErrorIs(t, err, endpointmeta.ErrRowNotTyped, "under %s", protocol)
			}
			assert.Empty(t, client.sent, "a refused row was sent to the registrar")
		})
	}

	// The control: a typed row is sent, as it is.
	r := newTestRegistry()
	client := &registerCounter{}
	r.client = client
	typed := &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Port: 8080, Ports: []uint32{8080}, PortProtocols: map[uint32]registryv1.PortProtocol{8080: h}}
	require.NoError(t, r.RegisterEndpoint(context.Background(), "ns/svc", registryv1.Service_PROTOCOL_HTTP, typed))
	require.Len(t, client.sent, 1)
	assert.Same(t, typed, client.sent[0].GetEndpoint())
}
