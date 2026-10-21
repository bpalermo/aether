package mtlspool

// A retry leaves the priority whose one endpoint failed. EndpointPriority
// bands a service's endpoints by locality; when a source's priority 0 is a
// single endpoint and that pod is going away, previous_hosts has no other host
// in the priority to steer a retry to, and without a retry priority every
// retry goes back to the dying endpoint and the caller gets the final 503.
//
// One Envoy plays the source and the destination proxies, every listener and
// cluster built by the agent's own generators: a service whose priority 0 is
// the dying endpoint and whose priority 1 is a healthy one. The source runs
// this release's route table, and a control source the same table with the
// retry priority removed, which is what the release before it served.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prioritizedServiceCluster is the caller's cluster for a mesh service as the
// agent builds it, subset load balancer included, with its endpoints in
// priority bands: bands[i] is priority i.
func prioritizedServiceCluster(t *testing.T, name string, bands ...*corev3.Address) *clusterv3.Cluster {
	t.Helper()
	cl := meshServiceCluster(t, name, false, "")
	cl.LbSubsetConfig = proxy.NewServiceCluster(name, name, name, nil).GetLbSubsetConfig()
	require.NotNil(t, cl.GetLbSubsetConfig(), "the production cluster balances through the subset load balancer")
	cl.LoadAssignment = &endpointv3.ClusterLoadAssignment{ClusterName: name}
	for i, a := range bands {
		cl.LoadAssignment.Endpoints = append(cl.LoadAssignment.Endpoints, &endpointv3.LocalityLbEndpoints{
			Priority: uint32(i),
			LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: a}},
			}},
		})
	}
	return cl
}

// withoutRetryPriority is rc with the retry priority removed from every route.
func withoutRetryPriority(t *testing.T, rc *routev3.RouteConfiguration) *routev3.RouteConfiguration {
	t.Helper()
	removed := 0
	for _, vh := range rc.GetVirtualHosts() {
		for _, r := range vh.GetRoutes() {
			if rp := r.GetRoute().GetRetryPolicy(); rp.GetRetryPriority() != nil {
				rp.RetryPriority = nil
				removed++
			}
		}
	}
	require.NotZero(t, removed, "the generated table carries no retry priority to remove: the control would be this release")
	return rc
}

// TestRetryLeavesTheDyingPriority: a request whose priority-0 endpoint fails
// is retried at priority 1 and succeeds; the control, the same table without
// the retry priority, retries onto the failing endpoint until it gives up.
// Both ways a pod can be going away are covered: its proxy no longer accepts
// (connect-failure, at the source), and its proxy accepts but its application
// is gone (the destination's outcome header, retriable-headers).
func TestRetryLeavesTheDyingPriority(t *testing.T) {
	healthy := startRecordingApp(t, "healthy", true)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refusedApp := closed.Addr().String()
	require.NoError(t, closed.Close())

	var listeners []*listenerv3.Listener
	var clusters []*clusterv3.Cluster
	inbound := func(name, app string) *corev3.Address {
		pod := begunPod(name)
		addr := begunSock("rp-" + name)
		listeners = append(listeners, productionInbound(t, pod, addr, false, false))
		c := staticCluster(begunAppCluster(pod), false, tcpAddress(app))
		clusters = append(clusters, c)
		return addr
	}
	healthyAddr := inbound("rphealthy", healthy.addr())
	appGoneAddr := inbound("rpappgone", refusedApp)
	// A destination proxy that no longer accepts: nothing listens there.
	proxyGoneAddr := begunSock("rp-proxygone")

	cases := []struct {
		service string
		dying   *corev3.Address
	}{
		{"rpproxygone", proxyGoneAddr},
		{"rpappgone", appGoneAddr},
	}
	var vhosts []*routev3.VirtualHost
	for _, c := range cases {
		svc := begunService(c.service)
		clusters = append(clusters, prioritizedServiceCluster(t, svc, c.dying, healthyAddr))
		vhosts = append(vhosts, proxy.BuildOutboundClusterVirtualHost(svc, []string{svc}))
	}
	listeners = append(listeners,
		productionOutbound(t, "source", proxy.BuildOutboundRouteConfiguration(vhosts, begunMeshDomain, nil)),
		productionOutbound(t, "source_control", withoutRetryPriority(t, proxy.BuildOutboundRouteConfiguration(vhosts, begunMeshDomain, nil))),
	)
	bs := &bootstrapv3.Bootstrap{
		Node:            &corev3.Node{Id: envoyNodeID, Cluster: "aether"},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{Listeners: listeners, Clusters: clusters},
	}
	e := runEnvoy(t, "retrypriority", bs, 1, "source", "source_control")

	get := func(addr, svc, uri string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+uri, nil)
		require.NoError(t, err)
		req.Host = svc
		resp, err := (&http.Client{Timeout: envoyWait}).Do(req)
		require.NoError(t, err)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	for _, c := range cases {
		svc := begunService(c.service)
		t.Run(c.service, func(t *testing.T) {
			// Several requests: a pass by chance would need every one of them
			// to start at priority 1, which the load (100/0) never does.
			for i := range 5 {
				uri := fmt.Sprintf("/%s/release/%d", c.service, i)
				status, body := get(e.addrs["source"], svc, uri)
				assert.Equalf(t, http.StatusOK, status, "%s: the retry did not leave priority 0 (body %q)", uri, body)
				assert.Equalf(t, "healthy", body, uri)
				assert.Equalf(t, 1, healthy.times(http.MethodGet, uri), "%s ran once, at priority 1", uri)

				uri = fmt.Sprintf("/%s/control/%d", c.service, i)
				status, body = get(e.addrs["source_control"], svc, uri)
				assert.Equalf(t, http.StatusServiceUnavailable, status,
					"%s: without the retry priority every retry must go back to the dying endpoint (body %q): the case does not reproduce the failure", uri, body)
				assert.Zerof(t, healthy.times(http.MethodGet, uri), "%s never reached priority 1", uri)
				assert.Falsef(t, strings.Contains(body, "healthy"), uri)
			}
		})
	}
}
