package services

// Properties of the generated Service as an object in the cluster: the API
// server accepts it, a converged one is never written, each stale annotation is
// drift on its own, and a Service another generator owns is left alone.

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"

	aetherlabels "aethermesh.dev/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// apiServerPortErrors applies the API server's ServicePort rules that a
// generated port list could break: a name on every port of a multi-port
// Service, valid and unique names, valid numbers, one port per (number,
// protocol), a numeric targetPort.
func apiServerPortErrors(ports []corev1.ServicePort) []string {
	var errs []string
	names := map[string]bool{}
	type slot struct {
		port     int32
		protocol corev1.Protocol
	}
	slots := map[slot]string{}
	for _, p := range ports {
		if len(ports) > 1 && p.Name == "" {
			errs = append(errs, "port without a name")
		}
		if len(p.Name) > 63 || !dns1123Label.MatchString(p.Name) {
			errs = append(errs, "invalid name "+p.Name)
		}
		if names[p.Name] {
			errs = append(errs, "duplicate name "+p.Name)
		}
		names[p.Name] = true
		if p.Port < 1 || p.Port > 65535 {
			errs = append(errs, fmt.Sprintf("invalid port %d", p.Port))
		}
		if p.Protocol != corev1.ProtocolTCP && p.Protocol != corev1.ProtocolUDP {
			errs = append(errs, "invalid protocol "+string(p.Protocol))
		}
		s := slot{p.Port, p.Protocol}
		if other, dup := slots[s]; dup {
			errs = append(errs, fmt.Sprintf("%s and %s share %d/%s", other, p.Name, p.Port, p.Protocol))
		}
		slots[s] = p.Name
		if p.TargetPort.Type != intstr.Int || p.TargetPort.IntVal < 1 || p.TargetPort.IntVal > 65535 {
			errs = append(errs, fmt.Sprintf("invalid targetPort %v", p.TargetPort))
		}
	}
	return errs
}

// randomPods is n pods of one service that may disagree about everything: the
// pod-level protocol, the declared port, and the class of any port number.
func randomPods(r *rand.Rand, n int) []declaredPod {
	protocols := []string{"", "http", "tcp", "udp"}
	numbers := []string{"1", "80", "443", "8080", "9000", "18081", "18082", "65535", "5353"}
	suffixes := []string{"", "", "=h1", "=h2", "=tcp", "=udp"}
	pods := make([]declaredPod, 0, n)
	for i := range n {
		port := ""
		if r.Intn(3) > 0 {
			port = numbers[r.Intn(len(numbers))]
		}
		var entries []string
		for range r.Intn(5) {
			entries = append(entries, numbers[r.Intn(len(numbers))]+suffixes[r.Intn(len(suffixes))])
		}
		pods = append(pods, declaredPod{
			ip:          fmt.Sprintf("10.0.%d.%d", r.Intn(3), i+1),
			annotations: annotated(protocols[r.Intn(len(protocols))], port, strings.Join(entries, ",")),
		})
	}
	return pods
}

// TestGenerator_ServiceIsValidStableAndOrderIndependent: for random services
// (one to five pods that may disagree about everything), under both annotation
// rules, the desired Service passes the API server's port rules and is the same
// whatever order the rows arrive in (the snapshot hands them over in map
// order), and a second and third reconcile write nothing.
func TestGenerator_ServiceIsValidStableAndOrderIndependent(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := range 400 {
		pods := randomPods(r, 1+r.Intn(5))
		for _, rule := range []LegacyAnnotationRule{LegacyAnnotationsByFirstListing, LegacyAnnotationsByDeclaredPort} {
			requireOrderIndependent(t, r, rule, pods)
			if i%8 == 0 {
				requireNoRewrite(t, rule, pods)
			}
		}
	}
}

func requireOrderIndependent(t *testing.T, r *rand.Rand, rule LegacyAnnotationRule, pods []declaredPod) {
	t.Helper()
	var first desiredService
	for attempt := range 12 {
		shuffled := slices.Clone(pods)
		r.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		d := desiredFor(t, rule, listingsEveryClass(t, shuffled))
		if errs := apiServerPortErrors(d.ports); len(errs) > 0 {
			t.Fatalf("rule %d: the API server would reject the Service: %v\npods: %v\nports: %v", rule, errs, pods, d.ports)
		}
		if attempt == 0 {
			first = d
			continue
		}
		require.Equal(t, first, d, "rule %d: the desired Service depends on the order of the rows\npods: %v", rule, pods)
	}
}

