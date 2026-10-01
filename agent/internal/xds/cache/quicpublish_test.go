package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quicPublisherIdle reports whether the coalescing publisher has no worker.
func quicPublisherIdle(c *SnapshotCache) bool {
	c.quicPublish.mu.Lock()
	defer c.quicPublish.mu.Unlock()
	return !c.quicPublish.running && !c.quicPublish.pending
}

// An admission that arrives while the publisher is BUILDING (here: blocked on
// snapshotMu, as behind a registry-refresh build) is never left unpublished:
// it marks the publish pending again and exactly one more build follows --
// one, not one per admission (issue #1086).
func TestQUICPublishAdmissionDuringBuildGetsOneMoreBuild(t *testing.T) {
	c := newQUICBurstCache(t)
	c.quicPublishWindow = -1 // no wait: the worker reaches the build at once
	ctx := context.Background()
	twins := burstTwins(c)
	before := c.version.Load()

	c.snapshotMu.Lock() // a build is in flight
	d, _ := c.ObserveQUICTwin(ctx, testQUICStream, twins[0])
	require.Equal(t, QUICTwinAdded, d)
	// The worker has taken the pending publish and is waiting on the build.
	require.Eventually(t, func() bool {
		c.quicPublish.mu.Lock()
		defer c.quicPublish.mu.Unlock()
		return c.quicPublish.running && !c.quicPublish.pending
	}, 5*time.Second, eventuallyTick)
	for _, name := range twins[1:] {
		d, _ := c.ObserveQUICTwin(ctx, testQUICStream, name)
		require.Equal(t, QUICTwinAdded, d)
	}
	c.snapshotMu.Unlock()

	require.Eventually(t, func() bool { return quicPublisherIdle(c) }, 5*time.Second, eventuallyTick)
	assert.Equal(t, len(twins), twinsInSnapshot(t, c, twins), "every twin is published")
	assert.Equal(t, uint64(2), c.version.Load()-before,
		"the in-flight build plus exactly one more for the %d admissions that arrived during it", len(twins)-1)
}

// A lone admission is published after the window, by one build, and the
// worker then exits: nothing keeps running between bursts.
func TestQUICPublishLoneAdmissionOneBuildThenIdle(t *testing.T) {
	c := newQUICBurstCache(t)
	ctx := context.Background()
	twin := burstTwins(c)[0]
	before := c.version.Load()

	start := time.Now()
	d, _ := c.ObserveQUICTwin(ctx, testQUICStream, twin)
	require.Equal(t, QUICTwinAdded, d)
	require.Eventually(t, func() bool { return twinsInSnapshot(t, c, []string{twin}) == 1 }, 5*time.Second, time.Millisecond)
	assert.GreaterOrEqual(t, time.Since(start), defaultQUICPublishWindow, "the first admission waits the window for the rest of its burst")
	require.Eventually(t, func() bool { return quicPublisherIdle(c) }, 5*time.Second, eventuallyTick)
	assert.Equal(t, uint64(1), c.version.Load()-before)

	// A repeat of a known pair requests nothing.
	d, _ = c.ObserveQUICTwin(ctx, testQUICStream, twin)
	require.Equal(t, QUICTwinKnown, d)
	assert.True(t, quicPublisherIdle(c), "a known pair publishes nothing")
}

// ResumeQUICSubscriptions (a fresh stream re-subscribing twins, #1033) rides
// the same publisher: N resumed pairs, one build.
func TestQUICPublishResumedSubscriptionsOneBuild(t *testing.T) {
	c := newQUICBurstCache(t)
	ctx := context.Background()
	twins := burstTwins(c)
	before := c.version.Load()

	require.Equal(t, len(twins), c.ResumeQUICSubscriptions(ctx, testQUICStream, twins))
	require.Eventually(t, func() bool { return quicPublisherIdle(c) && twinsInSnapshot(t, c, twins) == len(twins) }, 5*time.Second, eventuallyTick)
	assert.Equal(t, uint64(1), c.version.Load()-before)
}
