package mtlspool

// The service-wide filter on cap_http, live on the pinned proxy (second
// review of #1703). A full name's compiled virtual host belongs to one
// service, so the service-wide filter's config stays on the VIRTUAL HOST
// there, as on the proxy port, and the proxy combines it with a rule's own
// config itself. The refusals of that virtual host disable it per route
// (FilterConfig disabled: RouteEntryImplBase::filterDisabled answers from the
// route first, source/common/router/config_impl.cc, and a disabled filter is
// not created for the stream, source/common/http/filter_chain_helper.cc).

import (
	"net"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/common/extensionfilter"
	mutation_rulesv3 "github.com/envoyproxy/go-control-plane/envoy/config/common/mutation_rules/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	rbac_configv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	header_mutationv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_mutation/v3"
	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// flWait lets a republished snapshot land (as flLevels does).
func flWait() { time.Sleep(3 * time.Second) }

func lvString(v string) *structpb.Value { return structpb.NewStringValue(v) }

// flAuthzRig is the rig with an allowing authorization service behind the
// authz cluster (as TestPerPort_LiveFilterLevels_ExtAuthz sets it up).
func flAuthzRig(t *testing.T) (r *ppRig, capture, proxyPort string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	authv3.RegisterAuthorizationServer(srv, flAuthz{})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	ppBoot = func(c *cache.SnapshotCache) { c.SetAuthzSidecar(2*time.Second, false) }
	t.Cleanup(func() { ppBoot = nil })
	r, capture, proxyPort = ppStartFrom(t, "demo")
	r.mutate = func(res map[resourcev3.Type][]types.Resource) {
		res[resourcev3.ClusterType] = append(res[resourcev3.ClusterType], flAuthzCluster(t, ln.Addr().(*net.TCPAddr)))
	}
	return r, capture, proxyPort
}

// lvStripDisables removes every route-level "disabled" entry from cap_http:
// the red control, a refusal that runs the service-wide filter.
func lvStripDisables(res map[resourcev3.Type][]types.Resource) {
	for _, r := range res[resourcev3.RouteType] {
		rc, ok := r.(*routev3.RouteConfiguration)
		if !ok || rc.GetName() != proxy.CaptureHTTPChainName {
			continue
		}
		for _, vh := range rc.GetVirtualHosts() {
			for _, rt := range vh.GetRoutes() {
				for name, a := range rt.GetTypedPerFilterConfig() {
					fc := &routev3.FilterConfig{}
					if a.MessageIs(fc) && a.UnmarshalTo(fc) == nil && fc.GetDisabled() {
						delete(rt.TypedPerFilterConfig, name)
					}
				}
			}
		}
	}
}

// A refusal answered from a service's compiled virtual host (a Host naming a
// port the service is not dialled on: 421 port-mismatch) runs none of its
// service-wide filters, while the service's own route runs them: as on the
// per-port tables, where the refusal is another virtual host. Red control:
// the same table without the route-level disables runs them on the refusal.
func TestOneHCM_LiveRefusalRunsNoServiceWideFilter(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(t *testing.T) (string, *anypb.Any)
		// ran says, from the answer, whether the filter ran.
		ran func(r flResp) bool
	}{
		{
			name: "header_mutation",
			cfg: func(t *testing.T) (string, *anypb.Any) {
				return extensionfilter.HeaderMutationFilterName, flAny(t, flStamp("x-fl-service"))
			},
			ran: func(r flResp) bool { return r.header.Get("X-Fl-Service") == "ran" },
		},
		{
			name: "rbac (deny all)",
			cfg: func(t *testing.T) (string, *anypb.Any) {
				return extensionfilter.RBACFilterName, flAny(t, flRBAC(rbac_configv3.RBAC_DENY))
			},
			ran: func(r flResp) bool { return r.status == 403 },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, capture, _ := ppStartFrom(t, "demo")
			name, cfg := tc.cfg(t)
			flLevels(t, r, name, cfg, nil)
			served := flGet(capture, ppMesh+":8080")
			refused := flGet(capture, ppMesh+":9999")
			t.Logf("MEASURED %s: served Host %s:8080 -> %s; refusal Host %s:9999 -> %s", tc.name, ppMesh, served, ppMesh, refused)
			require.NoError(t, served.err)
			require.NoError(t, refused.err)
			require.True(t, tc.ran(served), "anti-vacuity: the service-wide filter runs on the service's route")
			require.Equal(t, 421, refused.status)
			require.Equal(t, proxy.RefusalPortMismatch, refused.header.Get("X-Aether-Error"))
			assert.False(t, tc.ran(refused), "the refusal ran the service-wide filter")

			// Red control: without the disables the refusal runs it.
			r.mutate = lvStripDisables
			r.publish()
			flWait()
			control := flGet(capture, ppMesh+":9999")
			t.Logf("MEASURED %s, control without the route-level disables: refusal -> %s", tc.name, control)
			require.NoError(t, control.err)
			assert.True(t, tc.ran(control), "control: the refusal runs the filter once nothing disables it")
		})
	}
}

