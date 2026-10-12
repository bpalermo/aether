package mtlspool

// A service-wide HTTPFilter is enabled on the service's virtual host; an
// HTTPRoute rule's ExtensionRef is on its route. When both name the SAME
// Envoy filter the proxy has two levels of per-route config, and what it does
// with them is the filter's own choice (common/extensionfilter, levels.go).
// A short name's virtual host is shared by several namespaces, so both levels
// have to sit on the route there. These tests measure, on the pinned proxy
// and for each allow-listed filter whose effect a client can see, what the
// two levels do under the FULL name, and that the short name does the same,
// on both paths. header_mutation: perport_callerns_test.go and
// TestPerPort_LiveFilterLevels_HeaderMutation below.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/common/extensionfilter"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	rbac_configv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	rbac_filterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rbac/v3"
	upstream_httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// flResp is one answer, with every response header the tests read.
type flResp struct {
	status int
	header http.Header
	err    error
}

func (r flResp) String() string {
	if r.err != nil {
		return "error: " + r.err.Error()
	}
	var seen []string
	for _, k := range []string{"X-Fl-Service", "X-Fl-Route", "X-Fl-Authz-Context", "X-Aether-Error"} {
		if v := r.header.Get(k); v != "" {
			seen = append(seen, k+"="+v)
		}
	}
	return fmt.Sprintf("status=%d %s", r.status, strings.Join(seen, " "))
}

func flGet(addr, host string) flResp {
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	req.Host = host
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return flResp{err: err}
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return flResp{status: resp.StatusCode, header: resp.Header}
}

// flSame runs the request under the full name and the short name on both
// paths, logs what was measured, and requires the short name to be answered
// as the full one for the headers given. It returns the full name's answer
// on the captured path, for the caller to say what the two levels did.
func flSame(t *testing.T, what, capture, proxyPort string, headers ...string) flResp {
	t.Helper()
	var first flResp
	for i, at := range []struct{ path, addr string }{{"captured dial to :8080", capture}, {"proxy port", proxyPort}} {
		full := flGet(at.addr, ppMesh+":8080")
		short := flGet(at.addr, "multi:8080")
		t.Logf("MEASURED %s, %-22s Host %s:8080 -> %s", what, at.path, ppMesh, full)
		t.Logf("MEASURED %s, %-22s Host multi:8080 -> %s", what, at.path, short)
		require.NoError(t, full.err)
		require.NoError(t, short.err)
		if full.status != short.status {
			t.Errorf("%s, %s: status %d under the full name, %d under the short name", what, at.path, full.status, short.status)
		}
		for _, h := range headers {
			if full.header.Get(h) != short.header.Get(h) {
				t.Errorf("%s, %s: %s is %q under the full name and %q under the short name", what, at.path, h, full.header.Get(h), short.header.Get(h))
			}
		}
		if i == 0 {
			first = full
		}
	}
	return first
}

func flAny(t *testing.T, m proto.Message) *anypb.Any {
	t.Helper()
	a, err := anypb.New(m)
	require.NoError(t, err)
	return a
}

// flLevels puts svcCfg on demo/multi as its service-wide filter and routeCfg
// on its one rule as the rule's own filter of the same kind.
func flLevels(t *testing.T, r *ppRig, name string, svcCfg, routeCfg *anypb.Any) {
	t.Helper()
	r.c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{ppSvc: {Name: name, Config: svcCfg}})
	rule := proxy.GammaRoute{Matches: []proxy.GammaMatch{{Prefix: "/"}}}
	if routeCfg != nil {
		rule.ExtensionFilters = []proxy.ExtensionFilter{{Name: name, Config: routeCfg}}
	}
	r.c.SetServiceRoutes(map[string][]proxy.GammaRoute{ppSvc: {rule}})
	require.NoError(t, r.c.LoadClustersFromRegistry(context.Background(), "cluster-1", ppNodeName, r.reg))
	r.publish()
	time.Sleep(3 * time.Second)
}

func flStamp(key string) *header_mutationv3.HeaderMutationPerRoute {
	return &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		ResponseMutations: []*mutation_rulesv3.HeaderMutation{{Action: &mutation_rulesv3.HeaderMutation_Append{Append: &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{Key: key, Value: "ran"}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		}}}},
	}}
}

// header_mutation: EVERY level applies.
func TestPerPort_LiveFilterLevels_HeaderMutation(t *testing.T) {
	r, capture, proxyPort := ppStartFrom(t, "demo")
	flLevels(t, r, extensionfilter.HeaderMutationFilterName, flAny(t, flStamp("x-fl-service")), flAny(t, flStamp("x-fl-route")))
	full := flSame(t, "header_mutation at both levels", capture, proxyPort, "X-Fl-Service", "X-Fl-Route")
	require.Equal(t, 200, full.status)
	require.Equal(t, "ran", full.header.Get("X-Fl-Service"), "the full name: the service-wide level applies beside the route's")
	require.Equal(t, "ran", full.header.Get("X-Fl-Route"))
}

func flRBAC(action rbac_configv3.RBAC_Action) *rbac_filterv3.RBACPerRoute {
	// One policy that matches every request: with ALLOW everything is allowed,
	// with DENY everything is denied.
	return &rbac_filterv3.RBACPerRoute{Rbac: &rbac_filterv3.RBAC{Rules: &rbac_configv3.RBAC{
		Action: action,
		Policies: map[string]*rbac_configv3.Policy{"all": {
			Permissions: []*rbac_configv3.Permission{{Rule: &rbac_configv3.Permission_Any{Any: true}}},
			Principals:  []*rbac_configv3.Principal{{Identifier: &rbac_configv3.Principal_Any{Any: true}}},
		}},
	}}}
}

