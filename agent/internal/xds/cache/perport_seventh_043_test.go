package cache

// Seventh adversarial review of the proposal-043 stack (head 7ecbd2a3). Tests
// adopted from the review, where each failed.

import (
	"context"
	"fmt"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// R7-N4. Ruling 8, the filter half, on the tables the sixth review's fix left
// alone. The fix made cap_http_other stop refusing a name it has by its mesh
// authority alone. The table of a port number that HAS a chain (another
// service declares the number as HTTP, or the pods of this one do) still
// refuses it. The same mesh service behind a user's Service ("own", pods
// declare 8080 HTTP), dialled at a user Service's address on
//
//   - 9090, a number another service ("multi") declares as HTTP;
//   - 8080, the number its own pods declare (the commonest Service of all:
//     port 8080, targetPort 8080);
//
// with the mesh name in Host. Without the filter each of these passes through
// to the address that was dialled and is answered (seen live on kind, with
// the filter: 421). The control is the sixth review's case, port 8000.
func TestPerPort_AFilterOnAUserServiceRefusesNoMeshNameOnADialledPortThatHasAChain(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	own := a043Service{name: "own", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1"}
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	mesh := "own.demo." + a043Mesh
	build := func(filter, held bool) *SnapshotCache {
		c := a043Cache(t, []a043Service{multi})
		c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/own": {8000: 8080}})
		if held {
			// "own" is an upstream a pod of the node declares (as the e2e
			// suite's client does): the node holds its rows with no filter.
			c.SetStaticDependencies([]string{"demo/own", "demo/multi"})
		}
		if filter {
			c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/own": r3ChainCfg(t)})
		}
		reg := a043Registry([]a043Service{own, multi})
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.generateSnapshot(ctx))
		return c
	}
	for _, held := range []bool{false, true} {
		before, after := a043Tables(t, build(false, held)), a043Tables(t, build(true, held))
		for _, tc := range []struct {
			dialled uint32
			host    string
			control bool
		}{
			{8000, mesh + ":8000", true},
			{8000, mesh, true},
			{9090, mesh, false},
			{9090, mesh + ":8080", false},
			{9090, mesh + ":9090", false},
			{8080, mesh + ":8080", false},
			{8080, mesh, false},
			{8080, mesh + ":8000", false},
		} {
			name := fmt.Sprintf("cap_http_%d", tc.dialled)
			if before[name] == nil {
				name = proxy.CaptureHTTPOtherRouteName
			}
			require.NotNil(t, after[name])
			b := r5Resolve(t, before[name], "demo", tc.host, "/")
			a := r5Resolve(t, after[name], "demo", tc.host, "/")
			t.Logf("held-before-the-filter=%v dial :%d Host %s on %s: no filter %q; with the filter %q", held, tc.dialled, tc.host, name, b.sig, a.sig)
			worked := b.pass || b.delivered
			if !worked {
				continue
			}
			ok := a.pass || (a.delivered && a.filter)
			if tc.control {
				require.Truef(t, ok, "control (the sixth review's case): %s Host %s: %q", name, tc.host, a.sig)
				continue
			}
			assert.Truef(t, ok, "held-before=%v: a captured dial to port %d with Host %s was answered %q with no filter, and is %q once a service-wide filter is attached (table %s, vhost %q): the filter turned a working call into a refusal",
				held, tc.dialled, tc.host, b.sig, a.sig, name, a.vhost)
			// The rule, both halves. The port the pods themselves serve HTTP
			// on, with a Host that names it or none, is a mesh delivery WITH
			// the filter; every other spelling leaves to the address that was
			// dialled, as before the filter.
			if tc.dialled == 8080 && tc.host != mesh+":8000" {
				assert.Truef(t, a.delivered && a.filter, "dial :%d Host %s: %q, want a delivery with the filter", tc.dialled, tc.host, a.sig)
				assert.Equal(t, []string{mesh, "quic:" + mesh + "@demo/client"}, a.clusters, "to the cluster of the pods' port")
			} else {
				assert.Truef(t, a.pass, "dial :%d Host %s: %q, want it to leave to the address that was dialled", tc.dialled, tc.host, a.sig)
			}
		}
	}
}

// TestPerPort_AMeshOnlyNameTheNodeKnowsNoPortOfIsOfferedWithItsFilter: the
// same service while the node holds no row of it (the registry has none yet,
// or is unreachable). Nothing says which ports it has, so its mesh authority
// on the table of a dialled port is offered on demand, as the catch-all
// offered it before the filter, and from a virtual host of its own that
// carries the filter. It is not passed through (the mesh name with the port of
// the table names a mesh cluster), and never fetched without the filter.
func TestPerPort_AMeshOnlyNameTheNodeKnowsNoPortOfIsOfferedWithItsFilter(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	mesh := "own.demo." + a043Mesh
	for _, mapped := range []bool{false, true} {
		c := a043Cache(t, []a043Service{multi})
		if mapped {
			c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/own": {8000: 8080}})
		}
		c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/own": r3ChainCfg(t)})
		reg := a043Registry([]a043Service{multi}) // no row of "own"
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.generateSnapshot(ctx))
		_, known := c.PodPortClasses(ctx, "demo/own")
		require.False(t, known, "control: the node holds no row of the service")
		a := r5Resolve(t, a043Tables(t, c)["cap_http_9090"], "demo", mesh+":9090", "/")
		assert.Truef(t, a.delivered && a.filter, "mapped=%v: dial :9090 Host %s:9090 is %q, want the port's cluster offered WITH the filter", mapped, mesh, a.sig)
		assert.Equal(t, []string{mesh + ":9090"}, a.clusters)
	}
}
