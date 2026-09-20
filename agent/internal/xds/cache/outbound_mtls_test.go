package cache

import (
	"context"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	filter_state_overridev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_mappers/filter_state_override/v3"
	on_demand_secretv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/cert_selectors/on_demand_secret/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const nodeIdentity = "spiffe://aether.internal/ns/aether-system/sa/aether-agent"

// TestServiceClusterMTLSInjected verifies that once a local pod and the node
// identity are known, each service cluster carries the per-source upstream mTLS
// the mesh actually uses: since #842 that is ONE transport socket whose
// custom_tls_certificate_selector resolves the client certificate per connection
// from the source identity in filter state, with the node SVID as the default.
func TestServiceClusterMTLSInjected(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	// A local pod establishes the netns -> SPIFFE ID mapping and trust domain.
	pod := &cniv1.CNIPod{
		Name:             "echo-1",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-a",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	// The node SVID being served gates the upstream mTLS injection.
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"aether-test/echo": {makeEndpoint("10.0.0.9", "cluster-1", "node-1", 18080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)

	echo, ok := clusters["echo.aether-test.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok, "echo cluster must be present")
	// Since #842 per-source mTLS is ONE socket with a per-connection certificate
	// selector, not a matcher over a per-identity socket list.
	assert.Nil(t, echo.GetTransportSocketMatcher(), "the per-identity matcher is gone (#842)")
	assert.Empty(t, echo.GetTransportSocketMatches())
	require.NotNil(t, echo.GetTransportSocket(), "service cluster must carry the mesh upstream socket")

	utc := &tlsv3.UpstreamTlsContext{}
	require.NoError(t, echo.GetTransportSocket().GetTypedConfig().UnmarshalTo(utc))

	// The selector/mapper NAMES are informational — Envoy resolves these
	// extensions by the typed_config type URL, and `envoy --mode validate`
	// accepts a bogus name (measured on the pinned proxy, 2026-09-20). So
	// nothing downstream catches a rename: pinning them here is the only guard,
	// and it is a cheap one.
	sel := utc.GetCommonTlsContext().GetCustomTlsCertificateSelector()
	require.NotNil(t, sel, "service cluster must carry the per-connection certificate selector")
	assert.Equal(t, "envoy.tls.certificate_selectors.on_demand_secret", sel.GetName())

	onDemand := &on_demand_secretv3.Config{}
	require.NoError(t, sel.GetTypedConfig().UnmarshalTo(onDemand))
	assert.Equal(t, "envoy.tls.upstream_certificate_mappers.filter_state_override",
		onDemand.GetCertificateMapper().GetName(),
		"the mapper that reads the source identity off the downstream-shared filter state")

	mapper := &filter_state_overridev3.Config{}
	require.NoError(t, onDemand.GetCertificateMapper().GetTypedConfig().UnmarshalTo(mapper))
	assert.Equal(t, nodeIdentity, mapper.GetDefaultValue(),
		"a connection with no source identity presents the node SVID (the old on_no_match)")

	// No workload identity is named anywhere on the cluster: the local pod's
	// certificate is selected at handshake time from filter state.
	b, err := proto.Marshal(echo)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "spiffe://aether.internal/ns/aether-test/sa/echo")
}

// TestServiceClusterNoMTLSWithoutNodeIdentity verifies upstream mTLS is not
// injected until the node SVID is served: the certificate selector's mapper
// requires a non-empty default_value, and the node SVID is it.
func TestServiceClusterNoMTLSWithoutNodeIdentity(t *testing.T) {
	c := newTestCache("node-1")
	declareDeps(c, "aether-test/echo")
	ctx := context.Background()

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"aether-test/echo": {makeEndpoint("10.0.0.9", "cluster-1", "node-2", 18080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	echo, ok := snap.GetResources(resourcev3.ClusterType)["echo.aether-test.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok, "echo cluster must be present")
	// Nothing mTLS at all before the node SVID: the selector's default_value is
	// the node identity and the proto requires it to be non-empty, so the
	// cluster is emitted bare rather than with an unservable empty default.
	assert.Nil(t, echo.GetTransportSocketMatcher(), "no mTLS matcher before the node SVID is served")
	assert.Nil(t, echo.GetTransportSocket(), "no upstream mTLS socket before the node SVID is served")
}

// TestServiceClusterSANPinning verifies the snapshot-time injection pins the
// upstream peer identity to the service's expected SPIFFE IDs, derived from
// its endpoints' namespaces. This is the anti-registry-poisoning control: a
// valid-but-wrong SVID must fail the handshake.
func TestServiceClusterSANPinning(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name:             "echo-1",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-a",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	ep1 := makeEndpoint("10.0.0.9", "cluster-1", "node-1", 18080) // ns "default" (makeEndpoint)
	ep2 := makeEndpoint("10.0.0.10", "cluster-1", "node-2", 18080)
	ep2.KubernetesMetadata.Namespace = "aether-test"
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{"aether-test/echo": {ep1, ep2}}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	echo, ok := snap.GetResources(resourcev3.ClusterType)["echo.aether-test.aether.internal"].(*clusterv3.Cluster)
	require.True(t, ok)

	wantSANs := []string{
		"spiffe://aether.internal/ns/aether-test/sa/echo",
		"spiffe://aether.internal/ns/default/sa/echo",
	}
	// The SERVER-identity pin is unchanged by #842 — it was never per-source, so
	// collapsing the per-source socket list did not touch it. There is now one
	// socket to check instead of one per local ServiceAccount.
	require.NotNil(t, echo.GetTransportSocket())
	utc := &tlsv3.UpstreamTlsContext{}
	require.NoError(t, echo.GetTransportSocket().GetTypedConfig().UnmarshalTo(utc))
	combined := utc.GetCommonTlsContext().GetCombinedValidationContext()
	require.NotNil(t, combined, "the mesh upstream socket pins the server identity")
	var got []string
	for _, sm := range combined.GetDefaultValidationContext().GetMatchTypedSubjectAltNames() {
		got = append(got, sm.GetMatcher().GetExact())
	}
	assert.Equal(t, wantSANs, got, "sorted namespace union renders the expected identities")
}
