package v1

import (
	"encoding/json"
	"testing"

	configv1 "aethermesh.dev/api/aether/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEndpointPolicy_JSONRoundtrip verifies the jsonshim (un)marshals through
// protojson, so the CR's camelCase field names (targetRef, udsSocket) are the ones
// the CRD schema and the webhook see.
func TestEndpointPolicy_JSONRoundtrip(t *testing.T) {
	in := &EndpointPolicy{
		Spec: configv1.EndpointPolicySpec_builder{
			TargetRef: configv1.PolicyTargetRef_builder{Kind: "Service", Name: "echo"}.Build(),
			UdsSocket: "s/app.sock",
		}.Build(),
	}
	in.Name = "echo-uds"
	in.Namespace = "team-a"

	data, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"targetRef"`)
	assert.Contains(t, string(data), `"udsSocket":"s/app.sock"`)

	out := &EndpointPolicy{}
	require.NoError(t, json.Unmarshal(data, out))
	assert.Equal(t, "echo-uds", out.GetName())
	assert.Equal(t, "team-a", out.GetNamespace())
	assert.Equal(t, "s/app.sock", out.Spec.GetUdsSocket())
	assert.Equal(t, "Service", out.Spec.GetTargetRef().GetKind())
	assert.Equal(t, "echo", out.Spec.GetTargetRef().GetName())
}

// TestEndpointPolicy_DeepCopy verifies the spec is cloned, not aliased — a shared
// proto message would let a cache consumer mutate another's copy.
func TestEndpointPolicy_DeepCopy(t *testing.T) {
	in := &EndpointPolicy{
		Spec: configv1.EndpointPolicySpec_builder{
			TargetRef: configv1.PolicyTargetRef_builder{Kind: "Service", Name: "echo"}.Build(),
			UdsSocket: "s/app.sock",
		}.Build(),
	}
	out := in.DeepCopy()
	require.NotNil(t, out.Spec)
	assert.NotSame(t, in.Spec, out.Spec)
	assert.Equal(t, in.Spec.GetUdsSocket(), out.Spec.GetUdsSocket())

	assert.Nil(t, (&EndpointPolicy{}).DeepCopy().Spec)
}
