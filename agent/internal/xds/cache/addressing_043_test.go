package cache

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// The addressing rule of proposal 043, held end to end through the cache: the
// registry listings go in, a snapshot comes out, and every assertion is made on
// the route tables and listeners of that snapshot, resolved the way the proxy
// resolves them. What these tests exist to catch is the SILENT wrong answer: a
// request delivered to another port, or to another service, with a 200.

const (
	a043Mesh = "aether.internal"
	a043NS   = "demo"
)

// a043Service is one workload of the fixture: the port its pods declare in
// endpoint.aether.io/port and the class of each port.
type a043Service struct {
	name   string
	ns     string
	legacy uint32
	ports  map[uint32]registryv1.PortProtocol
	ip     string
	vip    string
}

func (s a043Service) key() string { return s.ns + "/" + s.name }

// a043Fixture is the leg table's workload set (proposal 043, "Addressing legs
// (Phase 1)"), plus a raw-TCP-only service and a second namespace that shares
// a bare name.
func a043Fixture() []a043Service {
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	return []a043Service{
		{name: "httponly", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.1.1", vip: "10.96.1.1"},
		{name: "multi", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.1.2", vip: "10.96.1.2"},
		{name: "eighty", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{80: h, 8080: h, 9090: h}, ip: "10.0.1.3", vip: "10.96.1.3"},
		{name: "mixed", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}, ip: "10.0.1.4", vip: "10.96.1.4"},
		{name: "tcpfirst", ns: a043NS, legacy: 9000, ports: map[uint32]registryv1.PortProtocol{9000: tcp, 8080: h}, ip: "10.0.1.5", vip: "10.96.1.5"},
		{name: "nine", ns: a043NS, legacy: 9000, ports: map[uint32]registryv1.PortProtocol{9000: h}, ip: "10.0.1.6", vip: "10.96.1.6"},
		{name: "tcponly", ns: a043NS, legacy: 9000, ports: map[uint32]registryv1.PortProtocol{9000: tcp}, ip: "10.0.1.7", vip: "10.96.1.7"},
		// The same bare name in another namespace: "multi" alone names neither.
		{name: "multi", ns: "other", legacy: 7070, ports: map[uint32]registryv1.PortProtocol{7070: h}, ip: "10.0.2.2", vip: "10.96.2.2"},
	}
}

// a043Registry lists each service under every protocol it serves a port of,
// with the same row, as both registry backends do.
func a043Registry(services []a043Service) *mockRegistry {
	return &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			out := map[string][]*registryv1.ServiceEndpoint{}
			for _, s := range services {
				serves := false
				ep := makeEndpoint(s.ip, "cluster-1", "node-2", s.legacy)
				ep.KubernetesMetadata.Namespace = s.ns
				ep.PortProtocols = s.ports
				for port, class := range s.ports {
					ep.Ports = append(ep.Ports, port)
					if registryv1.Service_Protocol(class) == protocol {
						serves = true
					}
				}
				slices.Sort(ep.Ports)
				if serves {
					out[s.key()] = append(out[s.key()], ep)
				}
			}
			return out, nil
		},
	}
}

// a043Cache builds a capture-enabled, redirect-all cache holding the fixture
// and one local pod, with a snapshot set.
func a043Cache(t *testing.T, services []a043Service) *SnapshotCache {
	t.Helper()
	ctx := context.Background()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "client-0", Namespace: a043NS, ServiceAccount: "client", NetworkNamespace: "/var/run/netns/cni-client",
	}, a043Mesh))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	a043Project(c, services)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	return c
}

// a043Project feeds what the mesh-Service reconciler projects: the name and
// the address of every generated Service, and nothing else.
func a043Project(c *SnapshotCache, services []a043Service) {
	a043ProjectWith(c, services, true)
}

// a043ProjectNamesOnly projects the Services without their real ports: a
// generated Service the agent has seen before the registrar wrote its ports,
// or one written by a registrar that did not write them. Of such a service
// the node knows nothing until it holds an endpoint.
func a043ProjectNamesOnly(c *SnapshotCache, services []a043Service) {
	a043ProjectWith(c, services, false)
}

func a043ProjectWith(c *SnapshotCache, services []a043Service, withPorts bool) {
	authorities := map[string]string{}
	var vips []capture.CaptureTCPService
	for _, s := range services {
		authorities[s.key()] = fmt.Sprintf("%s.%s.svc.cluster.local", s.name, s.ns)
		svc := capture.CaptureTCPService{ServiceName: s.key(), ClusterIP: s.vip}
		for port, class := range s.ports {
			switch {
			case !withPorts:
			case class == registryv1.PortProtocol_PORT_PROTOCOL_HTTP:
				svc.DeclaredHTTP = append(svc.DeclaredHTTP, port)
			default:
				svc.DeclaredOther = append(svc.DeclaredOther, port)
			}
		}
		slices.Sort(svc.DeclaredHTTP)
		slices.Sort(svc.DeclaredOther)
		vips = append(vips, svc)
	}
	c.SetCaptureAuthorities(authorities)
	c.SetCaptureTCPServices(vips)
}

// a043Tables returns the snapshot's route tables by name.
func a043Tables(t *testing.T, c *SnapshotCache) map[string]*routev3.RouteConfiguration {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	out := map[string]*routev3.RouteConfiguration{}
	for name, res := range snap.GetResources(resourcev3.RouteType) {
		rc, ok := res.(*routev3.RouteConfiguration)
		require.True(t, ok)
		out[name] = rc
	}
	return out
}

// a043Answer is what a route table does with one request.
type a043Answer struct {
	// cluster is the cluster the request is routed to, "" when it is not.
	cluster string
	// refusal is the x-aether-error of a 421, "" when the request is not
	// refused.
	refusal string
	// status is the direct-response status, 0 when the request is routed.
	status uint32
	// onDemand: the cluster is named by the request's own authority.
	onDemand bool
	vhost    string
	body     string
}

// a043Resolve picks the virtual host the way the proxy does (an exact domain,
// then the longest prefix wildcard "x*", then "*"), then the first route whose
// path and :authority matchers accept the request "GET / " with authority
// host.
func a043Resolve(t *testing.T, table *routev3.RouteConfiguration, host string) a043Answer {
	t.Helper()
	require.NotNil(t, table, "no such route table")
	var exact, wildcard, catchAll *routev3.VirtualHost
	wildcardLen := -1
	seen := map[string]string{}
	for _, vh := range table.GetVirtualHosts() {
		for _, d := range vh.GetDomains() {
			if prev, dup := seen[d]; dup {
				t.Fatalf("table %s: domain %q is claimed by both %q and %q; the proxy rejects the whole table", table.GetName(), d, prev, vh.GetName())
			}
			seen[d] = vh.GetName()
			switch {
			case d == "*":
				catchAll = vh
			case strings.HasSuffix(d, "*"):
				if p := strings.TrimSuffix(d, "*"); strings.HasPrefix(host, p) && len(p) > wildcardLen {
					wildcard, wildcardLen = vh, len(p)
				}
			case d == host:
				exact = vh
			}
		}
	}
	vh := catchAll
	if wildcard != nil {
		vh = wildcard
	}
	if exact != nil {
		vh = exact
	}
	require.NotNil(t, vh, "table %s has no catch-all", table.GetName())
	for _, r := range vhostRoutes(t, vh) {
		if p := r.GetMatch().GetPath(); p != "" && p != "/" {
			continue
		}
		matched := true
		for _, h := range r.GetMatch().GetHeaders() {
			if h.GetName() != ":authority" {
				matched = false
				break
			}
			matched = matched && mustRegexp(t, h.GetStringMatch().GetSafeRegex().GetRegex()).MatchString(host)
		}
		if !matched {
			continue
		}
		ans := a043Answer{vhost: vh.GetName()}
		if dr := r.GetDirectResponse(); dr != nil {
			ans.status = dr.GetStatus()
			ans.body = dr.GetBody().GetInlineString()
			for _, h := range r.GetResponseHeadersToAdd() {
				if h.GetHeader().GetKey() == proxy.NoHTTPPortErrorHeader {
					ans.refusal = h.GetHeader().GetValue()
				}
			}
			return ans
		}
		ra := r.GetRoute()
		require.NotNil(t, ra, "a route of %s/%s is neither a direct response nor a route", table.GetName(), vh.GetName())
		switch {
		case ra.GetClusterHeader() != "":
			ans.cluster, ans.onDemand = host, true
		case ra.GetCluster() != "":
			ans.cluster = ra.GetCluster()
		default:
			// The per-source HTTP/3 selection wraps the h2 cluster; its
			// on_no_match is the h2 cluster the route had.
			ans.cluster = quicSelectionDefault(t, ra)
		}
		return ans
	}
	t.Fatalf("table %s, virtual host %s: no route matches %q", table.GetName(), vh.GetName(), host)
	return a043Answer{}
}

// a043Names returns the name families of a fixture service.
func a043Names(s a043Service, bare bool) (mesh string, kube []string) {
	mesh = fmt.Sprintf("%s.%s.%s", s.name, s.ns, a043Mesh)
	kube = []string{
		fmt.Sprintf("%s.%s.svc.cluster.local", s.name, s.ns),
		fmt.Sprintf("%s.%s.svc", s.name, s.ns),
		fmt.Sprintf("%s.%s", s.name, s.ns),
	}
	if bare {
		kube = append(kube, s.name)
	}
	return mesh, kube
}

// a043Cluster is the cluster that serves HTTP port port of s.
func a043Cluster(s a043Service, port uint32) string {
	mesh, _ := a043Names(s, false)
	if port == s.legacy {
		return mesh
	}
	return fmt.Sprintf("%s:%d", mesh, port)
}

func a043BareIsUnique(services []a043Service, s a043Service) bool {
	n := 0
	for _, o := range services {
		if o.name == s.name {
			n++
		}
	}
	return n == 1
}

