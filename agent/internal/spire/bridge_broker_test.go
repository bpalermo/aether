package spire

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"aethermesh.dev/common/spire/spiretest"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	brokerpb "github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSubscribeSendsTheSecurityHeaderAndTheFullReference pins the two things the
// Broker Endpoint specification makes mandatory and a client can silently get
// wrong: the `broker.spiffe.io: true` metadata on EVERY call (without it the
// endpoint answers InvalidArgument and nothing would ever be served), and a
// reference carrying BOTH the namespaced key and the UID — key alone would let a
// recycled pod name inherit the previous pod's identity.
func TestSubscribeSendsTheSecurityHeaderAndTheFullReference(t *testing.T) {
	f := newServedFixture(t, nopStore{})

	require.NoError(t, f.bridge.SubscribePod("/proc/42/ns/net", testWorkload, testPodRef))

	require.Eventually(t, func() bool {
		return f.broker.LastReference() != nil
	}, 5*time.Second, 10*time.Millisecond, "the broker must have resolved a reference")

	require.Zero(t, f.broker.BadHeaders(), "every call must carry the broker security header")

	ref := f.broker.LastReference()
	require.Equal(t, "pods", ref.GetType().GetPlural())
	require.Equal(t, "core", ref.GetType().GetGroup(), "core resources use the literal group \"core\"")
	require.Equal(t, testPodRef.Namespace, ref.GetKey().GetNamespace())
	require.Equal(t, testPodRef.Name, ref.GetKey().GetName())
	require.Equal(t, testPodRef.UID, ref.GetUid(), "the UID must be sent so SPIRE verifies the pod it resolved")
}

// TestReferenceNotFoundRetriesUntilResolved is the CNI ADD race: the Broker API
// resolves a reference at request time, so an ADD can beat the pod into the
// kubelet's list. The selector-based delegated path never needed this, so the
// retry is new code and this is what proves it converges.
func TestReferenceNotFoundRetriesUntilResolved(t *testing.T) {
	store := &identityStore{}
	f := newServedFixture(t, store)
	reader := installTestBridgeMetrics(t, f.bridge)

	f.broker.SetNotFoundFor(testPodRef.Namespace, testPodRef.Name, 3)

	require.NoError(t, f.bridge.SubscribePod("/proc/42/ns/net", testWorkload, testPodRef))

	require.Eventually(t, func() bool {
		f.bridge.mu.RLock()
		defer f.bridge.mu.RUnlock()
		_, served := f.bridge.secrets[testWorkload]
		return served
	}, 10*time.Second, 10*time.Millisecond, "the SVID must be served once the reference resolves")

	require.GreaterOrEqual(t, counterValue(t, reader, "aether.agent.spire.broker.reference_not_found"), int64(3),
		"every unresolved attempt must be counted")
	require.Equal(t, int64(0), counterValue(t, reader, "aether.agent.spire.broker.permission_denied"))
}

// TestPermissionDeniedIsCountedAndRetried covers the provider refusing the
// agent: with accessPolicy enforced and no impersonation grant, EVERY pod on the
// node fails this way, and the counter is the only thing that says so out loud.
func TestPermissionDeniedIsCountedAndRetried(t *testing.T) {
	f := newServedFixture(t, nopStore{})
	reader := installTestBridgeMetrics(t, f.bridge)

	f.broker.SetPermissionDeniedFor(testPodRef.Namespace, testPodRef.Name, true)
	f.bridge.backoffMax = 20 * time.Millisecond // PermissionDenied jumps straight to max

	require.NoError(t, f.bridge.SubscribePod("/proc/42/ns/net", testWorkload, testPodRef))

	require.Eventually(t, func() bool {
		return counterValue(t, reader, "aether.agent.spire.broker.permission_denied") >= 2
	}, 10*time.Second, 10*time.Millisecond, "a denied reference must be counted and retried, not abandoned")

	f.bridge.mu.RLock()
	_, served := f.bridge.secrets[testWorkload]
	f.bridge.mu.RUnlock()
	require.False(t, served, "a denied pod must not be served an identity")
}

