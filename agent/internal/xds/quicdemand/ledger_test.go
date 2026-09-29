package quicdemand

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The ledger's transitions (issue #1036): a retired pair is kept dormant only
// if the proxy holds a subscription for it; a dormant pair is revived when
// valid; a fresh stream's re-statement is the only thing that prunes it.
func TestLedger(t *testing.T) {
	t0 := time.Unix(1000, 0)
	l := NewLedger[string]()

	// Not subscribed: retiring forgets (the proxy drops a cluster it holds
	// without a subscription, and its next request re-fetches).
	assert.False(t, l.Retire(twinA, t0))
	assert.False(t, l.IsDormant(twinA))

	// Subscribed: retiring keeps it dormant, with its time.
	l.Subscribe(1, twinA)
	l.Subscribe(1, twinB)
	assert.True(t, l.Subscribed(twinA))
	assert.True(t, l.Retire(twinA, t0))
	assert.True(t, l.Retire(twinB, t0))
	assert.Equal(t, map[string]time.Time{twinA: t0, twinB: t0}, l.Dormant())

	// Revive only what is valid.
	woken := l.Revive(func(k string) bool { return k == twinA })
	assert.Equal(t, map[string]time.Time{twinA: t0}, woken)
	assert.Equal(t, map[string]time.Time{twinB: t0}, l.Dormant())
	assert.Nil(t, l.Revive(func(string) bool { return false }))

	// Park keeps an existing entry's time.
	l.Park(twinB, t0.Add(time.Hour))
	l.Park(twinC, t0.Add(time.Hour))
	assert.Equal(t, map[string]time.Time{twinB: t0, twinC: t0.Add(time.Hour)}, l.Dormant())

	// Wake a single pair.
	at, ok := l.Wake(twinC)
	assert.True(t, ok)
	assert.Equal(t, t0.Add(time.Hour), at)
	_, ok = l.Wake(twinC)
	assert.False(t, ok)

	// A fresh stream that re-subscribes B keeps it; the subscription set is
	// replaced (A is no longer subscribed).
	l.Park(twinC, t0)
	assert.ElementsMatch(t, []string{twinC}, l.Restate(1, []string{twinB}))
	assert.Equal(t, map[string]time.Time{twinB: t0}, l.Dormant())
	assert.False(t, l.Subscribed(twinA))
	assert.True(t, l.Subscribed(twinB))

	// A new proxy generation (hot-restart child) re-subscribes nothing. The
	// parent's stream is still live and still vouches for B (issue #1052);
	// once the parent exits, every dormant pair is pruned.
	assert.Empty(t, l.Restate(2, nil))
	assert.Equal(t, map[string]time.Time{twinB: t0}, l.Dormant())
	assert.ElementsMatch(t, []string{twinB}, l.Close(1))
	assert.Empty(t, l.Dormant())
	assert.False(t, l.Subscribed(twinB))
}

