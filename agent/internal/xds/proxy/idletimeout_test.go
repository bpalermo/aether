package proxy

import (
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hcmIdleTimeouts returns the downstream idle timeout of every HTTP connection
// manager on a listener (its filter chains and its default chain), keyed by
// "<chain name>/<stat prefix>". An HCM with no idle timeout reads as 0, which
// no assertion below accepts: an unset field is Envoy's default, and the point
// of aether#1350 is that neither side relies on a default.
func hcmIdleTimeouts(t *testing.T, l *listenerv3.Listener) map[string]time.Duration {
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
			var idle time.Duration
			if d := hcm.GetCommonHttpProtocolOptions().GetIdleTimeout(); d != nil {
				idle = d.AsDuration()
			}
			out[fc.GetName()+"/"+hcm.GetStatPrefix()] = idle
		}
	}
	return out
}

// requireEveryHCMIdle asserts a listener carries at least wantHCMs HTTP
// connection managers and that every one of them has the given idle timeout.
func requireEveryHCMIdle(t *testing.T, l *listenerv3.Listener, wantHCMs int, want time.Duration) {
	t.Helper()
	got := hcmIdleTimeouts(t, l)
	require.GreaterOrEqual(t, len(got), wantHCMs, "%s: HTTP connection managers found: %v", l.GetName(), got)
	for key, idle := range got {
		assert.Equal(t, want, idle, "%s: %s", l.GetName(), key)
	}
}

// TestIdleTimeoutValues pins the two numbers and the relations between them
// (aether#1350). The peer-facing value must exceed what a peer proxy's pool
// idles out at, so the peer always closes first; the app-facing value must
// exceed the peer-facing one by a wide margin, because its job is to exceed
// the idle timeout of any ordinary HTTP client.
func TestIdleTimeoutValues(t *testing.T) {
	assert.Equal(t, 5*time.Minute, peerFacingIdleTimeout)
	assert.Equal(t, time.Hour, appFacingIdleTimeout)
	assert.Greater(t, peerFacingIdleTimeout, config.UpstreamIdleTimeout,
		"the inbound timeout must exceed the peer's pool idle timeout so the peer disconnects first")
	assert.Greater(t, appFacingIdleTimeout, peerFacingIdleTimeout,
		"the app-facing timeout must not be the peer-facing one")
	assert.Equal(t, appFacingIdleTimeout, edgeDefaultIdleTimeout,
		"the edge's client-facing default and the node proxy's app-facing timeout are the same rule")
}

// TestBuildHTTPConnectionManagerUsesTheCallersIdleTimeout: the shared builder
// has no idle timeout of its own.
func TestBuildHTTPConnectionManagerUsesTheCallersIdleTimeout(t *testing.T) {
	for _, d := range []time.Duration{peerFacingIdleTimeout, appFacingIdleTimeout, 42 * time.Second} {
		hcm := buildHTTPConnectionManager("test", ReporterSource, "pod", "ns", nil, d)
		idle := hcm.GetCommonHttpProtocolOptions().GetIdleTimeout()
		require.NotNil(t, idle, "downstream idle timeout must be set")
		assert.Equal(t, d, idle.AsDuration())
	}
}

// TestMeshInboundKeepsThePeerFacingIdleTimeout: every HCM whose downstream is
// a peer proxy keeps 5 minutes: the mTLS inbound (default chain and per-port
// SNI chain), the cleartext inbound (SPIRE off) and the HTTP/3 inbound.
func TestMeshInboundKeepsThePeerFacingIdleTimeout(t *testing.T) {
	pod := quicTestPod() // two HTTP ports: a default chain and one SNI chain

	t.Run("mtls", func(t *testing.T) {
		inbound, _, _, _, err := GenerateListenersFromRegistryPod(pod, "example.org", "mesh.local", false, false, nil, nil, "")
		require.NoError(t, err)
		requireEveryHCMIdle(t, inbound, 2, peerFacingIdleTimeout)
	})
	t.Run("cleartext", func(t *testing.T) {
		inbound, _, _, _, err := GenerateListenersFromRegistryPod(pod, "example.org", "mesh.local", false, true, nil, nil, "")
		require.NoError(t, err)
		requireEveryHCMIdle(t, inbound, 1, peerFacingIdleTimeout)
	})
	t.Run("quic", func(t *testing.T) {
		l, err := NewInboundQUICListener(pod, "example.org", "mesh.local", false, false, nil, nil)
		require.NoError(t, err)
		require.NotNil(t, l)
		requireEveryHCMIdle(t, l, 2, peerFacingIdleTimeout)
	})
}

