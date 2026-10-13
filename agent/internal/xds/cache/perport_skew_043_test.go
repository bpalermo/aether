package cache

// Per-port addressing (proposal 043) against registry rows of other versions.

import (
	"context"
	"slices"
	"testing"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
)

// kubernetes26Registry is what a chart 2.6 registrar on the KUBERNETES
// backend lists (registry/internal/k8s/kubernetes.go at 009edf89,
// podServesProtocol: `endpointmeta.Protocol(pod.Annotations) == want`): a pod
// under its pod-level protocol ONLY, with the full row (port, ports,
// port_protocols). In the fixture the pod-level protocol is the class of the
// declared (legacy) port.
func kubernetes26Registry(services []a043Service) *mockRegistry {
	return &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			out := map[string][]*registryv1.ServiceEndpoint{}
			for _, s := range services {
				if registryv1.Service_Protocol(s.ports[s.legacy]) != protocol {
					continue
				}
				ep := makeEndpoint(s.ip, "cluster-1", "node-2", s.legacy)
				ep.KubernetesMetadata.Namespace = s.ns
				ep.PortProtocols = s.ports
				for port := range s.ports {
					ep.Ports = append(ep.Ports, port)
				}
				slices.Sort(ep.Ports)
				out[s.key()] = append(out[s.key()], ep)
			}
			return out, nil
		},
	}
}

// TestPerPort_Skew_27AgentBehind26KubernetesRegistrar reports what a 2.7 agent
// builds for the two mixed shapes when the registrar is still 2.6 on the
// kubernetes backend: every raw-TCP chain and every route, and whether the
// cluster behind it exists and has an endpoint.
func TestPerPort_Skew_27AgentBehind26KubernetesRegistrar(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "client-0", Namespace: a043NS, ServiceAccount: "client", NetworkNamespace: "/var/run/netns/cni-client",
	}, a043Mesh))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	a043Project(c, services)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", kubernetes26Registry(services)))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)
	endpoints := snap.GetResources(resourcev3.EndpointType)

	hosts := func(cluster string) (published bool, n int) {
		cl, ok := clusters[cluster].(*clusterv3.Cluster)
		if !ok {
			return false, 0
		}
		eds := cl.GetEdsClusterConfig().GetServiceName()
		if eds == "" {
			eds = cluster
		}
		cla, ok := endpoints[eds].(*endpointv3.ClusterLoadAssignment)
		if !ok {
			return true, 0
		}
		for _, l := range cla.GetEndpoints() {
			n += len(l.GetLbEndpoints())
		}
		return true, n
	}

	c.clusterMu.RLock()
	for _, name := range []string{"demo/mixed", "demo/tcpfirst"} {
		p := c.servicePorts[name]
		t.Logf("derived %s: http=%v tcp=%v legacy=%d/%s", name, p.http, p.tcp, p.legacyPort, className(p.legacyClass))
	}
	c.clusterMu.RUnlock()

	l := a043CaptureListener(t, c)
	t.Logf("chains: %v", chainNames(l))
	dead := 0
	for _, fc := range l.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			tp := &tcp_proxyv3.TcpProxy{}
			if f.GetTypedConfig().UnmarshalTo(tp) != nil {
				continue
			}
			published, n := hosts(tp.GetCluster())
			t.Logf("tcp chain %-55s -> %-45s published=%v hosts=%d", fc.GetName(), tp.GetCluster(), published, n)
			if !published {
				dead++
				t.Errorf("chain %s forwards to cluster %q, which the snapshot does not publish (tcp_proxy kills the connection)", fc.GetName(), tp.GetCluster())
			}
		}
	}

	for _, name := range []string{"cap_http_8080", "cap_http_9000", "cap_http_18081", "cap_http_80"} {
		table := a043Tables(t, c)[name]
		if table == nil {
			t.Logf("table %s: absent", name)
			continue
		}
		for _, vh := range table.GetVirtualHosts() {
			if vh.GetName() == "on_demand_catch_all" {
				continue
			}
			for _, r := range vh.GetRoutes() {
				ra := r.GetRoute()
				if ra == nil {
					if dr := r.GetDirectResponse(); dr != nil && slices.ContainsFunc(vh.GetDomains(), func(d string) bool {
						return len(d) > 5 && (d[:5] == "mixed" || d[:5] == "tcpfi")
					}) {
						t.Logf("table %s vhost %s (%v): direct response %d", name, vh.GetName(), vh.GetDomains(), dr.GetStatus())
					}
					continue
				}
				cluster := ra.GetCluster()
				if cluster == "" {
					cluster = quicSelectionDefault(t, ra)
				}
				if !slices.ContainsFunc(vh.GetDomains(), func(d string) bool {
					return len(d) > 5 && (d[:5] == "mixed" || d[:5] == "tcpfi")
				}) {
					continue
				}
				published, n := hosts(cluster)
				t.Logf("table %s vhost %s -> %s published=%v hosts=%d", name, vh.GetName(), cluster, published, n)
				if !published {
					t.Errorf("table %s, virtual host %s routes to cluster %q, which the snapshot does not publish", name, vh.GetName(), cluster)
				}
			}
		}
	}
}
