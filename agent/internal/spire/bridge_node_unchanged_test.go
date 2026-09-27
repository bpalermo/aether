package spire

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/common/spire/spiretest"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/require"
)

// TestNodeSVIDRedeliveryCountsUnchanged is the gate for issue #993. The node
// identity's unchanged series used to be seeded at zero and unreachable: the
// refresher returned early on an equal SVID without recording anything, and
// classified every other update as initial-or-rotated. Driven through the real
// wake path (the source's Updated channel, which go-spiffe fires on every
// Workload API response), SPIRE re-sending the same node SVID must count as
// unchanged once, with no snapshot bump and no push; a new certificate must count
// as rotated.
func TestNodeSVIDRedeliveryCountsUnchanged(t *testing.T) {
	ca := spiretest.NewCA(t)
	td := spiffeid.RequireTrustDomainFromString(spiretest.TrustDomain)
	identity := spiretest.NewPendingIdentity()
	store := &recordingStore{}

	b := NewBridge("/nonexistent/socket", store, identity, slog.New(slog.DiscardHandler))
	reader := installTestBridgeMetrics(t, b)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { b.runIdentityRefresh(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	const wait, tick = 5 * time.Second, 5 * time.Millisecond
	gen := func() uint64 {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.gen
	}
	// served reports whether the store holds the bridge's current generation,
	// i.e. the last push has actually reached it. publishedGen is written only
	// after SetSecrets returns, under pushMu.
	served := func() bool {
		want := gen()
		b.pushMu.Lock()
		defer b.pushMu.Unlock()
		return b.publishedGen == want
	}

	svid := ca.SVID(t, testAgentID)
	identity.Arrive(svid, ca.Bundle(td))
	require.Eventually(t, func() bool {
		return svidUpdates(t, reader, identityNode, updateInitial) == 1
	}, wait, tick, "the first node SVID must be served as initial")
	// The bundle half of the same wake is served right after the SVID half; wait
	// for it so the generation and push baseline below is settled.
	require.Eventually(t, func() bool {
		v, _ := counterPoint(t, reader, "aether.agent.spire.bundle_updates", attrBundle.String(bundleOwn), attrUpdate.String(updateInitial))
		return v == 1
	}, wait, tick)
	// ...and for its PUSH, not just its metric: refreshWorkloadBundle records
	// bundle_updates before it calls pushSecrets, so the counter reaching 1 does
	// not mean the initial wake's second push has landed. Snapshotting the push
	// count in that window read 1, the bundle push then arrived as a 2nd, and the
	// "must not push" assertion below blamed it on the redelivery (issue #1018).
	require.Eventually(t, served, wait, tick, "the initial wake's pushes must reach the store before the baseline is taken")
	genBefore, pushesBefore := gen(), len(store.snapshot())

	// SPIRE re-sends the very same SVID and bundle.
	identity.Arrive(svid, ca.Bundle(td))
	require.Eventually(t, func() bool {
		return svidUpdates(t, reader, identityNode, updateUnchanged) == 1
	}, wait, tick, "a redelivered, byte-identical node SVID must count as unchanged (issue #993)")
	require.Equal(t, genBefore, gen(), "an unchanged node SVID must not bump the snapshot generation")
	require.Len(t, store.snapshot(), pushesBefore, "an unchanged node SVID must not push")
	require.Equal(t, int64(1), svidUpdates(t, reader, identityNode, updateInitial))
	require.Zero(t, svidUpdates(t, reader, identityNode, updateRotated))

	// A new certificate is a rotation, and the unchanged count stays put.
	identity.Arrive(ca.SVID(t, testAgentID), ca.Bundle(td))
	require.Eventually(t, func() bool {
		return svidUpdates(t, reader, identityNode, updateRotated) == 1
	}, wait, tick, "a new node certificate must count as rotated")
	require.Equal(t, int64(1), svidUpdates(t, reader, identityNode, updateUnchanged))
	require.Equal(t, int64(1), svidUpdates(t, reader, identityNode, updateInitial))
	require.Greater(t, gen(), genBefore, "a rotated node SVID must bump the snapshot generation")
}
