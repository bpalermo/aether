package services

// The generated mesh Service says what the service serves with its PORTS, and
// with nothing else. Through chart 2.7.0 it also carried aether.io/app-protocol
// and aether.io/port, one protocol and one port for the whole service, which a
// chart 2.6 agent read to choose a capture chain. Nothing reads them, the
// generator no longer writes them, and it removes them from the Services it
// owns.

import (
	"context"
	"testing"

	aetherlabels "aethermesh.dev/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// staleAnnotations are the two annotations an older registrar stamped.
var staleAnnotations = []string{aetherlabels.AnnotationMeshPort, aetherlabels.AnnotationMeshAppProtocol}

// TestGenerator_NewServiceCarriesNoServiceLevelProtocol: a Service the
// generator creates has aether.io/service and neither of the two annotations
// that gave the service one protocol and one port, whatever the service
// serves.
func TestGenerator_NewServiceCarriesNoServiceLevelProtocol(t *testing.T) {
	g, c := newGen(snapshotOf(catalog{
		key("ns", "web"):   listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP})),
		key("ns", "db"):    listed(pod("ns", "10.0.0.2", 5432, classes{5432: classTCP})),
		key("ns", "dns"):   listed(pod("ns", "10.0.0.3", 5353, classes{5353: classUDP})),
		key("ns", "mixed"): listed(pod("ns", "10.0.0.4", 9000, classes{9000: classTCP, 8080: classHTTP})),
	}))
	g.reconcile(context.Background())

	for _, name := range []string{"web", "db", "dns", "mixed"} {
		svc, err := get(t, c, "ns", name)
		require.NoError(t, err, name)
		assert.Equal(t, map[string]string{aetherlabels.AnnotationMeshService: name}, svc.Annotations, name)
	}
}

// TestGenerator_RemovesStaleAnnotationsOnce: a Service the generator owns that
// still carries the annotations of an older registrar (chart 2.6, or 2.7.0)
// loses them on the next reconcile, with exactly one Update, and is not
// written again. Each annotation is removed on its own too, and everything
// else on the Service is kept: aether.io/service, an annotation someone else
// put there, the ports.
func TestGenerator_RemovesStaleAnnotationsOnce(t *testing.T) {
	want := withLegacy(realPort(8080, classHTTP), realPort(9000, classTCP))
	cat := catalog{key("ns", "svc"): listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP, 9000: classTCP}))}

	tests := []struct {
		name        string
		annotations map[string]string
		wantUpdates int
	}{
		{"both, as chart 2.6 stamped an HTTP service", map[string]string{
			aetherlabels.AnnotationMeshPort: "8080", aetherlabels.AnnotationMeshAppProtocol: "http",
		}, 1},
		{"both, with other values", map[string]string{
			aetherlabels.AnnotationMeshPort: "9000", aetherlabels.AnnotationMeshAppProtocol: "tcp",
		}, 1},
		{"aether.io/port alone", map[string]string{aetherlabels.AnnotationMeshPort: "8080"}, 1},
		{"aether.io/app-protocol alone", map[string]string{aetherlabels.AnnotationMeshAppProtocol: "http"}, 1},
		{"an empty value is still the annotation", map[string]string{aetherlabels.AnnotationMeshPort: ""}, 1},
		{"neither: nothing to do, nothing written", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "svc", Namespace: "ns",
					Labels:      map[string]string{aetherlabels.LabelMeshService: "true"},
					Annotations: map[string]string{aetherlabels.AnnotationMeshService: "svc", "example.com/kept": "yes"},
				},
				Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: want},
			}
			for k, v := range tt.annotations {
				existing.Annotations[k] = v
			}
			g, updates := countingGen(t, cat, existing)

			g.reconcile(context.Background())
			assert.Equal(t, tt.wantUpdates, *updates, "Update calls on the first reconcile")

			svc, err := get(t, g.Client, "ns", "svc")
			require.NoError(t, err)
			assert.Equal(t, map[string]string{aetherlabels.AnnotationMeshService: "svc", "example.com/kept": "yes"}, svc.Annotations)
			assert.Equal(t, want, svc.Spec.Ports)

			g.reconcile(context.Background())
			g.reconcile(context.Background())
			assert.Equal(t, tt.wantUpdates, *updates, "a later reconcile writes nothing")
		})
	}
}

