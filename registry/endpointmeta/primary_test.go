package endpointmeta

import (
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
)

const (
	svcHTTP = registryv1.Service_PROTOCOL_HTTP
	svcTCP  = registryv1.Service_PROTOCOL_TCP
	svcUDP  = registryv1.Service_PROTOCOL_UDP
)

// ep builds an endpoint whose primary port is port, with the given per-port
// classes (nil = an endpoint written before proposal 037, which carries none).
func ep(ip string, port uint32, pp map[uint32]registryv1.PortProtocol) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{Ip: ip, ClusterName: "c1", Port: port, PortProtocols: pp}
}

func TestPrimaryProtocol(t *testing.T) {
	tcpFirst := ep("10.0.0.1", 9000, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: http})
	// The endpoint is read from the HTTP listing because it serves an HTTP
	// port. Its PRIMARY is still raw TCP: the listing says what the endpoint
	// serves, not what its primary speaks (#1672).
	assert.Equal(t, tcp, PrimaryProtocol(tcpFirst, svcHTTP))
	assert.Equal(t, tcp, PrimaryProtocol(tcpFirst, svcTCP))

	httpFirst := ep("10.0.0.2", 8080, map[uint32]registryv1.PortProtocol{8080: http, 9000: tcp})
	assert.Equal(t, http, PrimaryProtocol(httpFirst, svcTCP))

	// No per-port map: the whole-pod classification, i.e. the listing's.
	assert.Equal(t, tcp, PrimaryProtocol(ep("10.0.0.3", 9000, nil), svcTCP))
	assert.Equal(t, udp, PrimaryProtocol(ep("10.0.0.3", 9001, nil), svcUDP))
	assert.Equal(t, http, PrimaryProtocol(ep("10.0.0.3", 8080, nil), svcHTTP))
	// An unspecified listing is the HTTP default, as everywhere else.
	assert.Equal(t, http, PrimaryProtocol(ep("10.0.0.3", 8080, nil), registryv1.Service_PROTOCOL_UNSPECIFIED))
	// A map that does not name the primary says nothing about it.
	assert.Equal(t, tcp, PrimaryProtocol(ep("10.0.0.4", 9000, map[uint32]registryv1.PortProtocol{8080: http}), svcTCP))
}

