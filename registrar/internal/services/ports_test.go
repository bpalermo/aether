package services

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/serviceref"
	"aethermesh.dev/registrar/internal/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	classHTTP = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	classTCP  = registryv1.PortProtocol_PORT_PROTOCOL_TCP
	classUDP  = registryv1.PortProtocol_PORT_PROTOCOL_UDP

	listHTTP = registryv1.Service_PROTOCOL_HTTP
	listTCP  = registryv1.Service_PROTOCOL_TCP
	listUDP  = registryv1.Service_PROTOCOL_UDP
)

type (
	classes = map[uint32]registryv1.PortProtocol
	// catalog is a snapshot's content: service key -> listing -> rows.
	catalog = map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
)

// pod is a registry row as both backends write it since proposal 037: the
// declared port, the served set and the class of every port.
func pod(ns, ip string, port uint32, portProtocols classes) *registryv1.ServiceEndpoint {
	served := make([]uint32, 0, len(portProtocols))
	for p := range portProtocols {
		served = append(served, p)
	}
	return &registryv1.ServiceEndpoint{
		Ip: ip, ClusterName: "c", Port: port, Ports: served, PortProtocols: portProtocols,
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: ns},
	}
}

// listed puts each row under every listing it serves a port of, which is what
// both backends do.
func listed(rows ...*registryv1.ServiceEndpoint) map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint {
	out := map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{}
	for _, row := range rows {
		for _, listing := range []registryv1.Service_Protocol{listHTTP, listTCP, listUDP} {
			for _, class := range row.GetPortProtocols() {
				if registryv1.Service_Protocol(class) == listing {
					out[listing] = append(out[listing], row)
					break
				}
			}
		}
	}
	return out
}

func snapshotOf(c catalog) *server.Snapshot {
	s := server.NewSnapshot()
	s.Replace(c)
	return s
}

func key(ns, svc string) string { return serviceref.New(ns, svc).Key() }

func legacyPort(name string, port int32) corev1.ServicePort {
	return corev1.ServicePort{Name: name, Port: port, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(port)}
}

func realPort(port int32, class registryv1.PortProtocol) corev1.ServicePort {
	sp := corev1.ServicePort{Port: port, TargetPort: intstr.FromInt32(port)}
	switch class {
	case classHTTP:
		sp.Name, sp.Protocol, sp.AppProtocol = "p"+itoa(port)+"-tcp", corev1.ProtocolTCP, new("http")
	case classTCP:
		sp.Name, sp.Protocol, sp.AppProtocol = "p"+itoa(port)+"-tcp", corev1.ProtocolTCP, new("tcp")
	case classUDP:
		sp.Name, sp.Protocol = "p"+itoa(port)+"-udp", corev1.ProtocolUDP
	}
	return sp
}

func itoa(p int32) string { return strconv.Itoa(int(p)) }

var legacyFour = []corev1.ServicePort{
	legacyPort("mesh", 18081), legacyPort("mesh-tcp", 18082), legacyPort("http", 80), legacyPort("https", 443),
}

func withLegacy(real ...corev1.ServicePort) []corev1.ServicePort {
	return append(append([]corev1.ServicePort{}, legacyFour...), real...)
}

func portsOf(t *testing.T, g *Generator, ns, name string) []corev1.ServicePort {
	t.Helper()
	svc, err := get(t, g.Client, ns, name)
	require.NoError(t, err)
	return svc.Spec.Ports
}

// TestGenerator_ServiceCarriesRealPorts: the generated Service has one port per
// port the service's pods declare, each with its own transport and class,
// beside the four legacy ports and in a fixed order (proposal 043, Phase 1).
func TestGenerator_ServiceCarriesRealPorts(t *testing.T) {
	g, _ := newGen(snapshotOf(catalog{
		key("ns", "mixed"): listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP, 9000: classTCP, 5353: classUDP})),
	}))
	g.reconcile(context.Background())

	// Exact, ordered equality: legacy first, then real ports by number. It
	// covers port = targetPort, the Kubernetes protocol, appProtocol (none on
	// UDP) and the p<port>-<tcp|udp> names in one comparison.
	assert.Equal(t, withLegacy(
		realPort(5353, classUDP),
		realPort(8080, classHTTP),
		realPort(9000, classTCP),
	), portsOf(t, g, "ns", "mixed"))
}