// TestAddressing_CapturedDialledPortDecides is legs A1, A2, A5, A7 and A9 of
// proposal 043 on the captured path, for every workload, every HTTP port and
// every name family: the table of the dialled port routes a request to that
// port's cluster or refuses it, and never anything else. No port has another
// meaning: a dial to port 80 is port 80, served when the service declares it
// and refused otherwise. (The proxy port, 18081, is not a dialled SERVICE
// port: its table is held by TestAddressing_ProxyPortOnTheCapturedPath.)
func TestAddressing_CapturedDialledPortDecides(t *testing.T) {
	services := a043Fixture()
	c := a043Cache(t, services)
	tables := a043Tables(t, c)

	// Every HTTP port number of the fixture has a table, and so do port 80,
	// the proxy port and cap_http_other.
	for _, name := range []string{"cap_http_80", "cap_http_7070", "cap_http_8080", "cap_http_9000", "cap_http_9090", "cap_http_18081", "cap_http_other"} {
		require.Contains(t, tables, name)
	}
	require.NotContains(t, tables, "cap_http_18082", "the TCP mesh port is never an HTTP chain's port")
	require.NotContains(t, tables, "cap_http", "the single table is gone")

	dialled := []uint32{80, 7070, 7777, 8080, 9000, 9090}
	for _, s := range services {
		mesh, kube := a043Names(s, a043BareIsUnique(services, s))
		for _, d := range dialled {
			table := tables[proxy.CaptureHTTPRouteName(d)]
			other := table == nil
			if other {
				table = tables[proxy.CaptureHTTPOtherRouteName]
			}
			for _, name := range append([]string{mesh}, kube...) {
				t.Run(fmt.Sprintf("%s/dial_%d/%s", s.key(), d, name), func(t *testing.T) {
					class, declared := s.ports[d]
					isHTTP := declared && class == registryv1.PortProtocol_PORT_PROTOCOL_HTTP

					// A1/A2: the Host names the dialled port. A9: a port-less
					// Host means the dialled port.
					for _, host := range []string{name, fmt.Sprintf("%s:%d", name, d)} {
						got := a043Resolve(t, table, host)
						switch {
						case isHTTP:
							// Port 80 included: a port-less Host on a dial to
							// a declared port 80 is port 80, like any port.
							assert.Equalf(t, a043Cluster(s, d), got.cluster, "Host %q on dialled port %d must reach port %d and no other (%+v)", host, d, d, got)
							assert.False(t, got.onDemand, "a known service is routed by its own virtual host")
						case other:
							// A7, on a port number no service declares as HTTP.
							assert.Equalf(t, proxy.RefusalPortNotDeclared, got.refusal, "%+v", got)
						case len(httpPortsOf(s)) == 0:
							// HTTP to a service that serves none, whatever was
							// dialled: it is told where it is reached.
							assert.Equalf(t, proxy.RefusalNoHTTPPort, got.refusal, "%+v", got)
						case declared:
							assert.Equalf(t, proxy.RefusalPortNotHTTP, got.refusal, "%+v", got)
						default:
							// A7. Port 80 of a service that does not declare
							// it is one more undeclared port: it used to
							// reach the port the pods declare.
							assert.Equalf(t, proxy.RefusalPortNotDeclared, got.refusal, "%+v", got)
						}
						if got.refusal != "" {
							assert.Equal(t, uint32(421), got.status)
							assert.Empty(t, got.cluster, "a refusal reaches no cluster")
							if got.refusal != proxy.RefusalNoHTTPPort {
								for p := range s.ports {
									assert.Contains(t, got.body, strconv.Itoa(int(p)), "the body lists the service's ports")
								}
							}
						}
					}

					// A5: a Host that names ANOTHER port is refused, whatever
					// that port is, and never obeyed.
					for _, otherPort := range []uint32{80, 8080, 9000, 9090, 18081, 7777} {
						if otherPort == d {
							continue
						}
						got := a043Resolve(t, table, fmt.Sprintf("%s:%d", name, otherPort))
						if d == 7777 {
							assert.Equalf(t, proxy.RefusalPortNotDeclared, got.refusal, "%+v", got)
						} else {
							assert.Equalf(t, proxy.RefusalPortMismatch, got.refusal, "dialled %d, Host port %d: %+v", d, otherPort, got)
						}
						assert.Empty(t, got.cluster)
					}
				})
			}
		}
	}
}

