package endpointpolicy

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	crdv1 "github.com/bpalermo/aether/common/apis/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func spec(kind, group, name, socket string) *configv1.EndpointPolicySpec {
	return configv1.EndpointPolicySpec_builder{
		TargetRef: configv1.PolicyTargetRef_builder{Group: group, Kind: kind, Name: name}.Build(),
		UdsSocket: socket,
	}.Build()
}

// TestValidate covers the accept path and every rejection the webhook owns: the
// proto rules (targetRef shape, socket shape) and the resolver's segment rules +
// AF_UNIX sun_path budget.
func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		spec    *configv1.EndpointPolicySpec
		wantErr string
	}{
		{
			name: "valid",
			spec: spec("Service", "", "echo", "s/app.sock"),
		},
		{
			name: "valid with explicit core group",
			spec: spec("Service", "core", "echo", "s/app.sock"),
		},
		{
			name:    "nil spec",
			wantErr: "spec is required",
		},
		{
			name:    "missing targetRef",
			spec:    configv1.EndpointPolicySpec_builder{UdsSocket: "s/app.sock"}.Build(),
			wantErr: "target_ref",
		},
		{
			name:    "targetRef kind is not Service",
			spec:    spec("Deployment", "apps", "echo", "s/app.sock"),
			wantErr: "targetRef.kind must be Service",
		},
		{
			name:    "targetRef group is not core",
			spec:    spec("Service", "apps", "echo", "s/app.sock"),
			wantErr: "targetRef.group must be the core group",
		},
		{
			name:    "targetRef name is empty",
			spec:    spec("Service", "", "", "s/app.sock"),
			wantErr: "failed validation",
		},
		{
			name:    "socket is empty",
			spec:    spec("Service", "", "echo", ""),
			wantErr: "failed validation",
		},
		{
			name:    "socket has no volume separator",
			spec:    spec("Service", "", "echo", "app.sock"),
			wantErr: "failed validation",
		},
		{
			name:    "socket has extra path segments",
			spec:    spec("Service", "", "echo", "s/nested/app.sock"),
			wantErr: "failed validation",
		},
		{
			name:    "socket traverses out of the volume",
			spec:    spec("Service", "", "echo", "../app.sock"),
			wantErr: "is not usable",
		},
		{
			// Shape-valid but over the sun_path budget: exactly the failure an
			// annotation would only surface as an agent error log.
			name:    "socket overflows the sun_path budget",
			spec:    spec("Service", "", "echo", "socket-volume/"+strings.Repeat("a", 24)+".sock"),
			wantErr: "over the 107-byte AF_UNIX limit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.spec)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestHandle verifies the admission path decodes through the jsonshim and maps a
// validation failure onto a denial.
func TestHandle(t *testing.T) {
	v := &Validator{Log: slog.New(slog.DiscardHandler)}

	ep := &crdv1.EndpointPolicy{Spec: spec("Service", "", "echo", "s/app.sock")}
	ep.Name = "echo-uds"
	ep.Namespace = "team-a"
	raw, err := json.Marshal(ep)
	require.NoError(t, err)

	resp := v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Object: runtime.RawExtension{Raw: raw}},
	})
	assert.True(t, resp.Allowed)

	ep.Spec = spec("Service", "", "echo", "../app.sock")
	raw, err = json.Marshal(ep)
	require.NoError(t, err)

	resp = v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Object: runtime.RawExtension{Raw: raw}},
	})
	assert.False(t, resp.Allowed)

	resp = v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Object: runtime.RawExtension{Raw: []byte("{")}},
	})
	assert.False(t, resp.Allowed)
}