// TestGenerator_RowWithoutPortProtocols: a row written before port_protocols
// existed declares its served set (or its one port) with the class of the
// listing it was read from.
func TestGenerator_RowWithoutPortProtocols(t *testing.T) {
	old := func(ip string, port uint32, served ...uint32) *registryv1.ServiceEndpoint {
		return &registryv1.ServiceEndpoint{
			Ip: ip, ClusterName: "c", Port: port, Ports: served,
			KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: "ns"},
		}
	}
	g, _ := newGen(snapshotOf(catalog{
		key("ns", "db"):   {listTCP: {old("10.0.0.1", 5432, 5432, 5433)}},
		key("ns", "one"):  {listTCP: {old("10.0.0.2", 6379)}},
		key("ns", "dns"):  {listUDP: {old("10.0.0.3", 5353)}},
		key("ns", "http"): {listHTTP: {old("10.0.0.4", 8080, 8080, 9090)}},
	}))
	g.reconcile(context.Background())

	assert.Equal(t, withLegacy(realPort(5432, classTCP), realPort(5433, classTCP)), portsOf(t, g, "ns", "db"))
	assert.Equal(t, withLegacy(realPort(6379, classTCP)), portsOf(t, g, "ns", "one"))
	assert.Equal(t, withLegacy(realPort(5353, classUDP)), portsOf(t, g, "ns", "dns"))
	assert.Equal(t, withLegacy(realPort(8080, classHTTP), realPort(9090, classHTTP)), portsOf(t, g, "ns", "http"))
}

// TestGenerator_PortsAreTheUnionAndLeaveWithTheirLastEndpoint: during a rollout
// that adds or removes a port the Service carries every port some pod declares,
// and a port leaves when the last pod declaring it leaves the registry.
func TestGenerator_PortsAreTheUnionAndLeaveWithTheirLastEndpoint(t *testing.T) {
	oldPod := pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP})
	newPod := pod("ns", "10.0.0.2", 8080, classes{8080: classHTTP, 9090: classHTTP, 9000: classTCP})

	snap := snapshotOf(catalog{key("ns", "web"): listed(oldPod)})
	g, _ := newGen(snap)
	g.reconcile(context.Background())
	require.Equal(t, withLegacy(realPort(8080, classHTTP)), portsOf(t, g, "ns", "web"))

	// The new pod joins: its ports appear on the existing Service.
	snap.Replace(catalog{key("ns", "web"): listed(oldPod, newPod)})
	g.reconcile(context.Background())
	require.Equal(t, withLegacy(realPort(8080, classHTTP), realPort(9000, classTCP), realPort(9090, classHTTP)),
		portsOf(t, g, "ns", "web"))

	// The old pod leaves: nothing changes, the new pod declares all three.
	snap.Replace(catalog{key("ns", "web"): listed(newPod)})
	g.reconcile(context.Background())
	require.Len(t, portsOf(t, g, "ns", "web"), 7)

	// Rolled back: the last pod declaring 9000 and 9090 is gone, and so are
	// the ports. 8080 stays.
	snap.Replace(catalog{key("ns", "web"): listed(oldPod)})
	g.reconcile(context.Background())
	assert.Equal(t, withLegacy(realPort(8080, classHTTP)), portsOf(t, g, "ns", "web"))
}