// Issue #1052: an agent restart that lands mid hot restart reconnects TWO proxy
// generations, the child (no ODCDS subscriptions) and the draining parent
// (which re-subscribes its twins), in either order. Dormant pairs restored from
// storage must end up held exactly by the generation that re-subscribed them,
// and pruned when that generation's stream ends while the other is live.
func TestLedgerOverlappingGenerationsAfterAgentRestart(t *testing.T) {
	t0 := time.Unix(1000, 0)
	const child, parent int64 = 2, 3

	restored := func() *Ledger[string] {
		l := NewLedger[string]()
		// Persisted dormant pairs, reloaded at agent start: no stream holds them.
		l.Park(twinA, t0)
		l.Park(twinB, t0)
		return l
	}

	t.Run("new generation first, then the old one", func(t *testing.T) {
		l := restored()
		// rev248 main-worker-03 15:39:16: the child's stream names nothing.
		assert.ElementsMatch(t, []string{twinA, twinB}, l.Restate(child, nil))
		assert.Empty(t, l.Dormant())
		// 15:39:27: the parent's stream re-subscribes A; the cache parks it
		// (its source is away).
		assert.Empty(t, l.Restate(parent, []string{twinA}))
		l.Park(twinA, t0)
		assert.True(t, l.Subscribed(twinA), "the parent holds A while its stream is live")
		assert.Equal(t, map[string]time.Time{twinA: t0}, l.Dormant())
		// The parent exits: nothing live holds A any more.
		assert.ElementsMatch(t, []string{twinA}, l.Close(parent))
		assert.Empty(t, l.Dormant(), "no phantom dormant pair once the parent is gone")
		assert.False(t, l.Subscribed(twinA))
		assert.False(t, l.Retire(twinA, t0), "a twin only the exited parent held is forgotten, not parked")
	})

	t.Run("old generation first, then the new one", func(t *testing.T) {
		l := restored()
		assert.ElementsMatch(t, []string{twinB}, l.Restate(parent, []string{twinA}),
			"B: no live stream holds it")
		assert.Equal(t, map[string]time.Time{twinA: t0}, l.Dormant())
		assert.Empty(t, l.Restate(child, nil), "the child's empty re-statement does not speak for the parent")
		assert.Equal(t, map[string]time.Time{twinA: t0}, l.Dormant())
		assert.ElementsMatch(t, []string{twinA}, l.Close(parent))
		assert.Empty(t, l.Dormant())
		assert.False(t, l.Subscribed(twinA))
	})

	t.Run("the child's own subscriptions outlive the parent", func(t *testing.T) {
		l := restored()
		assert.ElementsMatch(t, []string{twinB}, l.Restate(parent, []string{twinA}))
		assert.Empty(t, l.Restate(child, nil))
		// A request on the child routes to A's twin: the child subscribes too.
		l.Subscribe(child, twinA)
		assert.Empty(t, l.Close(parent), "the child holds A: it stays dormant")
		assert.Equal(t, map[string]time.Time{twinA: t0}, l.Dormant())
		assert.True(t, l.Subscribed(twinA))
	})
}

// The last live stream ending concludes nothing (the proxy may be
// reconnecting): its subscriptions are orphaned until the next fresh stream
// re-states them. A stream the ledger never saw is ignored.
func TestLedgerLastStreamEndOrphansUntilRestate(t *testing.T) {
	t0 := time.Unix(1000, 0)
	l := NewLedger[string]()
	assert.Nil(t, l.Close(7), "unknown stream")

	l.Subscribe(1, twinA)
	l.Subscribe(1, twinB)
	assert.True(t, l.Retire(twinA, t0))
	assert.True(t, l.Retire(twinB, t0))
	assert.Nil(t, l.Close(1))
	assert.True(t, l.Subscribed(twinA), "orphaned, not dropped")
	assert.True(t, l.Retire(twinA, t0))
	assert.Equal(t, map[string]time.Time{twinA: t0, twinB: t0}, l.Dormant())

	// The same proxy reconnects and re-subscribes A only: B is gone.
	assert.ElementsMatch(t, []string{twinB}, l.Restate(2, []string{twinA}))
	assert.Equal(t, map[string]time.Time{twinA: t0}, l.Dormant())
}

// A stream that ends while another generation is live has its holdings
// dropped even if it is the NEWER one (a child's stream reset mid handoff): its
// process re-states on the new stream, which is where its subscriptions live.
func TestLedgerStreamResetWhileAnotherGenerationIsLive(t *testing.T) {
	t0 := time.Unix(1000, 0)
	l := NewLedger[string]()
	assert.Empty(t, l.Restate(1, []string{twinA}))
	assert.Empty(t, l.Restate(2, nil))
	l.Subscribe(2, twinB)
	assert.True(t, l.Retire(twinA, t0))
	assert.True(t, l.Retire(twinB, t0))

	assert.ElementsMatch(t, []string{twinB}, l.Close(2))
	assert.Equal(t, map[string]time.Time{twinA: t0}, l.Dormant())
	// The child reconnects and re-subscribes B: the cache parks it again.
	assert.Empty(t, l.Restate(3, []string{twinB}))
	l.Park(twinB, t0)
	assert.True(t, l.Subscribed(twinB))
	assert.Equal(t, map[string]time.Time{twinA: t0, twinB: t0}, l.Dormant())
}
