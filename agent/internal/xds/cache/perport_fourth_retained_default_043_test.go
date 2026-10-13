package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/require"
)

// N3. Ruling 7: "never an HTTP request to an endpoint whose row does not
// declare that port as HTTP", grace or not.
//
// A service serves 80 (HTTP) and 9000 (raw TCP). The port its pods declare in
// endpoint.aether.io/port moves from 80 to 9000 (the rows are otherwise the
// same). The default HTTP entry (the bare cluster, which subscribes to the
// bare load assignment) leaves the listing-derived set and is RETAINED for the
// grace: the evidence check asks "does the service still declare 80 as HTTP",
// and it does. The bare load assignment is now the raw-TCP floor's, and a
// request the bare cluster sends lands on the destination's default inbound
// chain: port 9000, raw TCP.
//
// The refresh takes the retained entry's authorities away (yieldClaimedDomains:
// the live per-port entry claims "<fqdn>:80"). A rule event ANYWHERE then
// rebuilds every entry's virtual host from scratch (refreshEntryVhosts), the
// retained default entry's included, with all its authorities again.
func TestPerPort_RetainedDefaultEntryNeverSendsHTTPToTheRawTCPPort(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	ports := map[uint32]registryv1.PortProtocol{80: h, 9000: tcp}
	before := a043Service{name: "echo", ns: "demo", legacy: 80, ports: ports, ip: "10.0.9.1", vip: "10.96.9.1"}
	after := a043Service{name: "echo", ns: "demo", legacy: 9000, ports: ports, ip: "10.0.9.1", vip: "10.96.9.1"}
	st := r4State{svcs: []r4Svc{{a043Service: after, held: true}}}

	c := a043Cache(t, []a043Service{before})
	check := func(when string) {
		t.Helper()
		require.NoError(t, c.generateSnapshot(ctx))
		for name, e := range c.clusters {
			t.Logf("%s: entry %s absent=%v vhost=%v domains=%v", when, name, !e.absentSince.IsZero(), e.vhost != nil, e.vhost.GetDomains())
		}
		for _, tname := range []string{proxy.OutboundHTTPRouteName, "cap_http_80"} {
			for _, host := range []string{"echo.demo." + a043Mesh + ":80", "echo.demo." + a043Mesh, "echo:80"} {
				if table := a043Tables(t, c)[tname]; table != nil {
					t.Logf("%s: %s Host %s -> vhost %q: %s", when, tname, host, r3Pick(t, table, host).GetName(), r3Resolve(t, table, "demo", host, nil).first)
				}
			}
		}
		found := map[string]int{}
		r4NoHTTPToANonHTTPPort(t, c, st, found, func(s string) { t.Log(when + ": " + s) })
		for k, n := range found {
			t.Errorf("%s: %s (%d routes)", when, k, n)
		}
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{after})))
	check("1. after the refresh that reads the new rows")

	// A rule event that has nothing to do with the service.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/unrelated": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}}}})
	check("2. after an unrelated rule event, before the next refresh")

	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{after})))
	check("3. after the next refresh")
}
