package proxy

import (
	"testing"
	"time"

	cniv1 "aethermesh.dev/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hcmDrainTimeouts returns the drain_timeout of every HTTP connection manager
// on a listener (its filter chains and its default chain), keyed by
// "<chain name>/<stat prefix>". An unset drain_timeout reads as -1, so an HCM
// left at Envoy's default can never satisfy an assertion for a real value.
func hcmDrainTimeouts(t *testing.T, l *listenerv3.Listener) map[string]time.Duration {
	t.Helper()
	out := map[string]time.Duration{}
	chains := append([]*listenerv3.FilterChain{}, l.GetFilterChains()...)
	if l.GetDefaultFilterChain() != nil {
		chains = append(chains, l.GetDefaultFilterChain())
	}
	for _, fc := range chains {
		for _, f := range fc.GetFilters() {
			if f.GetName() != "envoy.http_connection_manager" {
				continue
			}
			hcm := &http_connection_managerv3.HttpConnectionManager{}
			require.NoError(t, f.GetTypedConfig().UnmarshalTo(hcm))
			d := time.Duration(-1)
			if hcm.GetDrainTimeout() != nil {
				d = hcm.GetDrainTimeout().AsDuration()
			}
			out[fc.GetName()+"/"+hcm.GetStatPrefix()] = d
		}
	}
	return out
}

// requireEveryHCMDrain asserts a listener carries at least wantHCMs HTTP
// connection managers and that every one of them has the given drain_timeout
// (-1: unset, Envoy's default).
func requireEveryHCMDrain(t *testing.T, l *listenerv3.Listener, wantHCMs int, want time.Duration) {
	t.Helper()
	require.NotNil(t, l)
	got := hcmDrainTimeouts(t, l)
	require.GreaterOrEqual(t, len(got), wantHCMs, "%s: HTTP connection managers found: %v", l.GetName(), got)
	for key, d := range got {
		assert.Equal(t, want, d, "%s: %s", l.GetName(), key)
	}
}

// proactiveDrainWindow is upstream Envoy's Network::proactiveDrainDelay window
// (source/common/network/drain_close_util.cc, upstream Envoy PR 47991):
// [drainTime*2/3, drainTime - drainTimeout]. ok is false when it is empty,
// in which case Envoy arms no proactive drain at all.
func proactiveDrainWindow(drainTime, drainTimeout time.Duration) (start, end time.Duration, ok bool) {
	start = drainTime * 2 / 3
	end = drainTime - drainTimeout
	return start, end, start < end
}

// TestDefaultConnectionDrainTimeoutArmsUpstreamsProactiveDrain pins the
// arithmetic the default exists for: under the chart's 10s drain time, Envoy's
// own 5s default leaves upstream's window empty (the reason the aether1054
// patch was carried), and 2s opens [6.67s, 8s], whose drain sequence ends by
// the 10s drain time.
func TestDefaultConnectionDrainTimeoutArmsUpstreamsProactiveDrain(t *testing.T) {
	const chartDrainTime = 10 * time.Second
	assert.Equal(t, 2*time.Second, DefaultConnectionDrainTimeout)

	_, _, ok := proactiveDrainWindow(chartDrainTime, 5*time.Second)
	assert.False(t, ok, "Envoy's default drain_timeout must leave the window empty under a 10s drain time")

	start, end, ok := proactiveDrainWindow(chartDrainTime, DefaultConnectionDrainTimeout)
	require.True(t, ok, "the default drain_timeout must leave upstream's proactive drain window non-empty")
	assert.Equal(t, 6666666666*time.Nanosecond, start)
	assert.Equal(t, 8*time.Second, end)
	assert.LessOrEqual(t, end+DefaultConnectionDrainTimeout, chartDrainTime,
		"a drain sequence started at the end of the window must finish within the drain time")
}

// TestNodeDrainTimeoutNeverFallsBackToEnvoysDefault: a node HCM built with a
// zero or negative value carries the default, never an unset field.
func TestNodeDrainTimeoutNeverFallsBackToEnvoysDefault(t *testing.T) {
	assert.Equal(t, DefaultConnectionDrainTimeout, nodeDrainTimeout(0))
	assert.Equal(t, DefaultConnectionDrainTimeout, nodeDrainTimeout(-time.Second))
	assert.Equal(t, 3*time.Second, nodeDrainTimeout(3*time.Second))
}

// TestNodeProxyHCMsCarryTheConfiguredDrainTimeout: every HTTP connection
// manager the node proxy serves carries the drain_timeout its builder was
// given -- the mTLS inbound (default and per-port chains), the cleartext
// inbound, the HTTP/3 inbound, the outbound listener, the capture chain
// (scoped and redirect-all) and the health gateway. 3s is not the default,
// so a builder that ignored its argument fails.
func TestNodeProxyHCMsCarryTheConfiguredDrainTimeout(t *testing.T) {
	const configured = 3 * time.Second
	pod := quicTestPod() // two HTTP ports: a default chain and one SNI chain
	plain := &cniv1.CNIPod{Name: "p1", Namespace: "default", NetworkNamespace: "/var/run/netns/p1"}

	t.Run("inbound mtls and outbound", func(t *testing.T) {
		inbound, outbound, _, _, err := GenerateListenersFromRegistryPod(pod, "example.org", "mesh.local", false, false, nil, nil, "", configured)
		require.NoError(t, err)
		requireEveryHCMDrain(t, inbound, 2, configured)
		requireEveryHCMDrain(t, outbound, 1, configured)
	})
	t.Run("inbound cleartext", func(t *testing.T) {
		inbound, err := NewInboundListener(pod, "example.org", false, true, nil, nil, configured)
		require.NoError(t, err)
		requireEveryHCMDrain(t, inbound, 1, configured)
	})
	t.Run("inbound http3", func(t *testing.T) {
		l, err := NewInboundQUICListener(pod, "example.org", "mesh.local", false, false, nil, nil, configured)
		require.NoError(t, err)
		requireEveryHCMDrain(t, l, 2, configured)
	})
	t.Run("outbound", func(t *testing.T) {
		l, err := GenerateOutboundHTTPListener(plain, "spiffe://example.org/ns/default/sa/test", "mesh.local", false, nil, configured)
		require.NoError(t, err)
		requireEveryHCMDrain(t, l, 1, configured)
	})
	for name, withPassthrough := range map[string]bool{"capture scoped": false, "capture redirect-all": true} {
		t.Run(name, func(t *testing.T) {
			l, err := GenerateCaptureListener(plain, "spiffe://example.org/ns/default/sa/test", 18001, "mesh.local", false,
				[]CaptureTCPService{{ClusterName: "redis.mesh.local", ClusterIP: "10.96.1.10", LegacyTCPPort: 9000}}, withPassthrough, nil, configured)
			require.NoError(t, err)
			requireEveryHCMDrain(t, l, 1, configured)
		})
	}
	t.Run("health gateway", func(t *testing.T) {
		l := BuildHealthGatewayListener("/run/aether/health.sock", []HealthGatewayProbe{NewHealthGatewayProbe("health_a", "")}, configured)
		requireEveryHCMDrain(t, l, 1, configured)
	})
	t.Run("zero means the default", func(t *testing.T) {
		inbound, outbound, _, _, err := GenerateListenersFromRegistryPod(pod, "example.org", "mesh.local", false, false, nil, nil, "", 0)
		require.NoError(t, err)
		requireEveryHCMDrain(t, inbound, 2, DefaultConnectionDrainTimeout)
		requireEveryHCMDrain(t, outbound, 1, DefaultConnectionDrainTimeout)
		requireEveryHCMDrain(t, BuildHealthGatewayListener("/run/aether/health.sock", nil, 0), 1, DefaultConnectionDrainTimeout)
	})
}

// TestEdgeHCMsKeepEnvoysDefaultDrainTimeout: the edge Envoy runs without
// --drain-time-s (Envoy's 600s default), where upstream's proactive drain
// window is already open, so no edge HCM sets drain_timeout.
func TestEdgeHCMsKeepEnvoysDefaultDrainTimeout(t *testing.T) {
	edge := map[string]*listenerv3.Listener{
		"phase 1 http":          BuildEdgeListener(EdgeListenerName, 8080, nil),
		"phase 1 https":         BuildEdgeListener(EdgeHTTPSListenerName, 8443, []string{"kubernetes/api-tls"}),
		"phase 1 redirect":      BuildEdgeRedirectListener(8080),
		"readiness":             BuildEdgeReadinessListener(DefaultEdgeReadinessPort),
		"per-gateway http":      BuildEdgeGatewayHTTPListener("ns", "gw", 18150, false, nil, nil),
		"per-gateway redirect":  BuildEdgeGatewayHTTPListener("ns", "gw", 18150, true, nil, nil),
		"per-gateway https":     BuildEdgeGatewayHTTPSListener("ns", "gw", 18443, []string{"kubernetes/api-tls"}, nil, nil),
		"per-gateway h3 (quic)": BuildEdgeGatewayHTTP3Listener("ns", "gw", 18443, []string{"kubernetes/api-tls"}, nil, nil),
	}
	for name, l := range edge {
		t.Run(name, func(t *testing.T) {
			requireEveryHCMDrain(t, l, 1, -1)
		})
	}
}
