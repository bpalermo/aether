package cache

import (
	"context"
	"strings"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
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

// TestOutboundVhost_ServiceChainFilter (M4 outbound parity): the chain filter must be
// enabled at the service's OUTBOUND vhost too — the outbound route table serves the
// same GAMMA/chain config as cap_http (2026-07-05 talos finding: requests riding the
// outbound listener missed the filter; only capture vhosts were decorated).
func TestOutboundVhost_ServiceChainFilter(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()
	pod := &cniv1.CNIPod{
		Name: "client-1", Namespace: "aether-test", ServiceAccount: "client",
		NetworkNamespace: "/var/run/netns/cni-x",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{
		"aether-test/echo": {Name: "envoy.filters.http.header_mutation", Config: cfg},
	})

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{"aether-test/echo": {makeEndpoint("10.0.0.9", "cluster-1", "node-2", 8080)}}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	// The outbound route table's echo vhost must carry the vhost-level TPFC.
	found := false
	for _, res := range snap.GetResources(resourcev3.RouteType) {
		rc, ok := res.(*routev3.RouteConfiguration)
		if !ok {
			continue
		}
		for _, vh := range rc.GetVirtualHosts() {
			if _, ok := vh.GetTypedPerFilterConfig()["envoy.filters.http.header_mutation"]; ok {
				found = true
			}
		}
	}
	assert.True(t, found, "outbound vhost must carry the service chain filter TPFC")
	// And the OUTBOUND listener HCM must carry the default-disabled chain entry
	// (TPFC can only override a filter present in the chain).
	foundChainEntry := false
	for name, res := range snap.GetResources(resourcev3.ListenerType) {
		if !strings.HasPrefix(name, "outbound_http_") {
			continue
		}
		l := res.(*listenerv3.Listener)
		b, _ := protojson.Marshal(l)
		if strings.Contains(string(b), "envoy.filters.http.header_mutation") {
			foundChainEntry = true
		}
	}
	assert.True(t, foundChainEntry, "outbound HCM must carry the default-disabled extension entry")
}

// TestExtensionHTTPFilters_AuthzSidecar (027 M1): when the sidecar is enabled the
// union carries the disabled ext_authz entry on top of any rule/chain filters.
func TestExtensionHTTPFilters_AuthzSidecar(t *testing.T) {
	c := newTestCache("node-1")
	assert.Empty(t, c.extensionHTTPFilters(), "no entries by default")
	c.SetAuthzSidecar(200*1000*1000, false) // 200ms
	fs := c.extensionHTTPFilters()
	require.Len(t, fs, 1)
	assert.Equal(t, "envoy.filters.http.ext_authz", fs[0].GetName())
	assert.True(t, fs[0].GetDisabled())
}

// TestInboundFilter_Seam (027 M3): an INBOUND-scope filter for the pod's own service
// lands on the pod's inbound listener — chain entry (via the authz-sidecar union) +
// route-vhost TPFC — through the full snapshot path; with the sidecar disabled it is
// dropped entirely (no TPFC referencing an absent chain filter = no NACK).
func TestInboundFilter_Seam(t *testing.T) {
	mk := func(sidecar bool) (*SnapshotCache, error) {
		c := newTestCache("node-1")
		if sidecar {
			c.SetAuthzSidecar(200_000_000, false)
		}
		ctx := context.Background()
		pod := &cniv1.CNIPod{
			Name: "echo-1", Namespace: "aether-test", ServiceAccount: "echo",
			NetworkNamespace: "/var/run/netns/cni-in",
		}
		if err := c.AddPod(ctx, pod, "aether.internal"); err != nil {
			return nil, err
		}
		cfg, err := anypb.New(&ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{
			CheckSettings: &ext_authzv3.CheckSettings{ContextExtensions: map[string]string{"policy": "x"}},
		}})
		if err != nil {
			return nil, err
		}
		c.SetServiceInboundFilters(map[string]proxy.ExtensionFilter{
			"aether-test/echo": {Name: "envoy.filters.http.ext_authz", Config: cfg},
		})
		return c, nil
	}

	// Sidecar ON: inbound listener carries the ext_authz chain entry + the TPFC.
	c, err := mk(true)
	require.NoError(t, err)
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	entryCount, tpfcCount := 0, 0
	for name, res := range snap.GetResources(resourcev3.ListenerType) {
		if !strings.HasPrefix(name, "inbound_") {
			continue
		}
		b, _ := protojson.Marshal(res.(*listenerv3.Listener))
		js := string(b)
		entryCount += strings.Count(js, `"envoy.filters.http.ext_authz"`)
		tpfcCount += strings.Count(js, "typedPerFilterConfig")
	}
	assert.Greater(t, entryCount, 0, "inbound HCM must carry the ext_authz chain entry")
	assert.Greater(t, tpfcCount, 0, "inbound route vhost must carry the TPFC")

	// Sidecar OFF: the filter is dropped — no ext_authz anywhere on the inbound.
	c2, err := mk(false)
	require.NoError(t, err)
	snap2, err := c2.GetSnapshot("node-1")
	require.NoError(t, err)
	for name, res := range snap2.GetResources(resourcev3.ListenerType) {
		if !strings.HasPrefix(name, "inbound_") {
			continue
		}
		b, _ := protojson.Marshal(res.(*listenerv3.Listener))
		assert.NotContains(t, string(b), "ext_authz", "sidecar off: filter must be dropped, not emitted")
	}
}