// TestGenerator_PortsDoNotLeak: a port comes only from a namespaced endpoint
// of the service it is on.
func TestGenerator_PortsDoNotLeak(t *testing.T) {
	foreign := &registryv1.ServiceEndpoint{
		Ip: "192.0.2.9", ClusterName: "c", Port: 9999, Ports: []uint32{9999, 7777},
		PortProtocols: classes{9999: classTCP, 7777: classHTTP},
	}
	oldForeign := &registryv1.ServiceEndpoint{Ip: "192.0.2.10", ClusterName: "c", Port: 6666}
	g, _ := newGen(snapshotOf(catalog{
		// Same name in two namespaces, and a neighbour in one of them.
		key("team-a", "web"): listed(pod("team-a", "10.0.0.1", 8080, classes{8080: classHTTP})),
		key("team-b", "web"): listed(pod("team-b", "10.0.0.2", 9000, classes{9000: classTCP})),
		key("team-a", "api"): listed(pod("team-a", "10.0.0.3", 7070, classes{7070: classHTTP, 5353: classUDP})),
		// A namespaced pod beside two rows that carry no namespace.
		key("team-a", "edge"): {
			listHTTP: {foreign, pod("team-a", "10.0.0.4", 8080, classes{8080: classHTTP}), oldForeign},
			listTCP:  {foreign},
		},
		// Nothing namespaced at all: no Service, as before.
		key("team-a", "ghost"): {listTCP: {foreign}},
		// A port a Service cannot carry is not one.
		key("team-a", "odd"): listed(pod("team-a", "10.0.0.5", 8080, classes{8080: classHTTP, 0: classTCP, 70000: classTCP})),
	}))
	g.reconcile(context.Background())

	assert.Equal(t, withLegacy(realPort(8080, classHTTP)), portsOf(t, g, "team-a", "web"))
	assert.Equal(t, withLegacy(realPort(9000, classTCP)), portsOf(t, g, "team-b", "web"))
	assert.Equal(t, withLegacy(realPort(5353, classUDP), realPort(7070, classHTTP)), portsOf(t, g, "team-a", "api"))
	assert.Equal(t, withLegacy(realPort(8080, classHTTP)), portsOf(t, g, "team-a", "edge"),
		"a row without a namespace contributes no port")
	assert.Equal(t, withLegacy(realPort(8080, classHTTP)), portsOf(t, g, "team-a", "odd"))

	edge, err := get(t, g.Client, "team-a", "edge")
	require.NoError(t, err)
	assert.Equal(t, "8080", edge.Annotations[aetherlabels.AnnotationMeshPort],
		"nor does it decide the frozen port annotation")

	_, err = get(t, g.Client, "team-a", "ghost")
	assert.True(t, apierrors.IsNotFound(err), "a service with no namespaced endpoint gets no Service")
}

// TestGenerator_RealPortReplacesLegacyPlaceholder: the API server rejects two
// ports with one number and protocol, so a real port that lands on a legacy
// placeholder takes its place. A UDP port on the same number does not collide.
func TestGenerator_RealPortReplacesLegacyPlaceholder(t *testing.T) {
	g, _ := newGen(snapshotOf(catalog{
		key("ns", "eighty"): listed(pod("ns", "10.0.0.1", 8080, classes{80: classHTTP, 8080: classHTTP})),
		key("ns", "tls"):    listed(pod("ns", "10.0.0.2", 443, classes{443: classTCP})),
		key("ns", "udp80"):  listed(pod("ns", "10.0.0.3", 80, classes{80: classUDP})),
		key("ns", "all"):    listed(pod("ns", "10.0.0.4", 80, classes{80: classHTTP, 443: classTCP, 18081: classHTTP, 18082: classTCP})),
	}))
	g.reconcile(context.Background())

	assert.Equal(t, []corev1.ServicePort{
		legacyPort("mesh", 18081), legacyPort("mesh-tcp", 18082), legacyPort("https", 443),
		realPort(80, classHTTP), realPort(8080, classHTTP),
	}, portsOf(t, g, "ns", "eighty"), `a pod declaring 80 as HTTP: p80-tcp with appProtocol http, and no "http" placeholder`)

	assert.Equal(t, []corev1.ServicePort{
		legacyPort("mesh", 18081), legacyPort("mesh-tcp", 18082), legacyPort("http", 80),
		realPort(443, classTCP),
	}, portsOf(t, g, "ns", "tls"))

	assert.Equal(t, withLegacy(realPort(80, classUDP)), portsOf(t, g, "ns", "udp80"),
		"80/UDP and the 80/TCP placeholder are different ports to Kubernetes")

	all := portsOf(t, g, "ns", "all")
	assert.Equal(t, []corev1.ServicePort{
		realPort(80, classHTTP), realPort(443, classTCP), realPort(18081, classHTTP), realPort(18082, classTCP),
	}, all)

	// The property the replacement exists for, on every Service generated here.
	for _, name := range []string{"eighty", "tls", "udp80", "all"} {
		type slot struct {
			port     int32
			protocol corev1.Protocol
		}
		seen := map[slot]string{}
		names := map[string]bool{}
		for _, p := range portsOf(t, g, "ns", name) {
			s := slot{p.Port, p.Protocol}
			assert.Empty(t, seen[s], "%s: ports %q and %q share %d/%s", name, seen[s], p.Name, p.Port, p.Protocol)
			seen[s] = p.Name
			assert.False(t, names[p.Name], "%s: duplicate port name %q", name, p.Name)
			names[p.Name] = true
		}
	}
}

