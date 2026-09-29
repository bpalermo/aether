package cache

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #1052 gates, at the cache: the ledger's "dormant" must keep meaning "a
// LIVE proxy generation holds the twin's on-demand subscription", which is the
// premise of republishing a dormant pair with no request (#1036).
//
// An agent restart that lands in the middle of a proxy hot restart reconnects
// two generations at once: the child (which holds no ODCDS subscriptions) and
// the draining parent (which re-subscribes its twins). On rev248 main-worker-03
// the child's stream came first (15:39:16Z, re-subscribed nothing: the dormant
// pairs were pruned) and the parent's second (15:39:27Z, re-subscribed them:
// parked dormant again). Before #1052 the parked pairs outlived the parent, so
// the ledger held dormant pairs no live generation subscribed to. The
// subscription state is now per stream: a pair is held only by the stream that
// re-subscribed or requested it, and a stream that ends while another is live
// takes its holdings with it.

const (
	genChild  int64 = 10
	genParent int64 = 11
)

// restartWithDormantPairs persists two dormant pairs (source-a and source-b
// left the node while the proxy held their twins' subscriptions) and returns
// the agent that restarts on that store, with neither source on the node.
func restartWithDormantPairs(t *testing.T) (*SnapshotCache, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	c := newQUICDemandCache(t, path)
	ctx := context.Background()
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b")
	for _, sa := range []string{"source-a", "source-b"} {
		require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-"+sa))
	}
	require.Len(t, c.DormantQUICPairs(), 2)
	c.FlushObservedUpstreams()
	require.Len(t, readStore(t, path).GetDormantQuicPairs(), 2)

	restarted := newQUICDemandCacheWith(t, path, "source-c")
	require.Equal(t, []string{echoTwin(restarted, "source-a"), echoTwin(restarted, "source-b")}, restarted.DormantQUICPairs(),
		"precondition: the dormant pairs are reloaded from the persisted ledger")
	return restarted, path
}

// The rev248 order: the new generation's stream re-states first (nothing), the
// old generation's second (A). A is dormant only while the old generation's
// stream lives; when it ends, A is pruned, persisted as gone, and not
// republished when its source returns (the live generation never subscribed:
// its next request fetches it, real first use).
func TestQUICDormantPairsOverlappingGenerationsNewThenOld(t *testing.T) {
	c, path := restartWithDormantPairs(t)
	ctx := context.Background()
	twinA := echoTwin(c, "source-a")

	assert.Zero(t, c.RestateQUICSubscriptions(ctx, genChild, nil))
	assert.Empty(t, c.DormantQUICPairs(), "no live stream holds either pair")

	assert.Zero(t, c.RestateQUICSubscriptions(ctx, genParent, []string{twinA}), "A's source is away: parked, not admitted")
	assert.Equal(t, []string{twinA}, c.DormantQUICPairs(), "the parent re-subscribed A: dormant while its stream lives")

	assert.Equal(t, 1, c.CloseQUICStream(ctx, genParent))
	assert.Empty(t, c.DormantQUICPairs(), "the parent exited while the child is live: no phantom dormant pair (issue #1052)")
	c.FlushObservedUpstreams()
	assert.Empty(t, readStore(t, path).GetDormantQuicPairs(), "and none is persisted")

	addSourcePod(t, c, "source-a", "/var/run/netns/cni-source-a-new")
	assert.Empty(t, readQUICState(t, c).twins, "no generation holds A's subscription: nothing republished with no request")

	// A later removal of a pair only the exited parent had subscribed to is
	// forgotten, not parked.
	observeOnStream(t, c, genChild, "source-c")
	require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-source-c"))
	assert.Equal(t, []string{echoTwin(c, "source-c")}, c.DormantQUICPairs(), "the child's own first use is held by the child")
}

