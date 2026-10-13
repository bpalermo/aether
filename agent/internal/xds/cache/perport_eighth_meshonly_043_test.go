package cache

// Eighth adversarial review of the proposal-043 stack (head f5ed0ee5). Tests
// adopted from the review.

import (
	"context"
	"fmt"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ruling 8, the filter half, as the owner gave it: for a service held
// by its mesh authority alone, a captured request is a mesh delivery WITH the
// filter where "the dialled port number is one the service's own pods declare
// as HTTP and the Host is port-less or names that port"; EVERY other case
// "does what it did before the filter existed (goes to the dialled address)".
//
// The code had a third arm: a port of the user's Service that owns the name
// (by its port map) was also a mesh delivery. That arm only existed on the
// table of a port NUMBER that has a chain, and a number has a chain when some
// OTHER mesh service declares it as HTTP. So the same request, "dial the
// owning Service's port with the mesh name in Host":
//
//   - on port 8000 (no service declares the number): leaves to the address
//     that was dialled, before and after the filter (the suite's leg U1);
//   - on port 9090 (another service, "multi", declares the number): passed
//     through before the filter, and was taken into the mesh and run through
//     the filter after it. With a filter that denies the caller the call that
//     answered 200 answered 403 (seen live on kind).
//
// The arm is gone: what such a dial does is decided by the service's own
// pods' ports and by nothing another service declares.
func TestPerPort_ADialToTheOwningServicesPortDoesNotDependOnOtherServicesPorts(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	own := a043Service{name: "own", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1"}
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	mesh := "own.demo." + a043Mesh
	build := func(filter bool) map[string]*routev3.RouteConfiguration {
		c := a043Cache(t, []a043Service{multi})
		// The user's Service that owns the name: two ports, both to the pods' 8080.
		c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/own": {8000: 8080, 9090: 8080}})
		c.SetStaticDependencies([]string{"demo/own", "demo/multi"})
		if filter {
			c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/own": r3ChainCfg(t)})
		}
		reg := a043Registry([]a043Service{own, multi})
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.generateSnapshot(ctx))
		return a043Tables(t, c)
	}
	before, after := build(false), build(true)
	for _, tc := range []struct {
		dialled uint32
		host    string
	}{
		{8000, mesh},
		{8000, mesh + ":8000"},
		{9090, mesh},
		{9090, mesh + ":9090"},
	} {
		name := fmt.Sprintf("cap_http_%d", tc.dialled)
		if before[name] == nil {
			name = proxy.CaptureHTTPOtherRouteName
		}
		b := r5Resolve(t, before[name], "demo", tc.host, "/")
		a := r5Resolve(t, after[name], "demo", tc.host, "/")
		t.Logf("dial own's Service port :%d Host %s on %s: no filter %q; with the filter %q", tc.dialled, tc.host, name, b.sig, a.sig)
		// Not a port number the pods declare: the ruling's "every other
		// case", which goes to the address that was dialled. With the port
		// of the table in Host it was fetched on demand before the filter
		// (for a port the pods do not have); it is not, with it.
		assert.Truef(t, a.pass, "dial :%d Host %s was %q before the filter and is %q with it, want it to leave to the address that was dialled",
			tc.dialled, tc.host, b.sig, a.sig)
	}
}

// TestPerPort_AMeshOnlyDialIsADeliveryOnlyOnAPortThePodsServeAsHTTP: the one
// arm that delivers reads the class of the dialled port number among the
// service's own pods, and nothing else: a number the pods serve as raw TCP, a
// number another service serves and they do not, and a number that is only a
// port of the owning Service all leave to the address that was dialled.
func TestPerPort_AMeshOnlyDialIsADeliveryOnlyOnAPortThePodsServeAsHTTP(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	// The pods serve 8080 as HTTP and 9000 as raw TCP. Another service
	// serves 8080, 9000 and 9090 as HTTP, so each number has a table.
	own := a043Service{name: "own", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}, ip: "10.0.9.1"}
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	mesh := "own.demo." + a043Mesh
	c := a043Cache(t, []a043Service{multi})
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/own": {9090: 8080, 9000: 9000}})
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/own": r3ChainCfg(t)})
	reg := a043Registry([]a043Service{own, multi})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.NoError(t, c.generateSnapshot(ctx))
	tables := a043Tables(t, c)
	for _, tc := range []struct {
		dialled   uint32
		delivered bool
		why       string
	}{
		{8080, true, "the pods serve it as HTTP"},
		{9000, false, "the pods serve it as raw TCP (and it is a port of the owning Service)"},
		{9090, false, "a port of the owning Service whose target the pods serve as HTTP: not the pods' number"},
	} {
		table := tables[fmt.Sprintf("cap_http_%d", tc.dialled)]
		require.NotNilf(t, table, "control: port %d has a table", tc.dialled)
		for _, host := range []string{mesh, fmt.Sprintf("%s:%d", mesh, tc.dialled)} {
			a := r5Resolve(t, table, "demo", host, "/")
			if tc.delivered {
				assert.Truef(t, a.delivered && a.filter, "dial :%d Host %s (%s): %q, want a delivery with the filter", tc.dialled, host, tc.why, a.sig)
				continue
			}
			assert.Truef(t, a.pass, "dial :%d Host %s (%s): %q, want it to leave to the address that was dialled", tc.dialled, host, tc.why, a.sig)
		}
	}
}

