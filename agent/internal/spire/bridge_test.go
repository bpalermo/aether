package spire

import (
	"testing"

	"github.com/go-logr/logr"
)

// TestBridgeStartedNotClosedBeforeStart verifies the Started channel only closes
// once Start has connected to the SPIRE agent.
func TestBridgeStartedNotClosedBeforeStart(t *testing.T) {
	b := NewBridge("/nonexistent/socket", nil, nil, logr.Discard())
	select {
	case <-b.Started():
		t.Fatal("Started() closed before Start was called")
	default:
	}
}

// TestSubscribePodNoopBeforeStart verifies SubscribePod is a synchronized no-op
// before Start has connected (the started-channel gate, not a racy nil-check).
func TestSubscribePodNoopBeforeStart(t *testing.T) {
	b := NewBridge("/nonexistent/socket", nil, nil, logr.Discard())
	if err := b.SubscribePod("/proc/1/ns/net", "spiffe://example.org/x", nil); err != nil {
		t.Fatalf("SubscribePod before Start must be a no-op, got: %v", err)
	}
	b.subsMu.Lock()
	defer b.subsMu.Unlock()
	if len(b.subscriptions) != 0 {
		t.Fatalf("no subscription must be recorded before Start, got %d", len(b.subscriptions))
	}
}
