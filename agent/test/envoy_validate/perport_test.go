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
	for _, p := range PerPortHTTPPorts() {
		assert.Truef(t, chains[proxy.CaptureHTTPRouteName(p)], "no HTTP chain for port %d", p)
	}
	assert.True(t, chains[proxy.CaptureHTTPOtherRouteName])
	// One port number, both classes, on one listener.
	assert.True(t, chains["cap_http_9000"])
	assert.True(t, chains["cap_tcp_tcp:pp-mixed.default."+meshDomain+"_9000"])
	assert.True(t, chains["cap_tcp_tcp:pp-tcpfirst.default."+meshDomain+"_9000"])
	// The any-port chain and its twins exist for the raw-TCP-only service
	// alone.
	anyPort := 0
	for name := range chains {
		if strings.HasPrefix(name, "cap_tcp_anyport_") {
			anyPort++
			assert.Contains(t, name, "pp-tcponly")
		}
	}
	assert.Equal(t, 1+len(PerPortHTTPPorts()), anyPort, "the any-port chain and one twin per HTTP port number")

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
}
