package envoy_validate

import (
	"fmt"

	"aethermesh.dev/agent/internal/xds/proxy"
	meshconst "aethermesh.dev/common/constants/mesh"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
)

// The per-port capture listener of proposal 043, in front of the real pinned
// proxy: one HTTP chain and one route table per HTTP port number, on a listener
// that also carries raw-TCP chains for the same port numbers.
//
// The fixture is the smallest mesh that has every shape the layout exists for:
//
//	pp-multi     8080, 9090 HTTP                 shares 8080 and 9090 ...
//	pp-eighty    80, 8080, 9090 HTTP             ... with this one
//	pp-mixed     8080 HTTP, 9000 raw TCP         the mixed shape
//	pp-tcpfirst  9000 raw TCP (declared), 8080 HTTP   the other mixed shape (#1672)
//	pp-nine      9000 HTTP                       HTTP on a number two others declare raw TCP
//	pp-tcponly   6379 raw TCP                    a raw-TCP legacy target: its own port and :18082, nothing else
//
// What a real Envoy has to accept here and nowhere else: an HTTP chain and
// several "/32 + destination_port" chains on one port number; a route table
// whose virtual hosts claim "<name>:*" beside "<name>" and "<name>:<port>";
// per-table catch-all regexes.

// PerPortService is one service of the per-port fixture.
type PerPortService struct {
	Name       string
	ClusterIP  string
	HTTPPorts  []uint32
	TCPPorts   []uint32
	LegacyPort uint32
}

// PerPortServices returns the fixture's services.
func PerPortServices() []PerPortService {
	return []PerPortService{
		{Name: "pp-multi", ClusterIP: "10.96.7.1", HTTPPorts: []uint32{8080, 9090}, LegacyPort: 8080},
		{Name: "pp-eighty", ClusterIP: "10.96.7.2", HTTPPorts: []uint32{80, 8080, 9090}, LegacyPort: 8080},
		{Name: "pp-mixed", ClusterIP: "10.96.7.3", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}, LegacyPort: 8080},
		{Name: "pp-tcpfirst", ClusterIP: "10.96.7.4", HTTPPorts: []uint32{8080}, TCPPorts: []uint32{9000}, LegacyPort: 9000},
		{Name: "pp-nine", ClusterIP: "10.96.7.5", HTTPPorts: []uint32{9000}, LegacyPort: 9000},
		{Name: "pp-tcponly", ClusterIP: "10.96.7.6", TCPPorts: []uint32{6379}, LegacyPort: 6379},
	}
}

// PerPortHTTPPorts is the HTTP port set of the fixture's mesh: every port a
// service declares as HTTP, and the two legacy numbers.
func PerPortHTTPPorts() []uint32 { return []uint32{80, 8080, 9000, 9090, meshconst.ProxyOutboundPort} }

func (s PerPortService) key() string  { return "default/" + s.Name }
func (s PerPortService) mesh() string { return proxy.ServiceClusterName(s.key(), meshDomain) }

func (s PerPortService) hasHTTP(p uint32) bool {
	for _, h := range s.HTTPPorts {
		if h == p {
			return true
		}
	}
	return false
}

func (s PerPortService) legacyIsHTTP() bool { return s.hasHTTP(s.LegacyPort) }

// httpCluster is the cluster of one of the service's HTTP ports: the default
// cluster for the legacy target, "<fqdn>:<port>" for any other.
func (s PerPortService) httpCluster(p uint32) string {
	if p == s.LegacyPort {
		return s.mesh()
	}
	return proxy.PortClusterName(s.key(), meshDomain, p)
}

func (s PerPortService) names() []string {
	return []string{
		s.Name + ".default.svc.cluster.local", s.mesh(), s.Name + ".default", s.Name + ".default.svc", s.Name,
	}
}

func withSuffix(names []string, suffixes ...string) []string {
	out := make([]string, 0, len(names)*len(suffixes))
	for _, sfx := range suffixes {
		for _, n := range names {
			out = append(out, n+sfx)
		}
	}
	return out
}

// captureInput renders the service as the capture listener generator's input,
// the way cache.captureTCPService does.
func (s PerPortService) captureInput() (proxy.CaptureTCPService, bool) {
	in := proxy.CaptureTCPService{
		ClusterName: proxy.TCPClusterName(s.key(), meshDomain),
		ClusterIP:   s.ClusterIP,
		HTTPPorts:   s.HTTPPorts,
		LegacyHTTP:  s.legacyIsHTTP(),
	}
	for _, p := range s.TCPPorts {
		if p == s.LegacyPort {
			in.LegacyTCPPort = p
			continue
		}
		in.TCPPorts = append(in.TCPPorts, p)
	}
	return in, in.LegacyTCPPort != 0 || len(in.TCPPorts) > 0
}

// PerPortRouteTable builds the fixture's capture route table of dialled port
// port (0 for cap_http_other) with the agent's own virtual-host builders, in
// the shape cache.appendNameVhosts gives it.
func PerPortRouteTable(port uint32) *routev3.RouteConfiguration {
	name := proxy.CaptureHTTPOtherRouteName
	if port != 0 {
		name = proxy.CaptureHTTPRouteName(port)
	}
	legacyTable := port == meshconst.ProxyOutboundPort || port == 80
	var vhosts []*routev3.VirtualHost
	for _, s := range PerPortServices() {
		vhosts = append(vhosts, s.tableVhosts(port, legacyTable)...)
	}
	return proxy.BuildCaptureRouteConfiguration(name, vhosts,
		proxy.BuildCaptureCatchAllVirtualHost(meshDomain, port, legacyTable, true))
}

