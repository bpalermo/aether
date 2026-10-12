package cache

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// shortFixture is two services of ONE name in two namespaces that differ in
// everything a request can tell apart, a third service of another name, and a
// namespace ("third") that has neither.
//
//	demo/echo:  8080 http (declared), 9090 http; a service-wide filter; an
//	            HTTPRoute rule on every port and one on 9090 only
//	other/echo: 80 http, 7070 http (declared), 9000 raw tcp; no filter
//	demo/solo:  8080 http
func shortFixture() []a043Service {
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	return []a043Service{
		{name: "echo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.7.1", vip: "10.96.7.1"},
		{name: "echo", ns: "other", legacy: 7070, ports: map[uint32]registryv1.PortProtocol{80: h, 7070: h, 9000: tcp}, ip: "10.0.7.2", vip: "10.96.7.2"},
		{name: "solo", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.7.3", vip: "10.96.7.3"},
	}
}

const shortFilter = "envoy.filters.http.header_mutation"

// shortCache builds the cache over shortFixture. With held false the node
// holds no endpoint of any service: what it knows of their ports is what the
// generated Services declare.
func shortCache(t *testing.T, held bool) *SnapshotCache {
	t.Helper()
	ctx := context.Background()
	services := shortFixture()
	c := a043Cache(t, nil)
	a043Project(c, services)
	cfg, err := anypb.New(&header_mutationv3.HeaderMutationPerRoute{})
	require.NoError(t, err)
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/echo": {Name: shortFilter, Config: cfg}})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/echo": {
		{Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("every-port")},
		{ParentPort: 9090, Matches: []proxy.GammaMatch{{Prefix: "/"}}, HeaderMutation: rtpMark("only-9090")},
	}})
	listed := services
	if !held {
		listed = nil
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(listed)))
	return c
}

// shortAnswer is what a request gets, in the terms a client can tell apart.
type shortAnswer struct {
	cluster, refusal string
	status           uint32
	onDemand         bool
	filter           bool
	marks            string
}

// shortResolve resolves host in table as a caller in ns.
func shortResolve(t *testing.T, table *routev3.RouteConfiguration, ns, host string) shortAnswer {
	t.Helper()
	callingFrom(t, ns)
	got := a043Resolve(t, table, host)
	ans := shortAnswer{cluster: got.cluster, refusal: got.refusal, status: got.status, onDemand: got.onDemand}
	vh := a043VhostNamed(table, got.vhost)
	require.NotNil(t, vh)
	_, ans.filter = vh.GetTypedPerFilterConfig()[shortFilter]
	var marks []string
	for _, r := range vhostRoutes(t, vh) {
		if _, onRoute := r.GetTypedPerFilterConfig()[shortFilter]; onRoute && r.GetRoute() != nil {
			ans.filter = true
		}
		for _, hd := range r.GetResponseHeadersToAdd() {
			if hd.GetHeader().GetKey() == "x-rv" {
				marks = append(marks, hd.GetHeader().GetValue())
			}
		}
	}
	ans.marks = strings.Join(marks, ",")
	// An authority-named cluster carries the name that was typed: compare it
	// by its port.
	if ans.onDemand {
		_, port, _ := strings.Cut(host, ":")
		ans.cluster = "on-demand:" + port
	}
	return ans
}

func a043VhostNamed(table *routev3.RouteConfiguration, name string) *routev3.VirtualHost {
	for _, vh := range table.GetVirtualHosts() {
		if vh.GetName() == name {
			return vh
		}
	}
	return nil
}

