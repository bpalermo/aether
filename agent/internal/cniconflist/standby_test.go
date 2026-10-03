package cniconflist

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStandbyObservesButDoesNotRepair: a surge standby (proposal 041) writes
// no node file while another agent owns the node, but its readiness still
// needs the chaining state, so it observes. The strip it saw is its to repair
// the moment it owns the node — without waiting for the next periodic check.
func TestStandbyObservesButDoesNotRepair(t *testing.T) {
	dir := t.TempDir()
	path := writeConf(t, dir, confName, chained(t))
	owned := make(chan struct{})
	r := &Reasserter{Dir: dir, Log: testLogger(), Interval: time.Hour, SettleDelay: 10 * time.Millisecond, Owned: owned}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Start(ctx) }()

	require.Eventually(t, func() bool { return r.ChainStatus().Observed }, 5*time.Second, 5*time.Millisecond)
	assert.True(t, r.ChainStatus().Chained, "the entry is primed from the chained conflist")

	// A competing writer strips the entry while this agent is a standby.
	writeConf(t, dir, confName, []byte(flannelOnly))
	require.Eventually(t, func() bool { return !r.ChainStatus().Chained }, 5*time.Second, 5*time.Millisecond,
		"the standby still observes the strip (its readiness depends on it)")
	time.Sleep(100 * time.Millisecond)
	assert.False(t, isChained(t, path), "a standby must not write the conflist")

	close(owned)
	require.Eventually(t, func() bool { return isChained(t, path) }, 5*time.Second, 5*time.Millisecond,
		"the new owner repairs the strip at takeover")
	cancel()
	<-done
}