// TestGenerator_StaleAnnotationsGoWithAPortChange: a Service that needs its
// ports converged and still carries the old annotations gets both in ONE
// write.
func TestGenerator_StaleAnnotationsGoWithAPortChange(t *testing.T) {
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "ns",
			Labels: map[string]string{aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{
				aetherlabels.AnnotationMeshService: "svc", aetherlabels.AnnotationMeshPort: "8080", aetherlabels.AnnotationMeshAppProtocol: "http",
			},
		},
		// As a chart 2.6 registrar left it: the placeholders and no real port.
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{
			legacyPort("mesh", 18081), legacyPort("mesh-tcp", 18082), legacyPort("http", 80), legacyPort("https", 443),
		}},
	}
	g, updates := countingGen(t, catalog{key("ns", "svc"): listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP}))}, existing)
	g.reconcile(context.Background())
	g.reconcile(context.Background())
	assert.Equal(t, 1, *updates)

	svc, err := get(t, g.Client, "ns", "svc")
	require.NoError(t, err)
	for _, a := range staleAnnotations {
		assert.NotContains(t, svc.Annotations, a)
	}
	assert.Equal(t, withLegacy(realPort(8080, classHTTP)), svc.Spec.Ports)
}

// TestGenerator_NeverTouchesAnAnnotationOnAServiceItDoesNotOwn: a user's
// Service that happens to carry these two annotation names (and the name of a
// mesh service) is not the generator's: it is not written, and the
// annotations stay. The clusterset Service of the MCS import generator, which
// carries the mesh-service label and its own aether.io/port, is not a registry
// service's and is not written either.
func TestGenerator_NeverTouchesAnAnnotationOnAServiceItDoesNotOwn(t *testing.T) {
	theirs := map[string]string{aetherlabels.AnnotationMeshPort: "8080", aetherlabels.AnnotationMeshAppProtocol: "http"}
	user := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", Annotations: map[string]string{}},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "svc"}, Ports: []corev1.ServicePort{{Name: "web", Port: 80}}},
	}
	clusterset := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc" + aetherlabels.ClustersetServiceNameSuffix, Namespace: "ns",
			Labels:      map[string]string{aetherlabels.LabelClustersetService: "true", aetherlabels.LabelMeshService: "true"},
			Annotations: map[string]string{},
		},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{legacyPort("mesh", 18081)}},
	}
	for k, v := range theirs {
		user.Annotations[k] = v
		clusterset.Annotations[k] = v
	}
	g, updates := countingGen(t, catalog{key("ns", "svc"): listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP}))}, user, clusterset)
	g.reconcile(context.Background())
	g.reconcile(context.Background())
	assert.Zero(t, *updates, "no Service here is the generator's to write")

	for _, name := range []string{user.Name, clusterset.Name} {
		svc, err := get(t, g.Client, "ns", name)
		require.NoError(t, err, name)
		assert.Equal(t, theirs, svc.Annotations, name)
	}
}

// TestGenerator_NoSchemeDefaultPlaceholders: the Service has no "http" (80) or
// "https" (443) port unless the pods declare that number, in which case it is
// a real port with its class. The two mesh ports, the proxy port (18081) and
// the L4 port (18082), are the only fixed ones: both are still dialled by
// name.
func TestGenerator_NoSchemeDefaultPlaceholders(t *testing.T) {
	g, _ := newGen(snapshotOf(catalog{
		key("ns", "plain"):  listed(pod("ns", "10.0.0.1", 8080, classes{8080: classHTTP})),
		key("ns", "eighty"): listed(pod("ns", "10.0.0.2", 8080, classes{80: classHTTP, 8080: classHTTP})),
		key("ns", "tls"):    listed(pod("ns", "10.0.0.3", 443, classes{443: classTCP})),
	}))
	g.reconcile(context.Background())

	mesh := []corev1.ServicePort{legacyPort("mesh", 18081), legacyPort("mesh-tcp", 18082)}
	with := func(real ...corev1.ServicePort) []corev1.ServicePort {
		return append(append([]corev1.ServicePort{}, mesh...), real...)
	}
	assert.Equal(t, with(realPort(8080, classHTTP)), portsOf(t, g, "ns", "plain"),
		"neither 80 nor 443: the pods declare neither")
	assert.Equal(t, with(realPort(80, classHTTP), realPort(8080, classHTTP)), portsOf(t, g, "ns", "eighty"),
		"a declared 80 is a real port, p80-tcp with appProtocol http")
	assert.Equal(t, with(realPort(443, classTCP)), portsOf(t, g, "ns", "tls"),
		"a declared 443 is a real port with its class")
	for _, name := range []string{"plain", "eighty", "tls"} {
		for _, p := range portsOf(t, g, "ns", name) {
			assert.NotContains(t, []string{"http", "https"}, p.Name, "%s: a scheme-default placeholder", name)
		}
	}
}
