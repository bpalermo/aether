package cache

import (
	"context"
	"sync"
	"time"
)

// defaultQUICPublishWindow is how long the first QUIC twin admission of a
// burst waits for the rest of the burst before the snapshot carrying them is
// built (issue #1086). It bounds the latency one lone admission pays; a burst
// that outlasts it is still published by at most one more build per build in
// flight (see quicPublisher).
//
// A k6 loader's first requests to 7-10 QUIC destinations reached the agent's
// ODCDS handler within ~110 ms on talos (2026-10-01, main-worker-05), against
// an on_demand budget of 2 s that the admission, the snapshot, the CDS push
// and the twin's warming (EDS + SDS) all have to fit in.
const defaultQUICPublishWindow = 10 * time.Millisecond

// quicPublisher coalesces the snapshot publishes that `quic:` twin admissions
// request (issue #1086).
//
// Before it, every admitted pair started its own goroutine running a full
// generateSnapshot. They all serialized on snapshotMu, and the first one to
// get the lock already carried every pair recorded so far -- admission
// records the pair synchronously -- so the rest rebuilt and re-set an
// identical snapshot, one after another. On talos each of those costs ~55 ms
// of CPU in go-control-plane's version-map hashing alone (proto marshal +
// sha256 of every resource, in SetSnapshot under the cache's own mutex) on an
// agent capped at 200m, i.e. 200-870 ms of wall clock each; the ADS stream
// goroutine needs that same mutex for every request it handles, including the
// EDS subscription the new twin must be answered on before Envoy can warm it.
// Six admissions on main-worker-05 were followed by eight snapshot builds in
// 4.4 s, the last four rebuilding an unchanged shape, and the four twins
// delivered at 30.14Z were still warming when their on_demand timeouts fired
// at 30.645-30.738Z: 51 x 503 NC cluster_not_found.
//
// The publisher keeps at most ONE publish pending and at most one running:
//
//   - The first request of a burst starts a worker, which waits the window
//     (so the burst's other admissions join it) and then builds once.
//   - A request that arrives while the worker is waiting is already covered.
//   - A request that arrives while the worker is building -- its pair may
//     have been recorded after the build read the dependency state -- marks
//     the publish pending again, and the worker builds exactly once more.
//
// So a burst of N admissions costs one or two builds instead of N, and the
// last build always starts after the last admission was recorded: nothing an
// admission recorded is ever left unpublished.
//
// Only the publish is coalesced. What an admission decides and records
// (recordQUICPair: the pair, its confirmation for the fetch-window prune, the
// ledger subscription, the persisted demand set) still happens synchronously
// on the ODCDS request, so the strand rule (#1036/#1052), the cert gate (#1051:
// the fan-out holds a twin whose source has no SVID, at build time) and the
// stream-reset classification (#1033) are untouched.
type quicPublisher struct {
	mu sync.Mutex
	// pending: a publish was requested that no build has started on yet.
	pending bool
	// running: a worker goroutine is live (waiting the window or building).
	running bool
	// requests counts the requests the next build answers, for its log line.
	requests int
}

// requestQUICPublish asks for a snapshot carrying every QUIC pair recorded so
// far, coalesced with every other request of the same burst. It never blocks:
// it is called from the xDS stream's request callback, and publishing a
// snapshot from inside it could block on the very stream whose request is
// being processed.
func (c *SnapshotCache) requestQUICPublish(ctx context.Context, admitted int) {
	p := &c.quicPublish
	p.mu.Lock()
	p.pending = true
	p.requests += admitted
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.mu.Unlock()
	go c.runQUICPublisher(context.WithoutCancel(ctx))
}

// runQUICPublisher is the publisher's worker: wait the window, build once if a
// publish is pending, repeat until a window passes with nothing pending.
func (c *SnapshotCache) runQUICPublisher(ctx context.Context) {
	p := &c.quicPublish
	window := c.quicPublishWindowValue()
	for {
		if window > 0 {
			time.Sleep(window)
		}
		p.mu.Lock()
		if !p.pending {
			p.running = false
			p.mu.Unlock()
			return
		}
		p.pending = false
		admitted := p.requests
		p.requests = 0
		p.mu.Unlock()

		start := time.Now()
		if err := c.generateSnapshot(ctx); err != nil {
			c.log.Error("failed to publish the snapshot for observed QUIC pairs", "admitted", admitted, "error", err)
			continue
		}
		c.log.InfoContext(ctx, "published observed east-west QUIC pairs: one snapshot for the coalesced admissions",
			"admitted", admitted, "build_ms", time.Since(start).Milliseconds())
	}
}

// quicPublishWindowValue returns the coalescing window: the test hook when
// set (negative = no wait at all), else the default.
func (c *SnapshotCache) quicPublishWindowValue() time.Duration {
	switch {
	case c.quicPublishWindow > 0:
		return c.quicPublishWindow
	case c.quicPublishWindow < 0:
		return 0
	default:
		return defaultQUICPublishWindow
	}
}
