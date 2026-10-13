package cache

// Third adversarial pass on the proposal-043 stack: tests adopted from the review.

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1694's fix for "tables after a rule event": the loopback virtual hosts are
// stored on the cluster entries, and a snapshot build rebuilds them when the
// rule generation moved (refreshEntryVhosts, vhostRuleGen).
//
// A registry refresh builds those same virtual hosts from the rule inputs it
// read BEFORE its registry listings (LoadClustersFromRegistry reads the
// routes and the filters, then makes the TCP and UDP listing RPCs, then takes
// the cluster lock), and it does not record which generation it built them
// at. The interleaving:
//
//  1. a refresh reads the rule inputs: no filter yet;
//  2. a service-wide filter arrives (ruleGen g -> g+1);
//  3. a snapshot is built (any pod event does it): refreshEntryVhosts brings
//     the entries to g+1, with the filter, and records vhostRuleGen = g+1;
//  4. the refresh takes the lock and REPLACES the entries' virtual hosts with
//     the ones built from what it read in step 1. vhostRuleGen is still g+1.
//
// From then on every snapshot build finds vhostRuleGen == ruleGen and leaves
// the loopback table alone: it delivers to the service WITHOUT its filter,
// while the capture tables of the same snapshot carry it, until the next rule
// event or the next registry refresh.
func TestPerPort_RegistryRefreshDoesNotHideNewerRules(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	svc := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1", vip: "10.96.9.1"}
	c := a043Cache(t, []a043Service{svc})

	base := a043Registry([]a043Service{svc})
	fired := false
	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			// The TCP listing is made after the refresh has read the rule
			// inputs and before it takes the cluster lock.
			if protocol == registryv1.Service_PROTOCOL_TCP && !fired {
				fired = true
				c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": r3ChainCfg(t)}) // step 2
				require.NoError(t, c.generateSnapshot(ctx))                                            // step 3
			}
			return base.listAllEndpointsFunc(ctx, protocol)
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg)) // steps 1 and 4
	require.True(t, fired)
	// Any number of later snapshot builds.
	require.NoError(t, c.generateSnapshot(ctx))
	require.NoError(t, c.generateSnapshot(ctx))

	filtered := func(table *routev3.RouteConfiguration, host string) (bool, string) {
		vh := r3Pick(t, table, host)
		_, ok := vh.GetTypedPerFilterConfig()[r3Filter]
		return ok, vh.GetName()
	}
	tables := a043Tables(t, c)
	host := "echo.demo." + a043Mesh + ":8080"
	capOK, capVh := filtered(tables["cap_http_8080"], host)
	loopOK, loopVh := filtered(tables[proxy.OutboundHTTPRouteName], host)
	proxyOK, _ := filtered(tables["cap_http_18081"], host)
	t.Logf("captured dial to :8080 -> virtual host %q, service-wide filter enabled=%v", capVh, capOK)
	t.Logf("loopback (out_http)    -> virtual host %q, service-wide filter enabled=%v", loopVh, loopOK)
	t.Logf("proxy port (captured)  -> service-wide filter enabled=%v", proxyOK)
	require.True(t, capOK, "the capture table carries the filter")
	assert.True(t, loopOK, "Host %s on the loopback listener is delivered WITHOUT demo/echo's service-wide filter, and no later snapshot build repairs it", host)
	assert.True(t, proxyOK, "Host %s on the proxy port (a captured dial to :18081) is delivered WITHOUT demo/echo's service-wide filter", host)
}
