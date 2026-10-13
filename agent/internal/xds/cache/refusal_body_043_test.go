package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusalBodyFor returns the body of the direct response the table gives host.
func refusalBodyFor(t *testing.T, c *SnapshotCache, table, host string) string {
	t.Helper()
	vh := r3Pick(t, a043Tables(t, c)[table], host)
	require.NotEmptyf(t, vh.GetRoutes(), "%s has no virtual host for %s", table, host)
	dr := vh.GetRoutes()[0].GetDirectResponse()
	require.NotNilf(t, dr, "%s does not refuse %s (virtual host %q)", table, host, vh.GetName())
	return dr.GetBody().GetInlineString()
}

// TestRefusalBody_NamesThePortsTheRefusalIsJudgedBy: a refusal's body lists
// the ports the service can be addressed on. For a name a Service of the
// user's owns, an authority's port is the SERVICE's port: "<name>:8080" is
// refused when the Service has no port 8080, and the body said "declares no
// such port. Its ports: 8080 (http)", the pods' port, which is the one port
// number the caller had just been refused on.
func TestRefusalBody_NamesThePortsTheRefusalIsJudgedBy(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	own := a043Service{name: "own", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}, ip: "10.0.9.1"}
	gen := a043Service{name: "gen", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	c := a043Cache(t, []a043Service{gen})
	c.SetStaticDependencies([]string{"demo/own", "demo/gen"})
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/own": {8000: 8080, 5432: 9000, 7000: 7070}})
	reg := a043Registry([]a043Service{own, gen})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.NoError(t, c.generateSnapshot(ctx))

	mesh := "own.demo." + a043Mesh
	for _, table := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
		body := refusalBodyFor(t, c, table, mesh+":8080")
		assert.Containsf(t, body, "declares no such port", "%s", table)
		assert.Containsf(t, body, "Its ports: 5432 (tcp), 8000 (http).", "%s: the Service's ports, with the class of the pod port behind each; not 7000, whose target no pod declares", table)
		assert.NotContainsf(t, body, "8080 (http)", "%s: the body offers the port it has just refused", table)

		body = refusalBodyFor(t, c, table, mesh+":5432")
		assert.Containsf(t, body, "does not serve HTTP on that port", "%s", table)
		assert.Containsf(t, body, "Its ports: 5432 (tcp), 8000 (http).", "%s", table)

		// A generated Service's ports are the pods' own.
		assert.Containsf(t, refusalBodyFor(t, c, table, "gen.demo."+a043Mesh+":9999"), "Its ports: 8080 (http).", "%s", table)
	}
}