// TestFederatedBundleUnionAddsAndRemoves covers the second half of the bundle
// rework: a peer trust domain enters the served validation contexts through a
// pod's stream and leaves with that pod, without disturbing anyone else's.
func TestFederatedBundleUnionAddsAndRemoves(t *testing.T) {
	const (
		netnsA = "/proc/1/ns/net"
		netnsB = "/proc/2/ns/net"
	)

	const ownTD = "spiffe://" + spiretest.TrustDomain

	store := &recordingStore{}
	b := newOrderingTestBridge(store)
	ctx := t.Context()

	// The agent's own Workload API bundle is always in the union — it is what
	// the Broker API's missing node-wide bundle stream was replaced with.
	b.mu.Lock()
	b.ownBundles = map[string][]byte{ownTD: spiretest.NewCA(t).BundleDER()}
	b.mu.Unlock()

	peerA := map[string][]byte{"spiffe://peer-a.example": spiretest.NewCA(t).BundleDER()}
	peerB := map[string][]byte{"spiffe://peer-b.example": spiretest.NewCA(t).BundleDER()}

	// Both pods contribute; the union is served alongside the agent's own bundle.
	require.NoError(t, b.handleSVIDUpdate(ctx, netnsA, brokerFederatedOnly(peerA)))
	require.NoError(t, b.handleSVIDUpdate(ctx, netnsB, brokerFederatedOnly(peerB)))
	require.Equal(t, []string{ownTD, "spiffe://peer-a.example", "spiffe://peer-b.example"}, servedValidationContexts(b))

	// A pod that stops reporting a bundle drops only its own contribution.
	require.NoError(t, b.handleSVIDUpdate(ctx, netnsA, brokerFederatedOnly(nil)))
	require.Equal(t, []string{ownTD, "spiffe://peer-b.example"}, servedValidationContexts(b))

	// And so does an unsubscribe. The bridge was never started, so drive the
	// subscription bookkeeping the way SubscribePod would have.
	close(b.started)
	b.ctx = ctx
	_, cancel := context.WithCancel(ctx)
	b.subscriptions[netnsB] = podSubscription{cancel: cancel, spiffeID: testWorkload, ref: testPodRef}
	require.NoError(t, b.UnsubscribePod(ctx, netnsB))
	require.Equal(t, []string{ownTD}, servedValidationContexts(b),
		"the last federated contributor leaving must not take the agent's own trust bundle with it")
}

// TestFederatedUnionNeverGoesEmpty is the S11 guard in its Broker-API shape: if
// every bundle input disappears at once — which in production means the agent's
// own identity is gone too — the previously served validation contexts are kept
// rather than published away, because an empty set is never a legitimate
// instruction to stop verifying peers.
func TestFederatedUnionNeverGoesEmpty(t *testing.T) {
	const netnsA = "/proc/1/ns/net"

	store := &recordingStore{}
	b := newOrderingTestBridge(store)
	reader := installTestBridgeMetrics(t, b)
	ctx := t.Context()

	peerA := map[string][]byte{"spiffe://peer-a.example": spiretest.NewCA(t).BundleDER()}
	require.NoError(t, b.handleSVIDUpdate(ctx, netnsA, brokerFederatedOnly(peerA)))
	require.Equal(t, []string{"spiffe://peer-a.example"}, servedValidationContexts(b))

	require.NoError(t, b.handleSVIDUpdate(ctx, netnsA, brokerFederatedOnly(nil)))
	require.Equal(t, []string{"spiffe://peer-a.example"}, servedValidationContexts(b),
		"the cached validation context must survive an input set that went empty")
	require.Equal(t, int64(1), counterValue(t, reader, "aether.agent.sds_push.empty_bundle_skipped"))
}

// brokerFederatedOnly builds a response that carries only federated bundles, the
// shape a pod with no entitlements of its own still produces.
func brokerFederatedOnly(federated map[string][]byte) *brokerpb.SubscribeToX509SVIDResponse {
	return &brokerpb.SubscribeToX509SVIDResponse{FederatedBundles: federated}
}

