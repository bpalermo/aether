package podmutate

import (
	"context"
	"log/slog"
	"testing"

	"aethermesh.dev/common/constants/annotations"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestReservedPortDenial: a mesh pod cannot declare 18081 or 18082 as an
// application port, in endpoint.aether.io/port or in endpoint.aether.io/ports,
// with any protocol suffix. Every other declaration is admitted, including a
// number that merely contains one of them.
func TestReservedPortDenial(t *testing.T) {
	pod := func(kv ...string) *corev1.Pod {
		a := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			a[kv[i]] = kv[i+1]
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: a}}
	}
	port, ports := annotations.AnnotationEndpointPort, annotations.AnnotationEndpointPorts
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want string
	}{
		"no annotation":                 {pod(), ""},
		"an ordinary port":              {pod(port, "8080", ports, "8080,9090=tcp"), ""},
		"contains the digits":           {pod(port, "1808", ports, "1808,180811"), ""},
		"primary 18081":                 {pod(port, "18081"), "port 18081"},
		"primary 18082, tcp":            {pod(port, "18082", annotations.AnnotationEndpointProtocol, "tcp"), "port 18082"},
		"listed 18081":                  {pod(port, "8080", ports, "8080,18081"), "port 18081"},
		"listed 18082 as raw tcp":       {pod(port, "8080", ports, "8080, 18082=tcp"), "port 18082"},
		"listed 18081 as udp":           {pod(port, "8080", ports, "8080,18081=udp"), "port 18081"},
		"both":                          {pod(port, "18081", ports, "18082"), "port 18081 and 18082"},
		"unparseable is not this error": {pod(port, "eighty", ports, "18081"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := reservedPortDenial(tc.pod)
			if tc.want == "" {
				assert.Empty(t, got)
				return
			}
			require.NotEmpty(t, got)
			assert.Contains(t, got, "the pod declares "+tc.want+" as an application port")
			assert.Contains(t, got, "18081 and 18082 are the mesh's own ports")
		})
	}
}

// TestMutator_DeniesAPodThatDeclaresAReservedPort: through the webhook itself.
// The denial names the port and both mesh ports; a pod that opted out of the
// mesh is not the mesh's to refuse.
func TestMutator_DeniesAPodThatDeclaresAReservedPort(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler))
	declared := map[string]string{annotations.AnnotationEndpointPort: "8080", annotations.AnnotationEndpointPorts: "8080,18081"}

	resp := m.Handle(context.Background(), request(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Annotations: declared}}))
	require.False(t, resp.Allowed, "a pod that declares 18081 was admitted")
	assert.Contains(t, resp.Result.Message, "the pod declares port 18081 as an application port")
	assert.Empty(t, resp.Patches)

	out := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Annotations: declared, Labels: map[string]string{aetherlabels.LabelAetherManaged: "false"}}}
	resp = m.Handle(context.Background(), request(out))
	assert.True(t, resp.Allowed, "a pod that opted out of the mesh is not refused")

	ok := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Annotations: map[string]string{annotations.AnnotationEndpointPort: "8080"}}}
	resp = m.Handle(context.Background(), request(ok))
	assert.True(t, resp.Allowed)
}
