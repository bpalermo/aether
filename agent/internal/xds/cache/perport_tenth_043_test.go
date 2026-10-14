package cache

// Tenth adversarial review of the proposal-043 stack (the merged head). Tests
// adopted from the review, and the ones that hold its findings about a
// refusal's body.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"aethermesh.dev/agent/internal/gamma"
	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// s10Node: the real reconciler over the real cache, one unrouted user Service
// "web" (8000 -> "web") in front of a mesh service, one slice that resolves
// the name. dep: the node declares the service as a dependency.
func s10Node(t *testing.T, dep bool, fail *bool, extra ...client.Object) (*SnapshotCache, *gamma.Reconciler, func() map[string]a043Answer, *bool) {
	t.Helper()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, gatewayv1.Install(scheme))
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8000, TargetPort: intstr.FromString("web")}}},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "web-1", Namespace: "demo", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: swPtr("http"), Port: swPtr(int32(8080))}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{svc, slice}, extra...)...).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, slices := list.(*discoveryv1.EndpointSliceList); slices && fail != nil && *fail {
				return errors.New("the API server is away")
			}
			return c.List(ctx, list, opts...)
		}}).Build()
	c := a043Cache(t, nil)
	if dep {
		c.SetStaticDependencies([]string{"demo/web"})
	}
	rec := &gamma.Reconciler{Client: cl, APIReader: cl, Sink: c, PodPorts: c, MeshDomain: a043Mesh, Log: slog.New(slog.DiscardHandler)}
	changed := false
	c.OnPodPortsChanged(func() { changed = true })
	mesh := "web.demo." + a043Mesh
	answers := func() map[string]a043Answer {
		require.NoError(t, c.generateSnapshot(ctx))
		tables := a043Tables(t, c)
		out := map[string]a043Answer{}
		for _, table := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
			for _, host := range []string{mesh + ":8000", mesh + ":8080", mesh} {
				out[table+" "+host] = a043Resolve(t, tables[table], host)
			}
		}
		return out
	}
	return c, rec, answers, &changed
}

func s10Web() a043Service {
	return a043Service{name: "web", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP}, ip: "10.0.9.1"}
}

// A node that does NOT declare the service (no upstream annotation,
// no route, no filter): before it holds a row the Service's name is unresolved
// in its port map (the reconciler does not read the slices of a Service it is
// not concerned with). The request must then be parked on demand, which is what
// makes the service a dependency, and NOT refused by the unresolved port: a
// refusal would never be fetched, the node would never hold the rows, and the
// name would never be resolved.
func TestPerPort_AColdNodeParksTheUnroutedNamedServiceAndDoesNotRefuseIt(t *testing.T) {
	ctx := context.Background()
	c, rec, answers, changed := s10Node(t, false, nil)
	_, err := rec.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{s10Web()})))
	for k, a := range answers() {
		t.Logf("not a dependency: %-60s vhost=%s cluster=%q onDemand=%v status=%d", k, a.vhost, a.cluster, a.onDemand, a.status)
		if !strings.HasSuffix(k, a043Mesh) { // the port-less name is the catch-all's own answer (404 / passthrough), as for any unknown name
			assert.Zerof(t, a.status, "%s: refused on a node that holds nothing of the service: nothing will ever fetch it", k)
			assert.Truef(t, a.onDemand, "%s", k)
		}
	}
	// The on-demand subscription makes it a dependency; the next refresh holds
	// the rows and says so; the reconcile then resolves the name.
	c.ObserveDependency(ctx, "demo/web")
	*changed = false
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{s10Web()})))
	require.True(t, *changed, "the refresh that brings the rows of an OBSERVED dependency tells the reconciler")
	_, err = rec.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	got := answers()
	mesh := "web.demo." + a043Mesh
	for _, table := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
		assert.Equalf(t, mesh, got[table+" "+mesh+":8000"].cluster, "%s: status %d", table, got[table+" "+mesh+":8000"].status)
	}
}