// servedValidationContexts returns the names of the validation contexts the
// bridge currently serves, sorted.
func servedValidationContexts(b *Bridge) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var names []string
	for name, secret := range b.secrets {
		if _, isValidation := secret.Type.(*tlsv3.Secret_ValidationContext); isValidation {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// TestAuthorizerRejectsANonSPIREAgentServer covers the endpoint specification's
// requirement that the broker authenticate the PROVIDER: a workload that merely
// happens to hold an SVID in our trust domain — and can bind the socket path —
// must not be able to hand the agent poisoned SVIDs.
func TestAuthorizerRejectsANonSPIREAgentServer(t *testing.T) {
	ca := spiretest.NewCA(t)
	identity := ca.Identity(t, testAgentID)

	// Same trust domain, but not a SPIRE agent path.
	_, sock := spiretest.StartBroker(t, ca, "spiffe://"+spiretest.TrustDomain+"/ns/evil/sa/impostor")

	client, err := newBrokerClient(sock, identity, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = client.SubscribeX509SVID(ctx, testPodRef)
	require.Error(t, err, "a server that is not a SPIRE agent identity must be rejected")
	require.Equal(t, codes.Unavailable, status.Code(err),
		"a rejected handshake surfaces as Unavailable, which the bridge retries")
}

// TestAuthorizerRejectsAForeignTrustDomainServer is the other half: a genuine
// SPIRE agent, but of a DIFFERENT trust domain. Chain verification alone would
// not catch it here, because the agent's bundle set has been given that trust
// domain's bundle too — only the authorizer does.
func TestAuthorizerRejectsAForeignTrustDomainServer(t *testing.T) {
	ownCA := spiretest.NewCA(t)
	foreignCA := spiretest.NewCA(t)
	foreignTD := spiffeid.RequireTrustDomainFromString("other.example")

	identity := ownCA.Identity(t, testAgentID, foreignCA.Bundle(foreignTD))
	_, sock := spiretest.StartBroker(t, foreignCA, "spiffe://other.example/spire/agent/k8s_psat/demo/node-a")

	client, err := newBrokerClient(sock, identity, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = client.SubscribeX509SVID(ctx, testPodRef)
	require.Error(t, err, "a SPIRE agent of another trust domain must be rejected")
}

// TestSVIDRotationOnALiveStream proves the steady-state path: a rotation pushed
// on an already-open stream reaches SDS. Without it a pod's certificate would
// simply expire.
func TestSVIDRotationOnALiveStream(t *testing.T) {
	f := newServedFixture(t, nopStore{})

	require.NoError(t, f.bridge.SubscribePod("/proc/42/ns/net", testWorkload, testPodRef))

	require.Eventually(t, func() bool {
		return servedSVIDVersion(f.bridge, testWorkload) == 1
	}, 10*time.Second, 10*time.Millisecond, "the first SVID must be served")

	require.Eventually(t, func() bool {
		return f.rotate(t, 7, nil) == 1
	}, 5*time.Second, 20*time.Millisecond, "the stream must be live to receive a rotation")

	require.Eventually(t, func() bool {
		return servedSVIDVersion(f.bridge, testWorkload) == 7
	}, 10*time.Second, 10*time.Millisecond, "a rotation on a live stream must reach SDS")
}

// servedSVIDVersion decodes the generation of the SVID currently served for id,
// or -1 if none is.
func servedSVIDVersion(b *Bridge, id string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	secret, ok := b.secrets[id]
	if !ok {
		return -1
	}
	return decodeSVIDVersion(secret)
}

// TestClassifyBrokerError pins the error policy proposal 036 decided, so a later
// edit cannot quietly turn a retryable condition into a permanent one.
func TestClassifyBrokerError(t *testing.T) {
	for _, tc := range []struct {
		code      codes.Code
		wantRetry bool
		wantSlow  bool
	}{
		{codes.NotFound, true, false},
		{codes.FailedPrecondition, true, false},
		{codes.PermissionDenied, true, true},
		{codes.Unauthenticated, true, false},
		{codes.InvalidArgument, false, false},
		{codes.Unavailable, true, false},
		{codes.Internal, true, false},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			got := classifyBrokerError(status.Error(tc.code, "test"))
			require.Equal(t, tc.wantRetry, got.retry)
			require.Equal(t, tc.wantSlow, got.slow)
		})
	}
}
