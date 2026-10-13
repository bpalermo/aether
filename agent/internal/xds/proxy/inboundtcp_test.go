package proxy

import (
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func podWithPorts(ports string) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:      "app-0",
		Namespace: "aether-test",
		Annotations: map[string]string{
			aetherannotations.AnnotationEndpointPort:  "8080",
			aetherannotations.AnnotationEndpointPorts: ports,
		},
	}
}

func chainByName(chains []*listenerv3.FilterChain, name string) *listenerv3.FilterChain {
	for _, c := range chains {
		if c.GetName() == name {
			return c
		}
	}
	return nil
}

// TestInboundChains_TCPPortReplacesHCM is the destination half of proposal 037.
//
// A raw-TCP port gets a tcp_proxy chain matched on the port as SNI, INSTEAD of
// the HCM chain that every served port gets today. Exactly one of the two: two
// chains matching the same server_names is a listener Envoy cannot
// disambiguate, and it rejects the whole LDS update rather than the offending
// chain — leaving the pod on its previous listener, or unreachable if new.
func TestInboundChains_TCPPortReplacesHCM(t *testing.T) {
	pod := podWithPorts("8080,9000=tcp,9090=h2")
	chains := buildInboundFilterChains(pod, "spiffe-cert", "spiffe-validation", "example.org", false, nil, nil)

	tcpChain := chainByName(chains, "in_tcp_aether-test_app-0_9000")
	require.NotNil(t, tcpChain, "a =tcp port must get a tcp_proxy chain")
	require.NotNil(t, tcpChain.GetFilterChainMatch())
	assert.Equal(t, []string{"9000"}, tcpChain.GetFilterChainMatch().GetServerNames(),
		"matched on the port as SNI — which is why the source sets SNI for a non-primary TCP port")
	require.Len(t, tcpChain.GetFilters(), 1)
	assert.Equal(t, "envoy.filters.network.tcp_proxy", tcpChain.GetFilters()[0].GetName())

	// The h2 port keeps its HCM chain.
	h2Chain := chainByName(chains, "in_aether-test_app-0_9090")
	if h2Chain == nil {
		// Naming differs between builders; assert by SNI instead.
		var found bool
		for _, c := range chains {
			sn := c.GetFilterChainMatch().GetServerNames()
			if len(sn) == 1 && sn[0] == "9090" {
				found = true
				assert.Equal(t, "envoy.filters.network.http_connection_manager", c.GetFilters()[len(c.GetFilters())-1].GetName(),
					"an HTTP port must keep its HCM chain")
			}
		}
		require.True(t, found, "the h2 port must still have a chain")
	}

	// No two chains may share a server_names value — the ambiguity Envoy rejects.
	seen := map[string]string{}
	for _, c := range chains {
		for _, sn := range c.GetFilterChainMatch().GetServerNames() {
			prev, dup := seen[sn]
			assert.False(t, dup, "server_names %q claimed by both %q and %q; Envoy rejects the whole listener", sn, prev, c.GetName())
			seen[sn] = c.GetName()
		}
	}
}

// TestInboundChains_PrimaryTCPPortKeepsTheFloor: a raw-TCP declared port keeps
// the default floor chain, which is what a no-SNI mesh connection reaches
// (#306) and what every source still sends.
//
// Until proposal 043 it was asserted here that the declared port gets NO SNI
// chain of its own, "dead config". It was not dead, it was wrong: the port's
// SNI chain existed anyway and was an HCM in front of a port that does not
// speak HTTP. Phase 1 gives every port an explicit chain of its own class, the
// declared one included, so that sources can name it by SNI in a later chart
// without the destination changing again.
func TestInboundChains_PrimaryTCPPortKeepsTheFloor(t *testing.T) {
	pod := podWithPorts("8080")
	pod.Annotations[aetherannotations.AnnotationEndpointProtocol] = "tcp"

	chains := buildInboundFilterChains(pod, "c", "v", "example.org", false, nil, nil)

	require.NotNil(t, chainByName(chains, "in_tcp_aether-test_app-0"), "the default floor chain must still be there")
	own := chainByName(chains, "in_tcp_aether-test_app-0_8080")
	require.NotNil(t, own, "the declared raw-TCP port has an SNI chain of its own (proposal 043, Phase 1)")
	assert.Equal(t, []string{"8080"}, own.GetFilterChainMatch().GetServerNames())
	for _, c := range chains {
		if sn := c.GetFilterChainMatch().GetServerNames(); len(sn) == 1 && sn[0] == "8080" {
			assert.Equal(t, "envoy.filters.network.tcp_proxy", c.GetFilters()[len(c.GetFilters())-1].GetName(),
				"chain %s: a raw-TCP port must not sit behind an HCM", c.GetName())
		}
	}
}

// TestInboundChains_MalformedAnnotationKeepsHTTP: a bad suffix must not strand
// the pod. It keeps its pre-037 HCM chains — what it served yesterday — rather
// than the builder guessing a protocol.
func TestInboundChains_MalformedAnnotationKeepsHTTP(t *testing.T) {
	pod := podWithPorts("8080,9000=quic")
	chains := buildInboundFilterChains(pod, "c", "v", "example.org", false, nil, nil)

	assert.Nil(t, chainByName(chains, "in_tcp_aether-test_app-0_9000"),
		"an unparseable annotation must not produce a TCP chain")
	assert.NotEmpty(t, chains, "and must not strand the pod with no chains at all")
}
