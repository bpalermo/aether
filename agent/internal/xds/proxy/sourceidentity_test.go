package proxy

import (
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	setFilterStatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/common/set_filter_state/v3"
	set_filter_state_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Release-one tests for issue #815: every listener chain that can originate
// mesh traffic stamps BOTH the netns filter-state key (which the cluster
// transport-socket matcher reads today) and the source pod's SPIFFE ID (which
// it will read in release two). Nothing about the clusters changes yet.

const (
	testSourcePod      = "echo-1"
	testSourceNS       = "aether-test"
	testSourceSA       = "echo"
	testTrustDomain    = "aether.internal"
	testSourceIdentity = "spiffe://" + testTrustDomain + "/ns/" + testSourceNS + "/sa/" + testSourceSA
)

func sourceTestPod() *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             testSourcePod,
		Namespace:        testSourceNS,
		ServiceAccount:   testSourceSA,
		NetworkNamespace: "/var/run/netns/cni-a",
	}
}

// filterStateValues decodes every set_filter_state network filter on a chain
// into an ordered list of (object key, inline value) pairs.
func filterStateValues(t *testing.T, chain *listenerv3.FilterChain) [][2]string {
	t.Helper()
	var out [][2]string
	for _, f := range chain.GetFilters() {
		if f.GetName() != "envoy.filters.network.set_filter_state" {
			continue
		}
		var cfg set_filter_state_v3.Config
		require.NoError(t, f.GetTypedConfig().UnmarshalTo(&cfg))
		for _, v := range cfg.GetOnNewConnection() {
			out = append(out, [2]string{
				v.GetObjectKey(),
				v.GetFormatString().GetTextFormatSource().GetInlineString(),
			})
		}
	}
	return out
}

// requireBothSourceKeys asserts a chain carries the netns key first and the
// source-identity key second, with the pod's SPIFFE ID as a literal.
func requireBothSourceKeys(t *testing.T, chain *listenerv3.FilterChain, what string) {
	t.Helper()
	got := filterStateValues(t, chain)
	require.Lenf(t, got, 2, "%s must carry exactly the two source filter-state entries", what)
	assert.Equalf(t, networkNamespaceFilterStateKey, got[0][0], "%s: netns key must stay first", what)
	assert.Equalf(t, "%FILTER_STATE(envoy.network.network_namespace:PLAIN)%", got[0][1], "%s: netns value unchanged", what)
	assert.Equalf(t, sourceIdentityFilterStateKey, got[1][0], "%s: identity key must follow the netns key", what)
	assert.Equalf(t, testSourceIdentity, got[1][1], "%s: identity must be the pod's SPIFFE ID, as a literal", what)
}

// TestSourceIdentityStampedOnEveryMeshOriginatingChain enumerates the listener
// kinds that can open an upstream mesh connection. Each one must stamp both
// keys — release two switches the cluster matcher to the identity key in one
// step, and any chain that sets only the old key would then fall to OnNoMatch
// (the node identity = the wrong client certificate, #686 territory).
func TestSourceIdentityStampedOnEveryMeshOriginatingChain(t *testing.T) {
	pod := sourceTestPod()
	id := SourceIdentityForPod(pod, testTrustDomain)
	require.Equal(t, testSourceIdentity, id)

	svc := CaptureTCPService{ClusterName: "tcp:svc-a." + testTrustDomain, ClusterIP: "10.96.1.10"}
	rules := []L4ServiceRoute{{Backends: []L4Backend{{Service: "svc-a", Cluster: "tcp:svc-a." + testTrustDomain, Weight: 1}}}}
	tlsRules := []L4ServiceRoute{{SNIHostnames: []string{"a.example.com"}, Backends: rules[0].Backends}}

	t.Run("per-pod outbound HTTP chain", func(t *testing.T) {
		requireBothSourceKeys(t, buildDefaultOutboundHTTPFilterChain(pod, id, testTrustDomain, false, nil), "outbound chain")
	})

	t.Run("capture HCM chain", func(t *testing.T) {
		requireBothSourceKeys(t, buildCaptureHTTPFilterChain(pod, id, testTrustDomain, false, false, nil), "capture HCM chain")
	})

	t.Run("capture TCP floor chain", func(t *testing.T) {
		requireBothSourceKeys(t, buildCaptureTCPFloorFilterChain(svc, id), "capture TCP floor chain")
	})

	t.Run("capture TCPRoute weighted floor chain", func(t *testing.T) {
		requireBothSourceKeys(t, BuildCaptureTCPRouteFilterChain(svc, rules, id), "TCPRoute floor chain")
	})

	t.Run("capture TLSRoute SNI chain", func(t *testing.T) {
		chains := BuildCaptureTLSRouteFilterChains(svc, tlsRules, id)
		require.Len(t, chains, 1)
		requireBothSourceKeys(t, chains[0], "TLSRoute SNI chain")
	})

	t.Run("whole capture listener", func(t *testing.T) {
		svcWithRoutes := svc
		svcWithRoutes.TCPRouteRules = rules
		svcWithRoutes.TLSRouteRules = tlsRules
		l, err := GenerateCaptureListener(pod, id, 18081, testTrustDomain, false, []CaptureTCPService{svcWithRoutes}, true, nil)
		require.NoError(t, err)
		named := 0
		for _, fc := range l.GetFilterChains() {
			named++
			requireBothSourceKeys(t, fc, "capture chain "+fc.GetName())
		}
		require.GreaterOrEqual(t, named, 3, "TLS SNI + TCP floor + HCM catch-all")

		// The ORIGINAL_DST passthrough default chain carries NEITHER key: it
		// forwards non-mesh egress in plaintext and selects no certificate.
		require.NotNil(t, l.GetDefaultFilterChain())
		assert.Empty(t, filterStateValues(t, l.GetDefaultFilterChain()),
			"the plaintext passthrough chain must not stamp a mesh source identity")
	})
}