func httpPortsOf(s a043Service) []uint32 {
	var out []uint32
	for p, class := range s.ports {
		if class == registryv1.PortProtocol_PORT_PROTOCOL_HTTP {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// a043HostDecides holds the rule of a table where nothing was dialled that
// could decide, so the port in Host is the port: the loopback table, and the
// table of the proxy port on the captured path.
//
//   - "<mesh name>:<P>", P an HTTP port: port P's cluster;
//   - P declared raw TCP: 421 port-not-http; any other P: 421
//     port-not-declared. 18081 is such a P: it is the proxy's port, not a
//     port of the service (it used to mean "the port the pods declare");
//   - the port-less name: port 80 when the service declares it, else 421
//     port-required, or no-http-port for a service with no HTTP port. Never
//     another port.
func a043HostDecides(t *testing.T, table *routev3.RouteConfiguration, s a043Service) {
	t.Helper()
	mesh, _ := a043Names(s, false)
	for port, class := range s.ports {
		got := a043Resolve(t, table, fmt.Sprintf("%s:%d", mesh, port))
		if class == registryv1.PortProtocol_PORT_PROTOCOL_HTTP {
			// A3.
			assert.Equalf(t, a043Cluster(s, port), got.cluster, "port %d: %+v", port, got)
			assert.False(t, got.onDemand)
		} else {
			// A6.
			assert.Equalf(t, proxy.RefusalPortNotHTTP, got.refusal, "port %d: %+v", port, got)
			assert.Equal(t, uint32(421), got.status)
		}
	}
	// A7, and the proxy's own port, which is one more undeclared port.
	for _, p := range []uint32{7777, 18081} {
		got := a043Resolve(t, table, fmt.Sprintf("%s:%d", mesh, p))
		assert.Equalf(t, proxy.RefusalPortNotDeclared, got.refusal, "port %d: %+v", p, got)
		assert.Empty(t, got.cluster, "port %d reaches no cluster", p)
	}
	// The port-less name is port 80, or refused.
	got := a043Resolve(t, table, mesh)
	switch {
	case s.ports[80] == registryv1.PortProtocol_PORT_PROTOCOL_HTTP:
		assert.Equalf(t, a043Cluster(s, 80), got.cluster, "%+v", got)
	case len(httpPortsOf(s)) > 0:
		assert.Equalf(t, proxy.RefusalPortRequired, got.refusal, "%+v", got)
		assert.Equal(t, mesh+"|port-required", got.vhost)
		assert.Empty(t, got.cluster, "a port-less name never reaches the port the pods declare")
	default:
		assert.Equalf(t, proxy.RefusalNoHTTPPort, got.refusal, "%+v", got)
	}
	for _, vh := range table.GetVirtualHosts() {
		assert.NotContains(t, vh.GetName(), "|legacy", "no virtual host serves a legacy spelling")
	}
}

// TestAddressing_ProxyPortOnTheCapturedPath: a captured dial to port 18081 is
// the mesh's HTTP proxy port on the captured path. 18081 is not a port of any
// service, so the dialled port decides nothing: the chain is served the
// loopback table's virtual hosts, and the port in Host is the port. There is
// no port-mismatch there, and a Host that names 18081 itself is a port the
// service does not declare. This is how a pod that is not on redirect-all,
// which has only the mesh ports diverted, reaches a service by name.
func TestAddressing_ProxyPortOnTheCapturedPath(t *testing.T) {
	services := a043Fixture()
	c := a043Cache(t, services)
	tables := a043Tables(t, c)
	table := tables["cap_http_18081"]
	require.NotNil(t, table)
	out := tables[proxy.OutboundHTTPRouteName]

	for _, s := range services {
		// The short name is not one of these: it has a virtual host of its
		// own on this table too (TestShortName_AnswersAsTheFullName).
		mesh, kube := a043Names(s, false)
		t.Run(s.key(), func(t *testing.T) {
			a043HostDecides(t, table, s)
			// Never a port-mismatch: nothing was dialled that a Host could
			// disagree with.
			for _, p := range []uint32{0, 80, 7777, 8080, 9000, 9090, 18081} {
				host := mesh
				if p != 0 {
					host = fmt.Sprintf("%s:%d", mesh, p)
				}
				got := a043Resolve(t, table, host)
				assert.NotEqualf(t, proxy.RefusalPortMismatch, got.refusal, "Host %s", host)
				// And the same answer as on the loopback listener.
				want := a043Resolve(t, out, host)
				assert.Equalf(t, want, got, "Host %s: the proxy port answers alike on both paths", host)
			}
			// Mesh names only, as on the loopback listener: a cluster.local or
			// bare name has no virtual host here. Under redirect-all it
			// leaves through the passthrough to what was dialled (something
			// else may listen on 18081); it reaches no mesh cluster.
			for _, name := range kube {
				for _, host := range []string{name, name + ":8080", name + ":18081"} {
					got := a043Resolve(t, table, host)
					assert.Equalf(t, "on_demand_catch_all", got.vhost, "Host %s: %+v", host, got)
					assert.Equalf(t, proxy.PassthroughClusterName, got.cluster, "Host %s: %+v", host, got)
				}
			}
		})
	}

	// The virtual hosts are the loopback table's, one for one; only the floor
	// differs (passthrough under redirect-all, where the loopback table
	// answers 404).
	names := func(rc *routev3.RouteConfiguration) []string {
		var out []string
		for _, vh := range rc.GetVirtualHosts() {
			if vh.GetName() != "on_demand_catch_all" {
				out = append(out, vh.GetName())
			}
		}
		return out
	}
	assert.Equal(t, names(out), names(table))
	assert.Equal(t, uint32(404), a043Resolve(t, out, "example.org:18081").status)
	assert.Equal(t, proxy.PassthroughClusterName, a043Resolve(t, table, "example.org:18081").cluster)

	// Without redirect-all the floor is 404 there too.
	c.SetCaptureRedirectAll(false)
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", a043Registry(services)))
	assert.Equal(t, uint32(404), a043Resolve(t, a043Tables(t, c)["cap_http_18081"], "example.org:18081").status)
}

// a043HostPortClusters checks every route of a host-decided table (the
// loopback table, the proxy port's) that reaches a mesh cluster: the cluster
// is that of a port its virtual host's own authorities spell, and a port-less
// authority is claimed only by the virtual host of port 80. Returns how many
// routes it saw.
func a043HostPortClusters(t *testing.T, table *routev3.RouteConfiguration, byMesh map[string]a043Service) int {
	t.Helper()
	routes := 0
	for _, vh := range table.GetVirtualHosts() {
		if vh.GetName() == "on_demand_catch_all" || vh.GetMatcher() != nil {
			// A short name's virtual host holds copies of the routes walked
			// here, per namespace (TestShortName_AnswersAsTheFullName).
			continue
		}
		mesh, _, _ := strings.Cut(vh.GetName(), "|")
		mesh, _, _ = strings.Cut(mesh, ":")
		s, known := byMesh[mesh]
		require.Truef(t, known, "table %s: virtual host %q belongs to no fixture service", table.GetName(), vh.GetName())
		for _, r := range vh.GetRoutes() {
			ra := r.GetRoute()
			if ra == nil {
				continue
			}
			cluster := ra.GetCluster()
			if cluster == "" {
				cluster = quicSelectionDefault(t, ra)
			}
			routes++
			for _, d := range vh.GetDomains() {
				port := uint32(80) // a port-less authority is port 80
				if _, p, ok := strings.Cut(d, ":"); ok {
					n, err := strconv.Atoi(p)
					require.NoErrorf(t, err, "table %s: virtual host %s routes from the authority %q", table.GetName(), vh.GetName(), d)
					port = uint32(n)
				}
				require.Equalf(t, registryv1.PortProtocol_PORT_PROTOCOL_HTTP, s.ports[port], "table %s: %s routes the authority %q although the service has no HTTP port %d", table.GetName(), vh.GetName(), d, port)
				assert.Equalf(t, a043Cluster(s, port), cluster, "table %s, virtual host %s, authority %q", table.GetName(), vh.GetName(), d)
			}
		}
	}
	return routes
}

// TestAddressing_NoRouteLeavesItsPort is the structural form of #1682: in the
// table of dialled port P, every route that reaches a mesh cluster reaches
// the cluster of port P of the service the virtual host belongs to; in the two
// tables where the Host decides, the cluster of the port the authority spells.
// It walks every route of every table instead of resolving chosen requests,
// so a route nobody thought to probe is covered too.
func TestAddressing_NoRouteLeavesItsPort(t *testing.T) {
	services := a043Fixture()
	c := a043Cache(t, services)
	byMesh := map[string]a043Service{}
	for _, s := range services {
		mesh, _ := a043Names(s, false)
		byMesh[mesh] = s
	}
	routes, hostDecided := 0, 0
	for name, table := range a043Tables(t, c) {
		port, isPortTable := proxy.CaptureHTTPRoutePort(name)
		if name == proxy.OutboundHTTPRouteName || (isPortTable && port == 18081) {
			hostDecided += a043HostPortClusters(t, table, byMesh)
			continue
		}
		if !isPortTable && name != proxy.CaptureHTTPOtherRouteName {
			continue
		}
		for _, vh := range table.GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				ra := r.GetRoute()
				if ra == nil {
					continue
				}
				if vh.GetName() == "on_demand_catch_all" {
					if ra.GetClusterHeader() != "" {
						require.True(t, isPortTable, "cap_http_other names no cluster on demand")
						re := mustRegexp(t, r.GetMatch().GetHeaders()[0].GetStringMatch().GetSafeRegex().GetRegex())
						assert.True(t, re.MatchString(fmt.Sprintf("x.y.%s:%d", a043Mesh, port)))
						assert.Falsef(t, re.MatchString(fmt.Sprintf("x.y.%s:%d", a043Mesh, port+1)), "table %s: an authority of another port may not name a cluster", name)
						assert.Falsef(t, re.MatchString("x.y."+a043Mesh), "table %s: a port-less authority may not name a cluster on demand (it would be the bare name's cluster, which is no port's)", name)
						assert.False(t, re.MatchString("x.y.svc.cluster.local"), "only a mesh authority is a cluster name")
						continue
					}
					assert.Equal(t, proxy.PassthroughClusterName, ra.GetCluster())
					continue
				}
				require.True(t, isPortTable, "cap_http_other routes nothing: %s", vh.GetName())
				mesh, _, _ := strings.Cut(vh.GetName(), "|")
				s, known := byMesh[mesh]
				require.Truef(t, known, "virtual host %q belongs to no fixture service", vh.GetName())
				cluster := ra.GetCluster()
				if cluster == "" {
					cluster = quicSelectionDefault(t, ra)
				}
				routes++
				require.Equalf(t, registryv1.PortProtocol_PORT_PROTOCOL_HTTP, s.ports[port], "table %s routes %s although the service has no HTTP port %d", name, vh.GetName(), port)
				assert.NotContains(t, vh.GetName(), "|legacy")
				assert.Equalf(t, a043Cluster(s, port), cluster, "table %s, virtual host %s", name, vh.GetName())
			}
		}
	}
	// One route per (service, HTTP port) of the fixture: ten in the per-port
	// tables, and ten in each of the two tables where the Host decides.
	assert.Equal(t, 10, routes, "the walk must have seen the fixture's routes, or it proves nothing")
	assert.Equal(t, 20, hostDecided, "and those of the loopback table and of the proxy port's")
}

// TestAddressing_Loopback is legs A3, A6 and A7 on the loopback listener
// (out_http): nothing is dialled, so the port in Host is the port, and a
// name without a port is port 80 or refused.
func TestAddressing_Loopback(t *testing.T) {
	services := a043Fixture()
	c := a043Cache(t, services)
	out := a043Tables(t, c)[proxy.OutboundHTTPRouteName]
	for _, s := range services {
		_, kube := a043Names(s, false)
		t.Run(s.key(), func(t *testing.T) {
			a043HostDecides(t, out, s)
			// The loopback listener answers mesh names only.
			for _, name := range kube {
				got := a043Resolve(t, out, name+":8080")
				assert.Equalf(t, uint32(404), got.status, "%s: %+v", name, got)
			}
		})
	}
	// A name that is no mesh service this node knows still takes the cold
	// path, by its own authority.
	got := a043Resolve(t, out, "nobody.demo."+a043Mesh+":8080")
	assert.True(t, got.onDemand)
}

// TestAddressing_ClustersArePerPortAndClass: an HTTP cluster exists for every
// (service, HTTP port) and for no raw-TCP port; a raw-TCP cluster for every
// (service, raw-TCP port) and for no HTTP port. A cluster of the wrong class
// for a port is how a request reaches a port that cannot answer it.
func TestAddressing_ClustersArePerPortAndClass(t *testing.T) {
	services := a043Fixture()
	c := a043Cache(t, services)
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)
	for _, s := range services {
		mesh, _ := a043Names(s, false)
		for port, class := range s.ports {
			h2 := a043Cluster(s, port)
			tcp := fmt.Sprintf("tcp:%s:%d", mesh, port)
			if class == registryv1.PortProtocol_PORT_PROTOCOL_HTTP {
				assert.Containsf(t, clusters, h2, "%s: HTTP port %d has no cluster", s.key(), port)
				assert.NotContainsf(t, clusters, tcp, "%s: HTTP port %d has a raw-TCP cluster", s.key(), port)
			} else {
				assert.Containsf(t, clusters, tcp, "%s: raw-TCP port %d has no cluster", s.key(), port)
				assert.NotContainsf(t, clusters, fmt.Sprintf("%s:%d", mesh, port), "%s: raw-TCP port %d has an h2 cluster", s.key(), port)
			}
		}
		if s.ports[s.legacy] != registryv1.PortProtocol_PORT_PROTOCOL_HTTP {
			assert.NotContainsf(t, clusters, mesh, "%s: no default h2 cluster for a raw-TCP legacy target (#1672)", s.key())
			assert.NotContainsf(t, clusters, mesh+":18081", "%s: nor its mesh-port alias", s.key())
		}
	}
}

// a043CaptureListener returns the local pod's capture listener.
func a043CaptureListener(t *testing.T, c *SnapshotCache) *listenerv3.Listener {
	t.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	for name, res := range snap.GetResources(resourcev3.ListenerType) {
		if strings.HasPrefix(name, "capture_") && !strings.Contains(name, "udp") {
			return res.(*listenerv3.Listener)
		}
	}
	t.Fatal("no capture listener in the snapshot")
	return nil
}

func chainNames(l *listenerv3.Listener) []string {
	out := make([]string, 0, len(l.GetFilterChains()))
	for _, fc := range l.GetFilterChains() {
		out = append(out, fc.GetName())
	}
	slices.Sort(out)
	return out
}

