package envoy_validate

import (
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
)

// The two downstream idle timeouts, as literals: this test pins the numbers,
// so a change to either constant in agent/internal/xds/proxy has to be made
// here too, on purpose.
const (
	// A peer proxy is the downstream. Peers idle their pools out at 30s
	// (config.UpstreamIdleTimeout), so this is a backstop for the ones that
	// leak.
	wantPeerFacingIdle = 5 * time.Minute
	// The local application (node proxy) or an external client (edge) is the
	// downstream. The server's idle timeout must exceed the client's: HTTP/1.1
	// has no GOAWAY.
	wantClientFacingIdle = time.Hour
)

// downstreamKind classifies an HTTP connection manager by its stat_prefix,
// which is what names its kind in the generators. ok is false for a prefix
// this test has never been told about, and the test fails on it: a new HCM
// has to say who its downstream is before it ships.
func downstreamKind(statPrefix string) (kind string, want time.Duration, wantSet, ok bool) {
	switch {
	case statPrefix == "inbound":
		// Mesh inbound, TCP and QUIC, mTLS and cleartext: peer proxies.
		return "peer-facing", wantPeerFacingIdle, true, true
	case statPrefix == "capture_http", statPrefix == "outbound_http":
		// Per-pod capture chain and per-pod outbound listener: the pod's app.
		return "app-facing", wantClientFacingIdle, true, true
	case strings.HasPrefix(statPrefix, "edge_gw_"):
		// Per-Gateway edge listeners: external clients.
		return "edge client-facing", wantClientFacingIdle, true, true
	case statPrefix == proxy.HealthGatewayListenerName:
		// The agent's liveness loop over a node-local UDS. It sets no idle
		// timeout (Envoy's default, 1h) and is not built by the shared HCM
		// builder; recorded here so that a change to it is seen.
		return "agent-facing", 0, false, true
	}
	return "", 0, false, false
}

// TestDownstreamIdleTimeoutFollowsWhoTheDownstreamIs (aether#1350): the mesh
// inbound keeps 5 minutes, and every HCM whose downstream is an application or
// an external client carries 1 hour. Until #1350 one constant served both, and
// the proxy idle-closed an application's kept-alive connections after 5
// minutes; an HTTP/1.1 client that reused one at that instant, and did not
// retry, got a reset.
//
// Over the generated fixture bytes that TestEnvoyValidate hands to Envoy, so
// the values checked are the values the pinned proxy accepted.
func TestDownstreamIdleTimeoutFollowsWhoTheDownstreamIs(t *testing.T) {
	fixtures := []struct {
		name string
		fn   func() ([]byte, error)
		// want is how many HCMs of each kind the fixture must carry, so that
		// neither half of the split can pass on a fixture that lost it.
		want map[string]int
	}{
		// TCP mTLS inbound + HTTP/3 inbound, the outbound listener, the health gateway.
		{"node_bootstrap.json", NodeBootstrapJSON, map[string]int{"peer-facing": 2, "app-facing": 1, "agent-facing": 1}},
		// SPIRE off: the downstream of the cleartext inbound is still a peer proxy.
		{"node_cleartext_bootstrap.json", NodeCleartextBootstrapJSON, map[string]int{"peer-facing": 1, "app-facing": 1}},
		{"node_uds_bootstrap.json", NodeUDSBootstrapJSON, map[string]int{"peer-facing": 1, "app-facing": 1}},
		{"capture_bootstrap.json", CaptureBootstrapJSON, map[string]int{"app-facing": 1}},
		// HTTP, HTTPS and HTTP/3 per-Gateway listeners.
		{"edge_bootstrap.json", EdgeBootstrapJSON, map[string]int{"edge client-facing": 3}},
	}

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			data, err := fx.fn()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			hcms, err := HCMIdleTimeouts(data)
			if err != nil {
				t.Fatalf("HCMIdleTimeouts: %v", err)
			}
			seen := map[string]int{}
			for _, h := range hcms {
				where := h.Listener + "/" + h.Chain + " (stat_prefix " + h.StatPrefix + ")"
				kind, want, wantSet, ok := downstreamKind(h.StatPrefix)
				if !ok {
					t.Errorf("%s: an HTTP connection manager this test cannot classify.\n"+
						"Say who its downstream is (a peer proxy, the local application, an external client) in downstreamKind", where)
					continue
				}
				seen[kind]++
				if h.Set != wantSet || h.Idle != want {
					t.Errorf("%s: %s idle timeout = %v (set=%v), want %v (set=%v)", where, kind, h.Idle, h.Set, want, wantSet)
				}
			}
			for kind, n := range fx.want {
				if seen[kind] < n {
					t.Errorf("fixture carries %d %s HTTP connection manager(s), want >= %d: that half of the check is vacuous", seen[kind], kind, n)
				}
			}
		})
	}

	// The relations the two numbers must keep, whatever they are changed to.
	if wantPeerFacingIdle <= config.UpstreamIdleTimeout {
		t.Errorf("peer-facing idle timeout %v must exceed the peers' pool idle timeout %v, so the peer closes first", wantPeerFacingIdle, config.UpstreamIdleTimeout)
	}
	if wantClientFacingIdle <= wantPeerFacingIdle {
		t.Errorf("client-facing idle timeout %v must exceed the peer-facing %v", wantClientFacingIdle, wantPeerFacingIdle)
	}
}
