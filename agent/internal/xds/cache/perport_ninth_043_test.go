package cache

// Ninth adversarial review of the proposal-043 stack (head 54edaef9). Tests
// adopted from the review, and the reproducer of its one blocker.

import (
	"context"
	"log/slog"
	"testing"
	"time"

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

// TestPerPort_NamedTargetPortOfARouteParent with a node that holds NO
// endpoint of "web" (cold). The unresolved port must be refused cold as warm
// (ruling 4), not routed to a cluster of pod port 0.
func TestPerPort_AnUnresolvedParentPortIsRefusedColdToo(t *testing.T) {
	ctx := context.Background()
	c := a043Cache(t, a043Fixture())
	mesh := "web.demo." + a043Mesh
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/web": {{
		ParentPort:     8080,
		Matches:        []proxy.GammaMatch{{Prefix: "/"}},
		Backends:       []proxy.GammaBackend{{Service: "demo/web", Cluster: mesh, Weight: 1, Port: 8080}},
		HeaderMutation: rtpMark("rule"),
	}}})
	c.SetRouteTargetPorts(map[string][]uint32{"demo/web": {8080}})
	c.SetServicePortMaps(map[string]map[uint32]uint32{"demo/web": {8080: 0}})
	// The registry lists the fixture only: no row of "web".
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(a043Fixture())))
	tables := a043Tables(t, c)
	for _, host := range []string{"web.demo.svc.cluster.local:8080", "web.demo.svc.cluster.local", "web.demo:8080", mesh + ":8080"} {
		ans := a043Resolve(t, tables["cap_http_8080"], host)
		t.Logf("cold, dial :8080, Host %s -> vhost=%s cluster=%q status=%d refusal=%q", host, ans.vhost, ans.cluster, ans.status, ans.refusal)
		assert.Equalf(t, uint32(421), ans.status, "Host %s, cold: routed to %q", host, ans.cluster)
		assert.Emptyf(t, ans.cluster, "Host %s", host)
	}
}

// TestPerPort_AnUnroutedServiceWithANamedTargetPortIsAddressedByItsMeshName:
// the ninth review's blocker, through the real route reconciler. A mesh
// service whose name a Service of the USER's owns, that Service having `port:
// 8000, targetPort: web` (a NAME, the common shape), and NO route naming it.
//
// The reconciler hands the cache the port map of every user Service and
// resolved a named targetPort only for a Service a route names. For this one
// the cache held {8000: 0} for ever: nobody read its EndpointSlices, nothing
// was logged, there was no route whose status could say so. On the loopback
// listener and the proxy port the service then had no authority at all (its
// Service port was unresolved, its pod port is not a port of the Service):
// 421, "Its ports: none", for as long as the Service existed.
//
// The reconcile of the route event comes before the node holds the service's
// rows; the refresh that brings them tells the reconciler (OnPodPortsChanged),
// which then reads the Service's slices: the Service fronts a mesh service the
// node holds, so its port map is one the tables use.
func TestPerPort_AnUnroutedServiceWithANamedTargetPortIsAddressedByItsMeshName(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	web := a043Service{name: "web", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.9.1"}
	mesh := "web.demo." + a043Mesh
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, gatewayv1.Install(scheme))
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8000, TargetPort: intstr.FromString("web")}}},
	}
	// An unrelated Service of the same shape, which fronts nothing of the mesh.
	other := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "demo"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8000, TargetPort: intstr.FromString("web")}}},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "web-1", Namespace: "demo", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: swPtr("http"), Port: swPtr(int32(8080))}},
	}
	var sliceReads []string
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, other, slice).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, slices := list.(*discoveryv1.EndpointSliceList); slices {
				lo := &client.ListOptions{}
				for _, o := range opts {
					o.ApplyToList(lo)
				}
				sliceReads = append(sliceReads, lo.LabelSelector.String())
			}
			return c.List(ctx, list, opts...)
		}}).Build()
	c := a043Cache(t, nil)
	c.SetStaticDependencies([]string{"demo/web"})
	now := time.Unix(1_700_000_000, 0)
	rec := &gamma.Reconciler{Client: cl, APIReader: cl, Sink: c, PodPorts: c, MeshDomain: a043Mesh, Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }}
	changed := false
	c.OnPodPortsChanged(func() { changed = true })
	reconcileOnce := func() {
		t.Helper()
		_, err := rec.Reconcile(ctx, reconcile.Request{})
		require.NoError(t, err)
	}

	reconcileOnce()
	assert.Empty(t, sliceReads, "the node holds nothing of the mesh behind either Service: no slice is read")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry([]a043Service{web})))
	require.True(t, changed, "control: the refresh that brought the rows tells the reconciler")
	reconcileOnce()
	assert.Equal(t, []string{discoveryv1.LabelServiceName + "=web"}, sliceReads, "one read, of the Service that fronts a mesh service this node holds; none of the unrelated one")

	require.NoError(t, c.generateSnapshot(ctx))
	tables := a043Tables(t, c)
	for _, table := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
		got := a043Resolve(t, tables[table], mesh+":8000")
		assert.Equalf(t, mesh, got.cluster, "%s, Host %s:8000: status %d %s: with a named targetPort and no route the service has no authority on the proxy port", table, mesh, got.status, got.refusal)
	}

	// The slice goes (the Service scaled to nothing): unresolved, refused, and
	// the refusal says why on the proxy port too, not "Its ports: none" alone.
	require.NoError(t, cl.Delete(ctx, slice))
	// The slice's event is the watch's business (gamma's own tests); here
	// the kept answer is simply too old to be used again.
	now = now.Add(11 * time.Minute)
	reconcileOnce()
	require.NoError(t, c.generateSnapshot(ctx))
	tables = a043Tables(t, c)
	for _, table := range []string{proxy.OutboundHTTPRouteName, "cap_http_18081"} {
		got := a043Resolve(t, tables[table], mesh+":8000")
		assert.Equalf(t, uint32(421), got.status, "%s: routed to %q while the name resolves to nothing", table, got.cluster)
		assert.Containsf(t, got.body, "Its Service's port 8000 has a targetPort that is a name the Service's EndpointSlices do not resolve yet", "%s: the refusal does not say why", table)
	}
}
