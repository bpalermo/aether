package cache

import (
	"context"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// N5. Ruling 8: "a mesh service fronted by a user-owned Service is not a
// captured mesh name: left as is". And ruling 5: a short name is passed
// through like any destination that is not the mesh's when it is not a mesh
// name.
//
// A user's Service "echo" (port 8000 -> targetPort 8080) fronts mesh pods
// "echo"; the registrar generates no Service. A CHAIN-scope HTTPFilter targets
// it; no HTTPRoute does, so the agent has no port map of it and claims only
// its MESH authority in the capture tables (tableNameKeys / serviceNames).
// The short names are derived from every mesh-authority claim
// (proxy.ShortNameVirtualHosts), so the short name IS claimed, for a caller in
// the service's own namespace, while "echo.demo" and the cluster.local name
// are not.
func TestShortName_AUserServiceWithAFilterPassesThroughLikeItsOtherNames(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	echo := a043Service{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1"}
	// Another, generated, service that makes 8000 an HTTP port number of the
	// mesh (so that cap_http_8000 exists; without it the dial lands on
	// cap_http_other, with the same answer).
	other := a043Service{name: "other", ns: "x", legacy: 8000, ports: map[uint32]registryv1.PortProtocol{8000: h}, ip: "10.0.9.2", vip: "10.96.9.2"}

	for _, withFilter := range []bool{false, true} {
		c := a043Cache(t, nil)
		a043Project(c, []a043Service{other})
		if withFilter {
			c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": r3ChainCfg(t)})
		}
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{echo, other})))
		require.NoError(t, c.generateSnapshot(ctx))
		tables := a043Tables(t, c)
		answers := map[string]string{}
		for _, q := range []struct{ table, host string }{
			{"cap_http_8000", "echo:8000"},
			{"cap_http_8000", "echo"},
			{"cap_http_8000", "echo.demo:8000"},
			{"cap_http_8000", "echo.demo.svc.cluster.local:8000"},
			{"cap_http_other", "echo:7000"},
			{"cap_http_other", "echo.demo.svc.cluster.local:7000"},
		} {
			require.NotNil(t, tables[q.table], q.table)
			sig := r3Resolve(t, tables[q.table], "demo", q.host, nil)
			answers[q.table+" "+q.host] = sig.first
			t.Logf("filter=%v  dial via %-14s Host %-36s -> %s", withFilter, q.table, q.host, sig.first)
		}
		if !withFilter {
			continue
		}
		// With the filter: the short name must be treated as the other names
		// of the user's Service are.
		for _, pair := range [][2]string{
			{"cap_http_8000 echo:8000", "cap_http_8000 echo.demo:8000"},
			{"cap_http_8000 echo", "cap_http_8000 echo.demo:8000"},
			{"cap_http_other echo:7000", "cap_http_other echo.demo.svc.cluster.local:7000"},
		} {
			short, full := answers[pair[0]], answers[pair[1]]
			assert.Equalf(t, strings.Contains(full, "passthrough"), strings.Contains(short, "passthrough"),
				"a user's Service with a service-wide filter and no route: %q is answered %q, where %q is answered %q", pair[0], short, pair[1], full)
		}
	}
}
