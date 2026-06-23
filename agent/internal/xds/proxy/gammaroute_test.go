package proxy

import (
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildOutboundServiceVirtualHost_NoRules: with no GAMMA rules it is the plain
// passthrough vhost (one route to the service cluster).
func TestBuildOutboundServiceVirtualHost_NoRules(t *testing.T) {
	vh := BuildOutboundServiceVirtualHost("svc-1.mesh", []string{"svc-1.mesh"}, nil)
	require.Len(t, vh.Routes, 1)
	assert.Equal(t, "svc-1.mesh", vh.Routes[0].GetRoute().GetCluster())
}

// TestBuildOutboundServiceVirtualHost_Rules: matches → routes, weighted backends →
// WeightedClusters, plus the additive trailing default to the service cluster.
func TestBuildOutboundServiceVirtualHost_Rules(t *testing.T) {
	rules := []GammaRoute{
		{
			Matches:  []GammaMatch{{Exact: "/admin", Headers: []GammaHeaderMatch{{Name: "x-env", Value: "beta"}}}},
			Backends: []GammaBackend{{Cluster: "svc-admin.mesh", Weight: 1}},
		},
		{
			Backends: []GammaBackend{{Cluster: "svc-1-v1.mesh", Weight: 90}, {Cluster: "svc-1-v2.mesh", Weight: 10}},
		},
	}
	vh := BuildOutboundServiceVirtualHost("svc-1.mesh", []string{"svc-1.mesh"}, rules)

	// rule0 (1 match) + rule1 (default "/" match) + trailing catch-all = 3 routes.
	require.Len(t, vh.Routes, 3)

	// rule0: exact path + header match → single backend.
	r0 := vh.Routes[0]
	assert.Equal(t, "/admin", r0.GetMatch().GetPath())
	require.Len(t, r0.GetMatch().GetHeaders(), 1)
	assert.Equal(t, "x-env", r0.GetMatch().GetHeaders()[0].GetName())
	assert.Equal(t, "svc-admin.mesh", r0.GetRoute().GetCluster())

	// rule1: no match → default "/", weighted split.
	r1 := vh.Routes[1]
	assert.Equal(t, "/", r1.GetMatch().GetPrefix())
	wc := r1.GetRoute().GetWeightedClusters().GetClusters()
	require.Len(t, wc, 2)
	assert.Equal(t, uint32(90), wc[0].GetWeight().GetValue())
	assert.Equal(t, "svc-1-v2.mesh", wc[1].GetName())

	// trailing additive default → the service's own cluster.
	assert.Equal(t, "svc-1.mesh", vh.Routes[2].GetRoute().GetCluster())
	assert.Equal(t, "/", vh.Routes[2].GetMatch().GetPrefix())

	_ = routev3.RouteMatch{} // keep import
}
