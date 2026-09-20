package proxy

import (
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	setFilterStatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/common/set_filter_state/v3"
	set_filter_state_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/set_filter_state/v3"
	on_demand_secretv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_selectors/on_demand_secret/v3"
	transport_sockets_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// Issue #815: every listener chain that can originate mesh traffic stamps BOTH
// the netns filter-state key and the source pod's SPIFFE ID. Release one added
// the second key; release two moved the cluster transport-socket matcher onto
// it. The netns key stays on the listeners until release three.

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

// requireBothSourceKeys asserts a chain carries the three source filter-state
// entries in their fixed order: netns, the aether identity key, and the
// certificate-mapper identity key. The last two carry the SAME value — the
// pod's SPIFFE ID as a literal — and differ only in name and object factory
// (issue #842; see SourceIdentityCertMapperFilterStateKey for why both are
// stamped for one release).
func requireBothSourceKeys(t *testing.T, chain *listenerv3.FilterChain, what string) {
	t.Helper()
	got := filterStateValues(t, chain)
	require.Lenf(t, got, 3, "%s must carry exactly the three source filter-state entries", what)
	assert.Equalf(t, networkNamespaceFilterStateKey, got[0][0], "%s: netns key must stay first", what)
	assert.Equalf(t, "%FILTER_STATE(envoy.network.network_namespace:PLAIN)%", got[0][1], "%s: netns value unchanged", what)
	assert.Equalf(t, SourceIdentityFilterStateKey, got[1][0], "%s: identity key must follow the netns key", what)
	assert.Equalf(t, testSourceIdentity, got[1][1], "%s: identity must be the pod's SPIFFE ID, as a literal", what)
	assert.Equalf(t, SourceIdentityCertMapperFilterStateKey, got[2][0], "%s: the certificate-mapper key comes last", what)
	assert.Equalf(t, testSourceIdentity, got[2][1], "%s: the mapper key carries the SAME SPIFFE ID — it IS the SDS secret name", what)
}

// TestSourceIdentityStampedOnEveryMeshOriginatingChain enumerates the listener
// kinds that can open an upstream mesh connection. Each one must stamp the
// identity key — the cluster matcher reads it since release two, and a chain
// that sets only the netns key falls to OnNoMatch (the node identity presented
// instead of the pod's, #686 territory). The netns key must stay too until
// release three, so both are asserted.
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
	mapperKey := decode(buildCertMapperIdentityFilterState(testSourceIdentity))

	assert.Equal(t, setFilterStatev3.FilterStateValue_ONCE, identity.GetSharedWithUpstream())
	assert.Equal(t, netns.GetSharedWithUpstream(), identity.GetSharedWithUpstream(),
		"the identity key must be shared with upstream exactly like the netns key")
	assert.Equal(t, "envoy.string", netns.GetFactoryKey())
	assert.Equal(t, "envoy.string", identity.GetFactoryKey())
	assert.Equal(t, netns.GetReadOnly(), identity.GetReadOnly(),
		"read-only semantics must match: nothing downstream writes either key")
	assert.Equal(t, netns.GetSkipIfEmpty(), identity.GetSkipIfEmpty())

	// The certificate-mapper key. SharedWithUpstream is not merely "like the
	// others" here, it is REQUIRED: both of its readers —
	// CommonUpstreamTransportSocketFactory::hashKey and the filter_state_override
	// mapper — look only at
	// TransportSocketOptions::downstreamSharedFilterStateObjects(), which is
	// populated exclusively from shared entries. An unshared object is invisible
	// to both, and the failure is silent (default_value, and no identity in the
	// pool key).
	assert.Equal(t, setFilterStatev3.FilterStateValue_ONCE, mapperKey.GetSharedWithUpstream(),
		"NOT optional: an unshared object never reaches downstreamSharedFilterStateObjects()")
	assert.Equal(t, hashableStringFactory, mapperKey.GetFactoryKey(),
		"the whole point of #842: envoy.string builds a non-Hashable StringAccessorImpl, "+
			"which hashKey's dynamic_cast<const Hashable*> rejects, so the identity would "+
			"contribute zero bytes to the upstream pool key")
	assert.NotEqual(t, identity.GetFactoryKey(), mapperKey.GetFactoryKey())
	assert.Equal(t, identity.GetFormatString().GetTextFormatSource().GetInlineString(),
		mapperKey.GetFormatString().GetTextFormatSource().GetInlineString(),
		"both identity keys carry the same value; only the name and factory differ")
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
		require.Len(t, got, 3)
		require.Equal(t, networkNamespaceFilterStateKey, got[0][0])
		require.Equal(t, SourceIdentityFilterStateKey, got[1][0])
		require.Equal(t, SourceIdentityCertMapperFilterStateKey, got[2][0])
	}
}

