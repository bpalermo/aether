package envoy_validate

import (
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPerPortFixtureIsNotVacuous holds the per-port bootstrap to the shapes it
// exists to put in front of the proxy, so that TestEnvoyValidate accepting it
// keeps meaning something if the fixture is edited: at least two services
// share an HTTP port number, one service is mixed (HTTP and raw TCP), and one
// port number is HTTP for one service and raw TCP for another.
func TestPerPortFixtureIsNotVacuous(t *testing.T) {
	bs, err := buildCapturePerPortBootstrap()
	require.NoError(t, err)
	listeners := bs.GetStaticResources().GetListeners()
	require.NotEmpty(t, listeners)

	chains := map[string]bool{}
	for _, fc := range listeners[0].GetFilterChains() {
		chains[fc.GetName()] = true
	}
	// One HTTP chain whatever the mesh declares (the one-HCM capture
	// listener; the proxy port's own chain went in chart 3.0.2), and the
	// tables the proxy is given to validate: every per-port table, the proxy
	// port's, and the compiled one.
	assert.True(t, chains[proxy.CaptureHTTPChainName])
	assert.False(t, chains[proxy.CaptureHTTPRouteName(18081)], "no chain of the proxy port")
	assert.False(t, chains["cap_http_8080"], "no per-port chain")
	hcms := 0
	for _, fc := range listeners[0].GetFilterChains() {
		for _, f := range fc.GetFilters() {
			if strings.HasSuffix(f.GetName(), "http_connection_manager") {
				hcms++
			}
		}
	}
	assert.Equal(t, 1, hcms, "one connection manager on the capture listener")
	validated := map[string]bool{}
	for _, l := range listeners[1:] {
		validated[strings.TrimSuffix(l.GetName(), "_validate")] = true
	}
	for _, p := range PerPortHTTPPorts() {
		assert.Truef(t, validated[proxy.CaptureHTTPRouteName(p)], "table of port %d not validated", p)
	}
	assert.True(t, validated[proxy.CaptureHTTPOtherRouteName])
	assert.True(t, validated[proxy.CaptureHTTPChainName], "the compiled table is validated")
	// One port number, both classes, on one listener.
	assert.True(t, chains["cap_tcp_tcp:pp-mixed.default."+meshDomain+"_9000"])
	assert.True(t, chains["cap_tcp_tcp:pp-tcpfirst.default."+meshDomain+"_9000"])
	// The raw-TCP-only service is reached on its own port and on the TCP
	// mesh port, and on nothing else: no any-port chain, and no twin of one
	// on an HTTP port number (removed in chart 3.0.0).
	var tcpOnly []string
	for name := range chains {
		assert.NotContains(t, name, "cap_tcp_anyport_")
		if strings.Contains(name, "pp-tcponly") {
			tcpOnly = append(tcpOnly, name)
		}
	}
	assert.ElementsMatch(t, []string{
		"cap_tcp_tcp:pp-tcponly.default." + meshDomain + "_6379",
		"cap_tcp_tcp:pp-tcponly.default." + meshDomain + "_18082",
	}, tcpOnly)

	// Two services share port 8080's table, each routed to its own cluster.
	routed := map[string]string{}
	for _, vh := range PerPortRouteTable(8080).GetVirtualHosts() {
		for _, r := range vh.GetRoutes() {
			if c := r.GetRoute().GetCluster(); c != "" && c != proxy.PassthroughClusterName {
				routed[vh.GetName()] = c
			}
		}
	}
	assert.GreaterOrEqual(t, len(routed), 4, "port 8080 is shared: %v", routed)
	seen := map[string]bool{}
	for vh, cluster := range routed {
		assert.Falsef(t, seen[cluster], "two virtual hosts of table 8080 share cluster %s", cluster)
		seen[cluster] = true
		assert.Truef(t, strings.HasPrefix(vh, strings.TrimSuffix(cluster, ":8080")), "virtual host %s routes to another service's cluster %s", vh, cluster)
	}
	// The mixed service's raw-TCP port is refused in its own number's table.
	found := false
	for _, vh := range PerPortRouteTable(9000).GetVirtualHosts() {
		if vh.GetName() == "pp-mixed.default."+meshDomain+"|refused" {
			found = true
			assert.Equal(t, uint32(421), vh.GetRoutes()[0].GetDirectResponse().GetStatus())
		}
	}
	assert.True(t, found)

	// The compiled table tells names apart by an exact or prefix :authority
	// match somewhere in the proxy port's block (this fixture's proxy-port
	// table knows mesh names only, so its other names are another class), so
	// validate mode parses that shape. The agent's own tables no longer need
	// it there since chart 3.0.3: the proxy port answers the Kubernetes names
	// as the mesh name, one class.
	tables, err := perPortTablesAndCompiled()
	require.NoError(t, err)
	assert.Positive(t, proxyPortRoutesByName(tables[len(tables)-1]), "no route of the proxy port told apart by name")
}