// TestShortName_AnswersAsTheFullName is the rule: a Host with no namespace
// means the service of that name in the CALLER's namespace. On every table,
// for every port spelling, a short name is routed, refused, parked and
// filtered exactly as "<name>.<caller's namespace>.<mesh domain>" is, with
// the same HTTPRoute rules; and when the caller's namespace has no service of
// the name, or the connection carries no namespace, it is refused, never
// answered by another namespace's service and never left to the catch-all.
//
// Both with the endpoints held and without (the node then knows the ports
// from the generated Services alone).
func TestShortName_AnswersAsTheFullName(t *testing.T) {
	for _, held := range []bool{true, false} {
		t.Run(fmt.Sprintf("endpoints held=%v", held), func(t *testing.T) {
			c := shortCache(t, held)
			tables := a043Tables(t, c)
			require.Contains(t, tables, "cap_http_8080")
			require.Contains(t, tables, "cap_http_18081")
			compared, refused := 0, 0
			for tname, table := range tables {
				for _, suffix := range []string{"", ":80", ":7070", ":8080", ":9090", ":9000", ":7777", ":18081"} {
					for _, ns := range []string{"demo", "other"} {
						full := shortResolve(t, table, ns, "echo."+ns+"."+a043Mesh+suffix)
						short := shortResolve(t, table, ns, "echo"+suffix)
						assert.Equalf(t, full, short, "table %s, caller in %s: Host echo%s must answer as echo.%s.%s%s", tname, ns, suffix, ns, a043Mesh, suffix)
						compared++
					}
					// A namespace with no service of the name, and a
					// connection with no namespace at all.
					for _, ns := range []string{"third", ""} {
						got := shortResolve(t, table, ns, "echo"+suffix)
						assert.Equalf(t, uint32(421), got.status, "table %s, caller in %q, Host echo%s: %+v", tname, ns, suffix, got)
						assert.Equalf(t, proxy.RefusalNameNotInNamespace, got.refusal, "table %s, caller in %q, Host echo%s", tname, ns, suffix)
						assert.Emptyf(t, got.cluster, "table %s, caller in %q, Host echo%s reached a cluster", tname, ns, suffix)
						refused++
					}
					// A name only one namespace has is still that namespace's.
					got := shortResolve(t, table, "other", "solo"+suffix)
					assert.Equalf(t, proxy.RefusalNameNotInNamespace, got.refusal, "table %s: solo%s from a namespace that has no solo: %+v", tname, suffix, got)
				}
			}
			require.Greater(t, compared, 60)
			require.Greater(t, refused, 60)

			// The two namespaces really differ, or the comparison above says
			// nothing: the same short authority reaches different clusters.
			demo := shortResolve(t, tables["cap_http_8080"], "demo", "echo:8080")
			other := shortResolve(t, tables["cap_http_8080"], "other", "echo:8080")
			assert.NotEmpty(t, demo.cluster)
			assert.True(t, demo.filter, "demo/echo's service-wide filter rides on its arm")
			assert.Equal(t, "every-port", demo.marks)
			assert.Equal(t, proxy.RefusalPortNotDeclared, other.refusal, "other/echo does not declare 8080")
			nine := shortResolve(t, tables[proxy.OutboundHTTPRouteName], "demo", "echo:9090")
			assert.Equal(t, "every-port,only-9090", nine.marks, "a rule whose parentRef names a port applies to the short name on that port")
			assert.True(t, nine.filter)
			// A name without a port is port 80: other/echo declares it,
			// demo/echo does not.
			assert.NotEmpty(t, shortResolve(t, tables[proxy.OutboundHTTPRouteName], "other", "echo").cluster)
			assert.Equal(t, proxy.RefusalPortRequired, shortResolve(t, tables[proxy.OutboundHTTPRouteName], "demo", "echo").refusal)
		})
	}
}

// TestShortName_EveryArmCarriesTheServicesFilter: inside a virtual host that
// several namespaces share, a service-wide filter can only ride on the routes.
// Every route of the arm of a service that has one, in every short virtual
// host of every table, must enable it when it delivers; and no arm of a
// service that has none may carry it.
func TestShortName_EveryArmCarriesTheServicesFilter(t *testing.T) {
	for _, held := range []bool{true, false} {
		c := shortCache(t, held)
		arms := 0
		for tname, table := range a043Tables(t, c) {
			for _, vh := range table.GetVirtualHosts() {
				if vh.GetMatcher() == nil || !strings.HasPrefix(vh.GetName(), "echo|short|") {
					continue
				}
				assert.Emptyf(t, vh.GetTypedPerFilterConfig(), "table %s, %s: a shared virtual host enables no service's filter itself", tname, vh.GetName())
				for _, ns := range matcherNamespaces(vh) {
					for _, r := range matcherArm(t, vh.GetMatcher(), ns) {
						_, has := r.GetTypedPerFilterConfig()[shortFilter]
						if ns == "demo" && r.GetRoute() != nil {
							arms++
							assert.Truef(t, has, "held=%v, table %s, %s, arm demo: a route delivers without demo/echo's filter", held, tname, vh.GetName())
						}
						if ns == "other" {
							assert.Falsef(t, has, "held=%v, table %s, %s, arm other: other/echo has no filter", held, tname, vh.GetName())
						}
					}
				}
				for _, r := range matcherArm(t, vh.GetMatcher(), "third") {
					assert.Nilf(t, r.GetRoute(), "table %s, %s: the no-match arm delivers", tname, vh.GetName())
				}
			}
		}
		require.Greater(t, arms, 4, "held=%v: the walk saw demo/echo's delivering routes", held)
	}
}

