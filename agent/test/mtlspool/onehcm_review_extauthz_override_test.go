package mtlspool

// A service-wide ext_authz HTTPFilter that overrides the authorization
// service, and an HTTPRoute rule's ext_authz ExtensionRef that does not
// (found by the adversarial review of #1703). At the pinned proxy the merged
// per-route config takes the service override from the MOST SPECIFIC level
// only (ext_authz.cc, FilterConfigPerRoute(less, more): "Do not inherit
// overrides from less specific configuration"): the rule's check goes to the
// filter's own service, on the captured path as on the proxy port.

import (
	"context"
	"net"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/common/extensionfilter"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type rvAuthz struct {
	authv3.UnimplementedAuthorizationServer
	who string
}

func (a rvAuthz) Check(context.Context, *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	return &authv3.CheckResponse{HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{
		ResponseHeadersToAdd: []*corev3.HeaderValueOption{{Header: &corev3.HeaderValue{Key: "x-fl-authz-context", Value: a.who}}},
	}}}, nil
}

func rvServe(t *testing.T, who string) *net.TCPAddr {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, rvAuthz{who: who})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().(*net.TCPAddr)
}

func TestOneHCM_LiveExtAuthzServiceOverrideNotInherited(t *testing.T) {
	def := rvServe(t, "default-service")
	over := rvServe(t, "override-service")

	ppBoot = func(c *cache.SnapshotCache) { c.SetAuthzSidecar(2*time.Second, false) }
	t.Cleanup(func() { ppBoot = nil })
	r, capture, proxyPort := ppStartFrom(t, "demo")
	r.mutate = func(res map[resourcev3.Type][]types.Resource) {
		oc := flAuthzCluster(t, over)
		oc.Name = "rv_override_authz"
		oc.LoadAssignment.ClusterName = oc.Name
		res[resourcev3.ClusterType] = append(res[resourcev3.ClusterType], flAuthzCluster(t, def), oc)
	}
	svc := &ext_authzv3.ExtAuthzPerRoute{Override: &ext_authzv3.ExtAuthzPerRoute_CheckSettings{CheckSettings: &ext_authzv3.CheckSettings{
		ContextExtensions: map[string]string{"tier": "service"},
		ServiceOverride: &ext_authzv3.CheckSettings_GrpcService{GrpcService: &corev3.GrpcService{
			TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: "rv_override_authz"}},
		}},
	}}}
	route := flAuthzSettings(map[string]string{"tier": "route"})
	flLevels(t, r, extensionfilter.ExtAuthzFilterName, flAny(t, svc), flAny(t, route))

	captured := flGet(capture, ppMesh+":8080")
	viaProxy := flGet(proxyPort, ppMesh+":8080")
	t.Logf("MEASURED captured dial (compiled cap_http) -> %s", captured)
	t.Logf("MEASURED proxy port (native levels)        -> %s", viaProxy)
	require.NoError(t, captured.err)
	require.NoError(t, viaProxy.err)
	require.Equal(t, viaProxy.header.Get("X-Fl-Authz-Context"), captured.header.Get("X-Fl-Authz-Context"),
		"the rule's authorization check went to a different authorization service on the captured path than natively")
}
