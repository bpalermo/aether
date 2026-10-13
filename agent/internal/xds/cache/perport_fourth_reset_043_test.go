package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// N1. A service with a service-wide filter loses EVERY virtual host of the
// loopback table (and of the proxy port's capture table) while the node still
// holds a retained raw-TCP floor entry of it, and its mesh authority falls to
// the catch-all's on-demand route, which carries neither the filter nor the
// service's HTTPRoute rules.
//
//	t0  echo serves 8080 (HTTP) and 9000 (raw TCP)
//	t1  echo serves 8080 only: the floor entries of 9000 are kept for the grace
//	t2  no row is listed (the pod restarts) and the generated Service declares
//	    9090: the HTTP entry of 8080 is REPLACED on the Service's evidence
//	    (retentionEvidence), forgetReplacedHTTPPortsLocked deletes
//	    servicePorts["demo/echo"], and the floor entries are still there.
//
// Then all three builders of the loopback table skip the service:
// outboundRefusalVhostsLocked walks servicePorts (gone);
// outboundDeclaredVhostsLocked and outboundColdVhostsLocked skip a service
// that has ANY cluster entry ("routed"), and a retained floor is one.
func TestPerPort_ReplacedServiceWithARetainedFloorKeepsItsFilterAndRules(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	t0 := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}, ip: "10.0.9.1", vip: "10.96.9.1"}
	t1 := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.2", vip: "10.96.9.1"}
	t2 := a043Service{name: "echo", ns: "demo", legacy: 9090, ports: map[uint32]registryv1.PortProtocol{9090: h}, ip: "10.0.9.3", vip: "10.96.9.1"}

	c := a043Cache(t, []a043Service{t0})
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": r3ChainCfg(t)})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("rule")}}})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{t0})))

	a043Project(c, []a043Service{t1})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{t1})))

	a043Project(c, []a043Service{t2})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	require.NoError(t, c.generateSnapshot(ctx))

	// What a cache that starts in the state of t2 answers: the oracle.
	fresh := a043Cache(t, nil)
	fresh.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": r3ChainCfg(t)})
	fresh.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("rule")}}})
	a043Project(fresh, []a043Service{t2})
	require.NoError(t, fresh.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	require.NoError(t, fresh.generateSnapshot(ctx))

	_, has := c.servicePorts["demo/echo"]
	t.Logf("servicePorts holds demo/echo: %v", has)
	for name, e := range c.clusters {
		t.Logf("entry %s absent=%v l4Floor=%v", name, !e.absentSince.IsZero(), e.l4Floor)
	}
	for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
		for _, host := range []string{"echo.demo." + a043Mesh + ":9090", "echo:9090"} {
			got := r3Resolve(t, a043Tables(t, c)[tname], "demo", host, nil)
			want := r3Resolve(t, a043Tables(t, fresh)[tname], "demo", host, nil)
			vh := r3Pick(t, a043Tables(t, c)[tname], host)
			t.Logf("%s Host %s\n   after the transitions: vhost %q: %s\n   started in this state: %s", tname, host, vh.GetName(), got.first, want.first)
			assert.Equalf(t, want.first, got.first, "%s, Host %s: the service-wide filter and the HTTPRoute rule are not in force", tname, host)
		}
	}
}
