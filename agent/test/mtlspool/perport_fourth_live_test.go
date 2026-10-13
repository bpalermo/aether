package mtlspool

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"aethermesh.dev/agent/internal/capture"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
)

// r4Registry is the rig's registry with the rows of demo/multi under the
// test's control: each row is listed under every protocol it serves a port of.
type r4Registry struct {
	ppRegistry
	mu    sync.Mutex
	multi []*registryv1.ServiceEndpoint
}

func (r *r4Registry) set(rows ...*registryv1.ServiceEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.multi = rows
}

func (r *r4Registry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	out, err := r.ppRegistry.ListAllEndpoints(ctx, protocol)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ep := range r.multi {
		for _, class := range ep.GetPortProtocols() {
			if registryv1.Service_Protocol(class) == protocol {
				out[ppSvc] = append(out[ppSvc], ep)
				break
			}
		}
	}
	return out, nil
}

func r4Row(classes map[uint32]registryv1.PortProtocol, declared uint32) *registryv1.ServiceEndpoint {
	ep := ppEndpoint("10.0.1.2", "multi")
	ep.Port = declared
	ep.Ports = nil
	for port := range classes {
		ep.Ports = append(ep.Ports, port)
	}
	ep.PortProtocols = classes
	return ep
}

// TestPerPort_LiveReplacedServiceWithARetainedFloorKeepsItsFilter is the
// request the fourth review inferred and did not send. demo/multi carries a
// service-wide filter and an HTTPRoute rule. It serves 8080 (HTTP) and 9000
// (raw TCP); then 8080 only, so its raw-TCP floor is retained for the grace;
// then its pod is gone and its generated Service declares 9090. The node holds
// a retained floor entry of the service and nothing else.
//
// A request to multi.demo.<mesh>:9090 on the proxy port must be parked on the
// service's OWN virtual host, with the filter and the rule, while the cluster
// is fetched. Before the fix the service had no virtual host at all in that
// state: the request fell to the catch-all's on-demand route, the cluster was
// fetched by the authority, and the request was delivered with neither.
func TestPerPort_LiveReplacedServiceWithARetainedFloorKeepsItsFilter(t *testing.T) {
	const (
		h   = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
		tcp = registryv1.PortProtocol_PORT_PROTOCOL_TCP
	)
	ctx := context.Background()
	r := ppStart(t, ppChain(t))
	reg := &r4Registry{}
	load := func() {
		require.NoError(t, r.c.LoadClustersFromRegistry(ctx, "cluster-1", ppNodeName, reg))
	}
	declare := func(multi []uint32) {
		r.c.SetCaptureTCPServices([]capture.CaptureTCPService{
			{ServiceName: ppSvc, ClusterIP: "10.96.1.2", DeclaredHTTP: multi},
			{ServiceName: "demo/other", ClusterIP: "10.96.1.3", DeclaredHTTP: []uint32{8080, 9090}},
			{ServiceName: ppElsewhere, ClusterIP: "10.96.1.4", DeclaredHTTP: []uint32{8080, 9090}},
		})
	}

	reg.set(r4Row(map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}, 8080))
	load()
	reg.set(r4Row(map[uint32]registryv1.PortProtocol{8080: h}, 8080))
	load()
	reg.set()
	declare([]uint32{9090})
	load()

	// The agent's answer to an on-demand request: the pod is back, on 9090.
	r.onClusterSubscribe = func(name string) {
		t.Logf("[agent] on-demand request for %q", name)
		time.Sleep(200 * time.Millisecond)
		reg.set(r4Row(map[uint32]registryv1.PortProtocol{9090: h}, 9090))
		load()
		r.publish()
	}
	addr := r3ProxyPort(t, r)

	got := r3Get(addr, ppMesh+":9090", "/")
	t.Logf("MEASURED proxy port, Host %s:9090, first request after the service was replaced by its Service -> %s rule-ran=%v", ppMesh, got, got.rule)
	require.NoError(t, got.err)
	require.Equal(t, 200, got.status, "the request is parked and delivered: %s", got)
	if !got.chain || !got.rule {
		t.Errorf("the first request to a service replaced by its Service ran without its service-wide filter (ran=%v) or its HTTPRoute rule (ran=%v): %s", got.chain, got.rule, got)
	}
	again := r3Get(addr, ppMesh+":9090", "/")
	t.Logf("MEASURED the same request once the node holds the service -> %s rule-ran=%v", again, again.rule)
	require.True(t, again.chain && again.rule, "control: the filter and the rule run on a later request: %s", again)
}
