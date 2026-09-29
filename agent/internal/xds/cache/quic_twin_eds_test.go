package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestQUICTwinsSubscribeToTheirOwnEDSResource is the unit gate for aether#1008.
//
// A `quic:` twin that shares its base's EDS resource name is deduplicated by
// Envoy's delta-ADS WatchMap when it is added AFTER the base is already
// subscribed (a new ServiceAccount's first pod on the node): no subscribe goes
// out, nothing is answered, and the twin warms for the full 15 s
// initial_fetch_timeout while every request from that source is 503/NC. So:
//
//	(a) every twin's eds_cluster_config.service_name is its own cluster name,
//	    never the base's;
//	(b) the snapshot carries a ClusterLoadAssignment under every twin's name
//	    whose endpoints are the base's;
//	(c) a twin introduced by a LATER snapshot (a new local identity) gets its
//	    load assignment in that same snapshot, so its subscribe is answered.
func TestQUICTwinsSubscribeToTheirOwnEDSResource(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()
	const td = "aether.internal"

	for _, p := range []struct{ name, sa string }{{"a-0", "source-a"}, {"b-0", "source-b"}} {
		require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
			Name: p.name, Namespace: "demo", ServiceAccount: p.sa,
			NetworkNamespace: "/var/run/netns/cni-" + p.name,
		}, td))
	}
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	c.SetCaptureAuthorities(map[string]string{"demo/echo": "echo.demo.svc.cluster.local"})
	declareDeps(c, "demo/echo")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"demo/echo": {
					makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080),
					makeEndpoint("10.0.3.2", "cluster-1", "node-3", 8080),
				},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	// Twins are demand-scoped (aether#1020): both sources have dialled echo.
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b")

	echo := proxy.ServiceClusterName("demo/echo", c.meshDomain)
	twin := func(sa string) string { return proxy.QUICClusterName("demo/echo", c.meshDomain, "demo/"+sa) }

	check := func(t *testing.T, sas ...string) {
		t.Helper()
		snap, err := c.GetSnapshot("node-1")
		require.NoError(t, err)
		clusters := snap.GetResources(resourcev3.ClusterType)
		clas := snap.GetResources(resourcev3.EndpointType)

		base, ok := clusters[echo].(*clusterv3.Cluster)
		require.True(t, ok, "h2 base %s missing: %v", echo, keysOf(clusters))
		baseEDS := base.GetEdsClusterConfig().GetServiceName()
		require.Equal(t, "demo/echo", baseEDS)
		baseCLA, ok := clas[baseEDS].(*endpointv3.ClusterLoadAssignment)
		require.True(t, ok, "base load assignment %s missing: %v", baseEDS, keysOf(clas))
		require.Len(t, baseCLA.GetEndpoints(), 2, "fixture must carry endpoints for the comparison to mean anything")

		for _, sa := range sas {
			name := twin(sa)
			cl, ok := clusters[name].(*clusterv3.Cluster)
			require.True(t, ok, "twin %s missing: %v", name, keysOf(clusters))
			eds := cl.GetEdsClusterConfig().GetServiceName()
			// (a)
			assert.Equal(t, name, eds, "twin %s must subscribe to its own EDS name", name)
			assert.NotEqual(t, baseEDS, eds, "twin %s shares the base's EDS name: a late twin is deduplicated away (aether#1008)", name)
			// (b)
			cla, ok := clas[eds].(*endpointv3.ClusterLoadAssignment)
			if !assert.True(t, ok, "no ClusterLoadAssignment published under twin %s's EDS name %q: %v", name, eds, keysOf(clas)) {
				continue
			}
			assert.Equal(t, eds, cla.GetClusterName())
			renamed, _ := proto.Clone(cla).(*endpointv3.ClusterLoadAssignment)
			renamed.ClusterName = baseCLA.GetClusterName()
			assert.True(t, proto.Equal(baseCLA, renamed), "twin %s's endpoints must be the base's", name)
		}
	}

	check(t, "source-a", "source-b")

	// (c) A new ServiceAccount's first pod on the node, and then its first
	// request to echo (the on-demand observation, aether#1020): the twin and
	// its load assignment must arrive in the SAME snapshot.
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "c-0", Namespace: "demo", ServiceAccount: "source-c",
		NetworkNamespace: "/var/run/netns/cni-c-0",
	}, td))
	require.NoError(t, c.generateSnapshot(ctx))
	before, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	_, earlyTwin := before.GetResources(resourcev3.ClusterType)[twin("source-c")]
	require.False(t, earlyTwin, "a pod alone must not build a twin: source-c has not dialled echo yet (aether#1020)")
	edsVersionBefore := before.GetVersion(resourcev3.EndpointType)
	observeQUIC(t, c, "demo/echo", "demo/source-c")
	after, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	_, hasTwin := after.GetResources(resourcev3.ClusterType)[twin("source-c")]
	_, hasCLA := after.GetResources(resourcev3.EndpointType)[twin("source-c")]
	require.True(t, hasTwin, "the new identity's twin must be published")
	assert.True(t, hasCLA, "the new identity's twin load assignment must ride the SAME snapshot as the twin")
	assert.NotEqual(t, edsVersionBefore, after.GetVersion(resourcev3.EndpointType), "the snapshot introducing the twin's CLA must carry a new version")
	check(t, "source-a", "source-b", "source-c")

	// Determinism: regenerating with no input change must not move any CLA's bytes.
	require.NoError(t, c.generateSnapshot(ctx))
	again, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	for name, res := range after.GetResources(resourcev3.EndpointType) {
		other, ok := again.GetResources(resourcev3.EndpointType)[name]
		require.True(t, ok, "%s vanished on regeneration", name)
		a, err := proto.MarshalOptions{Deterministic: true}.Marshal(res)
		require.NoError(t, err)
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(other)
		require.NoError(t, err)
		assert.Equal(t, a, b, "%s: identical inputs must produce identical bytes", name)
	}
}