// The other order: the old generation re-states first (A; B is pruned), the
// new one second (nothing -- which must not speak for the old one). While the
// old stream lives A is dormant and is republished if its source returns; once
// it ends, A is pruned.
func TestQUICDormantPairsOverlappingGenerationsOldThenNew(t *testing.T) {
	c, path := restartWithDormantPairs(t)
	ctx := context.Background()
	twinA := echoTwin(c, "source-a")

	assert.Zero(t, c.RestateQUICSubscriptions(ctx, genParent, []string{twinA}))
	assert.Equal(t, []string{twinA}, c.DormantQUICPairs(), "B: no live stream holds it; A: the parent does")

	assert.Zero(t, c.RestateQUICSubscriptions(ctx, genChild, nil))
	assert.Equal(t, []string{twinA}, c.DormantQUICPairs(), "the child's empty re-statement does not prune the parent's subscription")
	c.FlushObservedUpstreams()
	require.Len(t, readStore(t, path).GetDormantQuicPairs(), 1)

	assert.Equal(t, 1, c.CloseQUICStream(ctx, genParent))
	assert.Empty(t, c.DormantQUICPairs())
	c.FlushObservedUpstreams()
	assert.Empty(t, readStore(t, path).GetDormantQuicPairs())
	addSourcePod(t, c, "source-a", "/var/run/netns/cni-source-a-new")
	assert.Empty(t, readQUICState(t, c).twins)
}

// While the generation that holds a dormant pair's subscription is live, the
// pair is republished the moment it is valid again -- to that generation, whose
// open subscription receives the twin with no request.
func TestQUICDormantPairRepublishedForTheGenerationThatHoldsIt(t *testing.T) {
	c, _ := restartWithDormantPairs(t)
	ctx := context.Background()
	twinA := echoTwin(c, "source-a")

	assert.Zero(t, c.RestateQUICSubscriptions(ctx, genChild, nil))
	assert.Zero(t, c.RestateQUICSubscriptions(ctx, genParent, []string{twinA}))
	addSourcePod(t, c, "source-a", "/var/run/netns/cni-source-a-new")
	assert.Equal(t, []string{twinA}, readQUICState(t, c).twins, "the parent still holds the subscription: republished")
	assert.Equal(t, []string{twinA}, c.QUICPairs())
	assert.Empty(t, c.DormantQUICPairs())

	// The parent exits: the pair is served, not dormant, so there is nothing
	// to prune; the child holds the twin through the wildcard.
	assert.Zero(t, c.CloseQUICStream(ctx, genParent))
	assert.Equal(t, []string{twinA}, c.QUICPairs())
}

// The last live stream ending concludes nothing: the same proxy reconnecting
// re-states on its new stream, and only then is an unnamed dormant pair pruned.
func TestQUICLastStreamEndKeepsDormantUntilTheNextRestate(t *testing.T) {
	c := newQUICDemandCache(t, "")
	ctx := context.Background()
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b")
	for _, sa := range []string{"source-a", "source-b"} {
		require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-"+sa))
	}
	twinA, twinB := echoTwin(c, "source-a"), echoTwin(c, "source-b")
	require.Equal(t, []string{twinA, twinB}, c.DormantQUICPairs())

	assert.Zero(t, c.CloseQUICStream(ctx, testQUICStream))
	assert.Equal(t, []string{twinA, twinB}, c.DormantQUICPairs(), "the proxy may be reconnecting: nothing pruned")

	assert.Zero(t, c.RestateQUICSubscriptions(ctx, testQUICStream+1, []string{twinA}))
	assert.Equal(t, []string{twinA}, c.DormantQUICPairs())
	assert.Zero(t, c.CloseQUICStream(ctx, 99), "a stream the ledger never saw is ignored")
}

// observeOnStream records a first use of demo/echo by each source on stream.
func observeOnStream(t *testing.T, c *SnapshotCache, stream int64, sources ...string) {
	t.Helper()
	for _, sa := range sources {
		d, reason := c.recordQUICPair(stream, echoTwin(c, sa))
		require.NotEqual(t, QUICTwinRefused, d, "pair demo/echo <- demo/%s refused: %s", sa, reason)
	}
	require.NoError(t, c.generateSnapshot(context.Background()))
}
