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
