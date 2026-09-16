package registrar

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWatchLoop_FilterReassertIsNeverLost drives the real watch loop against the
// in-process fake and hammers the window between the loop reading the filter to
// assert and publishing the canceller for the stream it is about to open.
//
// The fake holds every stream open until its context ends, so the ONLY thing
// that can make the loop reconnect here is a filter re-assert cancelling the
// live stream. A re-assert that is lost therefore never self-heals: the loop
// sits on a stream carrying the stale scope, which is the #682 stall shape —
// the demand set shrank (or grew) and the registrar was never told (#772, S8).
//
// Bursts of SetServiceFilter calls are what hit the window: each one cancels the
// live stream while the loop is already re-opening from the previous one. With
// the read and the publish in one filterMu critical section, every interleaving
// converges on the newest filter.
//
// Reproduction: with the read and the publish split back into two critical
// sections (and a runtime.Gosched() between them to widen a window that is
// otherwise a handful of instructions), this fails 2/10 runs — the loop wedges
// on a stream carrying a filter nobody will ever correct. With the fix it is
// 10/10 green.
func TestWatchLoop_FilterReassertIsNeverLost(t *testing.T) {
	fake, dialOpts := startFakeRegistrar(t, nil)
	r, logs := newLoggingRegistry(t, dialOpts)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, r.Initialize(ctx))
	defer func() { _ = r.Close() }()

	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	require.NoError(t, r.WaitReady(readyCtx))

	// Each round bursts many distinct filters as fast as the loop can churn
	// streams, then requires that the LAST one reached the server. A re-assert
	// lost mid-burst is invisible (a later one supersedes it); a lost LAST one
	// is terminal, because nothing else will ever cancel that stream.
	for round := range 40 {
		var want []string
		for i := range 4 {
			want = []string{fmt.Sprintf("default/svc-%d-%d", round, i)}
			r.SetServiceFilter(want)
		}
		require.Eventuallyf(t, func() bool {
			last := fake.lastRequest()
			return last != nil && slices.Equal(last.GetFilter().GetServices(), want)
		}, 10*time.Second, time.Millisecond,
			"round %d: the loop never re-asserted %v; it is stuck on a stream with a stale filter. logs:\n%s",
			round, want, logs.String())
	}
}

// TestAssertFilter_PublishAndReadAreOneCriticalSection is the unit-level form of
// the same invariant, without the gRPC machinery: for every interleaving with a
// concurrent SetServiceFilter, either this stream carries the new filter or its
// canceller was invoked. Never neither — "neither" is a stream nobody will ever
// correct.
func TestAssertFilter_PublishAndReadAreOneCriticalSection(t *testing.T) {
	const iterations = 500
	updated := []string{"default/svc-b"}

	for i := range iterations {
		r := newTestRegistry()
		r.SetServiceFilter([]string{"default/svc-a"})

		var mu sync.Mutex
		var cancelled bool
		var asserted []string

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			// The loop: publish this stream's canceller and read the filter it
			// must assert on it.
			svcs, _ := r.assertFilter(func() {
				mu.Lock()
				cancelled = true
				mu.Unlock()
			})
			mu.Lock()
			asserted = svcs
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			<-start
			r.SetServiceFilter(updated)
		}()
		close(start)
		wg.Wait()

		mu.Lock()
		stale := !stringSetsEqual(asserted, updated)
		corrected := cancelled
		mu.Unlock()
		require.Falsef(t, stale && !corrected,
			"iteration %d: stream opened with the stale filter %v and nothing cancelled it", i, asserted)
	}
}
