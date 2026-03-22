package proxy

import (
	"testing"

	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestRouterHttpFilter(t *testing.T) {
	filter := routerHttpFilter()

	require.NotNil(t, filter)
	assert.Equal(t, httpRouterFilterName, filter.GetName())
	require.NotNil(t, filter.GetTypedConfig())

	var router routerv3.Router
	err := proto.Unmarshal(filter.GetTypedConfig().GetValue(), &router)
	require.NoError(t, err)
}

func TestHttpFilter(t *testing.T) {
	filter := httpFilter("my-filter", &routerv3.Router{})

	require.NotNil(t, filter)
	assert.Equal(t, "my-filter", filter.GetName())
	assert.NotNil(t, filter.GetTypedConfig())
}
