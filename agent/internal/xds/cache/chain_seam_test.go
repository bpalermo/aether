package cache

import (
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// TestCaptureVhosts_ServiceChainFilter (M4 seam test): SetServiceChainFilters must
// surface as vhost-level typed_per_filter_config on the service's capture vhost.
func TestCaptureVhosts_ServiceChainFilter(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	// The service must have a capture authority (mesh Service) for a vhost to render.
	// Deliberately NO declared dependency and NO GAMMA routes: the chain filter
	// itself must put the service in scope (2026-07-05 kind repro: a route-less
	// chain-filtered service never got its dedicated vhost, so the filter silently
	// never applied — requests fell to the ODCDS catch-all).
	c.SetCaptureAuthorities(map[string]string{"aether-test/echo": "echo.aether-test.svc.cluster.local"})

	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{
		"aether-test/echo": {Name: "envoy.filters.http.header_mutation", Config: cfg},
	})

	vhosts := c.captureVhosts()
	require.NotEmpty(t, vhosts, "echo authority must render a vhost")
	found := false
	for _, vh := range vhosts {
		if vh.GetName() == "echo.aether-test.aether.internal" || len(vh.GetDomains()) > 0 {
			if _, ok := vh.GetTypedPerFilterConfig()["envoy.filters.http.header_mutation"]; ok {
				found = true
			}
		}
	}
	assert.True(t, found, "the chain filter must be enabled at the service vhost (typed_per_filter_config)")
}
