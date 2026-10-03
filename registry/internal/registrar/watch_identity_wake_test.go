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
// The ClientConn's redial backoff is now capped short (registrarConnectParams;
// #1137 removed the ResetConnectBackoff that used to do this job). This pins
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

// TestWatchLoop_ReconnectWaitEndsWhenTheConnectionIsReady is issue #1123's
// registry term. The wake above retries at once, but that first retry races
// the redial: the ClientConn still answers it
// with the failure it cached before identity, the loop classifies it as the
// post-identity reconnect, and then slept a whole initialBackoff (1s + jitter)
// although the connection came up a few milliseconds later. On talos-main that
// was the steady ~1.05s between `identity acquired` and `watch stream
// connected` on every node of the 2026-10-02 agent rolls (03:05Z, 06:53Z), all
// of it inside the window in which the node's proxy has no ADS stream.
//
// The reconnect wait now ends as soon as the ClientConn reports READY, so the
// stream follows identity by a dial, not by a backoff interval.
func TestWatchLoop_ReconnectWaitEndsWhenTheConnectionIsReady(t *testing.T) {
	for i := range 5 {
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
		require.NoError(t, r.Initialize(ctx))

		require.Eventually(t, func() bool {
			return findRecord(logRecords(t, logs), "watch stream deferred until this agent has an SVID") != nil
		}, 30*time.Second, 10*time.Millisecond, "logs:\n%s", logs.String())

		_, lis, _ := serveCatalog(t, "svc-a", "v1")
		target.set(lis)
		ready.Store(true)

		start := time.Now()
		r.NotifyIdentityReady()

		select {
		case <-r.Reconnects():
		case <-time.After(10 * time.Second):
			t.Fatalf("run %d: the watch stream did not connect after identity arrived; logs:\n%s", i, logs.String())
		}
		took := time.Since(start)
		cancel()
		_ = r.Close()
		assert.Less(t, took, 500*time.Millisecond,
			"run %d: the stream must follow identity by a dial, not by initialBackoff (%s); logs:\n%s", i, initialBackoff, logs.String())
	}
}

// TestWatchLoop_RedialFollowsALongIdentityWaitWithinTheCap pins what replaced
// #740's ResetConnectBackoff (removed by #1137: it races grpc-go's subchannel
// creation). Identity stays pending long enough for the ClientConn's own redial
// backoff to climb all the way to its cap, and nothing resets it when the SVID
// lands. The stream must still follow identity within about the cap
// (registrarConnectParams: 500ms plus jitter), where gRPC's default ladder
// (1s, 1.6s, 2.56s, ... 120s) would leave it seconds away and, after a real
// outage, minutes.
func TestWatchLoop_RedialFollowsALongIdentityWaitWithinTheCap(t *testing.T) {
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
	}, 30*time.Second, 10*time.Millisecond, "logs:\n%s", logs.String())

	// The cap is reached after ~0.9s of failures; three seconds is well past it
	// and, with gRPC's default policy, two rungs into a ladder that keeps
	// climbing.
	time.Sleep(3 * time.Second)

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
	assert.Less(t, time.Since(start), time.Second,
		"the redial must follow identity within the ClientConn's capped backoff (%s); logs:\n%s",
		registrarConnectParams.Backoff.MaxDelay, logs.String())
}
