package v1

import (
	"encoding/json"
	"testing"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEdgeRouteJSONRoundTrip(t *testing.T) {
	in := &EdgeRoute{}
	in.Name = "api"
	in.Namespace = "aether-edge"
	in.Spec = &configv1.EdgeRouteSpec{}
	in.Spec.SetHosts([]string{"api.example.com", "api2.example.com"})
	in.Spec.SetService("svc-1")
	in.Spec.SetPort(8080)

	data, err := json.Marshal(in)
	require.NoError(t, err)

	out := &EdgeRoute{}
	require.NoError(t, json.Unmarshal(data, out))

	assert.Equal(t, "api", out.Name)
	assert.Equal(t, "aether-edge", out.Namespace)
	require.NotNil(t, out.Spec)
	assert.Equal(t, []string{"api.example.com", "api2.example.com"}, out.Spec.GetHosts())
	assert.Equal(t, "svc-1", out.Spec.GetService())
	assert.Equal(t, uint32(8080), out.Spec.GetPort())
}

// TestEdgeRouteUnmarshalLenient verifies unknown spec fields are ignored
// (forward-compatibility), matching the MeshConfig contract.
func TestEdgeRouteUnmarshalLenient(t *testing.T) {
	raw := `{"metadata":{"name":"api"},"spec":{"service":"svc-1","futureField":true}}`
	out := &EdgeRoute{}
	require.NoError(t, json.Unmarshal([]byte(raw), out))
	require.NotNil(t, out.Spec)
	assert.Equal(t, "svc-1", out.Spec.GetService())
}

func TestEdgeRouteDeepCopy(t *testing.T) {
	in := &EdgeRoute{}
	in.Name = "api"
	in.Spec = &configv1.EdgeRouteSpec{}
	in.Spec.SetService("svc-1")
	in.Spec.SetHosts([]string{"api.example.com"})

	out := in.DeepCopy()
	require.NotNil(t, out.Spec)
	assert.Equal(t, "svc-1", out.Spec.GetService())

	// Mutating the copy's spec must not touch the original (deep clone).
	out.Spec.SetService("svc-2")
	assert.Equal(t, "svc-1", in.Spec.GetService())
}
