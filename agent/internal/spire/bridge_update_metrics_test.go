package spire

import (
	"context"
	"log/slog"
	"testing"

	"aethermesh.dev/common/spire/spiretest"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// counterPoint returns one attribute set's value for a counter, and whether that
// exact series is exported at all — the seeding contract is per attribute set.
func counterPoint(t *testing.T, reader *sdkmetric.ManualReader, name string, attrs ...attribute.KeyValue) (int64, bool) {
	t.Helper()
	want := attribute.NewSet(attrs...)
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.Truef(t, ok, "metric %s is %T, want Sum[int64]", name, m.Data)
			for _, dp := range sum.DataPoints {
				if dp.Attributes.Equals(&want) {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}

func svidUpdates(t *testing.T, reader *sdkmetric.ManualReader, identity, update string) int64 {
	t.Helper()
	v, ok := counterPoint(t, reader, "aether.agent.spire.svid_updates", attrIdentity.String(identity), attrUpdate.String(update))
	require.Truef(t, ok, "svid_updates{%s,%s} not exported", identity, update)
	return v
}

// TestUpdateCountersAreSeeded: a healthy agent that has not seen a rotation yet
// must still export the rotated series at zero — "no series" and "zero
// rotations" have to be different answers to a grading query (issue #717).
func TestUpdateCountersAreSeeded(t *testing.T) {
	reader := installTestBridgeMetrics(t, newOrderingTestBridge(&recordingStore{}))

	for _, identity := range []string{identityPod, identityNode} {
		for _, update := range []string{updateInitial, updateRotated, updateUnchanged} {
			require.Zero(t, svidUpdates(t, reader, identity, update))
		}
	}
	for _, bundle := range []string{bundleOwn, bundleFederated} {
		for _, update := range []string{updateInitial, updateRotated} {
			v, ok := counterPoint(t, reader, "aether.agent.spire.bundle_updates", attrBundle.String(bundle), attrUpdate.String(update))
			require.Truef(t, ok, "bundle_updates{%s,%s} not exported", bundle, update)
			require.Zero(t, v)
		}
	}
}

// TestPodSVIDUpdateClassification: the first SVID for a pod is initial, the same
// bytes again (what a re-subscribe after a stream failure delivers) is unchanged,
// a different certificate is a rotation — and a pod that comes back after an
// unsubscribe starts over at initial, so a rolling restart never reads as a
// rotation.
func TestPodSVIDUpdateClassification(t *testing.T) {
	const netns = "/proc/1/ns/net"

	b := newOrderingTestBridge(&recordingStore{})
	reader := installTestBridgeMetrics(t, b)
	ctx := t.Context()
	ca := spiretest.NewCA(t)

	first := svidResponse(t, ca, testWorkload, 1, nil)
	require.NoError(t, b.handleSVIDUpdate(ctx, netns, first))
	require.NoError(t, b.handleSVIDUpdate(ctx, netns, first))
	require.NoError(t, b.handleSVIDUpdate(ctx, netns, svidResponse(t, ca, testWorkload, 2, nil)))

	require.Equal(t, int64(1), svidUpdates(t, reader, identityPod, updateInitial))
	require.Equal(t, int64(1), svidUpdates(t, reader, identityPod, updateUnchanged))
	require.Equal(t, int64(1), svidUpdates(t, reader, identityPod, updateRotated))

	// A federated-bundle-only response carries no SVID and counts as none.
	require.NoError(t, b.handleSVIDUpdate(ctx, netns, brokerFederatedOnly(map[string][]byte{"spiffe://peer.example": spiretest.NewCA(t).BundleDER()})))
	require.Equal(t, int64(1), svidUpdates(t, reader, identityPod, updateRotated))
	fed, ok := counterPoint(t, reader, "aether.agent.spire.bundle_updates", attrBundle.String(bundleFederated), attrUpdate.String(updateInitial))
	require.True(t, ok)
	require.Equal(t, int64(1), fed)

	// Unsubscribe forgets the pod; its successor in the same netns path is initial.
	close(b.started)
	b.ctx = ctx
	_, cancel := context.WithCancel(ctx)
	b.subscriptions[netns] = podSubscription{cancel: cancel, spiffeID: testWorkload, ref: testPodRef}
	require.NoError(t, b.UnsubscribePod(ctx, netns))
	require.NoError(t, b.handleSVIDUpdate(ctx, netns, svidResponse(t, ca, testWorkload, 3, nil)))
	require.Equal(t, int64(2), svidUpdates(t, reader, identityPod, updateInitial))
	require.Equal(t, int64(1), svidUpdates(t, reader, identityPod, updateRotated))
}

// TestNodeIdentityUpdateClassification: the agent's own SVID and trust bundle are
// initial once, silent while unchanged (the refresher polls), and rotated when
// SPIRE hands the agent a new certificate or a new root.
func TestNodeIdentityUpdateClassification(t *testing.T) {
	ca := spiretest.NewCA(t)
	td := spiffeid.RequireTrustDomainFromString(spiretest.TrustDomain)
	identity := spiretest.NewPendingIdentity()
	identity.Arrive(ca.SVID(t, testAgentID), ca.Bundle(td))

	b := NewBridge("/nonexistent/socket", &recordingStore{}, identity, slog.New(slog.DiscardHandler))
	reader := installTestBridgeMetrics(t, b)
	ctx := t.Context()

	ownBundle := func(update string) int64 {
		v, ok := counterPoint(t, reader, "aether.agent.spire.bundle_updates", attrBundle.String(bundleOwn), attrUpdate.String(update))
		require.True(t, ok)
		return v
	}

	for range 3 {
		require.NoError(t, b.refreshNodeSVID(ctx))
		require.NoError(t, b.refreshWorkloadBundle(ctx))
	}
	require.Equal(t, int64(1), svidUpdates(t, reader, identityNode, updateInitial))
	require.Zero(t, svidUpdates(t, reader, identityNode, updateRotated))
	require.Equal(t, int64(1), ownBundle(updateInitial))
	require.Zero(t, ownBundle(updateRotated))

	// A rotated SVID under the same root: the SVID counts, the bundle does not.
	identity.Arrive(ca.SVID(t, testAgentID), ca.Bundle(td))
	require.NoError(t, b.refreshNodeSVID(ctx))
	require.NoError(t, b.refreshWorkloadBundle(ctx))
	require.Equal(t, int64(1), svidUpdates(t, reader, identityNode, updateRotated))
	require.Zero(t, ownBundle(updateRotated))

	// A new root.
	newCA := spiretest.NewCA(t)
	identity.Arrive(newCA.SVID(t, testAgentID), newCA.Bundle(td))
	require.NoError(t, b.refreshWorkloadBundle(ctx))
	require.Equal(t, int64(1), ownBundle(updateRotated))
}