// An API outage and an unrouted Service that is being delivered. The answer
// of the last read of its slices is kept, so a reconcile during the outage
// that has nothing to read of it (no slice event of the Service, the same
// Service object) refuses nothing: it used to list the slices again on every
// reconcile, fail, and refuse what was delivered. When there IS something to
// read (here the Service's named ports change) and the read fails, the
// Service's names are refused, not served from the older answer, and the
// reconcile fails so that it is retried; it comes back with the API.
func TestPerPort_AReconcileDuringAnAPIOutageRefusesOnlyWhatItCouldNotRead(t *testing.T) {
	ctx := context.Background()
	fail := false
	c, rec, answers, _ := s10Node(t, true, &fail)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{s10Web()})))
	_, err := rec.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	mesh := "web.demo." + a043Mesh
	k := proxy.OutboundHTTPRouteName + " " + mesh + ":8000"
	require.Equal(t, mesh, answers()[k].cluster, "control: delivered")

	fail = true
	_, err = rec.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err, "a reconcile that had nothing to read of the Service failed during the outage")
	assert.Equal(t, mesh, answers()[k].cluster, "delivered before the outage, refused by a reconcile during it that had no reason to read the Service's slices")

	// The Service gains a second named port: its slices must be read, and cannot.
	svc := &corev1.Service{}
	require.NoError(t, rec.Get(ctx, types.NamespacedName{Namespace: "demo", Name: "web"}, svc))
	svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "admin", Port: 8001, TargetPort: intstr.FromString("adm")})
	require.NoError(t, rec.Update(ctx, svc))
	_, err = rec.Reconcile(ctx, reconcile.Request{})
	require.Error(t, err, "the reconcile fails, so that it is retried")
	a := answers()[k]
	assert.Equal(t, uint32(421), a.status, "refused, not served from the answer read for other ports")
	assert.Contains(t, a.body, "do not resolve yet")

	fail = false
	_, err = rec.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	assert.Equal(t, mesh, answers()[k].cluster)
}

// The refusal of the port-less authority of a name whose ONLY
// Service port is unresolved, on a node that holds no row (a filter makes the
// name the mesh's): does the body say why?
func TestRefusalBody_ThePortlessRefusalOfAColdFilteredNameSaysWhy(t *testing.T) {
	ctx := context.Background()
	c := a043Cache(t, nil)
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/web": {8000: 0}})
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/web": r3ChainCfg(t)})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil)))
	require.NoError(t, c.generateSnapshot(ctx))
	tables := a043Tables(t, c)
	mesh := "web.demo." + a043Mesh
	for _, host := range []string{mesh, mesh + ":8000", mesh + ":8080"} {
		a := a043Resolve(t, tables[proxy.OutboundHTTPRouteName], host)
		t.Logf("cold, filtered, {8000: 0}: Host %-36s status=%d refusal=%q body=%q", host, a.status, a.refusal, a.body)
		assert.Equalf(t, uint32(421), a.status, "Host %s", host)
		assert.Containsf(t, a.body, "do not resolve yet", "Host %s: the refusal does not say that the Service's port is unresolved", host)
		// Nothing is known of the service's ports here: the body must not
		// say what it serves, and must not send the reader to raw TCP.
		assert.NotContainsf(t, a.body, "serves no HTTP port", "Host %s", host)
		assert.NotContainsf(t, a.body, "Reach it over TCP", "Host %s", host)
		if host == mesh {
			assert.Contains(t, a.body, mesh+" has no port it can be addressed on yet.")
		}
	}
}