// TestPerPort_AMeshOnlyDialUnderScopedCaptureIsNotPassedThrough: where the
// node captures nothing but mesh addresses there is no passthrough cluster,
// and "what the catch-all does for a name that is not the mesh's" is a 404.
// The virtual host that stands in for it must say the same, not route to a
// cluster the snapshot does not carry.
func TestPerPort_AMeshOnlyDialUnderScopedCaptureIsNotPassedThrough(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	own := a043Service{name: "own", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1"}
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	mesh := "own.demo." + a043Mesh
	for _, redirectAll := range []bool{true, false} {
		c := a043Cache(t, []a043Service{multi})
		c.SetCaptureRedirectAll(redirectAll)
		c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/own": r3ChainCfg(t)})
		reg := a043Registry([]a043Service{own, multi})
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.generateSnapshot(ctx))
		table := a043Tables(t, c)["cap_http_9090"]
		require.NotNil(t, table)
		a := r5Resolve(t, table, "demo", mesh+":9090", "/")
		other := r5Resolve(t, table, "demo", "not-a-mesh-name.example:9090", "/")
		assert.Equalf(t, other.sig, a.sig, "redirect-all=%v: the mesh-only name on a port it has not must answer as a name that is not the mesh's does", redirectAll)
		if redirectAll {
			assert.True(t, a.pass, a.sig)
		} else {
			assert.Equal(t, "direct 404 ", a.sig)
		}
	}
}

// A stated behaviour, held so that a change of it is seen. A mesh-only name
// the node knows no port of (no row registered under the filter's target
// name: a user Service whose pods run under another ServiceAccount, a service
// scaled to zero) cannot be told a mesh delivery from any other dial. Its
// mesh authority with the port of the table was offered on demand before the
// filter and still is, with the filter. The PORT-LESS mesh authority, which
// left to the address that was dialled before the filter, is parked on demand
// too, for "<fqdn>:<dialled port>", with the filter: on every table that has
// a chain, until the node holds rows of the service. A Host that names
// another port than the one dialled leaves as before.
func TestPerPort_AColdMeshOnlyNameIsParkedWithItsFilter(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	mesh := "own.demo." + a043Mesh
	build := func(filter bool) map[string]*routev3.RouteConfiguration {
		c := a043Cache(t, []a043Service{multi})
		if filter {
			c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/own": r3ChainCfg(t)})
		}
		reg := a043Registry([]a043Service{multi})
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.generateSnapshot(ctx))
		return a043Tables(t, c)
	}
	before, after := build(false), build(true)
	for _, host := range []string{mesh, mesh + ":9090", mesh + ":8080"} {
		b := r5Resolve(t, before["cap_http_9090"], "demo", host, "/")
		a := r5Resolve(t, after["cap_http_9090"], "demo", host, "/")
		switch host {
		case mesh:
			require.True(t, b.pass, "control: the port-less mesh name left to the dialled address before the filter")
			assert.Truef(t, a.delivered && a.filter, "port-less: %q, want it parked on demand WITH the filter", a.sig)
			assert.Equal(t, []string{mesh + ":9090"}, a.clusters, "for the cluster of the port that was dialled")
		case mesh + ":9090":
			require.Truef(t, b.onDemand && !b.filter, "control: offered on demand, with no filter to run, before the filter: %q", b.sig)
			assert.Truef(t, a.delivered && a.filter, "the port of the table: %q, want it offered WITH the filter", a.sig)
		default:
			require.True(t, b.pass)
			assert.Truef(t, a.pass, "another port than the one dialled: %q, want it to leave as before", a.sig)
		}
	}
}
