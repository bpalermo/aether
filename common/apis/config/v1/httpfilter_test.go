package v1

import (
	"encoding/json"
	"testing"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// TestHTTPFilter_JSONRoundtrip verifies the jsonshim (un)marshals an HTTPFilter
// through protojson, preserving the opaque spec.typedConfig Any by its @type — the
// reconciler depends on getting a usable *anypb.Any back out.
func TestHTTPFilter_JSONRoundtrip(t *testing.T) {
	cfg, err := anypb.New(&header_to_metadatav3.Config{
		RequestRules: []*header_to_metadatav3.Config_Rule{{
			Header:          "x-tenant",
			OnHeaderPresent: &header_to_metadatav3.Config_KeyValuePair{MetadataNamespace: "aether", Key: "tenant"},
		}},
	})
	require.NoError(t, err)

	in := &HTTPFilter{
		Spec: configv1.HTTPFilterSpec_builder{
			Filter:      "envoy.filters.http.header_to_metadata",
			TypedConfig: cfg,
			Scope:       configv1.HTTPFilterSpec_SCOPE_ROUTE,
		}.Build(),
	}
	in.Name = "h2m"
	in.Namespace = "team-a"

	data, err := json.Marshal(in)
	require.NoError(t, err)
	// The opaque Any is serialized with its @type envelope.
	assert.Contains(t, string(data), "header_to_metadata.v3.Config")

	var out HTTPFilter
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, "h2m", out.Name)
	require.NotNil(t, out.Spec)
	assert.Equal(t, "envoy.filters.http.header_to_metadata", out.Spec.GetFilter())
	assert.Equal(t, configv1.HTTPFilterSpec_SCOPE_ROUTE, out.Spec.GetScope())

	// The typedConfig Any survives the roundtrip and unmarshals back to its message.
	require.NotNil(t, out.Spec.GetTypedConfig())
	got := &header_to_metadatav3.Config{}
	require.NoError(t, out.Spec.GetTypedConfig().UnmarshalTo(got))
	require.Len(t, got.GetRequestRules(), 1)
	assert.Equal(t, "x-tenant", got.GetRequestRules()[0].GetHeader())
}

// TestHTTPFilter_DeepCopy verifies DeepCopy clones the proto spec (incl. the opaque
// Any) rather than sharing it.
func TestHTTPFilter_DeepCopy(t *testing.T) {
	cfg, err := anypb.New(&header_to_metadatav3.Config{})
	require.NoError(t, err)
	in := &HTTPFilter{Spec: configv1.HTTPFilterSpec_builder{Filter: "f", TypedConfig: cfg}.Build()}
	out := in.DeepCopy()
	require.NotNil(t, out.Spec)
	assert.NotSame(t, in.Spec, out.Spec, "spec is cloned, not shared")
	assert.Equal(t, "f", out.Spec.GetFilter())
}
