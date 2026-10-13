package endpointmeta

import (
	"errors"
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRowProblem: port_protocols is required. A row is typed when its map
// names every port it lists, each with a class; anything else has a reason.
func TestRowProblem(t *testing.T) {
	const (
		none = registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED
		h    = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
		tcp  = registryv1.PortProtocol_PORT_PROTOCOL_TCP
		udp  = registryv1.PortProtocol_PORT_PROTOCOL_UDP
	)
	type classes = map[uint32]registryv1.PortProtocol
	row := func(port uint32, ports []uint32, pp classes) *registryv1.ServiceEndpoint {
		return &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Port: port, Ports: ports, PortProtocols: pp}
	}
	for _, c := range []struct {
		name string
		row  *registryv1.ServiceEndpoint
		want string
	}{
		{"one typed port", row(8080, nil, classes{8080: h}), ""},
		{"every listed port typed, three classes", row(8080, []uint32{8080, 9000, 5353}, classes{8080: h, 9000: tcp, 5353: udp}), ""},
		{"no declared port, a list and a map that agree", row(0, []uint32{8080}, classes{8080: h}), ""},

		{"nil row", nil, RowNoPortProtocols},
		{"no map", row(8080, []uint32{8080, 9090}, nil), RowNoPortProtocols},
		{"an empty map", row(8080, nil, classes{}), RowNoPortProtocols},
		{"only an address", row(0, nil, nil), RowNoPortProtocols},

		{"the declared port is not named", row(8080, []uint32{9090}, classes{9090: h}), RowPortNotTyped},
		{"a class this build does not know", row(8080, nil, classes{8080: registryv1.PortProtocol(7)}), RowPortNotTyped},

		{"the map names a port the lists do not", row(8080, []uint32{8080}, classes{8080: h, 9000: tcp}), RowPortNotListed},
		{"no declared port and no list, a typed map", row(0, nil, classes{8080: h}), RowPortNotListed},
		{"a port of the list is not named", row(8080, []uint32{8080, 9090}, classes{8080: h}), RowPortNotTyped},
		{"the declared port is named with no class", row(8080, nil, classes{8080: none}), RowPortNotTyped},
		{"a port only the map names, with no class", row(8080, nil, classes{8080: h, 9000: none}), RowPortNotTyped},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, RowProblem(c.row))
			err := CheckRow(c.row)
			if c.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrRowNotTyped))
			assert.Contains(t, err.Error(), c.want, "the error names the reason")
		})
	}
}

// TestPortProtocols_AlwaysTypesEveryAdvertisedPort: the one parser both
// writers build a row from (the node agent at CNI ADD, the kubernetes backend
// from a pod) names the primary port and every port of the list, each with a
// class, for every annotation set it accepts. Neither writer can therefore
// produce a row RowProblem refuses; a set it does not accept produces no row.
func TestPortProtocols_AlwaysTypesEveryAdvertisedPort(t *testing.T) {
	ports := []string{"", "8080", "8080,9090", "9090=h2,9000=tcp", "5353=udp", "8080=h1, 9000=tcp ,5353=udp", "18081,18082=tcp", "9000"}
	protocols := []string{"", "http", "tcp", "udp"}
	primaries := []string{"", "8080", "9000", "80"}
	n := 0
	for _, list := range ports {
		for _, protocol := range protocols {
			for _, primary := range primaries {
				a := map[string]string{}
				if list != "" {
					a[aetherannotations.AnnotationEndpointPorts] = list
				}
				if protocol != "" {
					a[aetherannotations.AnnotationEndpointProtocol] = protocol
				}
				if primary != "" {
					a[aetherannotations.AnnotationEndpointPort] = primary
				}
				pp, err := PortProtocols(a)
				require.NoErrorf(t, err, "%v", a)
				port, err := Port(a)
				require.NoError(t, err)
				served, err := Ports(a, port)
				require.NoError(t, err)
				row := &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Port: uint32(port), Ports: served, PortProtocols: pp}
				assert.Emptyf(t, RowProblem(row), "annotations %v give a row that is not typed: port %d ports %v port_protocols %v", a, port, served, pp)
				n++
			}
		}
	}
	require.Equal(t, len(ports)*len(protocols)*len(primaries), n)

	// What the parser refuses, it refuses whole: there is no row to judge.
	for _, a := range []map[string]string{
		{aetherannotations.AnnotationEndpointPorts: "8080=grpc"},
		{aetherannotations.AnnotationEndpointProtocol: "sctp"},
		{aetherannotations.AnnotationEndpointPorts: "http"},
	} {
		pp, err := PortProtocols(a)
		require.Errorf(t, err, "%v", a)
		assert.Nil(t, pp)
	}
}