// tableVhosts returns the service's virtual hosts in the table of dialled
// port port.
func (s PerPortService) tableVhosts(port uint32, legacyTable bool) []*routev3.VirtualHost {
	names := s.names()
	refusal := func(kind, reason string, doms []string) *routev3.VirtualHost {
		return proxy.BuildRefusalVirtualHost(s.mesh()+"|"+kind, doms, reason, "aether: refused\n")
	}
	if port == 0 {
		return []*routev3.VirtualHost{refusal("undeclared", proxy.RefusalPortNotDeclared, withSuffix(names, "", ":*"))}
	}
	exact := fmt.Sprintf(":%d", port)
	routable := s.hasHTTP(port)
	shadowed := false
	rest := withSuffix(names, "", exact)
	var vhosts []*routev3.VirtualHost
	if routable {
		doms := rest
		if shadowed {
			doms, rest = withSuffix(names, exact), withSuffix(names, "")
		}
		vhosts = append(vhosts, proxy.BuildServiceVirtualHost(fmt.Sprintf("%s|%d", s.mesh(), port), s.httpCluster(port), doms, nil))
	}
	switch {
	case legacyTable && (!routable || shadowed):
		vhosts = append(vhosts, s.legacyVhost(rest))
	case !routable:
		vhosts = append(vhosts, refusal("refused", proxy.RefusalPortNotDeclared, rest))
	}
	return append(vhosts, refusal("mismatch", proxy.RefusalPortMismatch, withSuffix(names, ":*")))
}

// legacyVhost is the service's virtual host for a legacy spelling: its legacy
// target when that is HTTP, a refusal otherwise.
func (s PerPortService) legacyVhost(doms []string) *routev3.VirtualHost {
	switch {
	case s.legacyIsHTTP():
		return proxy.BuildServiceVirtualHost(s.mesh()+"|legacy", s.mesh(), doms, nil)
	case len(s.HTTPPorts) > 0:
		return proxy.BuildRefusalVirtualHost(s.mesh()+"|legacy", doms, proxy.RefusalPortRequired, "aether: refused\n")
	default:
		return proxy.BuildRefusalVirtualHost(s.mesh()+"|legacy", doms, proxy.RefusalNoHTTPPort, "aether: refused\n")
	}
}

// CapturePerPortBootstrapJSON builds the per-port capture bootstrap: the real
// per-pod capture listener for the fixture, and one loopback listener per
// route table that serves that table inline, so the proxy validates the
// tables themselves (the capture listener's own chains fetch theirs over RDS).
func CapturePerPortBootstrapJSON() ([]byte, error) {
	b, err := buildCapturePerPortBootstrap()
	if err != nil {
		return nil, err
	}
	return marshalBootstrap(b)
}

func buildCapturePerPortBootstrap() (*bootstrapv3.Bootstrap, error) {
	pod := testPod()
	// Access logging ON, as in the other capture fixtures: every tcp_proxy
	// chain then carries the L4 access log, which the chain gate looks for.
	proxy.SetAccessLogConfig(proxy.AccessLogConfig{Enabled: true, SuccessSampleRate: 100})
	defer proxy.SetAccessLogConfig(proxy.AccessLogConfig{})

	var inputs []proxy.CaptureTCPService
	clusters := []*clusterv3.Cluster{xdsCluster(), proxy.NewPassthroughOriginalDstCluster(), accessLogCluster()}
	for _, s := range PerPortServices() {
		if in, ok := s.captureInput(); ok {
			inputs = append(inputs, in)
			clusters = append(clusters, newTCPFloorCluster(s.key(), s.Name))
			for _, p := range in.TCPPorts {
				clusters = append(clusters, newTCPPortCluster(s.key(), s.Name, p))
			}
		}
		for _, p := range s.HTTPPorts {
			if p == s.LegacyPort {
				clusters = append(clusters, newHTTPDefaultCluster(s.key(), s.Name))
				continue
			}
			clusters = append(clusters, newServiceCluster(s.httpCluster(p), trustDomain, "default", s.Name))
		}
	}
	capture, err := proxy.GenerateCaptureListener(
		pod,
		proxy.SourceIdentityForPod(pod, trustDomain),
		meshconst.ProxyCapturePort,
		meshDomain,
		false, // emitStatsPod
		inputs,
		PerPortHTTPPorts(),
		true, // withPassthrough (redirect-all, the shipped default)
		nil,  // extensionFilters
	)
	if err != nil {
		return nil, fmt.Errorf("GenerateCaptureListener: %w", err)
	}
	listeners := []*listenerv3.Listener{capture}
	for i, port := range append(PerPortHTTPPorts(), 0) {
		table := PerPortRouteTable(port)
		hcm := &http_connection_managerv3.HttpConnectionManager{
			StatPrefix: table.GetName() + "_validate",
			RouteSpecifier: &http_connection_managerv3.HttpConnectionManager_RouteConfig{
				RouteConfig: table,
			},
			HttpFilters: []*http_connection_managerv3.HttpFilter{{
				Name: "envoy.filters.http.router",
				ConfigType: &http_connection_managerv3.HttpFilter_TypedConfig{
					TypedConfig: mustAny(&routerv3.Router{}),
				},
			}},
		}
		listeners = append(listeners, &listenerv3.Listener{
			Name: table.GetName() + "_validate",
			Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
				Address:       "127.0.0.1",
				PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: uint32(15100 + i)},
			}}},
			FilterChains: []*listenerv3.FilterChain{{
				Filters: []*listenerv3.Filter{{
					Name:       "envoy.filters.network.http_connection_manager",
					ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(hcm)},
				}},
			}},
		})
	}
	return newBootstrap(clusters, listeners), nil
}
