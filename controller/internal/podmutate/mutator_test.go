package podmutate

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/bpalermo/aether/common/constants"
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

// patchesLabel reports whether the response carries a JSON-patch op that sets the
// managed label — either as a per-key add (path "/metadata/labels/aether.io~1managed")
// or as a whole-map add (path "/metadata/labels", key in the value) when labels
// started nil. Checking the marshaled patch covers both.
func patchesLabel(resp admission.Response) bool {
	b, _ := json.Marshal(resp.Patches)
	return strings.Contains(string(b), constants.LabelAetherManaged)
}

// TestMutator_InjectsLabelAndNdots: a pod with neither the managed label nor ndots
// (the namespace-injection entry point) gets both.
func TestMutator_InjectsLabelAndNdots(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler))
	resp := m.Handle(context.Background(), request(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}))
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "label + dnsConfig patch produced")
	assert.True(t, patchesLabel(resp), "the aether.io/managed label is added")
}

// TestMutator_RespectsOptOut: a pod explicitly setting aether.io/managed=false is
// left untouched even in a managed namespace.
func TestMutator_RespectsOptOut(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{constants.LabelAetherManaged: "false"}}}
	resp := m.Handle(context.Background(), request(pod))
	require.True(t, resp.Allowed)
	assert.Empty(t, resp.Patches, "opt-out pod gets no patch")
}

// TestMutator_NoOpWhenAlreadyManagedWithNdots: an already-labeled pod that already
// sets ndots needs no patch (idempotent / the explicit-label ndots path steady state).
func TestMutator_NoOpWhenAlreadyManagedWithNdots(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler))
	v := "2"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{constants.LabelAetherManaged: "true"}},
		Spec:       corev1.PodSpec{DNSConfig: &corev1.PodDNSConfig{Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &v}}}},
	}
	resp := m.Handle(context.Background(), request(pod))
	require.True(t, resp.Allowed)
	assert.Empty(t, resp.Patches, "no patch when already managed and ndots set")
}

// TestMutator_LabelOnlyWhenNdotsDisabled: with ndots disabled (NDots=""), a managed
// namespace still gets the label, just no dnsConfig.
func TestMutator_LabelOnlyWhenNdotsDisabled(t *testing.T) {
	m := NewMutator("", slog.New(slog.DiscardHandler))
	resp := m.Handle(context.Background(), request(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}))
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches)
	assert.True(t, patchesLabel(resp), "label added even with ndots disabled")
}

// TestMutator_RespectsExistingNdots: an already-managed pod that sets its own ndots
// keeps it (no ndots churn), and needs no other change.
func TestMutator_RespectsExistingNdots(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler))
	v := "1"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{constants.LabelAetherManaged: "true"}},
		Spec:       corev1.PodSpec{DNSConfig: &corev1.PodDNSConfig{Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &v}}}},
	}
	resp := m.Handle(context.Background(), request(pod))
	require.True(t, resp.Allowed)
	assert.Empty(t, resp.Patches, "no patch when already managed and ndots set")
}
