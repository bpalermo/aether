package podmutate

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	aetherlabels "aethermesh.dev/common/constants/labels"
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
	return strings.Contains(string(b), aetherlabels.LabelAetherManaged)
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
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{aetherlabels.LabelAetherManaged: "false"}}}
	resp := m.Handle(context.Background(), request(pod))
	require.True(t, resp.Allowed)
	assert.Empty(t, resp.Patches, "opt-out pod gets no patch")
}

// dnsOpts builds a pod dnsConfig option list from name/value pairs.
func dnsOpts(kv ...string) []corev1.PodDNSConfigOption {
	opts := make([]corev1.PodDNSConfigOption, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		v := kv[i+1]
		opts = append(opts, corev1.PodDNSConfigOption{Name: kv[i], Value: &v})
	}
	return opts
}

// managedPod is an already-labeled pod carrying the given dnsConfig options.
func managedPod(opts ...corev1.PodDNSConfigOption) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Labels: map[string]string{aetherlabels.LabelAetherManaged: "true"}},
		Spec:       corev1.PodSpec{DNSConfig: &corev1.PodDNSConfig{Options: opts}},
	}
}

// TestMutator_NoOpWhenAlreadyManagedWithMeshDNSOptions: an already-labeled pod that
// already carries the full mesh resolv.conf option set needs no patch (idempotent /
// the explicit-label steady state).
func TestMutator_NoOpWhenAlreadyManagedWithMeshDNSOptions(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler))
	pod := managedPod(dnsOpts("ndots", "2", "timeout", resolverTimeout, "attempts", resolverAttempts)...)
	resp := m.Handle(context.Background(), request(pod))
	require.True(t, resp.Allowed)
	assert.Empty(t, resp.Patches, "no patch when already managed with every mesh DNS option set")
}

// TestMutator_LabelAndBudgetWhenNdotsDisabled: with ndots disabled (NDots=""), a
// managed namespace still gets the label and still gets the retransmit budget — the
// budget is unconditional because a lost datagram costs 5s whichever resolver
// answers, mesh-DNS or kube-dns.
func TestMutator_LabelAndBudgetWhenNdotsDisabled(t *testing.T) {
	m := NewMutator("", slog.New(slog.DiscardHandler))
	resp := m.Handle(context.Background(), request(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}))
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches)
	assert.True(t, patchesLabel(resp), "label added even with ndots disabled")

	opts := m.dnsOptions()
	require.Len(t, opts, 3)
	assert.Equal(t, dnsOption{ndotsOption, ""}, opts[0], "ndots carries no value, so it is skipped")
	assert.Equal(t, dnsOption{timeoutOption, resolverTimeout}, opts[1])
	assert.Equal(t, dnsOption{attemptsOption, resolverAttempts}, opts[2])
}

// TestMutator_RespectsExistingNdots: an already-managed pod that sets its own ndots
// keeps it (no ndots churn). It still gains the retransmit budget it lacks.
func TestMutator_RespectsExistingNdots(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler))
	pod := managedPod(dnsOpts("ndots", "1")...)
	resp := m.Handle(context.Background(), request(pod))
	require.True(t, resp.Allowed)
	require.NotEmpty(t, resp.Patches, "the missing timeout/attempts options are still added")
	// The pod's own ndots survives untouched.
	assert.False(t, setDNSOption(managedPod(dnsOpts("ndots", "1")...), ndotsOption, "2"),
		"an ndots the pod already declares is never overridden")
}

// TestMutator_InjectsRetransmitBudget pins the #726 fix: a fresh managed pod is given
// options timeout:1 attempts:3, so a single lost DNS datagram costs ~1s instead of the
// stock 5s resolv.conf retransmit — which Go's per-name singleflight otherwise turns
// into a 5s outage of that name for the whole pod.
func TestMutator_InjectsRetransmitBudget(t *testing.T) {
	m := NewMutator("2", slog.New(slog.DiscardHandler))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	require.True(t, setDNSOption(pod, timeoutOption, resolverTimeout))
	require.True(t, setDNSOption(pod, attemptsOption, resolverAttempts))

	got := map[string]string{}
	for _, o := range pod.Spec.DNSConfig.Options {
		got[o.Name] = *o.Value
	}
	assert.Equal(t, map[string]string{timeoutOption: "1", attemptsOption: "3"}, got)

	// And the webhook actually emits them for a pod that has neither.
	resp := m.Handle(context.Background(), request(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}))
	require.True(t, resp.Allowed)
	b, err := json.Marshal(resp.Patches)
	require.NoError(t, err)
	assert.Contains(t, string(b), timeoutOption)
	assert.Contains(t, string(b), attemptsOption)
}

// TestSetDNSOption covers the injection primitive directly: it adds a missing option,
// never overrides one the workload author already declared, treats an empty value as
// "option disabled", and materializes a nil dnsConfig.
func TestSetDNSOption(t *testing.T) {
	t.Run("adds when absent", func(t *testing.T) {
		pod := &corev1.Pod{}
		assert.True(t, setDNSOption(pod, timeoutOption, "1"))
		require.NotNil(t, pod.Spec.DNSConfig, "a nil dnsConfig is materialized")
		require.Len(t, pod.Spec.DNSConfig.Options, 1)
		assert.Equal(t, "1", *pod.Spec.DNSConfig.Options[0].Value)
	})
	t.Run("never overrides an explicit value", func(t *testing.T) {
		pod := managedPod(dnsOpts(timeoutOption, "7")...)
		assert.False(t, setDNSOption(pod, timeoutOption, "1"))
		require.Len(t, pod.Spec.DNSConfig.Options, 1)
		assert.Equal(t, "7", *pod.Spec.DNSConfig.Options[0].Value, "the workload's own value wins")
	})
	t.Run("empty value is a no-op", func(t *testing.T) {
		pod := &corev1.Pod{}
		assert.False(t, setDNSOption(pod, ndotsOption, ""))
		assert.Nil(t, pod.Spec.DNSConfig, "a disabled option must not materialize a dnsConfig")
	})
}