// rbac: the MOST SPECIFIC level is the whole config. Measured both ways
// round, so that "the route's wins" is told from "either denies" and from
// "either allows".
func TestPerPort_LiveFilterLevels_RBAC(t *testing.T) {
	r, capture, proxyPort := ppStartFrom(t, "demo")

	flLevels(t, r, extensionfilter.RBACFilterName, flAny(t, flRBAC(rbac_configv3.RBAC_DENY)), nil)
	full := flSame(t, "rbac: the service denies, the route says nothing", capture, proxyPort)
	require.Equal(t, 403, full.status, "anti-vacuity: the service-wide rbac is enforced")

	flLevels(t, r, extensionfilter.RBACFilterName, flAny(t, flRBAC(rbac_configv3.RBAC_DENY)), flAny(t, flRBAC(rbac_configv3.RBAC_ALLOW)))
	full = flSame(t, "rbac: the service denies, the route allows", capture, proxyPort)
	require.Equal(t, 200, full.status, "the full name: the route's rbac replaces the service's")

	flLevels(t, r, extensionfilter.RBACFilterName, flAny(t, flRBAC(rbac_configv3.RBAC_ALLOW)), flAny(t, flRBAC(rbac_configv3.RBAC_DENY)))
	full = flSame(t, "rbac: the service allows, the route denies", capture, proxyPort)
	require.Equal(t, 403, full.status)
}

// flAuthz is an authorization service that allows every request and says, in
// a response header, which context extensions the check carried.
type flAuthz struct {
	authv3.UnimplementedAuthorizationServer
}

func (flAuthz) Check(_ context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	ctx := req.GetAttributes().GetContextExtensions()
	keys := make([]string, 0, len(ctx))
	for k, v := range ctx {
		keys = append(keys, k+"="+v)
	}
	slices.Sort(keys)
	return &authv3.CheckResponse{
		// No status is status OK.
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{
			ResponseHeadersToAdd: []*corev3.HeaderValueOption{{Header: &corev3.HeaderValue{Key: "x-fl-authz-context", Value: strings.Join(keys, ",")}}},
		}},
	}, nil
}

func flAuthzSettings(ctx map[string]string) *ext_authzv3.ExtAuthzPerRoute {
	return &ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{ContextExtensions: ctx}}}
}

// ext_authz: the levels are MERGED. The context extensions a check carries
// are the union of the service's and the route's, the route's entry winning a
// key; a route that disables the check disables it.
func TestPerPort_LiveFilterLevels_ExtAuthz(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, flAuthz{})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	ppBoot = func(c *cache.SnapshotCache) { c.SetAuthzSidecar(2*time.Second, false) }
	t.Cleanup(func() { ppBoot = nil })
	r, capture, proxyPort := ppStartFrom(t, "demo")
	// The chart provisions the authz cluster in the bootstrap; here it rides
	// with the other clusters.
	r.mutate = func(res map[resourcev3.Type][]types.Resource) {
		res[resourcev3.ClusterType] = append(res[resourcev3.ClusterType], flAuthzCluster(t, ln.Addr().(*net.TCPAddr)))
	}

	flLevels(t, r, extensionfilter.ExtAuthzFilterName, flAny(t, flAuthzSettings(map[string]string{"tenant": "a", "tier": "service"})), nil)
	full := flSame(t, "ext_authz: the service's context alone", capture, proxyPort, "X-Fl-Authz-Context")
	require.Equal(t, 200, full.status)
	require.Equal(t, "tenant=a,tier=service", full.header.Get("X-Fl-Authz-Context"), "anti-vacuity: the service-wide ext_authz runs and carries its context")

	flLevels(t, r, extensionfilter.ExtAuthzFilterName,
		flAny(t, flAuthzSettings(map[string]string{"tenant": "a", "tier": "service"})),
		flAny(t, flAuthzSettings(map[string]string{"tier": "route", "action": "write"})))
	full = flSame(t, "ext_authz: context at both levels", capture, proxyPort, "X-Fl-Authz-Context")
	require.Equal(t, 200, full.status)
	require.Equal(t, "action=write,tenant=a,tier=route", full.header.Get("X-Fl-Authz-Context"),
		"the full name: the union of the two levels, the route's entry winning a key")

	flLevels(t, r, extensionfilter.ExtAuthzFilterName,
		flAny(t, flAuthzSettings(map[string]string{"tenant": "a"})),
		flAny(t, &ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_Disabled{Disabled: true}}))
	full = flSame(t, "ext_authz: the route disables the check", capture, proxyPort, "X-Fl-Authz-Context")
	require.Equal(t, 200, full.status)
	require.Empty(t, full.header.Get("X-Fl-Authz-Context"), "the full name: no check is made")
}

func flAuthzCluster(t *testing.T, addr *net.TCPAddr) *clusterv3.Cluster {
	t.Helper()
	h2 := flAny(t, &upstream_httpv3.HttpProtocolOptions{UpstreamProtocolOptions: &upstream_httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
		ExplicitHttpConfig: &upstream_httpv3.HttpProtocolOptions_ExplicitHttpConfig{
			ProtocolConfig: &upstream_httpv3.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{Http2ProtocolOptions: &corev3.Http2ProtocolOptions{}},
		},
	}})
	return &clusterv3.Cluster{
		Name:                 proxy.AuthzSidecarClusterName,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
		LoadAssignment: &endpointv3.ClusterLoadAssignment{ClusterName: proxy.AuthzSidecarClusterName, Endpoints: []*endpointv3.LocalityLbEndpoints{{
			LbEndpoints: []*endpointv3.LbEndpoint{{HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{
				Address: &corev3.Address{Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{
					Address: addr.IP.String(), PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: uint32(addr.Port)},
				}}},
			}}}},
		}}},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": h2},
	}
}
