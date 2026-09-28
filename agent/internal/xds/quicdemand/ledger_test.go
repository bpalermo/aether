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
	l.Subscribe(twinA)
	l.Subscribe(twinB)
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
	assert.ElementsMatch(t, []string{twinC}, l.Restate([]string{twinB}))
	assert.Equal(t, map[string]time.Time{twinB: t0}, l.Dormant())
	assert.False(t, l.Subscribed(twinA))
	assert.True(t, l.Subscribed(twinB))

	// A new proxy generation (hot-restart child) re-subscribes nothing: every
	// dormant pair is pruned.
	assert.ElementsMatch(t, []string{twinB}, l.Restate(nil))
	assert.Empty(t, l.Dormant())
	assert.False(t, l.Subscribed(twinB))
}