// TestAddressing_ListenerChains holds the capture listener's chain set against
// the fixture: an HTTP chain per HTTP port number, a raw-TCP chain per
// (service, raw-TCP port), the two legacy chains of a raw-TCP legacy target,
// and no chain that takes "any other port" of an address.
func TestAddressing_ListenerChains(t *testing.T) {
	services := a043Fixture()
	c := a043Cache(t, services)
	l := a043CaptureListener(t, c)
	names := chainNames(l)

	for _, want := range []string{
		"cap_http_80", "cap_http_7070", "cap_http_8080", "cap_http_9000", "cap_http_9090", "cap_http_18081", "cap_http_other",
		// mixed: 9000 is raw TCP, and its legacy target is HTTP.
		"cap_tcp_tcp:mixed.demo.aether.internal_9000",
		// tcpfirst: a raw-TCP legacy target on its own port and on :18082.
		"cap_tcp_tcp:tcpfirst.demo.aether.internal_9000",
		"cap_tcp_tcp:tcpfirst.demo.aether.internal_18082",
		// tcponly: the same, and nothing else.
		"cap_tcp_tcp:tcponly.demo.aether.internal_9000",
		"cap_tcp_tcp:tcponly.demo.aether.internal_18082",
	} {
		assert.Contains(t, names, want)
	}
	for _, name := range names {
		assert.NotContains(t, name, "anyport", "no service has an any-port chain: a raw-TCP service is reached on the ports it declares")
		assert.NotContains(t, name, "cap_tcp_tcp:mixed.demo.aether.internal_18082", "the TCP mesh port names nothing on a service whose legacy target is HTTP")
		assert.NotContains(t, name, "cap_tcp_tcp:nine", "a port declared HTTP gets no raw-TCP chain, whatever another service declares for the number")
	}

	// A10: nine's HTTP on 9000 and mixed's raw TCP on 9000 are told apart by
	// the address. The raw-TCP chains of port 9000 match an address; the HTTP
	// chain of port 9000 matches none.
	addresses := map[string][]string{}
	for _, fc := range l.GetFilterChains() {
		m := fc.GetFilterChainMatch()
		if m.GetDestinationPort().GetValue() != 9000 {
			continue
		}
		for _, r := range m.GetPrefixRanges() {
			addresses[fc.GetName()] = append(addresses[fc.GetName()], r.GetAddressPrefix())
		}
		if len(m.GetPrefixRanges()) == 0 {
			addresses[fc.GetName()] = nil
		}
	}
	assert.Equal(t, map[string][]string{
		"cap_http_9000": nil,
		"cap_tcp_tcp:mixed.demo.aether.internal_9000":    {"10.96.1.4"},
		"cap_tcp_tcp:tcpfirst.demo.aether.internal_9000": {"10.96.1.5"},
		"cap_tcp_tcp:tcponly.demo.aether.internal_9000":  {"10.96.1.7"},
	}, addresses)

	// No two chains match the same thing: the proxy rejects such a listener
	// whole.
	seen := map[string]string{}
	for _, fc := range l.GetFilterChains() {
		key := fc.GetFilterChainMatch().String()
		if prev, dup := seen[key]; dup {
			t.Errorf("chains %q and %q have the same match %s", prev, fc.GetName(), key)
		}
		seen[key] = fc.GetName()
	}
}

// TestAddressing_ChainAndTableTravelTogether: every cap_http_<P> chain of a
// listener in a snapshot names a route table of that snapshot, when a port
// number enters the mesh and when it leaves. A chain naming a table the
// snapshot does not carry leaves the listener warming (#817).
func TestAddressing_ChainAndTableTravelTogether(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	check := func(wantPort uint32, present bool) {
		t.Helper()
		tables := a043Tables(t, c)
		l := a043CaptureListener(t, c)
		named := 0
		for _, fc := range l.GetFilterChains() {
			if !strings.HasPrefix(fc.GetName(), proxy.CaptureHTTPRoutePrefix) {
				continue
			}
			named++
			assert.Containsf(t, tables, fc.GetName(), "chain %s names a route table the snapshot does not carry", fc.GetName())
		}
		require.Greater(t, named, 2)
		assert.Equal(t, present, slices.Contains(chainNames(l), proxy.CaptureHTTPRouteName(wantPort)))
		_, hasTable := tables[proxy.CaptureHTTPRouteName(wantPort)]
		assert.Equal(t, present, hasTable)
	}
	check(7070, true)

	// The only service with port 7070 leaves the mesh for good: its Service
	// goes, its endpoints go, and its retained clusters age out.
	c.serviceRetentionGrace = 1
	remaining := services[:len(services)-1]
	a043Project(c, remaining)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(remaining)))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(remaining)))
	check(7070, false)

	// And a new port number enters.
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	grown := append(slices.Clone(remaining), a043Service{name: "late", ns: a043NS, legacy: 6060, ports: map[uint32]registryv1.PortProtocol{6060: h}, ip: "10.0.1.9", vip: "10.96.1.9"})
	a043Project(c, grown)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(grown)))
	check(6060, true)
}

// TestAddressing_EndpointChurnDoesNotRebuildListeners: pods coming and going
// with the same declarations leave the capture listener the very same object,
// so the proxy is not sent it again (a listener update drains connections).
func TestAddressing_EndpointChurnDoesNotRebuildListeners(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	before := a043CaptureListener(t, c)

	churned := slices.Clone(services)
	for i := range churned {
		churned[i].ip = strings.Replace(churned[i].ip, "10.0.", "10.9.", 1)
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(churned)))
	assert.Same(t, before, a043CaptureListener(t, c), "endpoint churn rebuilt the capture listener")

	// Negative control: a declaration that changes does rebuild it.
	churned[0].ports = map[uint32]registryv1.PortProtocol{8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP, 5555: registryv1.PortProtocol_PORT_PROTOCOL_TCP}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(churned)))
	after := a043CaptureListener(t, c)
	assert.NotSame(t, before, after)
	assert.Contains(t, chainNames(after), "cap_tcp_tcp:httponly.demo.aether.internal_5555")
}

// TestAddressing_RulesFollowTheParentPort is leg A4: a rule whose parentRef
// names no port applies on every HTTP port of the service, on both paths; a
// rule that names a port applies on that port only.
func TestAddressing_RulesFollowTheParentPort(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	mark := func(v string) *proxy.GammaHeaderMutation {
		return &proxy.GammaHeaderMutation{AddResponse: []proxy.GammaHeaderKV{{Name: "x-mp-route", Value: v}}}
	}
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/multi": {
			{Matches: []proxy.GammaMatch{{Prefix: "/all"}}, HeaderMutation: mark("all")},
			{ParentPort: 9090, Matches: []proxy.GammaMatch{{Prefix: "/only9090"}}, HeaderMutation: mark("9090")},
		},
	})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	tables := a043Tables(t, c)

	marks := func(table *routev3.RouteConfiguration, host string) map[string]string {
		out := map[string]string{}
		for _, vh := range table.GetVirtualHosts() {
			if !slices.Contains(vh.GetDomains(), host) {
				continue
			}
			for _, r := range vh.GetRoutes() {
				for _, h := range r.GetResponseHeadersToAdd() {
					if h.GetHeader().GetKey() == "x-mp-route" {
						cluster := r.GetRoute().GetCluster()
						if cluster == "" {
							cluster = quicSelectionDefault(t, r.GetRoute())
						}
						out[h.GetHeader().GetValue()] = cluster
					}
				}
			}
		}
		return out
	}
	const mesh = "multi.demo.aether.internal"
	// Captured: each port's table, each rule routed to THAT port's cluster.
	assert.Equal(t, map[string]string{"all": mesh}, marks(tables["cap_http_8080"], mesh+":8080"))
	assert.Equal(t, map[string]string{"all": mesh + ":9090", "9090": mesh + ":9090"}, marks(tables["cap_http_9090"], mesh+":9090"))
	assert.Equal(t, map[string]string{"all": mesh + ":9090", "9090": mesh + ":9090"}, marks(tables["cap_http_9090"], "multi.demo.svc.cluster.local"))
	// Loopback, and the proxy port on the captured path (a dial to 18081):
	// the port in Host is the port, and its rules are that port's.
	for _, name := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
		table := tables[name]
		assert.Equal(t, map[string]string{"all": mesh}, marks(table, mesh+":8080"), name)
		assert.Equal(t, map[string]string{"all": mesh + ":9090", "9090": mesh + ":9090"}, marks(table, mesh+":9090"), name)
		// 18081 and the port-less name are not ports of the service: no
		// virtual host that routes claims them, so no rule applies there
		// (until chart 2.7.0 both carried the rules of the port the pods
		// declare).
		for _, host := range []string{mesh + ":18081", mesh} {
			assert.Empty(t, marks(table, host), "%s, Host %s", name, host)
			assert.NotEmpty(t, a043Resolve(t, table, host).refusal, "%s, Host %s", name, host)
		}
	}
	// Negative control: another service's virtual hosts carry none of it.
	assert.Empty(t, marks(tables["cap_http_9090"], "eighty.demo.aether.internal:9090"))
}

