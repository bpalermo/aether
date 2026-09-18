package spire

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"aethermesh.dev/common/spire/spiretest"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	brokerpb "github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker"
	"github.com/stretchr/testify/require"
)

// Identities used across the broker-backed bridge tests.
const (
	testAgentID  = "spiffe://" + spiretest.TrustDomain + "/ns/aether-system/sa/aether-agent"
	testWorkload = "spiffe://" + spiretest.TrustDomain + "/ns/aether-test/sa/echo"
)

// testPodRef is the pod reference every broker-backed test subscribes with.
var testPodRef = PodRef{Namespace: "aether-test", Name: "echo-7c9f", UID: "a1b2c3d4-e5f6-7890-abcd-ef1234567890"}

// nopStore is a SecretStore that accepts and discards all pushes.
type nopStore struct{}

func (nopStore) SetSecrets(context.Context, []*tlsv3.Secret) error { return nil }

// brokerFixture is a bridge wired to a fake SPIFFE Broker Endpoint over mutual
// TLS, exactly as production is wired: a real brokerClient, a real UDS, a real
// mTLS handshake and the real security-header interceptors.
type brokerFixture struct {
	bridge   *Bridge
	broker   *spiretest.FakeBroker
	ca       *spiretest.CA
	identity *spiretest.Identity
	store    SecretStore
}

// startBrokerBridge starts a fake broker and a bridge against it, and runs the
// bridge until the test ends. backoff is shortened so reconnects happen inside
// the test budget.
func startBrokerBridge(t *testing.T, store SecretStore, identity *spiretest.Identity, ca *spiretest.CA) *brokerFixture {
	t.Helper()

	fake, sock := spiretest.StartBroker(t, ca, spiretest.DefaultBrokerServerID)

	b := NewBridge(sock, store, identity, slog.New(slog.DiscardHandler))
	b.backoffInitial = 10 * time.Millisecond
	b.backoffMax = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err, "Start must return nil on context cancellation")
		case <-time.After(10 * time.Second):
			t.Error("Start did not return after cancellation")
		}
	})

	select {
	case <-b.Started():
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not start")
	}

	return &brokerFixture{bridge: b, broker: fake, ca: ca, identity: identity, store: store}
}

// newServedFixture is the common case: an agent identity that already exists and
// a broker that resolves testPodRef to one SVID.
func newServedFixture(t *testing.T, store SecretStore) *brokerFixture {
	t.Helper()

	ca := spiretest.NewCA(t)
	f := startBrokerBridge(t, store, ca.Identity(t, testAgentID), ca)
	f.setEntry(t, 1, nil)
	return f
}

// setEntry points the broker at a fresh SVID generation for testPodRef.
func (f *brokerFixture) setEntry(t *testing.T, version int, federated map[string][]byte) {
	t.Helper()
	f.broker.SetEntry(testPodRef.Namespace, testPodRef.Name, &spiretest.BrokerEntry{
		SVIDs:            []*brokerpb.X509SVID{f.ca.BrokerSVID(t, testWorkload, version)},
		FederatedBundles: federated,
	})
}

// rotate pushes a new SVID generation onto every live stream for testPodRef and
// reports how many streams received it.
func (f *brokerFixture) rotate(t *testing.T, version int, federated map[string][]byte) int {
	t.Helper()
	return f.broker.Rotate(testPodRef.Namespace, testPodRef.Name, &spiretest.BrokerEntry{
		SVIDs:            []*brokerpb.X509SVID{f.ca.BrokerSVID(t, testWorkload, version)},
		FederatedBundles: federated,
	})
}

// TestSVIDStreamResubscribes verifies that a pod's subscription stream ending
// (e.g. a SPIRE agent restart) is re-subscribed instead of silently freezing the
// pod's SVID until expiry, and that the subscription stays tracked so
// UnsubscribePod still works.
func TestSVIDStreamResubscribes(t *testing.T) {
	f := newServedFixture(t, nopStore{})
	f.broker.SetCloseAfterFirst(testPodRef.Namespace, testPodRef.Name, true)

	const netns = "/proc/42/ns/net"
	require.NoError(t, f.bridge.SubscribePod(netns, testWorkload, testPodRef))

	require.Eventually(t, func() bool {
		return f.broker.Subscribes() >= 2
	}, 10*time.Second, 10*time.Millisecond, "bridge must re-subscribe after the stream ends")

	f.bridge.subsMu.Lock()
	_, exists := f.bridge.subscriptions[netns]
	f.bridge.subsMu.Unlock()
	require.True(t, exists, "subscription must remain tracked across reconnects")
}

// TestBridgeSurvivesSubscribeFailures is the successor to the bundle-stream
// reconnect test: the bridge runnable must never fail for a transient broker
// problem — that would take the whole agent down for a SPIRE hiccup. Here every
// subscribe is refused, and the bridge keeps retrying without returning.
func TestBridgeSurvivesSubscribeFailures(t *testing.T) {
	ca := spiretest.NewCA(t)
	f := startBrokerBridge(t, nopStore{}, ca.Identity(t, testAgentID), ca)
	// No entry registered: every subscribe gets NotFound.

	require.NoError(t, f.bridge.SubscribePod("/proc/43/ns/net", testWorkload, testPodRef))

	require.Eventually(t, func() bool {
		return f.broker.Subscribes() >= 3
	}, 10*time.Second, 10*time.Millisecond, "an unresolved reference must be retried, not abandoned")

	// The cleanup registered by startBrokerBridge asserts Start returned nil; if
	// Start had already returned, that assertion is what fails.
}
