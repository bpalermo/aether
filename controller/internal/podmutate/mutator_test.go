package podmutate

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func request(pod *corev1.Pod) admission.Request {
	raw, _ := json.Marshal(pod)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Object: runtime.RawExtension{Raw: raw}}}
}

func TestMutator_InjectsNdotsWhenAbsent(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler))
	resp := m.Handle(context.Background(), request(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}))
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "a dnsConfig patch is produced")
}

func TestMutator_RespectsExistingNdots(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler))
	v := "1"
	pod := &corev1.Pod{Spec: corev1.PodSpec{DNSConfig: &corev1.PodDNSConfig{
		Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &v}},
	}}}
	resp := m.Handle(context.Background(), request(pod))
	require.True(t, resp.Allowed)
	assert.Empty(t, resp.Patches, "no patch when the pod already sets ndots")
}
