package cache

// Sixth adversarial pass on the proposal-043 stack (head 270dd5b8). Tests
// adopted from the review.

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ruling 8, the filter half: "attaching a filter must not turn a working call
// into a refusal". A mesh service behind a user's Service (port 8000 -> the
// pods' 8080), no route. A captured dial to the Service's port with the mesh
// name in Host passes through and is answered. A service-wide filter is then
// attached: the same request was 421 port-not-declared, although the node held
// the Service's port map and the loopback table of the same snapshot routed
// "<mesh name>:8000" with the filter. (Leg U1 of e2e/multiprotocol.sh asserted
// that 421 as its proof that the filter had arrived; it asserts the dial is
// still answered now.)
func TestPerPort_AFilterOnAUserServiceDoesNotRefuseItsMeshNameOnACapturedDial(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	own := a043Service{name: "own", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1"}
	host := "own.demo." + a043Mesh + ":8000"
	dialled := func(c *SnapshotCache) (string, r5Answer) {
		require.NoError(t, c.generateSnapshot(ctx))
		tables := a043Tables(t, c)
		name := "cap_http_8000"
		if tables[name] == nil {
			name = proxy.CaptureHTTPOtherRouteName
		}
		return name, r5Resolve(t, tables[name], "demo", host, "/")
	}
	build := func(filter bool) *SnapshotCache {
		c := a043Cache(t, nil)
		c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/own": {8000: 8080}})
		if filter {
			c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/own": r3ChainCfg(t)})
		}
		// The service is another pod's upstream, or was called before: the
		// node holds its rows either way once the filter names it.
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{own})))
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{own})))
		return c
	}

	table, before := dialled(build(false))
	require.Truef(t, before.pass, "control, no filter: table %s passes the dial through to the user's Service: %s", table, before.sig)

	c := build(true)
	loop := r5Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], "demo", host, "/")
	require.Truef(t, loop.delivered && loop.filter, "control: the loopback table routes %s with the filter: %s", host, loop.sig)

	table, after := dialled(c)
	assert.Truef(t, after.pass || (after.delivered && after.filter),
		"with the filter, a captured dial to port 8000 with Host %s is answered %q by table %s (vhost %q): the call that worked is refused",
		host, after.sig, table, after.vhost)
}
