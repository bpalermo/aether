package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	xdstypev3 "github.com/cncf/xds/go/xds/type/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

// statsConfigFields unmarshals an aether_stats HTTP filter's TypedStruct config
// and returns its fields for assertion.
func statsConfigFields(t *testing.T, f *http_connection_managerv3.HttpFilter) map[string]*structpb.Value {
	t.Helper()
	assert.Equal(t, statsFilterName, f.GetName())
	ts := &xdstypev3.TypedStruct{}
	require.NoError(t, f.GetTypedConfig().UnmarshalTo(ts))
	require.Equal(t, statsConfigTypeURL, ts.GetTypeUrl())
	return ts.GetValue().GetFields()
}

// TestOutboundStatsFilterEmitPod verifies the --stats-emit-pod flag flips the
// emit_pod field while source_pod always carries the local pod name.
func TestOutboundStatsFilterEmitPod(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "shop-1", ServiceAccount: "shop"}

	off := statsConfigFields(t, outboundStatsFilter(pod, "aether.internal", false))
	assert.Equal(t, "source", off["reporter"].GetStringValue())
	assert.Equal(t, "shop", off["source_service"].GetStringValue())
	assert.Equal(t, "shop-1", off["source_pod"].GetStringValue())
	assert.False(t, off["emit_pod"].GetBoolValue())

	on := statsConfigFields(t, outboundStatsFilter(pod, "aether.internal", true))
	assert.True(t, on["emit_pod"].GetBoolValue())
	assert.Equal(t, "shop-1", on["source_pod"].GetStringValue())
}

// TestInboundStatsFilterEmitPod verifies the inbound (destination-reported)
// filter carries destination_pod and honors the emit_pod flag.
func TestInboundStatsFilterEmitPod(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "shop-1", ServiceAccount: "shop"}

	off := statsConfigFields(t, inboundStatsFilter(pod, false))
	assert.Equal(t, "destination", off["reporter"].GetStringValue())
	assert.Equal(t, "shop", off["destination_service"].GetStringValue())
	assert.Equal(t, "shop-1", off["destination_pod"].GetStringValue())
	assert.False(t, off["emit_pod"].GetBoolValue())

	on := statsConfigFields(t, inboundStatsFilter(pod, true))
	assert.True(t, on["emit_pod"].GetBoolValue())
	assert.Equal(t, "shop-1", on["destination_pod"].GetStringValue())
}