func requireNoRewrite(t *testing.T, rule LegacyAnnotationRule, pods []declaredPod) {
	t.Helper()
	g, c := newGen(snapshotOf(catalog{key("ns", "svc"): listingsEveryClass(t, pods)}))
	g.LegacyAnnotations = rule
	g.reconcile(context.Background())
	svc, err := get(t, c, "ns", "svc")
	require.NoError(t, err)
	g.reconcile(context.Background())
	g.reconcile(context.Background())
	again, err := get(t, c, "ns", "svc")
	require.NoError(t, err)
	require.Equal(t, svc.ResourceVersion, again.ResourceVersion, "rule %d: a converged Service was rewritten\npods: %v", rule, pods)
}

// countingGen is a generator whose client counts the Update calls it receives.
func countingGen(t *testing.T, c catalog, objs ...client.Object) (*Generator, *int) {
	t.Helper()
	updates := 0
	cl := fake.NewClientBuilder().WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates++
			return cl.Update(ctx, obj, opts...)
		},
	}).Build()
	g, _ := newGen(snapshotOf(c))
	g.Client = cl
	return g, &updates
}

// TestGenerator_EachFrozenAnnotationIsDriftOnItsOwn: a managed Service whose
// ports are already right is updated when aether.io/app-protocol alone is
// stale, and when aether.io/port alone is stale, once each; and a Service with
// both annotations and the ports right is not written at all. A 2.6 agent
// reads the first annotation to choose a chain, so one left stale is a wrong
// chain for as long as the Service lives.
func TestGenerator_EachFrozenAnnotationIsDriftOnItsOwn(t *testing.T) {
	want := withLegacy(realPort(8080, classHTTP), realPort(9000, classTCP))
	cat := catalog{key("ns", "svc"): listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP, 9000: classTCP}))}

	tests := []struct {
		name string
		// stale edits the annotations of an otherwise converged Service.
		stale       func(a map[string]string)
		wantUpdates int
	}{
		{"fully converged", func(map[string]string) {}, 0},
		{"a stale app-protocol", func(a map[string]string) { a[aetherlabels.AnnotationMeshAppProtocol] = AppProtocolTCP }, 1},
		{"no app-protocol", func(a map[string]string) { delete(a, aetherlabels.AnnotationMeshAppProtocol) }, 1},
		{"a stale port", func(a map[string]string) { a[aetherlabels.AnnotationMeshPort] = "9000" }, 1},
		{"no port", func(a map[string]string) { delete(a, aetherlabels.AnnotationMeshPort) }, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := managed("ns", "svc", want...)
			tt.stale(existing.Annotations)
			g, updates := countingGen(t, cat, existing)

			g.reconcile(context.Background())
			assert.Equal(t, tt.wantUpdates, *updates, "Update calls on the first reconcile")

			svc, err := get(t, g.Client, "ns", "svc")
			require.NoError(t, err)
			assert.Equal(t, AppProtocolHTTP, svc.Annotations[aetherlabels.AnnotationMeshAppProtocol])
			assert.Equal(t, "8080", svc.Annotations[aetherlabels.AnnotationMeshPort])
			assert.Equal(t, want, svc.Spec.Ports)

			g.reconcile(context.Background())
			assert.Equal(t, tt.wantUpdates, *updates, "a second reconcile writes nothing")
		})
	}
}

// TestGenerator_LeavesClustersetServiceAlone: the MCS import generator labels
// its <svc>-clusterset Service with both clusterset-service and mesh-service.
// It is not a registry service, and this generator must not prune it: it did,
// on every reconcile, and the import generator made it again with a new
// address. A mesh Service that is merely stale is still pruned.
func TestGenerator_LeavesClustersetServiceAlone(t *testing.T) {
	clusterset := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web" + aetherlabels.ClustersetServiceNameSuffix, Namespace: "ns",
			Labels: map[string]string{
				aetherlabels.LabelClustersetService: "true",
				aetherlabels.LabelMeshService:       "true",
			},
		},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "mesh", Port: 18081, Protocol: corev1.ProtocolTCP}}},
	}
	stale := managed("ns", "gone", legacyFour...)
	g, c := newGen(snapshotOf(catalog{
		key("ns", "web"): listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP})),
	}), clusterset, stale)
	g.reconcile(context.Background())

	kept, err := get(t, c, "ns", clusterset.Name)
	require.NoError(t, err, "the clusterset VIP Service was deleted")
	assert.Equal(t, clusterset.Spec.Ports, kept.Spec.Ports, "the clusterset VIP Service was rewritten")

	_, err = get(t, c, "ns", "gone")
	assert.True(t, apierrors.IsNotFound(err), "a stale mesh Service is pruned")
}
