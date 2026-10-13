package services

// The generator against a chart 2.6 registrar: the two frozen annotations are
// what 2.6 stamped, shape by shape and also while the listings are partial, and
// each version sees a Service the other wrote as drift.

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"strconv"
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/registry"
	"aethermesh.dev/registry/endpointmeta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// declaredPod is a pod as its manifest declares it.
type declaredPod struct {
	ip          string
	annotations map[string]string
	// parsed is the pod's row when it was built ahead (parsedPod), so a test
	// that uses one pod in thousands of services parses it once.
	parsed *registryv1.ServiceEndpoint
}

func parsedPod(t testing.TB, ip string, annotations map[string]string) declaredPod {
	t.Helper()
	p := declaredPod{ip: ip, annotations: annotations}
	p.parsed = p.row(t)
	return p
}

func (p declaredPod) String() string {
	return fmt.Sprintf("{protocol=%q port=%q ports=%q}",
		p.annotations[aetherannotations.AnnotationEndpointProtocol],
		p.annotations[aetherannotations.AnnotationEndpointPort],
		p.annotations[aetherannotations.AnnotationEndpointPorts])
}

func annotated(protocol, port, ports string) map[string]string {
	a := map[string]string{}
	if protocol != "" {
		a[aetherannotations.AnnotationEndpointProtocol] = protocol
	}
	if port != "" {
		a[aetherannotations.AnnotationEndpointPort] = port
	}
	if ports != "" {
		a[aetherannotations.AnnotationEndpointPorts] = ports
	}
	return a
}

// row builds the registry row both backends build from the annotations: they
// use these same parsers.
func (p declaredPod) row(t testing.TB) *registryv1.ServiceEndpoint {
	t.Helper()
	if p.parsed != nil {
		return p.parsed
	}
	port, err := endpointmeta.Port(p.annotations)
	require.NoError(t, err)
	ports, err := endpointmeta.Ports(p.annotations, port)
	require.NoError(t, err)
	pp, err := endpointmeta.PortProtocols(p.annotations)
	require.NoError(t, err)
	return &registryv1.ServiceEndpoint{
		Ip: p.ip, ClusterName: "c", Port: uint32(port), Ports: ports, PortProtocols: pp,
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{Namespace: "ns"},
	}
}

type protocolListings = map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint

// listingsEveryClass is the 2.7 listing of both backends, and the etcd listing
// of 2.6 too: a pod under every class it serves a port of.
func listingsEveryClass(t testing.TB, pods []declaredPod) protocolListings {
	out := protocolListings{}
	for _, p := range pods {
		r := p.row(t)
		for _, proto := range endpointmeta.ProtocolsServed(r.GetPortProtocols()) {
			out[proto] = append(out[proto], r)
		}
	}
	return out
}

// listingsPodLevel is the 2.6 kubernetes listing: a pod under its pod-level
// protocol and nowhere else.
func listingsPodLevel(t testing.TB, pods []declaredPod) protocolListings {
	out := protocolListings{}
	for _, p := range pods {
		proto, err := endpointmeta.Protocol(p.annotations)
		require.NoError(t, err)
		out[proto] = append(out[proto], p.row(t))
	}
	return out
}

// stamped26 is what the 2.6 generator could stamp: the class of the first
// listing holding a namespaced endpoint, and the port of ANY endpoint of that
// listing (it took the first in map order).
func stamped26(listings protocolListings) (appProto string, ports map[string]bool) {
	for _, proto := range registry.ServedProtocols {
		eps := namespaced(listings[proto])
		if len(eps) == 0 {
			continue
		}
		ports = map[string]bool{}
		for _, ep := range eps {
			ports[strconv.Itoa(int(ep.GetPort()))] = true
		}
		return protocolAppProtocol[proto], ports
	}
	return "", nil
}

// annotationShapes is every combination of the three endpoint annotations over
// a small vocabulary: 168 pods.
func annotationShapes() []map[string]string {
	var out []map[string]string
	for _, protocol := range []string{"", "http", "tcp", "udp"} {
		for _, port := range []string{"", "8080", "9000"} {
			for _, ports := range []string{
				"", "9000", "8080", "8080,9000", "9000=tcp", "8080=tcp", "8080=h2", "9000=h1",
				"8080=udp", "5353=udp", "8080,9000=tcp", "9000,8080=h1", "8080=tcp,9000=h2",
				"9000=tcp,8080=h1,5353=udp",
			} {
				out = append(out, annotated(protocol, port, ports))
			}
		}
	}
	return out
}

