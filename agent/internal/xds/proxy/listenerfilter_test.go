package proxy

import (
	"testing"

	tls_inspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestBuildInboundListenerFilters(t *testing.T) {
	filters := buildInboundListenerFilters()

	require.Len(t, filters, 1)
	assert.Equal(t, listenerFilterTLSInspectorName, filters[0].GetName())
}

func TestTlsInspector(t *testing.T) {
	filter := tlsInspector()

	require.NotNil(t, filter)
	assert.Equal(t, listenerFilterTLSInspectorName, filter.GetName())
	require.NotNil(t, filter.GetTypedConfig())

	var inspector tls_inspectorv3.TlsInspector
	err := proto.Unmarshal(filter.GetTypedConfig().GetValue(), &inspector)
	require.NoError(t, err)
}

func TestListenerFilter(t *testing.T) {
	filter := listenerFilter("my-filter", &tls_inspectorv3.TlsInspector{})

	require.NotNil(t, filter)
	assert.Equal(t, "my-filter", filter.GetName())
	assert.NotNil(t, filter.GetTypedConfig())
}
