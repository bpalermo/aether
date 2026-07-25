package capture

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingRewriter records how many times the heartbeat asked for a re-stamp.
type countingRewriter struct {
	calls atomic.Int64
	fired chan struct{}
}

func (r *countingRewriter) RewriteMeshDNSSnapshot() {
	if r.calls.Add(1) == 1 {
		close(r.fired)
	}
}

// TestMeshDNSHeartbeatTicks: the runnable re-stamps the snapshot on its interval and
// stops cleanly (no error) when the manager's context is cancelled.
func TestMeshDNSHeartbeatTicks(t *testing.T) {
	r := &countingRewriter{fired: make(chan struct{})}
	h := &MeshDNSHeartbeat{Rewriter: r, Interval: time.Millisecond, Log: slog.New(slog.DiscardHandler)}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.Start(ctx) }()

	select {
	case <-r.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat never fired")
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "cancellation is a clean stop, never an error")
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not stop on context cancellation")
	}
	assert.Positive(t, r.calls.Load())
}

// TestMeshDNSHeartbeatDefaults: a zero Interval falls back to the package default, and
// the runnable opts out of leader election (every node must re-stamp its OWN snapshot).
func TestMeshDNSHeartbeatDefaults(t *testing.T) {
	r := &countingRewriter{fired: make(chan struct{})}
	h := &MeshDNSHeartbeat{Rewriter: r, Log: slog.New(slog.DiscardHandler)}
	assert.False(t, h.NeedLeaderElection())
	assert.Equal(t, 60*time.Second, MeshDNSHeartbeatInterval)

	// With the default interval nothing fires within the short cancellation window,
	// but Start must still exit cleanly.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.Start(ctx) }()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not stop on context cancellation")
	}
	assert.Zero(t, r.calls.Load(), "the default interval had not elapsed")
}