// TestRefusalBody_EveryRefusalOfANameWithAnUnresolvedPortSaysSo: a name whose
// Service has a port with a targetPort that is a name that does not resolve
// yet is told so by EVERY 421 it gets, whatever the refusal is about and
// whichever table gives it: the caller's question is "why can I not reach
// it", and the unresolved port is usually the answer. Five builders make
// these bodies; each is held here.
func TestRefusalBody_EveryRefusalOfANameWithAnUnresolvedPortSaysSo(t *testing.T) {
	ctx := context.Background()
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	const note = "Its Service's port 8000 has a targetPort that is a name the Service's EndpointSlices do not resolve yet: it is refused until they do."
	// The pods serve 8080 as HTTP and 9000 as raw TCP. The Service of the
	// user's that owns the name: 8000 -> a name that does not resolve,
	// 5432 -> 9000, 81 -> 8080. A route names it, so its names are the mesh's.
	web := a043Service{name: "web", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9000: tcp}, ip: "10.0.9.1"}
	multi := a043Service{name: "multi", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h, 9090: h}, ip: "10.0.9.2", vip: "10.96.9.2"}
	c := a043Cache(t, []a043Service{multi})
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/web": {8000: 0, 5432: 9000, 81: 8080}})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/web": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}}}})
	c.SetStaticDependencies([]string{"demo/web", "demo/multi"})
	reg := a043Registry([]a043Service{web, multi})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.NoError(t, c.generateSnapshot(ctx))
	tables := a043Tables(t, c)
	mesh := "web.demo." + a043Mesh
	for _, tc := range []struct {
		table, host, refusal, what string
	}{
		{proxy.OutboundHTTPRouteName, mesh, "port-required", "the port-less name on the loopback table (no port 80)"},
		{proxy.OutboundHTTPRouteName, mesh + ":5432", "port-not-http", "a raw-TCP port on the loopback table"},
		{proxy.OutboundHTTPRouteName, mesh + ":7777", "port-not-declared", "a port the Service does not have, on the loopback table"},
		{proxy.CaptureHTTPOtherRouteName, mesh + ":7777", "port-not-declared", "a dial to a port number that has no chain"},
		{"cap_http_9090", mesh + ":9090", "port-not-declared", "a dial to another service's port number"},
		{"cap_http_9090", mesh + ":81", "port-not-declared", "a Host that names another port, on a port the name is not served on"},
	} {
		require.NotNilf(t, tables[tc.table], "control: table %s", tc.table)
		got := a043Resolve(t, tables[tc.table], tc.host)
		require.Equalf(t, uint32(421), got.status, "control: %s (%s, Host %s) is a refusal: cluster %q", tc.what, tc.table, tc.host, got.cluster)
		require.Equalf(t, tc.refusal, got.refusal, "control: %s", tc.what)
		assert.Containsf(t, got.body, note, "%s (%s, Host %s, %s) does not say that port 8000 is unresolved", tc.what, tc.table, tc.host, tc.refusal)
	}
}

// TestRefusalBody_NoHTTPPortSaysAnUnresolvedName: "serves no HTTP port. Reach
// it over TCP instead" for a service that declares raw TCP only is right, and
// with an unresolved Service port beside it the body says that too: the port
// that does not resolve may be the HTTP one.
func TestRefusalBody_NoHTTPPortSaysAnUnresolvedName(t *testing.T) {
	ctx := context.Background()
	tcp := registryv1.PortProtocol_PORT_PROTOCOL_TCP
	db := a043Service{name: "db", ns: "demo", legacy: 9000, ports: map[uint32]registryv1.PortProtocol{9000: tcp}, ip: "10.0.9.3"}
	for _, unresolved := range []bool{false, true} {
		c := a043Cache(t, nil)
		portMap := map[uint32]uint32{5432: 9000}
		if unresolved {
			portMap[8000] = 0
		}
		c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/db": portMap})
		c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/db": {{Matches: []proxy.GammaMatch{{Prefix: "/"}}}}})
		c.SetStaticDependencies([]string{"demo/db"})
		reg := a043Registry([]a043Service{db})
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		require.NoError(t, c.generateSnapshot(ctx))
		got := a043Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], "db.demo."+a043Mesh)
		require.Equal(t, "no-http-port", got.refusal, got.body)
		assert.Contains(t, got.body, "serves no HTTP port. Reach it over TCP instead", "a service the node holds rows of, all raw TCP")
		if unresolved {
			assert.Contains(t, got.body, "Its Service's port 8000 has a targetPort that is a name")
		} else {
			assert.NotContains(t, got.body, "targetPort")
		}
	}
}
