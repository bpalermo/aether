package envoy_validate

import (
	"strings"
	"testing"
	"time"
)

// wantNodeConnectionDrainTimeout is proxy.DefaultConnectionDrainTimeout as a
// literal: the arming condition of upstream Envoy's proactive idle-connection
// drain (drain time > 3 x drain_timeout) is checked against the chart's 10s
// drain time below, so a change of the constant has to be made here on purpose.
const wantNodeConnectionDrainTimeout = 2 * time.Second

// TestNodeHCMsCarryTheConnectionDrainTimeout: every node-proxy HTTP
// connection manager in the fixtures TestEnvoyValidate hands to the pinned
// Envoy carries drain_timeout 2s, so the values checked are values Envoy
// accepted; every edge HCM leaves it unset (the edge runs on Envoy's 600s
// default drain time, where upstream's window is open anyway).
func TestNodeHCMsCarryTheConnectionDrainTimeout(t *testing.T) {
	const chartDrainTime = 10 * time.Second
	if start, end := chartDrainTime*2/3, chartDrainTime-wantNodeConnectionDrainTimeout; start >= end {
		t.Fatalf("upstream's proactive drain window [%v, %v] is empty under the chart's drain time", start, end)
	}

	fixtures := []struct {
		name string
		fn   func() ([]byte, error)
		edge bool
		min  int
	}{
		// TCP mTLS inbound + HTTP/3 inbound, the outbound listener, the health gateway.
		{"node_bootstrap.json", NodeBootstrapJSON, false, 4},
		{"node_cleartext_bootstrap.json", NodeCleartextBootstrapJSON, false, 2},
		{"node_uds_bootstrap.json", NodeUDSBootstrapJSON, false, 2},
		{"capture_bootstrap.json", CaptureBootstrapJSON, false, 1},
		{"capture_perport_bootstrap.json", CapturePerPortBootstrapJSON, false, 1},
		{"edge_bootstrap.json", EdgeBootstrapJSON, true, 3},
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
			if len(hcms) < fx.min {
				t.Fatalf("fixture carries %d HTTP connection managers, want >= %d", len(hcms), fx.min)
			}
			for _, h := range hcms {
				if strings.HasSuffix(h.StatPrefix, "_validate") {
					// A harness-built HCM that only carries a route table into
					// validation (perport.go, builders.go); not an agent HCM.
					continue
				}
				where := h.Listener + "/" + h.Chain + " (stat_prefix " + h.StatPrefix + ")"
				if fx.edge || strings.HasPrefix(h.StatPrefix, "edge_") {
					if h.DrainSet {
						t.Errorf("%s: edge HCM sets drain_timeout %v, want Envoy's default", where, h.Drain)
					}
					continue
				}
				if !h.DrainSet || h.Drain != wantNodeConnectionDrainTimeout {
					t.Errorf("%s: drain_timeout = %v (set=%v), want %v", where, h.Drain, h.DrainSet, wantNodeConnectionDrainTimeout)
				}
			}
		})
	}
}
