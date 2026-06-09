package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewInboundListener(t *testing.T) {
	pod := &cniv1.CNIPod{
		Name:             "pod-a",
		Namespace:        "aether-test",
		ServiceAccount:   "echo",
		NetworkNamespace: "/var/run/netns/cni-a",
		Ips:              []string{"10.0.0.1"},
	}

	l, err := NewInboundListener(pod, "example.org")
	require.NoError(t, err)
	assert.Equal(t, InboundListenerName(pod), l.GetName())

	// Bound into the pod's netns at the mesh inbound port (per-pod lifecycle + netpol).
	sa := l.GetAddress().GetSocketAddress()
	assert.Equal(t, uint32(defaultInboundPort), sa.GetPortValue())
	assert.Equal(t, "/var/run/netns/cni-a", sa.GetNetworkNamespaceFilepath())
	assert.Equal(t, corev3.TrafficDirection_INBOUND, l.GetTrafficDirection())

	// mTLS terminating filter chain presenting the pod's own SVID as the server cert.
	fc := l.GetFilterChains()[0]
	require.NotNil(t, fc.GetTransportSocket(), "inbound listener must terminate mTLS")

	hcm := decodeHCM(t, l)
	// XFCC set natively from the verified peer (the caller's SVID).
	assert.Equal(t, http_connection_managerv3.HttpConnectionManager_SANITIZE_SET, hcm.GetForwardClientCertDetails())
	assert.True(t, hcm.GetSetCurrentClientCertDetails().GetUri())

	// All requests route to the pod's app cluster.
	rc := hcm.GetRouteConfig()
	assert.False(t, rc.GetValidateClusters().GetValue(), "validation off so app_<pod> churn doesn't wedge the listener")
	vh := rc.GetVirtualHosts()[0]
	assert.Equal(t, []string{"*"}, vh.GetDomains())
	assert.Equal(t, AppClusterName(pod), vh.GetRoutes()[0].GetRoute().GetCluster())
}

func TestNewInboundListener_Errors(t *testing.T) {
	_, err := NewInboundListener(nil, "example.org")
	require.Error(t, err)

	_, err = NewInboundListener(&cniv1.CNIPod{Name: "no-netns"}, "example.org")
	require.Error(t, err)
}

func decodeHCM(t *testing.T, l *listenerv3.Listener) *http_connection_managerv3.HttpConnectionManager {
	t.Helper()
	hcm := &http_connection_managerv3.HttpConnectionManager{}
	require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm))
	return hcm
}
