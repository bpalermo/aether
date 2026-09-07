package registrar

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestNotifyIdentityReadyCutsTheRetrySleep is issue #740's finding 3 at the
// retry level: when the thing the loop was waiting for arrives, the rest of the
// backoff is pure added downtime.
//
// Both retry paths are covered because both were live during the outage — the
// deferral while the SVID was pending, and the ordinary failure path after it
// landed but before the transport recovered.
func TestNotifyIdentityReadyCutsTheRetrySleep(t *testing.T) {
	t.Run("failStream", func(t *testing.T) {
		r, _, _ := newIdentityGatedRegistry(t, func() bool { return true })

		backoff := 30 * time.Second // the cap: where a long outage leaves it
		go func() {
			time.Sleep(20 * time.Millisecond)
			r.NotifyIdentityReady()
		}()

		start := time.Now()
		require.True(t, r.failStream(t.Context(), "watch stream disconnected, retrying", errors.New("boom"), &backoff))
		assert.Less(t, time.Since(start), 2*time.Second, "the wake must cut the 30s backoff short")
	})

	t.Run("deferStream", func(t *testing.T) {
		r, _, _ := newIdentityGatedRegistry(t, func() bool { return false })

		go func() {
			time.Sleep(20 * time.Millisecond)
			r.NotifyIdentityReady()
		}()

		start := time.Now()
		require.True(t, r.deferStream(t.Context(), errors.New("no SPIRE SVID yet"), 30*time.Second))
		assert.Less(t, time.Since(start), 2*time.Second)
	})

	t.Run("shutdown still wins", func(t *testing.T) {
		// The wake must not turn a shutdown into another attempt.
		r, _, _ := newIdentityGatedRegistry(t, func() bool { return true })
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		backoff := 30 * time.Second
		assert.False(t, r.failStream(ctx, "watch stream disconnected, retrying", errors.New("boom"), &backoff))
	})

	t.Run("safe before Initialize", func(t *testing.T) {
		// The agent wires the wake from a goroutine that can fire at any moment,
		// including before the client has a connection to reset.
		r, _, _ := newIdentityGatedRegistry(t, func() bool { return false })
		require.Nil(t, r.conn)
		assert.NotPanics(t, r.NotifyIdentityReady)
	})
}

// TestWatchLoop_ConnectsImmediatelyWhenIdentityArrives is the end-to-end shape
// of the same finding, with the fake registrar standing in for the transport
// that cannot handshake until this workload has an SVID.
//
// On 2026-09-07 the SVID landed at 19:23:11 and readiness flipped 3s later, but
// the node's mTLS clients kept failing until 19:25:22 — 2m11s of "recovered"
// that was not, because the gRPC ClientConn had backed off to its ~120s cap and
// was answering from the cached handshake failure instead of dialling again.
// Nothing polls a ClientConn out of that; something has to reset it. This pins
// the observable consequence: once identity exists the very next attempt
// connects, in about a second rather than in minutes.
func TestWatchLoop_ConnectsImmediatelyWhenIdentityArrives(t *testing.T) {
	// Nothing is listening and identity is pending: every attempt fails the way
	// a handshake without a certificate does.
	target := &swappableListener{}
	var ready atomic.Bool

	r, logs := newLoggingRegistry(t, []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(target.dial),
	})
	r.config.IdentityReady = ready.Load
	metrics, _ := newTestClientMetrics(t)
	r.metrics = metrics

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, r.Initialize(ctx))
	defer func() { _ = r.Close() }()

	require.Eventually(t, func() bool {
		return findRecord(logRecords(t, logs), "watch stream deferred until this agent has an SVID") != nil
	}, 30*time.Second, 10*time.Millisecond, "the client must be waiting on identity first; logs:\n%s", logs.String())

	// SPIRE starts serving: identity exists, the peer is reachable, and the agent
	// says so.
	_, lis, _ := serveCatalog(t, "svc-a", "v1")
	target.set(lis)
	ready.Store(true)

	start := time.Now()
	r.NotifyIdentityReady()

	select {
	case <-r.Reconnects():
	case <-time.After(10 * time.Second):
		t.Fatalf("the watch stream did not connect after identity arrived; logs:\n%s", logs.String())
	}
	assert.Less(t, time.Since(start), 2*time.Second,
		"recovery must follow identity within about a second, not a backoff interval")
}