// warnings collects the WARN records a generator logs.
type warnings struct {
	mu      sync.Mutex
	records []slog.Record
}

func (w *warnings) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnings) WithAttrs([]slog.Attr) slog.Handler       { return w }
func (w *warnings) WithGroup(string) slog.Handler            { return w }
func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.records = append(w.records, r)
	}
	return nil
}

// take returns the "ports" attribute of each WARN logged since the last call.
func (w *warnings) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []string{}
	for _, r := range w.records {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "ports" {
				out = append(out, a.Value.String())
			}
			return true
		})
	}
	w.records = nil
	return out
}

// TestGenerator_PortClassConflict: when a service's pods declare one port
// number with different classes, the Service carries the class most pods
// declare, ties to HTTP then TCP then UDP, whatever order the pods are listed
// in, and says so once per distinct state.
func TestGenerator_PortClassConflict(t *testing.T) {
	ip := func(i int) string { return "10.0.0." + itoa(int32(i)) }
	// n pods declaring 9000 as class, starting at address first.
	pods := func(first, n int, class registryv1.PortProtocol) []*registryv1.ServiceEndpoint {
		out := make([]*registryv1.ServiceEndpoint, 0, n)
		for i := range n {
			out = append(out, pod("ns", ip(first+i), 9000, classes{9000: class}))
		}
		return out
	}
	join := func(groups ...[]*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
		var out []*registryv1.ServiceEndpoint
		for _, g := range groups {
			out = append(out, g...)
		}
		return out
	}
	reversed := func(in []*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint {
		out := make([]*registryv1.ServiceEndpoint, len(in))
		for i, ep := range in {
			out[len(in)-1-i] = ep
		}
		return out
	}

	tests := []struct {
		name string
		rows []*registryv1.ServiceEndpoint
		want registryv1.PortProtocol
		warn string
	}{
		{"TCP majority", join(pods(1, 1, classHTTP), pods(10, 2, classTCP)), classTCP, "port 9000: http=1 tcp=2, serving tcp"},
		{"HTTP majority", join(pods(1, 3, classHTTP), pods(10, 2, classTCP)), classHTTP, "port 9000: http=3 tcp=2, serving http"},
		{"UDP majority", join(pods(1, 1, classHTTP), pods(10, 1, classTCP), pods(20, 3, classUDP)), classUDP, "port 9000: http=1 tcp=1 udp=3, serving udp"},
		{"a tie goes to HTTP", join(pods(1, 2, classTCP), pods(10, 2, classHTTP)), classHTTP, "port 9000: http=2 tcp=2, serving http"},
		{"a tie without HTTP goes to TCP", join(pods(1, 2, classUDP), pods(10, 2, classTCP)), classTCP, "port 9000: tcp=2 udp=2, serving tcp"},
		{"a three-way tie goes to HTTP", join(pods(1, 1, classUDP), pods(10, 1, classTCP), pods(20, 1, classHTTP)), classHTTP, "port 9000: http=1 tcp=1 udp=1, serving http"},
		{
			// A pod that also serves an HTTP port is read twice, once per
			// listing. It still has one vote: counted per row it would make
			// this 2 to 1 for TCP.
			"a pod in two listings votes once",
			join(
				[]*registryv1.ServiceEndpoint{pod("ns", ip(1), 8080, classes{8080: classHTTP, 9000: classTCP})},
				pods(10, 1, classHTTP),
			),
			classHTTP, "port 9000: http=1 tcp=1, serving http",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, rows := range [][]*registryv1.ServiceEndpoint{tt.rows, reversed(tt.rows)} {
				log := &warnings{}
				g, _ := newGen(snapshotOf(catalog{key("ns", "svc"): listed(rows...)}))
				g.Log = slog.New(log)
				g.reconcile(context.Background())

				var got []corev1.ServicePort
				for _, p := range portsOf(t, g, "ns", "svc") {
					if p.Port == 9000 {
						got = append(got, p)
					}
				}
				assert.Equal(t, []corev1.ServicePort{realPort(9000, tt.want)}, got, "one ServicePort for the number")
				assert.Equal(t, []string{tt.warn}, log.take())
			}
		})
	}
}