func TestServicePrimaryOf(t *testing.T) {
	tcpFirst := func(ip string) *registryv1.ServiceEndpoint {
		return ep(ip, 9000, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: http})
	}
	httpFirst := func(ip string) *registryv1.ServiceEndpoint {
		return ep(ip, 8080, map[uint32]registryv1.PortProtocol{8080: http, 9000: tcp})
	}

	t.Run("no endpoints: unknown", func(t *testing.T) {
		got := ServicePrimaryOf()
		assert.False(t, got.Known())
		assert.False(t, got.Disagree())
		assert.Equal(t, registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED, got.Protocol)
		assert.Zero(t, got.Port)
		assert.False(t, ServicePrimaryOf(Listing{Protocol: svcHTTP}).Known())
	})

	t.Run("a TCP primary with an HTTP secondary is TCP, whichever listing is read first (#1672)", func(t *testing.T) {
		a := tcpFirst("10.0.0.1")
		for _, listings := range [][]Listing{
			{{Protocol: svcHTTP, Endpoints: []*registryv1.ServiceEndpoint{a}}, {Protocol: svcTCP, Endpoints: []*registryv1.ServiceEndpoint{a}}},
			{{Protocol: svcTCP, Endpoints: []*registryv1.ServiceEndpoint{a}}, {Protocol: svcHTTP, Endpoints: []*registryv1.ServiceEndpoint{a}}},
			{{Protocol: svcHTTP, Endpoints: []*registryv1.ServiceEndpoint{a}}},
		} {
			got := ServicePrimaryOf(listings...)
			assert.True(t, got.Known())
			assert.Equal(t, uint32(9000), got.Port)
			assert.Equal(t, tcp, got.Protocol)
			assert.False(t, got.Disagree(), "one pod listed under two protocols is one vote, not a disagreement")
			assert.Equal(t, []PrimaryCandidate{{Port: 9000, Protocol: tcp, Endpoints: 1}}, got.Candidates)
		}
	})

	t.Run("an HTTP primary with a TCP secondary is HTTP", func(t *testing.T) {
		a := httpFirst("10.0.0.1")
		got := ServicePrimaryOf(
			Listing{Protocol: svcHTTP, Endpoints: []*registryv1.ServiceEndpoint{a}},
			Listing{Protocol: svcTCP, Endpoints: []*registryv1.ServiceEndpoint{a}},
		)
		assert.Equal(t, uint32(8080), got.Port)
		assert.Equal(t, http, got.Protocol)
		assert.False(t, got.Disagree())
	})

	t.Run("endpoints written before proposal 037 take the listing's protocol", func(t *testing.T) {
		got := ServicePrimaryOf(Listing{Protocol: svcUDP, Endpoints: []*registryv1.ServiceEndpoint{ep("10.0.0.1", 9001, nil)}})
		assert.Equal(t, udp, got.Protocol)
		assert.Equal(t, uint32(9001), got.Port)
	})

	t.Run("disagreement: the primary of the most endpoints wins, in any order", func(t *testing.T) {
		a, b, c := tcpFirst("10.0.0.1"), tcpFirst("10.0.0.2"), httpFirst("10.0.0.3")
		want := []PrimaryCandidate{
			{Port: 9000, Protocol: tcp, Endpoints: 2},
			{Port: 8080, Protocol: http, Endpoints: 1},
		}
		for _, order := range [][]*registryv1.ServiceEndpoint{{a, b, c}, {c, a, b}, {b, c, a}} {
			got := ServicePrimaryOf(
				Listing{Protocol: svcHTTP, Endpoints: order},
				Listing{Protocol: svcTCP, Endpoints: order},
			)
			assert.True(t, got.Disagree())
			assert.Equal(t, uint32(9000), got.Port)
			assert.Equal(t, tcp, got.Protocol)
			assert.Equal(t, want, got.Candidates)
		}
	})

	t.Run("disagreement, tied: HTTP before TCP before UDP, then the lower port", func(t *testing.T) {
		a, b := tcpFirst("10.0.0.1"), httpFirst("10.0.0.2")
		for _, order := range [][]*registryv1.ServiceEndpoint{{a, b}, {b, a}} {
			got := ServicePrimaryOf(Listing{Protocol: svcHTTP, Endpoints: order}, Listing{Protocol: svcTCP, Endpoints: order})
			assert.True(t, got.Disagree())
			assert.Equal(t, http, got.Protocol)
			assert.Equal(t, uint32(8080), got.Port)
		}

		lo, hi := ep("10.0.0.1", 8080, nil), ep("10.0.0.2", 9090, nil)
		for _, order := range [][]*registryv1.ServiceEndpoint{{lo, hi}, {hi, lo}} {
			got := ServicePrimaryOf(Listing{Protocol: svcHTTP, Endpoints: order})
			assert.True(t, got.Disagree())
			assert.Equal(t, uint32(8080), got.Port)
		}
	})

	t.Run("the same address in two clusters is two endpoints", func(t *testing.T) {
		a := tcpFirst("10.0.0.1")
		b := tcpFirst("10.0.0.1")
		b.ClusterName = "c2"
		c := httpFirst("10.0.0.9")
		got := ServicePrimaryOf(Listing{Protocol: svcHTTP, Endpoints: []*registryv1.ServiceEndpoint{c, a, b}})
		assert.Equal(t, tcp, got.Protocol)
		assert.Equal(t, 2, got.Candidates[0].Endpoints)
	})

	t.Run("String names every candidate, for the log line that reports a disagreement", func(t *testing.T) {
		got := ServicePrimaryOf(Listing{Protocol: svcHTTP, Endpoints: []*registryv1.ServiceEndpoint{tcpFirst("10.0.0.1"), tcpFirst("10.0.0.2"), httpFirst("10.0.0.3")}})
		assert.Equal(t, "9000/tcp (2 endpoints), 8080/http (1 endpoint)", got.String())
		assert.Equal(t, "unknown", ServicePrimaryOf().String())
	})
}