// TestShortName_AServiceVirtualHostHasNothingTheArmDoesNotCopy: an arm is the
// routes of the full name's virtual host plus that virtual host's
// typed_per_filter_config. If a service's virtual host ever gains another
// field of its own (a retry policy, header rules, a rate limit), the short
// name would silently not have it: this fails then.
func TestShortName_AServiceVirtualHostHasNothingTheArmDoesNotCopy(t *testing.T) {
	c := shortCache(t, true)
	for tname, table := range a043Tables(t, c) {
		for _, vh := range table.GetVirtualHosts() {
			if vh.GetMatcher() != nil || vh.GetName() == "on_demand_catch_all" {
				continue
			}
			bare := &routev3.VirtualHost{Name: vh.GetName(), Domains: vh.GetDomains(), Routes: vh.GetRoutes(), TypedPerFilterConfig: vh.GetTypedPerFilterConfig()}
			assert.Truef(t, proto.Equal(bare, vh), "table %s: virtual host %s carries a field a short name's arm does not copy", tname, vh.GetName())
		}
	}
}

// TestShortName_NoServiceNameIsClaimedOutsideItsShortVirtualHost: the short
// name is claimed by the shared virtual host and by nothing else, in any
// table, whether one namespace has the name or several.
func TestShortName_NoServiceNameIsClaimedOutsideItsShortVirtualHost(t *testing.T) {
	c := shortCache(t, true)
	for tname, table := range a043Tables(t, c) {
		for _, vh := range table.GetVirtualHosts() {
			for _, d := range vh.GetDomains() {
				name, _, _ := strings.Cut(d, ":")
				if name != "echo" && name != "solo" {
					continue
				}
				assert.NotNilf(t, vh.GetMatcher(), "table %s: %s claims the short authority %q without resolving the caller's namespace", tname, vh.GetName(), d)
			}
		}
	}
}

// TestPerPort_ColdService_ADeclaredPortAddedLater: a service the node holds no
// endpoint of is judged by its Service's ports, so a change of those ports
// ALONE (same name, same address, still no endpoint) is a change: the port
// asks for a rebuild, and the port then has its chain, its table and its
// virtual host.
func TestPerPort_ColdService_ADeclaredPortAddedLater(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	svc := a043Service{name: "multi", ns: a043NS, ports: map[uint32]registryv1.PortProtocol{8080: h}, vip: "10.96.1.2"}
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	a043Project(c, []a043Service{svc})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	drain := func() bool {
		select {
		case <-c.depChanged:
			return true
		default:
			return false
		}
	}
	drain()
	require.NotContains(t, a043Tables(t, c), "cap_http_9090")

	// The same projection again is no change: nothing is rebuilt (a rebuild
	// of the capture listeners drains their connections).
	a043Project(c, []a043Service{svc})
	assert.False(t, drain(), "an unchanged projection asked for a rebuild")

	svc.ports = map[uint32]registryv1.PortProtocol{8080: h, 9090: h}
	a043Project(c, []a043Service{svc})
	require.True(t, drain(), "a change of the Service's ports alone asked for no rebuild")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	tables := a043Tables(t, c)
	require.Contains(t, tables, "cap_http_9090")
	const host = "multi." + a043NS + "." + a043Mesh + ":9090"
	got := spResolve(t, tables["cap_http_9090"], host)
	assert.Equal(t, "multi."+a043NS+"."+a043Mesh+":9090", got.cluster, "%+v", got)
}