// TestGenerator_PortClassConflictWarnsOncePerState: the reconcile loop runs
// every sync interval, and a rollout that retypes a port lasts many of them.
func TestGenerator_PortClassConflictWarnsOncePerState(t *testing.T) {
	a := pod("ns", "10.0.0.1", 9000, classes{9000: classHTTP})
	b := pod("ns", "10.0.0.2", 9000, classes{9000: classTCP})
	c := pod("ns", "10.0.0.3", 9000, classes{9000: classTCP})
	quiet := pod("ns", "10.0.0.9", 8080, classes{8080: classHTTP})

	log := &warnings{}
	snap := snapshotOf(catalog{
		key("ns", "svc"):   listed(a, b),
		key("ns", "quiet"): listed(quiet),
	})
	g, _ := newGen(snap)
	g.Log = slog.New(log)

	g.reconcile(context.Background())
	require.Equal(t, []string{"port 9000: http=1 tcp=1, serving http"}, log.take())

	for range 3 {
		g.reconcile(context.Background())
	}
	require.Empty(t, log.take(), "the same disagreement is not logged again")

	// A different state of the same disagreement is news.
	snap.Replace(catalog{key("ns", "svc"): listed(a, b, c), key("ns", "quiet"): listed(quiet)})
	g.reconcile(context.Background())
	g.reconcile(context.Background())
	require.Equal(t, []string{"port 9000: http=1 tcp=2, serving tcp"}, log.take())

	// Resolved: silence, and the Service has the one class left.
	snap.Replace(catalog{key("ns", "svc"): listed(b, c), key("ns", "quiet"): listed(quiet)})
	g.reconcile(context.Background())
	require.Empty(t, log.take())
	require.Equal(t, withLegacy(realPort(9000, classTCP)), portsOf(t, g, "ns", "svc"))

	// It comes back in a state already seen once: logged again, because it
	// went away in between.
	snap.Replace(catalog{key("ns", "svc"): listed(a, b, c), key("ns", "quiet"): listed(quiet)})
	g.reconcile(context.Background())
	assert.Equal(t, []string{"port 9000: http=1 tcp=2, serving tcp"}, log.take())
}

// managed is a Service the generator already owns, with the given ports and
// the frozen annotations of an HTTP service on 8080.
func managed(ns, name string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels: map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService:     name,
				aetherlabels.AnnotationMeshPort:        "8080",
				aetherlabels.AnnotationMeshAppProtocol: AppProtocolHTTP,
			},
		},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: ports},
	}
}

// TestGenerator_ConvergesTargetPortAndAppProtocol: a Service whose ports have
// the right names, numbers and protocols but the wrong targetPort or
// appProtocol is drift. The comparison used to ignore both.
func TestGenerator_ConvergesTargetPortAndAppProtocol(t *testing.T) {
	want := withLegacy(realPort(8080, classHTTP), realPort(9000, classTCP))
	mutate := func(f func(ports []corev1.ServicePort)) []corev1.ServicePort {
		ports := make([]corev1.ServicePort, len(want))
		copy(ports, want)
		f(ports)
		return ports
	}
	tests := []struct {
		name  string
		ports []corev1.ServicePort
	}{
		{"a raw-TCP port marked http", mutate(func(p []corev1.ServicePort) { p[5].AppProtocol = new("http") })},
		{"an HTTP port marked tcp", mutate(func(p []corev1.ServicePort) { p[4].AppProtocol = new("tcp") })},
		{"a real port with no appProtocol", mutate(func(p []corev1.ServicePort) { p[4].AppProtocol = nil })},
		{"a legacy port with an appProtocol", mutate(func(p []corev1.ServicePort) { p[0].AppProtocol = new("http") })},
		{"a real port with another targetPort", mutate(func(p []corev1.ServicePort) { p[5].TargetPort = intstr.FromInt32(9001) })},
		{"a real port with a named targetPort", mutate(func(p []corev1.ServicePort) { p[5].TargetPort = intstr.FromString("9000") })},
		{"a legacy port with another targetPort", mutate(func(p []corev1.ServicePort) { p[2].TargetPort = intstr.FromInt32(8080) })},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, _ := newGen(snapshotOf(catalog{
				key("ns", "svc"): listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP, 9000: classTCP})),
			}), managed("ns", "svc", tt.ports...))
			g.reconcile(context.Background())
			assert.Equal(t, want, portsOf(t, g, "ns", "svc"))
		})
	}
}