// TestClusterSelectsCertificateFromTheStampedKey is the #842 boundary. It
// replaces release two's TestClusterMatcherKeyedOnSourceIdentity, whose subject
// — an exact_match_map keyed on the source identity — no longer exists.
//
// The binding it checks is the same one, one link shorter: the listener stamps
// a key, and the cluster's certificate MAPPER reads that exact key. Before #842
// the intermediary was a matcher input naming the key; now it is the mapper's
// hardcoded lookup name, which the control plane must spell identically at the
// stamping end. Both ends are asserted against each other here, because a
// mismatch is silent — the mapper returns default_value and every workload's
// egress presents the node identity (#686 territory, reached by a new route).
func TestClusterSelectsCertificateFromTheStampedKey(t *testing.T) {
	const node = "spiffe://" + testTrustDomain + "/ns/aether-system/sa/aether-agent"

	c := NewServiceCluster("svc-a."+testTrustDomain, "svc-a", "svc-a", nil)
	InjectUpstreamMTLS(c, node, ValidationContextName(testTrustDomain), nil, "8080", "")

	// No per-identity structure survives on the cluster at all.
	assert.Nil(t, c.GetTransportSocketMatcher(), "#842 removed the transport-socket matcher")
	assert.Empty(t, c.GetTransportSocketMatches(), "#842 removed the per-identity socket list")

	var ctx transport_sockets_v3.UpstreamTlsContext
	require.NoError(t, c.GetTransportSocket().GetTypedConfig().UnmarshalTo(&ctx))
	sel := ctx.GetCommonTlsContext().GetCustomTlsCertificateSelector()
	require.NotNil(t, sel, "the cluster must carry the per-connection certificate selector")

	var onDemand on_demand_secretv3.Config
	require.NoError(t, sel.GetTypedConfig().UnmarshalTo(&onDemand))
	require.Equal(t, filterStateCertMapperName, onDemand.GetCertificateMapper().GetName(),
		"the mapper that reads filter state, not the sni/static_name mappers")

	// THE JOIN: what the listener stamps is what the mapper looks up. The
	// mapper's key is not configurable, so this is a constant-to-constant
	// comparison — which is the point: it is the one place the two ends meet,
	// and a divergence here is otherwise invisible until traffic arrives with
	// the wrong identity.
	stamped := filterStateValues(t, buildDefaultOutboundHTTPFilterChain(sourceTestPod(), testSourceIdentity, testTrustDomain, false, nil))
	keys := make([]string, 0, len(stamped))
	for _, kv := range stamped {
		keys = append(keys, kv[0])
	}
	assert.Contains(t, keys, SourceIdentityCertMapperFilterStateKey,
		"the originating chain must stamp the key the cluster's mapper reads")

	// And the value stamped under it is the SDS secret name the mapper will
	// return verbatim, so it has to be a real SPIFFE ID and not a netns path.
	assert.Equal(t, testSourceIdentity, stamped[len(stamped)-1][1])
	assert.NotContains(t, stamped[len(stamped)-1][1], "/var/run/netns/")

	// Nothing in the serialized cluster may mention a netns path or the netns
	// key: per-pod state in a cluster is what made every pod event re-hash
	// every cluster on the node (#815).
	b, err := proto.Marshal(c)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "/var/run/netns/")
	assert.NotContains(t, string(b), networkNamespaceFilterStateKey)
	// Nor any workload SPIFFE ID: the ONLY identity a mesh cluster names is the
	// node's (the mapper's default_value). That is what makes its bytes
	// independent of which workloads run here.
	assert.NotContains(t, string(b), testSourceIdentity)
}

// TestClusterIsIndependentOfLocalIdentities replaces
// TestClusterMatcherIsOrderIndependent. That test existed because the matcher's
// entries were built by ranging a Go map, so a reshuffled ORDER would re-hash
// the cluster and trigger a full CDS replace (#135) — it asserted the matcher
// was a function of the identity SET.
//
// Since #842 the cluster is not a function of the identity set either. There is
// no per-node input left to order, so the property is checked at its new and
// stronger boundary: the same service always renders the same bytes.
func TestClusterIsIndependentOfLocalIdentities(t *testing.T) {
	const node = "spiffe://" + testTrustDomain + "/ns/aether-system/sa/aether-agent"

	marshal := func() []byte {
		c := NewServiceCluster("svc-a."+testTrustDomain, "svc-a", "svc-a", nil)
		InjectUpstreamMTLS(c, node, ValidationContextName(testTrustDomain), nil, "8080", "")
		out, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
		require.NoError(t, err)
		return out
	}

	want := marshal()
	for range 8 {
		assert.Equal(t, want, marshal(),
			"a mesh cluster's bytes must be a pure function of the service and the node SVID")
	}
}