// TestAddressing_BackendRefPort: a backendRef's port is a port of the backend
// Service and selects that port's cluster. It was not read at all, so a route
// to port 9090 of a two-port service delivered to its other port (proposal
// 043, fact 1, row 11).
func TestAddressing_BackendRefPort(t *testing.T) {
	services := a043Fixture()
	all := deriveAllServicePorts(a043Listing(t, services, registryv1.Service_PROTOCOL_HTTP), a043Listing(t, services, registryv1.Service_PROTOCOL_TCP), nil)
	const multi, tcpfirst = "multi.demo.aether.internal", "tcpfirst.demo.aether.internal"
	userMap := map[string]map[uint32]uint32{"demo/multi": {80: 9090, 81: 0, 443: 8443}}
	cases := []struct {
		name     string
		backend  proxy.GammaBackend
		portMaps map[string]map[uint32]uint32
		want     string
		invalid  bool
	}{
		{"a declared HTTP port", proxy.GammaBackend{Service: "demo/multi", Cluster: multi, Port: 9090}, nil, multi + ":9090", false},
		{"the legacy target's port is the default cluster", proxy.GammaBackend{Service: "demo/multi", Cluster: multi, Port: 8080}, nil, multi, false},
		{"no port: the projected cluster", proxy.GammaBackend{Service: "demo/multi", Cluster: multi}, nil, multi, false},
		{"port 80 of a service that declares it", proxy.GammaBackend{Service: "demo/eighty", Cluster: "eighty.demo.aether.internal", Port: 80}, nil, "eighty.demo.aether.internal:80", false},
		// No legacy meaning: neither number is a port of the service.
		{"the mesh HTTP port is not a port of the service", proxy.GammaBackend{Service: "demo/multi", Cluster: multi, Port: 18081}, nil, multi, true},
		{"port 80 of a service that does not declare it", proxy.GammaBackend{Service: "demo/multi", Cluster: multi, Port: 80}, nil, multi, true},
		{"an undeclared port", proxy.GammaBackend{Service: "demo/multi", Cluster: multi, Port: 7777}, nil, multi, true},
		{"a raw-TCP port is not an HTTP backend", proxy.GammaBackend{Service: "demo/mixed", Cluster: "mixed.demo.aether.internal", Port: 9000}, nil, "mixed.demo.aether.internal", true},
		{"the mesh HTTP port of a service whose declared port is raw TCP", proxy.GammaBackend{Service: "demo/tcpfirst", Cluster: tcpfirst, Port: 18081}, nil, tcpfirst, true},
		// Not the projected (bare) cluster: that is the cluster of the port
		// the backend's pods declare, and answered a route to 9090 from it.
		{"a service this node knows no port of is named by the port the route names", proxy.GammaBackend{Service: "demo/cold", Cluster: "cold.demo.aether.internal", Port: 9090}, nil, "cold.demo.aether.internal:9090", false},
		{"a user Service this node knows no pod of: the pod port its Service maps the port to", proxy.GammaBackend{Service: "demo/cold", Cluster: "cold.demo.aether.internal", Port: 80}, map[string]map[uint32]uint32{"demo/cold": {80: 8080}}, "cold.demo.aether.internal:8080", false},
		{"a user Service this node knows no pod of: a named targetPort keeps the projected cluster", proxy.GammaBackend{Service: "demo/cold", Cluster: "cold.demo.aether.internal", Port: 80}, map[string]map[uint32]uint32{"demo/cold": {80: 0}}, "cold.demo.aether.internal", false},
		{"a user Service: its port resolves through targetPort", proxy.GammaBackend{Service: "demo/multi", Cluster: multi, Port: 80}, userMap, multi + ":9090", false},
		{"a user Service: a named targetPort cannot be resolved or refuted and keeps the projected cluster", proxy.GammaBackend{Service: "demo/multi", Cluster: multi, Port: 81}, userMap, multi, false},
		{"a user Service: a port it does not have, though the pods declare that number", proxy.GammaBackend{Service: "demo/multi", Cluster: multi, Port: 8080}, userMap, multi, true},
		{"a user Service: a targetPort the pods do not declare", proxy.GammaBackend{Service: "demo/multi", Cluster: multi, Port: 443}, userMap, multi, true},
		{"a user Service this node knows no pod of: a port it does not have is invalid already", proxy.GammaBackend{Service: "demo/cold", Cluster: "cold.demo.aether.internal", Port: 9090}, map[string]map[uint32]uint32{"demo/cold": {80: 8080}}, "cold.demo.aether.internal", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cluster, invalid := backendCluster(tc.backend, tc.portMaps, all, a043Mesh)
			assert.Equal(t, tc.invalid, invalid, "invalid")
			if !invalid {
				assert.Equal(t, tc.want, cluster)
			}
		})
	}

	// resolveRules never writes into the shared rules.
	shared := map[string][]proxy.GammaRoute{"demo/httponly": {{Backends: []proxy.GammaBackend{{Service: "demo/multi", Cluster: multi, Port: 9090, Weight: 1}}}}}
	got := resolveRules(ruleInputs{routes: shared}, all, a043Mesh, rulePlace{svc: "demo/httponly", pod: 8080})
	require.Len(t, got, 1)
	assert.Equal(t, multi+":9090", got[0].Backends[0].Cluster)
	assert.Equal(t, multi, shared["demo/httponly"][0].Backends[0].Cluster, "the shared rule was mutated")
}

func a043Listing(t *testing.T, services []a043Service, protocol registryv1.Service_Protocol) map[string][]*registryv1.ServiceEndpoint {
	t.Helper()
	out, err := a043Registry(services).listAllEndpointsFunc(context.Background(), protocol)
	require.NoError(t, err)
	return out
}

// TestAddressing_UserServicePortMap is the conformance shape: a user Service
// "echo" owns the name, maps port 80 to pod port 8080, and a route sends it to
// "echo-v1" port 80, itself a user Service mapping 80 to 8080. The client
// dials port 80.
func TestAddressing_UserServicePortMap(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	services := []a043Service{
		{name: "echo-v1", ns: a043NS, legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 3000: h}, ip: "10.0.3.1", vip: "10.96.3.1"},
	}
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	// No generated Service: the user's Services own both names.
	declareDeps(c, "demo/echo-v1")
	c.SetServicePortMaps(map[string]map[uint32]uint32{
		"demo/echo":    {80: 8080, 8080: 8080},
		"demo/echo-v1": {80: 8080, 3000: 3000, 9: 0},
	})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/echo": {{Backends: []proxy.GammaBackend{{Service: "demo/echo-v1", Cluster: "echo-v1.demo.aether.internal", Port: 80, Weight: 1}}}},
		"demo/echo-v1": {{
			ParentPort: 3000, Matches: []proxy.GammaMatch{{Prefix: "/x"}},
			Backends: []proxy.GammaBackend{{Service: "demo/echo-v1", Cluster: "echo-v1.demo.aether.internal", Port: 3000, Weight: 1}},
		}},
	})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
	tables := a043Tables(t, c)

	// The Service ports are the mesh's port numbers, not the pods'.
	for _, p := range []uint32{80, 3000, 8080} {
		require.Contains(t, tables, proxy.CaptureHTTPRouteName(p))
	}
	assert.Contains(t, tables, "cap_http_9", "a Service port with a named targetPort still has its table")

	// http://echo/ dials 80: the route's backend is echo-v1's SERVICE port 80,
	// which is pod port 8080, its default cluster.
	got := a043Resolve(t, tables["cap_http_80"], "echo")
	assert.Equal(t, "echo-v1.demo.aether.internal", got.cluster)
	got = a043Resolve(t, tables["cap_http_80"], "echo.demo.svc.cluster.local:80")
	assert.Equal(t, "echo-v1.demo.aether.internal", got.cluster)
	// echo-v1 dialled on its Service port 80 is its pod port 8080.
	got = a043Resolve(t, tables["cap_http_80"], "echo-v1.demo")
	assert.Equal(t, "echo-v1.demo.aether.internal", got.cluster)
	// Its Service port 3000 is pod port 3000, with the rule scoped to it.
	vh := findVHost(tables["cap_http_3000"].GetVirtualHosts(), "echo-v1.demo:3000")
	require.NotNil(t, vh)
	require.Len(t, vh.GetRoutes(), 2, "the rule of parent port 3000, and the default route")
	assert.Equal(t, "echo-v1.demo.aether.internal:3000", vh.GetRoutes()[0].GetRoute().GetCluster())
	assert.Equal(t, "echo-v1.demo.aether.internal:3000", vh.GetRoutes()[1].GetRoute().GetCluster())
	// And that rule is on no other port's virtual host.
	vh = findVHost(tables["cap_http_80"].GetVirtualHosts(), "echo-v1.demo")
	require.NotNil(t, vh)
	assert.Len(t, vh.GetRoutes(), 1)
	// Pod port 8080 is not a port of the Service echo-v1: dialling it is
	// refused, not routed by the pod's own numbering.
	got = a043Resolve(t, tables["cap_http_8080"], "echo-v1.demo:8080")
	assert.Equal(t, proxy.RefusalPortNotDeclared, got.refusal)
}