// TestGenerator_ConvergedServiceIsNotRewritten: the API server may return the
// ports in any order, and a reorder is not drift. Without this the generator
// would write every Service on every reconcile.
func TestGenerator_ConvergedServiceIsNotRewritten(t *testing.T) {
	want := withLegacy(realPort(8080, classHTTP), realPort(9000, classTCP))
	shuffled := []corev1.ServicePort{want[5], want[0], want[4], want[3], want[1], want[2]}

	g, c := newGen(snapshotOf(catalog{
		key("ns", "svc"): listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP, 9000: classTCP})),
	}), managed("ns", "svc", shuffled...))
	before, err := get(t, c, "ns", "svc")
	require.NoError(t, err)

	g.reconcile(context.Background())
	g.reconcile(context.Background())

	after, err := get(t, c, "ns", "svc")
	require.NoError(t, err)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion, "a converged Service must not be updated")
	assert.Equal(t, shuffled, after.Spec.Ports)

	// Control: the same check does see a write when there is drift.
	drifted := managed("ns", "other", shuffled[1:]...)
	g, c = newGen(snapshotOf(catalog{
		key("ns", "other"): listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP, 9000: classTCP})),
	}), drifted)
	before, err = get(t, c, "ns", "other")
	require.NoError(t, err)
	g.reconcile(context.Background())
	after, err = get(t, c, "ns", "other")
	require.NoError(t, err)
	assert.NotEqual(t, before.ResourceVersion, after.ResourceVersion)
}

// TestGenerator_FrozenAnnotations: aether.io/app-protocol and aether.io/port
// are what a chart 2.6 registrar stamped on the same backend, because only a
// 2.6 agent reads them (proposal 043, Phase 1).
//
// On a write-based backend that is the first listing the service is in. On the
// kubernetes backend the 2.6 listings held a pod under the class of its
// declared port only, so it is that class. The two differ exactly for a pod
// whose declared port is not HTTP and which also serves an HTTP port.
func TestGenerator_FrozenAnnotations(t *testing.T) {
	old := func(ip string, port uint32) *registryv1.ServiceEndpoint {
		return &registryv1.ServiceEndpoint{
			Ip: ip, ClusterName: "c", Port: port,
			KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: "ns"},
		}
	}
	httpPod := pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP})
	tcpFirst := pod("ns", "10.0.0.2", 9000, classes{9000: classTCP, 8080: classHTTP})

	tests := []struct {
		name     string
		listings map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint
		// want per rule: app-protocol, port.
		firstListing [2]string
		declaredPort [2]string
	}{
		{
			name:         "HTTP only",
			listings:     listed(httpPod),
			firstListing: [2]string{"http", "8080"},
			declaredPort: [2]string{"http", "8080"},
		},
		{
			name:         "raw TCP only",
			listings:     listed(pod("ns", "10.0.0.1", 5432, classes{5432: classTCP})),
			firstListing: [2]string{"tcp", "5432"},
			declaredPort: [2]string{"tcp", "5432"},
		},
		{
			name:         "UDP only",
			listings:     listed(pod("ns", "10.0.0.1", 5353, classes{5353: classUDP})),
			firstListing: [2]string{"udp", "5353"},
			declaredPort: [2]string{"udp", "5353"},
		},
		{
			name:         "HTTP declared port with a raw-TCP port",
			listings:     listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP, 9000: classTCP})),
			firstListing: [2]string{"http", "8080"},
			declaredPort: [2]string{"http", "8080"},
		},
		{
			// The case the rule exists for.
			name:         "raw-TCP declared port with an HTTP port",
			listings:     listed(tcpFirst),
			firstListing: [2]string{"http", "9000"},
			declaredPort: [2]string{"tcp", "9000"},
		},
		{
			name:         "UDP declared port with a raw-TCP port",
			listings:     listed(pod("ns", "10.0.0.1", 5353, classes{5353: classUDP, 9000: classTCP})),
			firstListing: [2]string{"tcp", "5353"},
			declaredPort: [2]string{"udp", "5353"},
		},
		{
			// A 2.6 kubernetes HTTP listing held only the HTTP-declared pod,
			// so its port is the one stamped, wherever the mixed pod sorts.
			// On a write-based backend "the first endpoint" of a listing is
			// the one with the lowest (cluster, address), whatever order the
			// snapshot hands the rows over in: it does so in map order, and
			// the annotation must not move with it.
			name: "pods that disagree",
			listings: map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				listHTTP: {tcpFirst, httpPod},
				listTCP:  {tcpFirst},
			},
			firstListing: [2]string{"http", "8080"},
			declaredPort: [2]string{"http", "8080"},
		},
		{
			name:         "a row without port_protocols has the class of its listing",
			listings:     map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{listTCP: {old("10.0.0.1", 6379)}},
			firstListing: [2]string{"tcp", "6379"},
			declaredPort: [2]string{"tcp", "6379"},
		},
		{
			// Not a row either backend writes: its declared port is of a class
			// it is not listed under. The service keeps its Service.
			name: "a declared port of a class the row is not listed under",
			listings: map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
				listHTTP: {pod("ns", "10.0.0.1", 9000, classes{9000: classTCP, 8080: classHTTP})},
			},
			firstListing: [2]string{"http", "9000"},
			declaredPort: [2]string{"http", "9000"},
		},
	}
	for _, tt := range tests {
		for rule, want := range map[LegacyAnnotationRule][2]string{
			LegacyAnnotationsByFirstListing: tt.firstListing,
			LegacyAnnotationsByDeclaredPort: tt.declaredPort,
		} {
			name := tt.name + "/first listing"
			if rule == LegacyAnnotationsByDeclaredPort {
				name = tt.name + "/declared port"
			}
			t.Run(name, func(t *testing.T) {
				g, c := newGen(snapshotOf(catalog{key("ns", "svc"): tt.listings}))
				g.LegacyAnnotations = rule
				g.reconcile(context.Background())

				svc, err := get(t, c, "ns", "svc")
				require.NoError(t, err)
				assert.Equal(t, want[0], svc.Annotations[aetherlabels.AnnotationMeshAppProtocol])
				assert.Equal(t, want[1], svc.Annotations[aetherlabels.AnnotationMeshPort])
				assert.Equal(t, "svc", svc.Annotations[aetherlabels.AnnotationMeshService])
			})
		}
	}
}