func desiredFor(t testing.TB, rule LegacyAnnotationRule, listings protocolListings) desiredService {
	t.Helper()
	g := &Generator{
		Snapshot: snapshotOf(catalog{key("ns", "svc"): listings}), MeshPort: 18081,
		Log: slog.New(slog.DiscardHandler), LegacyAnnotations: rule,
	}
	d, ok := g.buildDesiredServices(context.Background())[client.ObjectKey{Namespace: "ns", Name: "svc"}]
	require.True(t, ok, "the service has no desired Service")
	return d
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// contradictsPodLevel reports whether a pod's declared port carries a suffix
// that contradicts its pod-level protocol. That is the one case where the
// kubernetes rule is allowed to differ from 2.6 (proposal 043, Phase 1 as
// built): 2.6 listed such a pod under the pod-level protocol, and the rule
// reads the class of the declared port.
func contradictsPodLevel(t testing.TB, pods []declaredPod) bool {
	for _, p := range pods {
		level, err := endpointmeta.Protocol(p.annotations)
		require.NoError(t, err)
		r := p.row(t)
		if registryv1.Service_Protocol(r.GetPortProtocols()[r.GetPort()]) != level {
			return true
		}
	}
	return false
}

// TestGenerator_FrozenAnnotationsMatchChart26 holds the two frozen annotations
// to what a chart 2.6 registrar stamped on the same backend, for every
// annotation shape alone and for every ordered pair of shapes as two pods of
// one service. A difference fails unless it is the one stated deviation
// (contradictsPodLevel, kubernetes only).
func TestGenerator_FrozenAnnotationsMatchChart26(t *testing.T) {
	var firsts, seconds []declaredPod
	for _, a := range annotationShapes() {
		firsts = append(firsts, parsedPod(t, "10.0.0.1", a))
		seconds = append(seconds, parsedPod(t, "10.0.0.2", a))
	}
	var groups [][]declaredPod
	for _, a := range firsts {
		groups = append(groups, []declaredPod{a})
	}
	for i, a := range firsts {
		for j, b := range seconds {
			if i != j {
				groups = append(groups, []declaredPod{a, b})
			}
		}
	}

	for _, be := range []struct {
		name string
		rule LegacyAnnotationRule
		// chart26 is the listing a 2.6 registrar read on this backend.
		chart26 func(testing.TB, []declaredPod) protocolListings
		// deviates says whether a difference for these pods is the stated one.
		deviates func(testing.TB, []declaredPod) bool
	}{
		{"etcd", LegacyAnnotationsByFirstListing, listingsEveryClass, func(testing.TB, []declaredPod) bool { return false }},
		{"kubernetes", LegacyAnnotationsByDeclaredPort, listingsPodLevel, contradictsPodLevel},
	} {
		t.Run(be.name, func(t *testing.T) {
			same, stated := 0, 0
			for _, pods := range groups {
				wantProto, wantPorts := stamped26(be.chart26(t, pods))
				d := desiredFor(t, be.rule, listingsEveryClass(t, pods))
				gotPort := strconv.Itoa(int(d.port))
				switch {
				case d.appProtocol == wantProto && wantPorts[gotPort]:
					same++
				case be.deviates(t, pods):
					stated++
				default:
					t.Errorf("%v: 2.6 stamped %s/%v, 2.7 stamps %s/%s", pods, wantProto, keysOf(wantPorts), d.appProtocol, gotPort)
				}
			}
			// The comparison is not vacuous: most shapes are held to 2.6.
			assert.Greater(t, same, len(groups)/2, "%d of %d groups compared equal (%d the stated deviation)", same, len(groups), stated)
		})
	}
}

// TestGenerator_DeclaredPortRuleOnAPartialListing: under the declared-port rule
// the frozen annotations are those of the DECLARED port also when the generator
// sees the pod in only some of its listings. The kubernetes backend is listed
// one protocol at a time and a registration lands one protocol at a time, so a
// snapshot can hold a mixed pod in one of its two listings for a moment. A 2.6
// agent that read "tcp" on an HTTP-declared service in that moment would build
// a raw-TCP chain on its address.
func TestGenerator_DeclaredPortRuleOnAPartialListing(t *testing.T) {
	httpDeclared := pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP, 9000: classTCP})
	tcpDeclared := pod("ns", "10.0.0.2", 9000, classes{9000: classTCP, 8080: classHTTP})
	udpDeclared := pod("ns", "10.0.0.3", 5353, classes{5353: classUDP, 8080: classHTTP, 9000: classTCP})
	for _, tt := range []struct {
		name      string
		listings  protocolListings
		wantProto string
		wantPort  uint32
	}{
		{"an HTTP-declared pod seen in the TCP listing only", protocolListings{listTCP: {httpDeclared}}, "http", 8080},
		{"a TCP-declared pod seen in the HTTP listing only", protocolListings{listHTTP: {tcpDeclared}}, "tcp", 9000},
		{"a UDP-declared pod seen in the HTTP listing only", protocolListings{listHTTP: {udpDeclared}}, "udp", 5353},
		{"a UDP-declared pod seen in the TCP listing only", protocolListings{listTCP: {udpDeclared}}, "udp", 5353},
		{
			// The 2.6 order still holds among declared classes: HTTP first.
			"an HTTP-declared pod in the TCP listing beside a TCP-declared pod in both",
			protocolListings{listHTTP: {tcpDeclared}, listTCP: {tcpDeclared, httpDeclared}},
			"http", 8080,
		},
		{
			// The whole listings give the same answer as either half.
			"an HTTP-declared pod in both listings",
			protocolListings{listHTTP: {httpDeclared}, listTCP: {httpDeclared}},
			"http", 8080,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := desiredFor(t, LegacyAnnotationsByDeclaredPort, tt.listings)
			assert.Equal(t, tt.wantProto, d.appProtocol, "the frozen app-protocol is the class of the declared port")
			assert.Equal(t, tt.wantPort, d.port, "the frozen port is the declared port")
		})
	}
}

