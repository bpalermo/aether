package proxy

import (
	"testing"

	xdsconst "github.com/bpalermo/aether/agent/internal/xds/xdsconst"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
)

// TestUpstreamsFromPod verifies parsing of the config.aether.io/upstreams
// annotation: comma separation, whitespace trimming, empty-item and duplicate
// dropping, nil for absent/empty annotations, and namespace-qualification of the
// resulting keys (bare name -> "<pod-ns>/<name>", already-qualified value passes
// through) so they match the "<ns>/<svc>" registry / dependency-set keys
// (proposal 020 Part 1).
func TestUpstreamsFromPod(t *testing.T) {
	tests := []struct {
		name       string
		namespace  string
		annotation *string
		want       []string
	}{
		{
			name:       "absent annotation yields nil",
			namespace:  "default",
			annotation: nil,
			want:       nil,
		},
		{
			name:       "empty annotation yields nil",
			namespace:  "default",
			annotation: ptr(""),
			want:       nil,
		},
		{
			name:       "bare single service qualified with pod namespace",
			namespace:  "default",
			annotation: ptr("svc-payments"),
			want:       []string{"default/svc-payments"},
		},
		{
			name:       "bare comma-separated list qualified with pod namespace",
			namespace:  "aether-test",
			annotation: ptr("svc-payments,svc-ledger,svc-audit"),
			want:       []string{"aether-test/svc-payments", "aether-test/svc-ledger", "aether-test/svc-audit"},
		},
		{
			name:       "whitespace is trimmed then qualified",
			namespace:  "default",
			annotation: ptr(" svc-a , svc-b ,svc-c"),
			want:       []string{"default/svc-a", "default/svc-b", "default/svc-c"},
		},
		{
			name:       "empty items and duplicates dropped",
			namespace:  "default",
			annotation: ptr("svc-a,,svc-a, ,svc-b"),
			want:       []string{"default/svc-a", "default/svc-b"},
		},
		{
			name:       "only separators yields nil",
			namespace:  "default",
			annotation: ptr(", ,,"),
			want:       nil,
		},
		{
			name:       "already-qualified cross-namespace value passes through",
			namespace:  "default",
			annotation: ptr("other-ns/echo"),
			want:       []string{"other-ns/echo"},
		},
		{
			name:       "mix of bare and cross-namespace upstreams",
			namespace:  "aether-test",
			annotation: ptr("echo, billing-ns/billing , audit"),
			want:       []string{"aether-test/echo", "billing-ns/billing", "aether-test/audit"},
		},
		{
			name:       "bare name and its explicit pod-namespace form collapse to one",
			namespace:  "aether-test",
			annotation: ptr("echo,aether-test/echo"),
			want:       []string{"aether-test/echo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &cniv1.CNIPod{Name: "p", Namespace: tt.namespace}
			if tt.annotation != nil {
				pod.Annotations = map[string]string{xdsconst.AnnotationConfigUpstreams: *tt.annotation}
			}
			assert.Equal(t, tt.want, UpstreamsFromPod(pod))
		})
	}
}

func ptr(s string) *string { return &s }