// TestSourceIdentityFilterStateIsSharedLikeNetns: the matcher reads both keys
// out of TransportSocketOptions::downstreamSharedFilterStateObjects(), which is
// populated only from entries marked shared. The new key must therefore carry
// exactly the netns key's sharing semantics, or release two selects nothing.
func TestSourceIdentityFilterStateIsSharedLikeNetns(t *testing.T) {
	decode := func(f *listenerv3.Filter) *setFilterStatev3.FilterStateValue {
		var cfg set_filter_state_v3.Config
		require.NoError(t, f.GetTypedConfig().UnmarshalTo(&cfg))
		require.Len(t, cfg.GetOnNewConnection(), 1)
		return cfg.GetOnNewConnection()[0]
	}

	netns := decode(buildNetworkNamespaceFilterState())
	identity := decode(buildSourceIdentityFilterState(testSourceIdentity))

	assert.Equal(t, setFilterStatev3.FilterStateValue_ONCE, identity.GetSharedWithUpstream())
	assert.Equal(t, netns.GetSharedWithUpstream(), identity.GetSharedWithUpstream(),
		"the identity key must be shared with upstream exactly like the netns key")
	assert.Equal(t, netns.GetFactoryKey(), identity.GetFactoryKey(),
		"both keys are plain strings (envoy.string)")
	assert.Equal(t, netns.GetReadOnly(), identity.GetReadOnly(),
		"read-only semantics must match: the matcher never writes either key")
	assert.Equal(t, netns.GetSkipIfEmpty(), identity.GetSkipIfEmpty())
}

// TestSourceIdentityAbsentWithoutTrustDomain: before the trust domain is known
// there is no identity to stamp, and formatting one anyway would emit a
// malformed "spiffe:///ns/…". The chain then carries only the netns key —
// byte-for-byte the pre-#815 shape.
func TestSourceIdentityAbsentWithoutTrustDomain(t *testing.T) {
	assert.Empty(t, SourceIdentityForPod(sourceTestPod(), ""))
	assert.Empty(t, SourceIdentityForPod(nil, testTrustDomain))

	got := filterStateValues(t, buildDefaultOutboundHTTPFilterChain(sourceTestPod(), "", testTrustDomain, false, nil))
	require.Len(t, got, 1, "no trust domain: only the netns key")
	assert.Equal(t, networkNamespaceFilterStateKey, got[0][0])
}

// TestSourceFilterStatesOrderIsFixed: the chain's `filters` list is a repeated
// field, so its order is part of the listener's delta-xDS hash. Building the
// same chain twice must produce the same order, always netns then identity.
func TestSourceFilterStatesOrderIsFixed(t *testing.T) {
	for range 8 {
		got := filterStateValues(t, buildDefaultOutboundHTTPFilterChain(sourceTestPod(), testSourceIdentity, testTrustDomain, false, nil))
		require.Len(t, got, 2)
		require.Equal(t, networkNamespaceFilterStateKey, got[0][0])
		require.Equal(t, sourceIdentityFilterStateKey, got[1][0])
	}
}

// TestClusterMatcherStillKeyedOnNetns is the release-one boundary: the cluster
// transport_socket_matcher must NOT have moved to the new key yet. Flipping it
// is release two, and only after every proxy in the fleet is stamping both.
func TestClusterMatcherStillKeyedOnNetns(t *testing.T) {
	m := UpstreamTransportSocketMatcher(map[string]string{"/var/run/netns/cni-a": testSourceIdentity})
	require.NotNil(t, m)

	input := m.GetMatcherTree().GetInput()
	require.NotNil(t, input)
	assert.Equal(t, filterStateInputName, input.GetName())

	// The exact_match_map is still keyed by netns path, not identity.
	entries := m.GetMatcherTree().GetExactMatchMap().GetMap()
	require.Contains(t, entries, "/var/run/netns/cni-a",
		"release one must not change the cluster matcher; see sourceIdentityFilterStateKey")
	require.NotContains(t, entries, testSourceIdentity)
}