// TestServicePorts_Derivation holds the per-port model: the class of a port is
// the endpoints' own declaration, decided by vote when they disagree, in any
// order of rows and listings.
func TestServicePorts_Derivation(t *testing.T) {
	h, tcp, udp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP, registryv1.PortProtocol_PORT_PROTOCOL_UDP
	ep := func(ip string, port uint32, pp map[uint32]registryv1.PortProtocol, ports ...uint32) *registryv1.ServiceEndpoint {
		return &registryv1.ServiceEndpoint{Ip: ip, ClusterName: "c1", Port: port, Ports: ports, PortProtocols: pp}
	}
	lst := func(p registryv1.Service_Protocol, eps ...*registryv1.ServiceEndpoint) listedEndpoints {
		return listedEndpoints{protocol: p, endpoints: eps}
	}
	const sHTTP, sTCP, sUDP = registryv1.Service_PROTOCOL_HTTP, registryv1.Service_PROTOCOL_TCP, registryv1.Service_PROTOCOL_UDP

	t.Run("nothing known", func(t *testing.T) {
		got := deriveServicePorts()
		assert.False(t, got.known())
		assert.Zero(t, got.legacyHTTPPort())
		assert.Zero(t, got.legacyTCPPort())
		assert.Equal(t, "none", got.describe())
	})

	t.Run("a mixed pod is one endpoint, whichever listing is read first (#1672)", func(t *testing.T) {
		a := ep("10.0.0.1", 9000, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: h}, 8080, 9000)
		for _, listings := range [][]listedEndpoints{
			{lst(sHTTP, a), lst(sTCP, a)},
			{lst(sTCP, a), lst(sHTTP, a)},
		} {
			got := deriveServicePorts(listings...)
			assert.Equal(t, []uint32{8080}, got.http)
			assert.Equal(t, []uint32{9000}, got.tcp)
			assert.Equal(t, uint32(9000), got.legacyTCPPort(), "the declared port is raw TCP: listed under HTTP says the pod serves an HTTP port, not that this one is HTTP")
			assert.Zero(t, got.legacyHTTPPort())
			assert.Empty(t, got.conflicts, "one pod in two listings is not a disagreement")
			assert.Equal(t, "8080 (http), 9000 (tcp)", got.describe())
		}
	})

	t.Run("a port is served only where its class's listing holds the pod", func(t *testing.T) {
		// A registrar of chart 2.6 on the kubernetes backend lists a mixed
		// pod under its pod-level protocol alone. The raw-TCP port has no
		// cluster then (the cluster entries are built per listing), so it is
		// not a port of the service on this node: no chain, no route.
		a := ep("10.0.0.1", 9000, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: h}, 8080, 9000)
		got := deriveServicePorts(lst(sHTTP, a))
		assert.Equal(t, []uint32{8080}, got.http)
		assert.Empty(t, got.tcp)
		assert.Zero(t, got.legacyTCPPort(), "the declared port's class has no listing here")
		// The row still says what the declared port IS. A listing that has
		// not caught up (the pod's HTTP row is written before its raw-TCP
		// one) must not make the service look like one with no declared
		// port: that built a default HTTP cluster on the raw-TCP port, which
		// then held the port-less authority and answered it 502.
		assert.Equal(t, uint32(9000), got.legacyPort)
		assert.Equal(t, tcp, got.legacyClass)
		assert.Zero(t, got.legacyHTTPPort())
		got = deriveServicePorts(lst(sTCP, a))
		assert.Equal(t, []uint32{9000}, got.tcp)
		assert.Empty(t, got.http)
	})

	t.Run("a row written before proposal 037 takes its listing's class", func(t *testing.T) {
		got := deriveServicePorts(lst(sTCP, ep("10.0.0.1", 9000, nil)))
		assert.Equal(t, []uint32{9000}, got.tcp)
		assert.Equal(t, uint32(9000), got.legacyTCPPort())
		got = deriveServicePorts(lst(sUDP, ep("10.0.0.1", 5353, nil)))
		assert.Equal(t, []uint32{5353}, got.udp)
		got = deriveServicePorts(lst(registryv1.Service_PROTOCOL_UNSPECIFIED, ep("10.0.0.1", 8080, nil, 8080, 9090)))
		assert.Equal(t, []uint32{8080, 9090}, got.http)
		assert.Equal(t, uint32(8080), got.legacyHTTPPort())
	})

	t.Run("a port the map does not name takes the listing's class; an unusable port is dropped", func(t *testing.T) {
		row := ep("10.0.0.1", 9000, map[uint32]registryv1.PortProtocol{8080: h, 70000: h, 0: h}, 9000, 8080)
		got := deriveServicePorts(lst(sTCP, row))
		assert.Empty(t, got.http, "8080 is named http, and no HTTP listing holds the pod")
		assert.Equal(t, []uint32{9000}, got.tcp, "9000 is not named: the listing's class; 0 and 70000 are no ports")
	})

	t.Run("a class conflict: most endpoints win, in any order", func(t *testing.T) {
		a := ep("10.0.0.1", 8080, map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp})
		b := ep("10.0.0.2", 8080, map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp})
		x := ep("10.0.0.3", 8080, map[uint32]registryv1.PortProtocol{8080: h, 9000: h})
		for _, order := range [][]*registryv1.ServiceEndpoint{{a, b, x}, {x, a, b}, {b, x, a}} {
			got := deriveServicePorts(lst(sHTTP, order...), lst(sTCP, a, b))
			assert.Equal(t, []uint32{8080}, got.http)
			assert.Equal(t, []uint32{9000}, got.tcp)
			assert.Equal(t, []string{"9000: 1 http, 2 tcp"}, got.conflicts)
		}
	})

	t.Run("a tied class conflict: http, then tcp, then udp", func(t *testing.T) {
		a := ep("10.0.0.1", 8080, map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp})
		x := ep("10.0.0.3", 8080, map[uint32]registryv1.PortProtocol{8080: h, 9000: h})
		u := ep("10.0.0.4", 8080, map[uint32]registryv1.PortProtocol{8080: h, 9000: udp})
		for _, order := range [][]*registryv1.ServiceEndpoint{{a, x}, {x, a}} {
			assert.Equal(t, []uint32{8080, 9000}, deriveServicePorts(lst(sHTTP, order...)).http)
		}
		for _, order := range [][]*registryv1.ServiceEndpoint{{a, u}, {u, a}} {
			got := deriveServicePorts(lst(sHTTP, order...), lst(sTCP, a), lst(sUDP, u))
			assert.Equal(t, []uint32{9000}, got.tcp)
			assert.Empty(t, got.udp)
		}
	})

	t.Run("the legacy target when pods disagree: most endpoints, then http, then the lower port (decision 4)", func(t *testing.T) {
		t1 := ep("10.0.0.1", 9000, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: h})
		t2 := ep("10.0.0.2", 9000, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: h})
		h1 := ep("10.0.0.3", 8080, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: h})
		for _, order := range [][]*registryv1.ServiceEndpoint{{t1, t2, h1}, {h1, t1, t2}, {t2, h1, t1}} {
			port, class := legacyTarget(lst(sHTTP, order...), lst(sTCP, order...))
			assert.Equal(t, uint32(9000), port)
			assert.Equal(t, tcp, class)
		}
		for _, order := range [][]*registryv1.ServiceEndpoint{{t1, h1}, {h1, t1}} {
			port, class := legacyTarget(lst(sHTTP, order...))
			assert.Equal(t, uint32(8080), port, "tied: http first")
			assert.Equal(t, h, class)
		}
		lo, hi := ep("10.0.0.5", 8080, nil), ep("10.0.0.6", 9090, nil)
		for _, order := range [][]*registryv1.ServiceEndpoint{{lo, hi}, {hi, lo}} {
			port, _ := legacyTarget(lst(sHTTP, order...))
			assert.Equal(t, uint32(8080), port, "tied on class: the lower port")
		}
	})

	t.Run("the same address in two clusters is two endpoints", func(t *testing.T) {
		a := ep("10.0.0.1", 9000, map[uint32]registryv1.PortProtocol{9000: tcp})
		b := ep("10.0.0.1", 9000, map[uint32]registryv1.PortProtocol{9000: tcp})
		b.ClusterName = "c2"
		x := ep("10.0.0.9", 8080, map[uint32]registryv1.PortProtocol{8080: h})
		port, class := legacyTarget(lst(sHTTP, x), lst(sTCP, a, b))
		assert.Equal(t, uint32(9000), port)
		assert.Equal(t, tcp, class)
	})

	t.Run("equal ignores what does not change a chain", func(t *testing.T) {
		a := deriveServicePorts(lst(sHTTP, ep("10.0.0.1", 8080, map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp})))
		b := deriveServicePorts(lst(sHTTP, ep("10.9.9.9", 8080, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: h})))
		assert.True(t, a.equal(b))
		c := deriveServicePorts(lst(sHTTP, ep("10.0.0.1", 8080, map[uint32]registryv1.PortProtocol{8080: h, 9000: h})))
		assert.False(t, a.equal(c), "a class change is a change")
		d := deriveServicePorts(lst(sHTTP, ep("10.0.0.1", 9000, map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp})))
		assert.False(t, a.equal(d), "a legacy target change is a change")
	})
}

// TestServicePorts_RetainedWithTheClusters: a service that vanishes from the
// listing keeps its derived ports for as long as it keeps its retained
// clusters, so a single-replica restart does not take a port number out of
// the mesh and back; and loses them when the clusters are pruned.
func TestServicePorts_RetainedWithTheClusters(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	before := a043CaptureListener(t, c)

	// "nine", the only HTTP service on port 9000, loses its only pod. Its
	// Service is still there (the registrar prunes on its own schedule).
	var without []a043Service
	for _, s := range services {
		if s.name != "nine" {
			without = append(without, s)
		}
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(without)))
	assert.Same(t, before, a043CaptureListener(t, c), "a transiently empty service rebuilt the capture listener")
	got := a043Resolve(t, a043Tables(t, c)["cap_http_9000"], "nine.demo.aether.internal:9000")
	assert.Equal(t, "nine.demo.aether.internal", got.cluster, "it keeps routing to its retained (empty) cluster: a retriable 503, not a refusal")

	// Past the grace the clusters are pruned and the ports with them.
	c.serviceRetentionGrace = 1
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(without)))
	c.clusterMu.RLock()
	_, kept := c.servicePorts["demo/nine"]
	c.clusterMu.RUnlock()
	assert.False(t, kept)
}

func mustRegexp(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	require.NoError(t, err, "pattern %q", pattern)
	return re
}

// quicSelectionDefault returns the h2 cluster of a route action rewritten by
// the per-source HTTP/3 selection: the cluster a request with no arm takes.
func quicSelectionDefault(t *testing.T, ra *routev3.RouteAction) string {
	t.Helper()
	_, noMatch, ok := proxy.QUICSelectionArms(&routev3.Route{Action: &routev3.Route_Route{Route: ra}})
	require.True(t, ok, "a route action with neither a cluster nor a selection plugin: %v", ra)
	return noMatch
}

// TestAddressing_RoutePortsReachTheListener: a port number that enters the
// mesh through a route alone (a parentRef port, a user Service's port map)
// gets its chain on the next snapshot. Nothing else rebuilds the listeners
// for it: the routes did not change, and neither did the Services or the
// registry.
func TestAddressing_RoutePortsReachTheListener(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"demo/route-only": {{Backends: []proxy.GammaBackend{{Service: "demo/multi", Cluster: "multi.demo.aether.internal", Weight: 1}}}},
	})
	require.NoError(t, c.generateSnapshot(ctx))
	require.NotContains(t, chainNames(a043CaptureListener(t, c)), "cap_http_6543")

	c.SetRouteTargetPorts(map[string][]uint32{"demo/route-only": {6543}})
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Contains(t, chainNames(a043CaptureListener(t, c)), "cap_http_6543")
	assert.Contains(t, a043Tables(t, c), "cap_http_6543")

	c.SetRouteTargetPorts(nil)
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/route-only": {6544: 8080}})
	require.NoError(t, c.generateSnapshot(ctx))
	names := chainNames(a043CaptureListener(t, c))
	assert.Contains(t, names, "cap_http_6544")
	assert.NotContains(t, names, "cap_http_6543", "a port number that left the mesh loses its chain")
}

