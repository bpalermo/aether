package proxy

import (
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
)

// nil spec → all best-practice defaults (an empty EdgeConfig still hardens).
func TestApplyEdgeHardening_Defaults(t *testing.T) {
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	ApplyEdgeHardening(hcm, nil, nil)
	assert.True(t, hcm.GetUseRemoteAddress().GetValue(), "use_remote_address defaults true")
	assert.Equal(t, corev3.HttpProtocolOptions_REJECT_REQUEST, hcm.GetCommonHttpProtocolOptions().GetHeadersWithUnderscoresAction())
	assert.Equal(t, uint32(100), hcm.GetHttp2ProtocolOptions().GetMaxConcurrentStreams().GetValue())
	assert.Equal(t, uint32(65536), hcm.GetHttp2ProtocolOptions().GetInitialStreamWindowSize().GetValue())
	assert.Equal(t, uint32(1048576), hcm.GetHttp2ProtocolOptions().GetInitialConnectionWindowSize().GetValue())
	assert.Equal(t, 300*time.Second, hcm.GetStreamIdleTimeout().AsDuration())
	assert.Equal(t, 300*time.Second, hcm.GetRequestTimeout().AsDuration())
	assert.Equal(t, 3600*time.Second, hcm.GetCommonHttpProtocolOptions().GetIdleTimeout().AsDuration())
}

// explicit spec overrides the defaults; request_timeout 0 is honored (disabled).
func TestApplyEdgeHardening_Overrides(t *testing.T) {
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	spec := configv1.EdgeConfigSpec_builder{
		UseRemoteAddress:             wrapperspb.Bool(false),
		HeadersWithUnderscoresAction: configv1.EdgeConfigSpec_HEADERS_WITH_UNDERSCORES_ACTION_ALLOW.Enum(),
		RequestTimeout:               durationpb.New(0),
		Http2:                        configv1.Http2Options_builder{MaxConcurrentStreams: wrapperspb.UInt32(50)}.Build(),
	}.Build()
	ApplyEdgeHardening(hcm, nil, spec)
	assert.False(t, hcm.GetUseRemoteAddress().GetValue())
	assert.Equal(t, corev3.HttpProtocolOptions_ALLOW, hcm.GetCommonHttpProtocolOptions().GetHeadersWithUnderscoresAction())
	assert.Equal(t, int64(0), hcm.GetRequestTimeout().AsDuration().Nanoseconds(), "request_timeout 0 = disabled honored")
	assert.Equal(t, uint32(50), hcm.GetHttp2ProtocolOptions().GetMaxConcurrentStreams().GetValue())
}
