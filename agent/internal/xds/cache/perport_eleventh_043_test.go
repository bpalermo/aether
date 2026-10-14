package cache

// Review of the follow-up to the tenth review: what a refusal's body says on
// the capture tables, and how long it may get.

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRefusalBody_NoHTTPPortOnACaptureTableSaysAnUnresolvedName: the same
// refusal as on the loopback table (TestRefusalBody_NoHTTPPortSaysAnUnresolvedName),
// where a capture table gives it: HTTP dialled to a port number of another
// service, for a name whose pods serve raw TCP only. With an unresolved
// Service port beside it the body says that too.
func TestRefusalBody_NoHTTPPortOnACaptureTableSaysAnUnresolvedName(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	db := a043Service{name: "db", ns: "demo", legacy: 9000, ports: map[uint32]registryv1.PortProtocol{9000: tcp}, ip: "10.0.9.3"}
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	for _, unresolved := range []bool{false, true} {
		c := a043Cache(t, []a043Service{multi})
		portMap := map[uint32]uint32{5432: 9000}
		if unresolved {
			portMap[8000] = 0
		}
		c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/db": portMap})
		c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/db": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}}}})
		c.SetStaticDependencies([]string{"demo/db", "demo/multi"})
		reg := a043Registry([]a043Service{db, multi})
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.generateSnapshot(ctx))
		tables := a043Tables(t, c)
		require.NotNil(t, tables["cap_http_9090"], "control: the table of another service's HTTP port number")
		got := a043Resolve(t, tables["cap_http_9090"], "db.demo."+a043Mesh+":9090")
		require.Equal(t, "no-http-port", got.refusal, got.body)
		assert.Contains(t, got.body, "serves no HTTP port. Reach it over TCP instead")
		if unresolved {
			assert.Contains(t, got.body, "Its Service's port 8000 has a targetPort that is a name")
		} else {
			assert.NotContains(t, got.body, "targetPort")
		}
	}
}

// TestRefusalBody_AServiceOfManyUnresolvedPortsDoesNotBreakItsTables: every
// refusal of a name whose Service has unresolved ports lists them, and a
// Service may have thousands. Envoy rejects a route table in which a direct
// response's body is longer than 4096 bytes, and with the table every route
// of every other service in it: one Service created before its pods took the
// node's HTTP routing with it. Every body is cut to what Envoy accepts, and
// says that it was.
func TestRefusalBody_AServiceOfManyUnresolvedPortsDoesNotBreakItsTables(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	build := func() map[string]map[string]string {
		c := a043Cache(t, []a043Service{multi})
		portMap := map[uint32]uint32{}
		for p := uint32(10000); p < 12000; p++ {
			portMap[p] = 0
		}
		c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/web": portMap})
		c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/web": r3ChainCfg(t)})
		c.SetStaticDependencies([]string{"demo/multi"})
		reg := a043Registry([]a043Service{multi})
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.generateSnapshot(ctx))
		bodies := map[string]map[string]string{}
		for name, rc := range a043Tables(t, c) {
			bodies[name] = map[string]string{}
			for _, vh := range rc.GetVirtualHosts() {
				for i, r := range vh.GetRoutes() {
					if body := r.GetDirectResponse().GetBody().GetInlineString(); body != "" {
						bodies[name][vh.GetName()+"#"+string(rune('0'+i))] = body
					}
				}
			}
		}
		return bodies
	}
	bodies := build()
	long := 0
	for table, of := range bodies {
		for vh, body := range of {
			require.LessOrEqualf(t, len(body), proxy.MaxRefusalBody, "table %s, virtual host %s: Envoy rejects the whole table for this body", table, vh)
			require.Truef(t, utf8.ValidString(body), "table %s, virtual host %s", table, vh)
			if strings.Contains(body, "do not resolve yet") || strings.HasSuffix(body, "(truncated)\n") {
				long++
				assert.Lenf(t, body, proxy.MaxRefusalBody, "table %s, virtual host %s", table, vh)
				assert.Truef(t, strings.HasSuffix(body, " ... (truncated)\n"), "table %s, virtual host %s: a body that was cut does not say so: %q", table, vh, body[len(body)-40:])
				assert.Containsf(t, body, "Its Service's port 10000, 10001, 10002,", "table %s, virtual host %s: the ports are listed in their order", table, vh)
			}
		}
	}
	require.NotZero(t, long, "control: no refusal of the Service with 2000 unresolved ports was found")
	// The same on every node and at every build: a body that differed would
	// be a new version of the table for nothing.
	assert.Equal(t, bodies, build())
}
