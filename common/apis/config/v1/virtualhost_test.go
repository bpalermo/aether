package v1

import (
	"encoding/json"
	"testing"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleVirtualHostSpec() *configv1.VirtualHostSpec {
	spec := &configv1.VirtualHostSpec{}
	spec.SetHosts([]string{"api.example.com"})
	m := &configv1.RouteMatch{}
	m.SetPrefix("/users")
	b := &configv1.RouteBackend{}
	b.SetService("svc-1")
	b.SetPort(8080)
	hr := &configv1.HTTPRoute{}
	hr.SetMatch(m)
	hr.SetBackend(b)
	spec.SetRoutes([]*configv1.HTTPRoute{hr})
	tls := &configv1.VirtualHostTLS{}
	tls.SetSecretName("api-tls")
	spec.SetTls(tls)
	return spec
}

// TestVirtualHostJSONRoundTrip verifies the protojson shim round-trips the spec
// (proto JSON field names, nested messages, the path oneof, and TLS).
func TestVirtualHostJSONRoundTrip(t *testing.T) {
	in := &VirtualHost{Spec: sampleVirtualHostSpec()}
	in.Name = "api"
	in.Kind = VirtualHostKind

	data, err := json.Marshal(in)
	require.NoError(t, err)

	var out VirtualHost
	require.NoError(t, json.Unmarshal(data, &out))
	require.NotNil(t, out.Spec)
	assert.Equal(t, []string{"api.example.com"}, out.Spec.GetHosts())
	require.Len(t, out.Spec.GetRoutes(), 1)
	assert.Equal(t, "/users", out.Spec.GetRoutes()[0].GetMatch().GetPrefix())
	assert.Equal(t, "svc-1", out.Spec.GetRoutes()[0].GetBackend().GetService())
	assert.Equal(t, uint32(8080), out.Spec.GetRoutes()[0].GetBackend().GetPort())
	assert.Equal(t, "api-tls", out.Spec.GetTls().GetSecretName())
}

// TestVirtualHostDeepCopy verifies the proto spec is cloned, not shared.
func TestVirtualHostDeepCopy(t *testing.T) {
	in := &VirtualHost{Spec: sampleVirtualHostSpec()}
	out := in.DeepCopy()
	require.NotNil(t, out)
	assert.Equal(t, []string{"api.example.com"}, out.Spec.GetHosts())

	// Mutating the copy's proto must not touch the original.
	out.Spec.SetHosts([]string{"other.example.com"})
	assert.Equal(t, []string{"api.example.com"}, in.Spec.GetHosts())
}

// TestVirtualHostLenientDecode verifies unknown spec fields are ignored
// (DiscardUnknown) for rolling-upgrade forward-compatibility.
func TestVirtualHostLenientDecode(t *testing.T) {
	raw := `{"metadata":{"name":"api"},"spec":{"hosts":["api.example.com"],"unknownField":true,"routes":[{"match":{"prefix":"/"},"backend":{"service":"svc-1"}}]}}`
	var out VirtualHost
	require.NoError(t, json.Unmarshal([]byte(raw), &out))
	require.NotNil(t, out.Spec)
	assert.Equal(t, []string{"api.example.com"}, out.Spec.GetHosts())
	require.Len(t, out.Spec.GetRoutes(), 1)
	assert.Equal(t, "svc-1", out.Spec.GetRoutes()[0].GetBackend().GetService())
}