// TestGenerator_DeclaredPortRulePicksTheFirstEndpointOfTheClass: when several
// pods declare a port of the winning class, the frozen port is that of the
// first by (cluster, address), whichever listings each one is seen in.
func TestGenerator_DeclaredPortRulePicksTheFirstEndpointOfTheClass(t *testing.T) {
	first := pod("ns", "10.0.0.1", 9000, classes{9000: classTCP, 8080: classHTTP})
	second := pod("ns", "10.0.0.2", 9001, classes{9001: classTCP, 8080: classHTTP})
	for name, listings := range map[string]protocolListings{
		"whole listings":                  {listHTTP: {second, first}, listTCP: {second, first}},
		"the first pod in HTTP only":      {listHTTP: {first}, listTCP: {second}},
		"the first pod in TCP only":       {listHTTP: {second}, listTCP: {first}},
		"both pods in the HTTP listing":   {listHTTP: {second, first}},
		"the second pod in both listings": {listHTTP: {second}, listTCP: {second, first}},
	} {
		t.Run(name, func(t *testing.T) {
			d := desiredFor(t, LegacyAnnotationsByDeclaredPort, listings)
			assert.Equal(t, "tcp", d.appProtocol)
			assert.Equal(t, uint32(9000), d.port)
		})
	}
}

// samePorts26 is the 2.6 generator's port comparison: names, numbers and
// Kubernetes protocols, and neither targetPort nor appProtocol.
func samePorts26(got, want []corev1.ServicePort) bool {
	if len(got) != len(want) {
		return false
	}
	index := make(map[string]corev1.ServicePort, len(got))
	for _, p := range got {
		index[p.Name] = p
	}
	for _, w := range want {
		g, ok := index[w.Name]
		if !ok || g.Port != w.Port || g.Protocol != w.Protocol {
			return false
		}
	}
	return true
}

// legacyFour26 is the whole port list a 2.6 generator wrote.
func legacyFour26() []corev1.ServicePort {
	return []corev1.ServicePort{
		{Name: "mesh", Port: 18081, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(18081)},
		{Name: "mesh-tcp", Port: mesh.ProxyL4OutboundPort, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(mesh.ProxyL4OutboundPort)},
		{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(80)},
		{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(443)},
	}
}

// TestGenerator_Chart26AndChart27SeeEachOthersPortsAsDrift: a rollback and an
// upgrade both converge. Whatever ports a 2.7 generator left, the 2.6
// comparison calls drift, so a 2.6 registrar rewrites them to its four; and the
// four a 2.6 generator left are drift for 2.7, because every service has a real
// port.
func TestGenerator_Chart26AndChart27SeeEachOthersPortsAsDrift(t *testing.T) {
	r := rand.New(rand.NewSource(43))
	for i := range 500 {
		pods := randomPods(r, 1+r.Intn(4))
		ports := desiredFor(t, LegacyAnnotationsByFirstListing, listingsEveryClass(t, pods)).ports
		if samePorts26(ports, legacyFour26()) {
			t.Fatalf("case %d: a 2.6 registrar would leave the 2.7 ports as they are: %v\npods %v", i, ports, pods)
		}
		if samePorts(legacyFour26(), ports) {
			t.Fatalf("case %d: a 2.7 registrar would leave the 2.6 ports as they are, want %v\npods %v", i, ports, pods)
		}
	}
}
