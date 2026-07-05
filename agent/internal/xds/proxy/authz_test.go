package proxy

import (
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ext_authzv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_authz/v3"
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
