package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #1073 gates: the fetch-window prune (--east-west-quic-pair-fetch-window)
// must never remove a twin the proxy uses.
//
// The #979 proving soak (talos-main, 1.0.12-33ff5e9): after a proxy restart
// every twin reaches the new generation through the wildcard, with no on-demand
// fetch and no subscription. An agent-only roll then re-stated all of them on
// the fresh stream as HELD (initial_resource_versions only), and exactly one
// window later every agent logged `pruned persisted east-west QUIC pairs with
// no on-demand fetch since agent start` and removed them while k6 and the
// prober were still using them: ~200 x 503 NC per burst fleet-wide, and an
// Envoy SIGBUS on one node (#1074).

// stripDemandConfirmed rewrites the store at path the way an agent before
// #1073 wrote it: no pair carries demand_confirmed.
func stripDemandConfirmed(t *testing.T, path string) {
	t.Helper()
	stored := readStore(t, path)
	for _, p := range stored.GetQuicPairs() {
		p.SetDemandConfirmed(false)
	}
	data, err := observedMarshal.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// restateFresh is what the observer does with a fresh stream's first CDS
// request (restateTwins): held-only twins, then the re-subscribed set.
func restateFresh(c *SnapshotCache, stream int64, heldOnly, resubscribed []string) int {
	ctx := context.Background()
	served := c.ConfirmHeldQUICTwins(ctx, heldOnly)
	c.RestateQUICSubscriptions(ctx, stream, resubscribed)
	return served
}

// The #1073 shape: pre-#1073 store, agent-only restart, the running proxy
// re-states every twin as held; past the window nothing is pruned, the twins
// stay in CDS (no absent answer), and the confirmation is persisted -- so a
// later node reboot, where the fresh proxy holds nothing and uses the twins
// without ever fetching them, does not prune them an hour later either.
//
// Red before #1073: all three pruned at quicStart+window.
func TestQUICHeldTwinsSurviveTheFetchWindowAfterAgentRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	c := newQUICDemandCache(t, path)
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b", "demo/source-c")
	c.FlushObservedUpstreams()
	stripDemandConfirmed(t, path)

	restarted := newQUICDemandCache(t, path)
	ctx := context.Background()
	all := []string{echoTwin(restarted, "source-a"), echoTwin(restarted, "source-b"), echoTwin(restarted, "source-c")}
	require.ElementsMatch(t, all, restarted.QUICPairs(), "precondition: restored")

	const stream int64 = 7
	assert.Equal(t, 3, restateFresh(restarted, stream, all, nil), "every held twin is served")

	window := DefaultQUICPairFetchWindow
	for _, at := range []time.Duration{window, window + time.Minute, 10 * window} {
		restarted.pruneUnfetchedQUICPairs(restarted.quicStart.Add(at))
		assert.ElementsMatch(t, all, restarted.QUICPairs(), "a held twin in use is never pruned (%s after start)", at)
	}
	require.NoError(t, restarted.generateSnapshot(ctx))
	st := readQUICState(t, restarted)
	assert.ElementsMatch(t, all, st.twins, "no twin leaves CDS: nothing is answered absent")
	assert.ElementsMatch(t, all, st.twinCLAs)

	restarted.FlushObservedUpstreams()
	for _, p := range readStore(t, path).GetQuicPairs() {
		assert.True(t, p.GetDemandConfirmed(), "the held pair's confirmation is persisted: %s", p.GetSource())
	}

	// Node reboot: agent AND proxy fresh. The new proxy's stream holds nothing
	// and re-subscribes nothing; its twins arrive through the wildcard.
	rebooted := newQUICDemandCache(t, path)
	restateFresh(rebooted, 1, nil, nil)
	rebooted.pruneUnfetchedQUICPairs(rebooted.quicStart.Add(10 * window))
	assert.ElementsMatch(t, all, rebooted.QUICPairs(), "a confirmed pair is never pruned by the window after a reboot")
}

// A truly unused pair -- persisted without evidence, held by no stream, never
// fetched -- is still pruned once the window elapses, and the mixed case
// prunes exactly that one: held-only, re-subscribed and fetched pairs are kept,
// even after the stream that re-subscribed one ends while another is live.
func TestQUICFetchWindowPrunesOnlyPairsWithNoEvidenceOfUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	c := newQUICDemandCacheWith(t, path, "source-a", "source-b", "source-c", "source-d")
	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-b", "demo/source-c", "demo/source-d")
	c.FlushObservedUpstreams()
	stripDemandConfirmed(t, path)

	restarted := newQUICDemandCacheWith(t, path, "source-a", "source-b", "source-c", "source-d")
	ctx := context.Background()
	held, subscribed, fetched, unused := echoTwin(restarted, "source-a"), echoTwin(restarted, "source-b"),
		echoTwin(restarted, "source-c"), echoTwin(restarted, "source-d")

	// Parent generation's fresh stream: holds `held` via the wildcard,
	// re-subscribes `subscribed`. `unused` is restored but the proxy holds
	// nothing for it (not in initial_resource_versions).
	const parent, child int64 = 1, 2
	restateFresh(restarted, parent, []string{held}, []string{subscribed})
	// A hot restart: the child's fresh stream holds and subscribes nothing yet;
	// the parent exits.
	restateFresh(restarted, child, nil, nil)
	restarted.CloseQUICStream(ctx, parent)
	// `fetched` is a real first use on the child.
	d, _ := restarted.ObserveQUICTwin(ctx, child, fetched)
	require.Equal(t, QUICTwinKnown, d)

	window := DefaultQUICPairFetchWindow
	restarted.pruneUnfetchedQUICPairs(restarted.quicStart.Add(window - time.Second))
	require.ElementsMatch(t, []string{held, subscribed, fetched, unused}, restarted.QUICPairs(), "inside the window nothing is pruned")

	restarted.pruneUnfetchedQUICPairs(restarted.quicStart.Add(window))
	assert.ElementsMatch(t, []string{held, subscribed, fetched}, restarted.QUICPairs(),
		"only the pair with no evidence of use is pruned")
	require.NoError(t, restarted.generateSnapshot(ctx))
	assert.ElementsMatch(t, []string{held, subscribed, fetched}, readQUICState(t, restarted).twins)

	restarted.FlushObservedUpstreams()
	stored := readStore(t, path).GetQuicPairs()
	require.Len(t, stored, 3)
	for _, p := range stored {
		assert.True(t, p.GetDemandConfirmed(), "%s", p.GetSource())
	}

	// The pruned pair, if it still has traffic, re-fetches: real first use,
	// confirmed from then on.
	d, _ = restarted.ObserveQUICTwin(ctx, child, unused)
	assert.Equal(t, QUICTwinAdded, d)
	restarted.pruneUnfetchedQUICPairs(restarted.quicStart.Add(10 * window))
	assert.ElementsMatch(t, []string{held, subscribed, fetched, unused}, restarted.QUICPairs())
}

// A held-only twin whose pair the agent does not serve confirms nothing: it is
// answered absent (issue #1033), and no pair appears.
func TestQUICHeldTwinOfAnUnknownPairConfirmsNothing(t *testing.T) {
	c := newQUICDemandCache(t, "")
	assert.Zero(t, restateFresh(c, 1, []string{echoTwin(c, "source-a"), "quic:malformed"}, nil))
	assert.Empty(t, c.QUICPairs())
	c.depMu.RLock()
	defer c.depMu.RUnlock()
	assert.Empty(t, c.quicConfirmed)
}
