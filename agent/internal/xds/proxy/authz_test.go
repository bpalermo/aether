package proxy

import (
	"testing"
	"time"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
	set_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/set_metadata/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthzSidecarHTTPFilter (027 M1): the entry carries the FULL transport
// (static UDS cluster, timeout, failure mode, V3) and is DISABLED — zero effect
// until a route opts in via typed_per_filter_config.
func TestAuthzSidecarHTTPFilter(t *testing.T) {
	f := AuthzSidecarHTTPFilter(150*time.Millisecond, false)
	require.Equal(t, ExtAuthzFilterName, f.GetName())
	assert.True(t, f.GetDisabled(), "must be disabled: enablement is per-route (025 machinery)")

	cfg := &ext_authzv3.ExtAuthz{}
	require.NoError(t, f.GetTypedConfig().UnmarshalTo(cfg))
	assert.Equal(t, AuthzSidecarClusterName, cfg.GetGrpcService().GetEnvoyGrpc().GetClusterName())
	assert.Equal(t, 150*time.Millisecond, cfg.GetGrpcService().GetTimeout().AsDuration())
	assert.Equal(t, corev3.ApiVersion_V3, cfg.GetTransportApiVersion())
	assert.False(t, cfg.GetFailureModeAllow(), "fail-closed by default")

	// Fail-open variant.
	fo := AuthzSidecarHTTPFilter(time.Second, true)
	cfg2 := &ext_authzv3.ExtAuthz{}
	require.NoError(t, fo.GetTypedConfig().UnmarshalTo(cfg2))
	assert.True(t, cfg2.GetFailureModeAllow())
}

// TestSourceMetadataHTTPFilter: per-pod identity in aether.source + the strip helper.
func TestSourceMetadataHTTPFilter(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "client-1", Namespace: "team-a", ServiceAccount: "client"}
	f := SourceMetadataHTTPFilter(pod, "aether.internal")
	require.Equal(t, "envoy.filters.http.set_metadata", f.GetName())
	cfg := &set_metadatav3.Config{}
	require.NoError(t, f.GetTypedConfig().UnmarshalTo(cfg))
	require.Len(t, cfg.GetMetadata(), 1)
	md := cfg.GetMetadata()[0]
	assert.Equal(t, SourceMetadataNamespace, md.GetMetadataNamespace())
	v := md.GetValue().GetFields()
	assert.Equal(t, "spiffe://aether.internal/ns/team-a/sa/client", v["spiffeId"].GetStringValue())
	assert.Equal(t, "client", v["serviceAccount"].GetStringValue())

	// The system ext_authz entry forwards the namespace.
	ea := AuthzSidecarHTTPFilter(time.Second, false)
	eacfg := &ext_authzv3.ExtAuthz{}
	require.NoError(t, ea.GetTypedConfig().UnmarshalTo(eacfg))
	assert.Contains(t, eacfg.GetMetadataContextNamespaces(), SourceMetadataNamespace)

	// WithoutSourceMetadata strips it (the inbound form).
	union := []*http_connection_managerv3.HttpFilter{f, ea}
	assert.True(t, HasExtAuthz(union))
	stripped := WithoutSourceMetadata(union)
	require.Len(t, stripped, 1)
	assert.Equal(t, ExtAuthzFilterName, stripped[0].GetName())
}