// TestAddressing_ScaleWorkedExample builds the worked example of proposal
// 043's "Scale" section and holds the counts it states: 200 services with an
// address, three HTTP port numbers (80, 8080, 9090), 20 (service, raw-TCP
// port) pairs, 50 pods on the node.
//
// Two shapes of the 20 pairs are measured, because they do not cost the same:
// a raw-TCP port beside an HTTP legacy target is one chain; a raw-TCP LEGACY
// target is the chain on its own port and the one on the TCP mesh port.
func TestAddressing_ScaleWorkedExample(t *testing.T) {
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	build := func(tcpOnly bool) (*SnapshotCache, []a043Service) {
		var services []a043Service
		for i := 0; i < 200; i++ {
			s := a043Service{
				name: fmt.Sprintf("svc-%03d", i), ns: a043NS, legacy: 8080,
				ports: map[uint32]registryv1.PortProtocol{8080: h},
				ip:    fmt.Sprintf("10.0.%d.%d", 10+i/200, 1+i%200), vip: fmt.Sprintf("10.96.%d.%d", 10+i/200, 1+i%200),
			}
			switch {
			case i < 20 && tcpOnly:
				s.legacy, s.ports = 5432, map[uint32]registryv1.PortProtocol{5432: tcp}
			case i < 20:
				s.ports[5432] = tcp
			case i%3 == 0:
				s.ports[9090] = h
			case i%7 == 0:
				s.ports[80] = h
			}
			services = append(services, s)
		}
		ctx := context.Background()
		c := newTestCache("node-1")
		c.SetCaptureEnabled(true)
		c.SetCaptureRedirectAll(true)
		for p := 0; p < 50; p++ {
			require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
				Name: fmt.Sprintf("client-%d", p), Namespace: a043NS, ServiceAccount: "client", NetworkNamespace: fmt.Sprintf("/var/run/netns/cni-%d", p),
			}, a043Mesh))
		}
		require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
		a043Project(c, services)
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(services)))
		return c, services
	}

	for _, shape := range []struct {
		name       string
		tcpOnly    bool
		wantChains int
	}{
		// 4 HTTP port numbers (80, 8080, 9090 and the legacy 18081) + other + 20.
		{"20 raw-TCP ports beside an HTTP legacy target", false, 4 + 1 + 20},
		// Per raw-TCP-only service: its port and :18082.
		{"20 raw-TCP legacy targets with no HTTP port", true, 4 + 1 + 20*2},
	} {
		t.Run(shape.name, func(t *testing.T) {
			c, _ := build(shape.tcpOnly)
			snap, err := c.GetSnapshot("node-1")
			require.NoError(t, err)

			captureListeners, chains, hcms, listenerBytes := 0, 0, 0, 0
			for name, res := range snap.GetResources(resourcev3.ListenerType) {
				if !strings.HasPrefix(name, "capture_") {
					continue
				}
				l := res.(*listenerv3.Listener)
				captureListeners++
				listenerBytes += proto.Size(l)
				assert.Len(t, l.GetFilterChains(), shape.wantChains)
				chains += len(l.GetFilterChains())
				for _, fc := range l.GetFilterChains() {
					if strings.HasPrefix(fc.GetName(), "cap_http_") {
						hcms++
					}
				}
				assert.NotNil(t, l.GetDefaultFilterChain(), "plus the passthrough default chain")
			}
			require.Equal(t, 50, captureListeners)
			assert.Equal(t, 50*5, hcms, "H+1 HCM configs per pod, H counting the legacy mesh HTTP port")

			tables, vhosts, tableBytes := 0, 0, 0
			for name, res := range snap.GetResources(resourcev3.RouteType) {
				rc := res.(*routev3.RouteConfiguration)
				tables++
				if name != proxy.OutboundHTTPRouteName {
					vhosts += len(rc.GetVirtualHosts())
					tableBytes += proto.Size(rc)
				}
			}
			assert.Equal(t, 6, tables, "cap_http_80, _8080, _9090, _18081, cap_http_other and out_http")
			t.Logf("%s: chains per pod listener=%d (+1 default), per node=%d; HCM configs per pod=5, per node=%d; route tables per node=%d; capture virtual hosts=%d; capture listeners=%d KiB total (%d KiB each); capture route tables=%d KiB total; clusters=%d",
				shape.name, shape.wantChains, chains, hcms, tables, vhosts, listenerBytes/1024, listenerBytes/1024/50, tableBytes/1024, len(snap.GetResources(resourcev3.ClusterType)))
		})
	}
}

// TestAddressing_TablesAreKeptWhileTheirInputsAre: the capture route tables
// are the same objects from one snapshot to the next while nothing they are
// built from changed (so the proxy is not sent them and the agent does not
// hash them again), and new ones as soon as any input does. A table kept
// across a change would route by yesterday's ports.
func TestAddressing_TablesAreKeptWhileTheirInputsAre(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	table := func() *routev3.RouteConfiguration { return a043Tables(t, c)["cap_http_8080"] }
	last := table()
	kept := func(what string) {
		t.Helper()
		now := table()
		assert.Samef(t, last, now, "%s rebuilt the tables", what)
		last = now
	}
	rebuilt := func(what string) {
		t.Helper()
		now := table()
		assert.NotSamef(t, last, now, "%s left a stale table", what)
		last = now
	}

	require.NoError(t, c.generateSnapshot(ctx))
	kept("a snapshot build with no change")

	churned := slices.Clone(services)
	for i := range churned {
		churned[i].ip = strings.Replace(churned[i].ip, "10.0.", "10.9.", 1)
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(churned)))
	kept("endpoint churn")

	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/multi": {{Matches: []proxy.GammaMatch{{Prefix: "/x"}}}}})
	require.NoError(t, c.generateSnapshot(ctx))
	rebuilt("a route")
	require.Len(t, findVHost(last.GetVirtualHosts(), "multi.demo.aether.internal:8080").GetRoutes(), 2)

	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/elsewhere": {80: 8080}})
	require.NoError(t, c.generateSnapshot(ctx))
	rebuilt("a port map")

	c.SetRouteTargetPorts(map[string][]uint32{"demo/multi": {8080}})
	require.NoError(t, c.generateSnapshot(ctx))
	rebuilt("a route target port")

	renamed := slices.Clone(churned)
	renamed[0].name = "httponly2"
	a043Project(c, renamed)
	require.NoError(t, c.generateSnapshot(ctx))
	rebuilt("a mesh Service")

	// A port's class changes: 8080 of "mixed" becomes raw TCP.
	retyped := slices.Clone(renamed)
	retyped[3].ports = map[uint32]registryv1.PortProtocol{8080: registryv1.PortProtocol_PORT_PROTOCOL_TCP, 9000: registryv1.PortProtocol_PORT_PROTOCOL_TCP}
	retyped[3].legacy = 9000
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(retyped)))
	rebuilt("a port's class")
	// The service now serves no HTTP port at all, and is told so.
	got := a043Resolve(t, last, "mixed.demo.aether.internal:8080")
	assert.Equal(t, proxy.RefusalNoHTTPPort, got.refusal)

	// The Services' names alone: a second namespace takes a short name, which
	// then has an arm for each. Nothing else changes (no address, no
	// dependency), so only the authorities' generation says so.
	short := findVHost(last.GetVirtualHosts(), "eighty")
	require.NotNil(t, short)
	require.Equal(t, []string{"demo"}, matcherNamespaces(short))
	c.captureMu.RLock()
	authorities := maps.Clone(c.captureAuthorities)
	c.captureMu.RUnlock()
	authorities["elsewhere/eighty"] = "eighty.elsewhere.svc.cluster.local"
	c.SetCaptureAuthorities(authorities)
	require.NoError(t, c.generateSnapshot(ctx))
	rebuilt("a mesh Service name")
	short = findVHost(last.GetVirtualHosts(), "eighty")
	require.NotNil(t, short, "a short name two namespaces share stays a name: each caller gets its own namespace's")
	assert.Contains(t, matcherNamespaces(short), "demo", "the first namespace's callers are not affected by the second")

	// The set of tables asked for is part of what was built.
	names := func(rs []types.Resource) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.(*routev3.RouteConfiguration).GetName())
		}
		return out
	}
	assert.Equal(t, []string{"cap_http_80", "cap_http_other"}, names(c.captureRouteTables([]uint32{80})))
	assert.Equal(t, []string{"cap_http_80", "cap_http_1234", "cap_http_other"}, names(c.captureRouteTables([]uint32{80, 1234})))

	// The QUIC arms have no generation: they are compared.
	c.captureTables.arms = map[string]map[string]string{"demo/multi": {"spiffe://x": "quic:stale"}}
	require.NoError(t, c.generateSnapshot(ctx))
	rebuilt("a change of the QUIC arms")
}

