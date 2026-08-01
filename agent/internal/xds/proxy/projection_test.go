package proxy

import (
	"testing"

	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// TestConfigProjection_RoundTrip verifies a fully-populated GammaRoute set survives
// ToConfigProjection → FromConfigProjection unchanged — the export (hub) / import
// (spoke) contract for proposal 026.
func TestConfigProjection_RoundTrip(t *testing.T) {
	cfg, err := anypb.New(&header_to_metadatav3.Config{})
	require.NoError(t, err)
	in := []GammaRoute{
		{
			Matches: []GammaMatch{
				{Prefix: "/v2", Headers: []GammaHeaderMatch{{Name: "x-canary", Value: "1"}}},
				{Exact: "/exact"},
				{Regex: "/svc/.*"},
			},
			Backends: []GammaBackend{
				{Service: "team-a/echo-v1", Cluster: "echo-v1.team-a.aether.internal", Weight: 70},
				{Service: "team-a/echo-v2", Cluster: "echo-v2.team-a.aether.internal", Weight: 30},
			},
			Timeout: durationpb.New(5_000_000_000),
			HeaderMutation: &GammaHeaderMutation{
				SetRequest:     []GammaHeaderKV{{Name: "X-Set", Value: "v"}},
				AddRequest:     []GammaHeaderKV{{Name: "X-Add", Value: "v"}},
				RemoveRequest:  []string{"X-Rm"},
				SetResponse:    []GammaHeaderKV{{Name: "X-RSet", Value: "v"}},
				AddResponse:    []GammaHeaderKV{{Name: "X-RAdd", Value: "v"}},
				RemoveResponse: []string{"X-RRm"},
			},
			ExtensionFilters: []ExtensionFilter{{Name: "envoy.filters.http.header_mutation", Config: cfg}},
		},
		{
			Matches:  []GammaMatch{{Prefix: "/redirect"}},
			Redirect: &GammaRedirect{Hostname: "example.org", StatusCode: 301, Port: 8443, PathType: "ReplaceFullPath", PathValue: "/new", ListenerPort: 8080},
		},
		{
			Matches:    []GammaMatch{{Prefix: "/rw"}},
			Backends:   []GammaBackend{{Service: "team-a/echo", Cluster: "echo.team-a.aether.internal", Weight: 1}},
			URLRewrite: &GammaURLRewrite{Hostname: "internal", PathType: "ReplacePrefixMatch", PathValue: "/api"},
		},
	}

	p := ToConfigProjection("team-a/echo", "cluster-a", "v42", in)
	assert.Equal(t, "team-a/echo", p.GetService())
	assert.Equal(t, "cluster-a", p.GetOriginCluster())
	assert.Equal(t, "v42", p.GetVersion())

	out := FromConfigProjection(p)
	require.Len(t, out, len(in))
	// Round-trip equality on the value fields.
	assert.Equal(t, in[0].Matches, out[0].Matches)
	assert.Equal(t, in[0].Backends, out[0].Backends)
	assert.Equal(t, in[0].HeaderMutation, out[0].HeaderMutation)
	assert.True(t, in[0].Timeout.AsDuration() == out[0].Timeout.AsDuration())
	require.Len(t, out[0].ExtensionFilters, 1)
	assert.Equal(t, "envoy.filters.http.header_mutation", out[0].ExtensionFilters[0].Name)
	assert.Equal(t, in[1].Redirect, out[1].Redirect)
	assert.Equal(t, in[2].URLRewrite, out[2].URLRewrite)
}

func TestFromConfigProjection_Nil(t *testing.T) {
	assert.Nil(t, FromConfigProjection(nil))
}