// TestGenerator_FrozenAnnotationDoesNotTypeThePorts: the annotation is one
// value per Service and is not where a port's class comes from. A raw-TCP
// declared port beside an HTTP port keeps both classes on the ports whichever
// way the annotation is frozen.
func TestGenerator_FrozenAnnotationDoesNotTypeThePorts(t *testing.T) {
	for _, rule := range []LegacyAnnotationRule{LegacyAnnotationsByFirstListing, LegacyAnnotationsByDeclaredPort} {
		g, _ := newGen(snapshotOf(catalog{
			key("ns", "svc"): listed(pod("ns", "10.0.0.2", 9000, classes{9000: classTCP, 8080: classHTTP})),
		}))
		g.LegacyAnnotations = rule
		g.reconcile(context.Background())
		assert.Equal(t, withLegacy(realPort(8080, classHTTP), realPort(9000, classTCP)), portsOf(t, g, "ns", "svc"), "rule %d", rule)
	}
}

// TestGenerator_ManagedServiceWithoutAnnotations: a Service that carries the
// managed label and no annotation at all (someone removed them) is converged,
// not a write to a nil map.
func TestGenerator_ManagedServiceWithoutAnnotations(t *testing.T) {
	bare := managed("ns", "svc")
	bare.Annotations = nil
	g, c := newGen(snapshotOf(catalog{
		key("ns", "svc"): listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP})),
	}), bare)
	require.NotPanics(t, func() { g.reconcile(context.Background()) })

	svc, err := get(t, c, "ns", "svc")
	require.NoError(t, err)
	assert.Equal(t, "8080", svc.Annotations[aetherlabels.AnnotationMeshPort])
	assert.Equal(t, AppProtocolHTTP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol])
	assert.Equal(t, withLegacy(realPort(8080, classHTTP)), svc.Spec.Ports)
}