// a043DisagreeingRegistry wraps the fixture's registry with one more service,
// "split", whose pods disagree about everything: pod A declares 8080 as its
// HTTP port; pods B and C declare 9000 raw TCP as theirs and 8080 raw TCP too.
// It is the state of a rollout that retypes a port, and the state in which
// two builders that each decided from their own listing would disagree.
func a043DisagreeingRegistry(services []a043Service) *mockRegistry {
	base := a043Registry(services)
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	row := func(ip string, port uint32, pp map[uint32]registryv1.PortProtocol) *registryv1.ServiceEndpoint {
		ep := makeEndpoint(ip, "cluster-1", "node-2", port)
		ep.KubernetesMetadata.Namespace = a043NS
		ep.PortProtocols = pp
		for p := range pp {
			ep.Ports = append(ep.Ports, p)
		}
		slices.Sort(ep.Ports)
		return ep
	}
	a := row("10.0.8.1", 8080, map[uint32]registryv1.PortProtocol{8080: h})
	b := row("10.0.8.2", 9000, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: tcp})
	c := row("10.0.8.3", 9000, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: tcp})
	return &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			out, err := base.listAllEndpointsFunc(ctx, protocol)
			switch protocol {
			case registryv1.Service_PROTOCOL_HTTP:
				out["demo/split"] = []*registryv1.ServiceEndpoint{a}
			case registryv1.Service_PROTOCOL_TCP:
				out["demo/split"] = []*registryv1.ServiceEndpoint{b, c}
			}
			return out, err
		},
	}
}

// TestAddressing_EveryChainAndRouteHasItsCluster: every raw-TCP chain of the
// capture listener forwards to a cluster the same snapshot publishes, and so
// does every route of every table that names a cluster. A tcp_proxy chain
// without its cluster accepts the connection and kills it, with no NACK and
// nothing in a log (tcp_proxy has no on-demand path); a route to a cluster
// that does not exist is a 503 after the on-demand timeout.
//
// Held over the fixture AND over a service whose pods disagree about their
// ports, which is where a chain decided from one listing and a cluster built
// from another would part.
func TestAddressing_EveryChainAndRouteHasItsCluster(t *testing.T) {
	ctx := context.Background()
	services := a043Fixture()
	c := a043Cache(t, services)
	withSplit := append(slices.Clone(services), a043Service{name: "split", ns: a043NS, vip: "10.96.8.1"})
	a043Project(c, withSplit)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043DisagreeingRegistry(services)))

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)
	endpoints := snap.GetResources(resourcev3.EndpointType)

	// The disagreement is decided one way everywhere: most pods declare 9000
	// raw TCP as their port and 8080 as raw TCP.
	c.clusterMu.RLock()
	split := c.servicePorts["demo/split"]
	c.clusterMu.RUnlock()
	require.Equal(t, uint32(9000), split.legacyTCPPort())
	require.Equal(t, []uint32{8080, 9000}, split.tcp)
	require.Empty(t, split.http)
	require.NotEmpty(t, split.conflicts)
	// So it has no default h2 cluster, although one of its pods, read alone,
	// declares an HTTP port as its own: every builder reads the one answer.
	assert.NotContains(t, clusters, "split.demo.aether.internal")
	// And no h2 cluster for 8080 from the one pod that declares it HTTP: no
	// route leads to it (the service's 8080 is raw TCP, and its HTTP
	// authority is refused), and a cluster that exists can be asked for by
	// name. It was: a request for "<fqdn>:8080" parked while the node knew
	// nothing of the service was delivered to that pod when the cluster
	// arrived, where every later request is refused.
	assert.NotContains(t, clusters, "split.demo.aether.internal:8080")
	assert.Contains(t, clusters, "tcp:split.demo.aether.internal:8080")

	tcpChains := 0
	l := a043CaptureListener(t, c)
	for _, fc := range l.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			tp := &tcp_proxyv3.TcpProxy{}
			if f.GetTypedConfig().UnmarshalTo(tp) != nil {
				continue
			}
			tcpChains++
			name := tp.GetCluster()
			cl, ok := clusters[name].(*clusterv3.Cluster)
			if !assert.Truef(t, ok, "chain %s forwards to cluster %q, which the snapshot does not publish", fc.GetName(), name) {
				continue
			}
			eds := cl.GetEdsClusterConfig().GetServiceName()
			assert.Containsf(t, endpoints, eds, "chain %s: cluster %s subscribes to %q, which nothing publishes", fc.GetName(), name, eds)
		}
	}
	require.Greater(t, tcpChains, 6)
	assert.Contains(t, chainNames(l), "cap_tcp_tcp:split.demo.aether.internal_8080")
	assert.Contains(t, chainNames(l), "cap_tcp_tcp:split.demo.aether.internal_9000")

	routed := 0
	for name, table := range a043Tables(t, c) {
		for _, vh := range table.GetVirtualHosts() {
			if vh.GetName() == "on_demand_catch_all" {
				continue
			}
			for _, r := range vh.GetRoutes() {
				ra := r.GetRoute()
				if ra == nil {
					continue
				}
				cluster := ra.GetCluster()
				if cluster == "" {
					cluster = quicSelectionDefault(t, ra)
				}
				routed++
				assert.Containsf(t, clusters, cluster, "table %s, virtual host %s routes to a cluster the snapshot does not publish", name, vh.GetName())
			}
		}
	}
	require.Greater(t, routed, 20, "the walk must have seen the tables' routes")

	// No resource name is published twice: the proxy keeps one, and which
	// one changes from build to build.
	for _, typ := range []resourcev3.Type{resourcev3.ClusterType, resourcev3.EndpointType, resourcev3.RouteType, resourcev3.ListenerType} {
		assert.Empty(t, duplicateDomains(snap.GetResources(typ)), "%s", typ)
	}
}

// duplicateDomains returns the domains claimed by more than one virtual host
// of a route table, for every route table among resources.
func duplicateDomains(resources map[string]types.Resource) []string {
	var dups []string
	for name, res := range resources {
		rc, ok := res.(*routev3.RouteConfiguration)
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, vh := range rc.GetVirtualHosts() {
			for _, d := range vh.GetDomains() {
				if seen[d] {
					dups = append(dups, name+": "+d)
				}
				seen[d] = true
			}
		}
	}
	return dups
}

// TestAddressing_ReuseFollowsTheServicesPorts: a service's HTTP entries are
// rebuilt when its legacy target changes although the rows of its HTTP listing
// did not. The legacy target is decided over every listing, so a pod that is
// only in the raw-TCP listing can move it; entries reused on the HTTP rows
// alone (#1115) would keep a default cluster the route tables no longer lead
// to, and lack the per-port one they do.
func TestAddressing_ReuseFollowsTheServicesPorts(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	row := func(ip string, port uint32, pp map[uint32]registryv1.PortProtocol) *registryv1.ServiceEndpoint {
		ep := makeEndpoint(ip, "cluster-1", "node-2", port)
		ep.KubernetesMetadata.Namespace = a043NS
		ep.PortProtocols = pp
		return ep
	}
	a := row("10.0.9.1", 8080, map[uint32]registryv1.PortProtocol{8080: h})
	b := row("10.0.9.2", 9000, map[uint32]registryv1.PortProtocol{9000: tcp, 8080: h})
	c3 := row("10.0.9.3", 9000, map[uint32]registryv1.PortProtocol{9000: tcp})
	tcpRows := []*registryv1.ServiceEndpoint{b}
	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			switch protocol {
			case registryv1.Service_PROTOCOL_HTTP:
				return map[string][]*registryv1.ServiceEndpoint{"demo/moving": {a, b}}, nil
			case registryv1.Service_PROTOCOL_TCP:
				return map[string][]*registryv1.ServiceEndpoint{"demo/moving": tcpRows}, nil
			}
			return nil, nil
		},
	}
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	a043Project(c, []a043Service{{name: "moving", ns: a043NS, vip: "10.96.9.1"}})
	clusters := func() map[string]types.Resource {
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		snap, err := c.GetSnapshot("node-1")
		require.NoError(t, err)
		return snap.GetResources(resourcev3.ClusterType)
	}
	const mesh = "moving.demo.aether.internal"

	// One pod declares 8080 (HTTP), one 9000 (raw TCP): a tie, which goes to
	// HTTP. The service has a default cluster.
	first := clusters()
	require.Contains(t, first, mesh)

	// A second raw-TCP pod, which serves no HTTP port and is therefore not in
	// the HTTP listing: the rows the HTTP entries were built from are the
	// same, and the service's declared port is now 9000, raw TCP.
	tcpRows = []*registryv1.ServiceEndpoint{b, c3}
	second := clusters()
	pc, ok := second[mesh+":8080"].(*clusterv3.Cluster)
	require.True(t, ok, "port 8080 is now reached by its per-port cluster")
	assert.Equal(t, mesh+":8080", pc.GetEdsClusterConfig().GetServiceName(), "a real per-port cluster with a membership of its own, not the alias of a default cluster that is gone")
	for _, table := range []string{"cap_http_8080", proxy.OutboundHTTPRouteName} {
		got := a043Resolve(t, a043Tables(t, c)[table], mesh+":8080")
		assert.Equal(t, mesh+":8080", got.cluster, table)
	}
	// The old default entry is retained, empty, for the grace period, like
	// any entry that left the listing. It must not keep claiming
	// "<fqdn>:8080" beside the new entry: one domain in two virtual hosts and
	// the proxy rejects the whole loopback table (a043Resolve fails on a
	// duplicate domain).
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	assert.Empty(t, duplicateDomains(snap.GetResources(resourcev3.RouteType)))
	// Nor may it keep an authority that routes: the port-less name and
	// "<fqdn>:18081" are refused where the Host decides, whatever is
	// retained. (Neither is a port of the service; the retained default
	// cluster is the cluster of a port, 8080, that has its own now.)
	for _, table := range []string{"cap_http_18081", proxy.OutboundHTTPRouteName} {
		got := a043Resolve(t, a043Tables(t, c)[table], mesh)
		assert.Equal(t, proxy.RefusalPortRequired, got.refusal, table)
		got = a043Resolve(t, a043Tables(t, c)[table], mesh+":18081")
		assert.Equal(t, proxy.RefusalPortNotDeclared, got.refusal, table)
	}

	// Past the grace the default cluster is gone.
	c.serviceRetentionGrace = 1
	third := clusters()
	assert.NotContains(t, third, mesh, "the default cluster outlived the legacy target it was the cluster of")
	assert.Contains(t, third, mesh+":8080")
}
