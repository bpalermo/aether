package config

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXDSConfigSourceADS(t *testing.T) {
	src := XDSConfigSourceADS()
	_, ok := src.GetConfigSourceSpecifier().(*corev3.ConfigSource_Ads)
	assert.True(t, ok, "ADS config source uses the Ads specifier")
}

func TestSDSConfigSourceFromCluster(t *testing.T) {
	src := SDSConfigSourceFromCluster("spire_agent")

	assert.Equal(t, corev3.ApiVersion_V3, src.GetResourceApiVersion())
	acs := src.GetApiConfigSource()
	require.NotNil(t, acs)
	assert.Equal(t, corev3.ApiConfigSource_GRPC, acs.GetApiType())
	assert.Equal(t, corev3.ApiVersion_V3, acs.GetTransportApiVersion())
	require.Len(t, acs.GetGrpcServices(), 1)
	assert.Equal(t, "spire_agent", acs.GetGrpcServices()[0].GetEnvoyGrpc().GetClusterName())
}
