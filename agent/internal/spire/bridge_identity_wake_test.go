package spire

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"aethermesh.dev/common/spire/spiretest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPodSubscriptionsRetryAsSoonAsTheAgentHasAnIdentity is issue #1123's
// client-certificate term. A restarted agent re-subscribes every stored pod
// before its own SVID exists, so each subscribe fails the Broker Endpoint's
// mutual TLS and sleeps the stream backoff (1 s, doubling). When the SVID
// lands, nothing woke those sleeps: the pods' certificates arrived on the
// backoff's schedule, ~1.05 s after the agent's identity on the 2026-10-02
// talos rolls and up to 2.7 s (main-worker-05, 06:54Z), and the first xDS
// serve waits for them (#1103's client-certificate gate) while the node's
// proxy has no ADS stream.
//
// The subscriptions now retry the moment the agent's identity is served, once
// the broker connection is back up.
func TestPodSubscriptionsRetryAsSoonAsTheAgentHasAnIdentity(t *testing.T) {
	for i := range 3 {
		ca := spiretest.NewCA(t)
		fake, sock := spiretest.StartBroker(t, ca, spiretest.DefaultBrokerServerID)
		source := spiretest.NewPendingIdentity()

		b := NewBridge(sock, nopStore{}, source, slog.New(slog.DiscardHandler))
		// Production backoff: the point is what happens inside it.
		b.backoffInitial = initialStreamBackoff
		b.backoffMax = maxStreamBackoff

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- b.Start(ctx) }()
		<-b.Started()

		f := &brokerFixture{bridge: b, broker: fake, ca: ca, identity: source}
		f.setEntry(t, 1, nil)
		require.NoError(t, b.SubscribePod("/proc/42/ns/net", testWorkload, testPodRef))

		// Let the first attempt fail on the missing client certificate and the
		// loop enter its backoff.
		time.Sleep(300 * time.Millisecond)
		require.Zero(t, fake.Subscribes(), "no subscribe can reach the broker before the agent has a certificate")

		svid := ca.SVID(t, testAgentID)
		start := time.Now()
		source.Arrive(svid, ca.Bundle(svid.ID.TrustDomain()))

		require.Eventually(t, func() bool { return servedSVIDVersion(b, testWorkload) == 1 }, 10*time.Second, 5*time.Millisecond,
			"run %d: the pod's SVID was never served", i)
		took := time.Since(start)

		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("Start did not return after cancellation")
		}
		assert.Less(t, took, 500*time.Millisecond,
			"run %d: a pod subscription must follow the agent's identity by a handshake, not by the stream backoff (%s)", i, initialStreamBackoff)
	}
}