// ext_authz: the same, with an authorization service that marks every
// response it allows. Separate because it needs the authz cluster.
func TestOneHCM_LiveRefusalRunsNoServiceWideExtAuthz(t *testing.T) {
	r, capture, _ := flAuthzRig(t)
	flLevels(t, r, extensionfilter.ExtAuthzFilterName, flAny(t, flAuthzSettings(map[string]string{"tier": "service"})), nil)
	served := flGet(capture, ppMesh+":8080")
	refused := flGet(capture, ppMesh+":9999")
	t.Logf("MEASURED ext_authz: served -> %s; refusal -> %s", served, refused)
	require.NoError(t, served.err)
	require.NoError(t, refused.err)
	require.Equal(t, "tier=service", served.header.Get("X-Fl-Authz-Context"), "anti-vacuity: the check runs on the service's route")
	require.Equal(t, 421, refused.status)
	assert.Empty(t, refused.header.Get("X-Fl-Authz-Context"), "the refusal ran the service-wide check")
}

func lvQuery(key, value string) *corev3.KeyValueMutation {
	return &corev3.KeyValueMutation{Append: &corev3.KeyValueAppend{Record: &corev3.KeyValuePair{Key: key, Value: lvString(value)}}}
}

// A short name's virtual host is shared by several namespaces' services, so
// both levels must sit on one route there, and a pair no single config
// applies in the proxy's order (the rule mutates the query, the service a
// request header) is refused 500 filter-levels on both paths, while the full
// name is served natively.
func TestOneHCM_LiveShortNameRefusesInexactHeaderMutationLevels(t *testing.T) {
	r, capture, proxyPort := ppStartFrom(t, "demo")
	svc := &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		RequestMutations: []*mutation_rulesv3.HeaderMutation{r2Append("x-fl-seen", "%REQ(:PATH)%")},
	}}
	route := &header_mutationv3.HeaderMutationPerRoute{Mutations: &header_mutationv3.Mutations{
		QueryParameterMutations: []*corev3.KeyValueMutation{lvQuery("q", "1")},
	}}
	flLevels(t, r, extensionfilter.HeaderMutationFilterName, flAny(t, svc), flAny(t, route))
	for _, at := range []struct{ path, addr string }{{"captured dial to :8080", capture}, {"proxy port", proxyPort}} {
		full := r2GetPath(at.addr, ppMesh+":8080", "/")
		short := r2GetPath(at.addr, "multi:8080", "/")
		t.Logf("MEASURED %-22s full -> %s; short -> %s", at.path, full, short)
		require.NoError(t, full.err)
		require.NoError(t, short.err)
		assert.Equal(t, 200, full.status, "%s: the full name is served, its levels native", at.path)
		assert.Equal(t, 500, short.status, "%s: the short name cannot carry the two levels exactly", at.path)
		assert.Equal(t, proxy.RefusalFilterLevels, short.header.Get("X-Aether-Error"), at.path)
	}
}

func lvH2M(header string, remove bool) *header_to_metadatav3.Config {
	return &header_to_metadatav3.Config{RequestRules: []*header_to_metadatav3.Config_Rule{{
		Header: header,
		OnHeaderPresent: &header_to_metadatav3.Config_KeyValuePair{
			MetadataNamespace: "aether.test", Key: header, Type: header_to_metadatav3.Config_STRING,
		},
		Remove: remove,
	}}}
}

// header_to_metadata at both levels: the most specific level is the whole
// config. The service's rule removes x-fl-probe, the route's does not, so
// with both the backend still sees the header, on the captured path as on the
// proxy port; with the service's alone it does not (anti-vacuity).
func TestOneHCM_LiveHeaderToMetadataBothLevels(t *testing.T) {
	r, capture, proxyPort := ppStartFrom(t, "demo")
	probe := map[string]string{"x-fl-probe": "p"}

	flLevels(t, r, extensionfilter.HeaderToMetadataFilterName, flAny(t, lvH2M("x-fl-probe", true)), nil)
	alone := flGetWith(capture, ppMesh+":8080", probe)
	t.Logf("MEASURED service level alone, captured -> status=%d probe-seen=%q", alone.status, alone.header.Get("X-Fl-Probe-Seen"))
	require.NoError(t, alone.err)
	require.Equal(t, 200, alone.status)
	require.Empty(t, alone.header.Get("X-Fl-Probe-Seen"), "anti-vacuity: the service's rule removes the header")

	flLevels(t, r, extensionfilter.HeaderToMetadataFilterName, flAny(t, lvH2M("x-fl-probe", true)), flAny(t, lvH2M("x-fl-other", false)))
	captured := flGetWith(capture, ppMesh+":8080", probe)
	viaProxy := flGetWith(proxyPort, ppMesh+":8080", probe)
	t.Logf("MEASURED both levels: captured -> status=%d probe-seen=%q; proxy port -> status=%d probe-seen=%q",
		captured.status, captured.header.Get("X-Fl-Probe-Seen"), viaProxy.status, viaProxy.header.Get("X-Fl-Probe-Seen"))
	require.NoError(t, captured.err)
	require.NoError(t, viaProxy.err)
	require.Equal(t, "p", viaProxy.header.Get("X-Fl-Probe-Seen"), "natively the route's config replaces the service's")
	assert.Equal(t, viaProxy.header.Get("X-Fl-Probe-Seen"), captured.header.Get("X-Fl-Probe-Seen"))
	assert.Equal(t, viaProxy.status, captured.status)
}