// TestAppFacingListenersGetTheAppFacingIdleTimeout: every HCM whose downstream
// is the local application gets 1 hour: the per-pod outbound listener (18081)
// and the per-pod capture chain, scoped and redirect-all.
func TestAppFacingListenersGetTheAppFacingIdleTimeout(t *testing.T) {
	pod := &cniv1.CNIPod{Name: "p1", Namespace: "default", NetworkNamespace: "/var/run/netns/p1"}

	t.Run("outbound", func(t *testing.T) {
		_, outbound, _, _, err := GenerateListenersFromRegistryPod(pod, "example.org", "mesh.local", false, false, nil, nil, "")
		require.NoError(t, err)
		requireEveryHCMIdle(t, outbound, 1, appFacingIdleTimeout)
	})
	for name, withPassthrough := range map[string]bool{"capture scoped": false, "capture redirect-all": true} {
		t.Run(name, func(t *testing.T) {
			l, err := GenerateCaptureListener(pod, "spiffe://example.org/ns/default/sa/test", 18001, "mesh.local", false,
				[]CaptureTCPService{{ClusterName: "redis.mesh.local", ClusterIP: "10.96.1.10", PrimaryIsTCP: true}}, withPassthrough, nil)
			require.NoError(t, err)
			requireEveryHCMIdle(t, l, 1, appFacingIdleTimeout)
		})
	}
}

// TestEdgeListenersIdleTimeout: the edge's downstream is an external client,
// never a peer proxy. Its per-Gateway listeners already carried 1 hour through
// ApplyEdgeHardening; the Phase 1 shared listeners used to inherit the mesh
// inbound's 5 minutes and now state the same 1 hour. The readiness listener is
// probed by the kubelet, which reuses no connection, and is left as it was.
func TestEdgeListenersIdleTimeout(t *testing.T) {
	clientFacing := map[string]*listenerv3.Listener{
		"phase 1 http":          BuildEdgeListener(EdgeListenerName, 8080, nil),
		"phase 1 https":         BuildEdgeListener(EdgeHTTPSListenerName, 8443, []string{"kubernetes/api-tls"}),
		"phase 1 redirect":      BuildEdgeRedirectListener(8080),
		"per-gateway http":      BuildEdgeGatewayHTTPListener("ns", "gw", 18150, false, nil, nil),
		"per-gateway redirect":  BuildEdgeGatewayHTTPListener("ns", "gw", 18150, true, nil, nil),
		"per-gateway https":     BuildEdgeGatewayHTTPSListener("ns", "gw", 18443, []string{"kubernetes/api-tls"}, nil, nil),
		"per-gateway h3 (quic)": BuildEdgeGatewayHTTP3Listener("ns", "gw", 18443, []string{"kubernetes/api-tls"}, nil, nil),
	}
	for name, l := range clientFacing {
		t.Run(name, func(t *testing.T) {
			require.NotNil(t, l)
			requireEveryHCMIdle(t, l, 1, edgeDefaultIdleTimeout)
		})
	}
	t.Run("readiness", func(t *testing.T) {
		requireEveryHCMIdle(t, BuildEdgeReadinessListener(DefaultEdgeReadinessPort), 1, peerFacingIdleTimeout)
	})
}
