package spire

import (
	"context"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/common/spire/spiretest"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/require"
)

// identityStore records what the bridge pushed into the xDS cache.
type identityStore struct {
	mu                 sync.Mutex
	nodeID             string
	pushes             int
	validationContexts int
}

func (s *identityStore) SetSecrets(_ context.Context, secrets []*tlsv3.Secret) error {
	contexts := 0
	for _, secret := range secrets {
		if _, isValidation := secret.Type.(*tlsv3.Secret_ValidationContext); isValidation {
			contexts++
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushes++
	s.validationContexts = contexts
	return nil
}

func (s *identityStore) SetNodeIdentity(_ context.Context, nodeSpiffeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodeID = nodeSpiffeID
	return nil
}

func (s *identityStore) identity() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nodeID
}

func (s *identityStore) contexts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validationContexts
}

// TestNodeSVIDServedWhenIdentityArrivesLate is the bridge half of #740. The
// agent no longer blocks on SPIRE, so the bridge routinely starts with an
// identity source that has nothing to give. It must (a) not treat that as a
// failure, and (b) serve the node identity the moment the source announces one —
// not up to 30s later on the refresh tick, which is the only thing that used to
// wake it.
func TestNodeSVIDServedWhenIdentityArrivesLate(t *testing.T) {
	ca := spiretest.NewCA(t)
	store := &identityStore{}
	source := spiretest.NewPendingIdentity()

	startBrokerBridge(t, store, source, ca)

	// No identity yet: nothing is claimed, and the bridge is still running (the
	// fixture's cleanup asserts Start returned nil, and only on cancellation).
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, store.identity(), "the bridge must not invent a node identity before SPIRE issues one")

	const nodeID = "spiffe://" + spiretest.TrustDomain + "/ns/aether-system/sa/aether-agent"
	svid := ca.SVID(t, nodeID)
	source.Arrive(svid, ca.Bundle(svid.ID.TrustDomain()))

	// One second, deliberately: the refresh ticker is 30s, so anything that
	// passes here passed because of the update wake.
	require.Eventually(t, func() bool {
		return store.identity() == nodeID
	}, time.Second, 10*time.Millisecond,
		"the node identity must be served as soon as the SVID arrives, not on the next 30s tick")
}

// TestValidationContextServedWithoutPods is the other half of what the Broker
// API changed. The delegated path took trust bundles from a node-wide bundle
// stream; the Broker API has no such stream (SubscribeToX509Bundles also needs a
// workload reference), so validation contexts now come from the agent's OWN
// Workload API bundle. A node with ZERO managed pods must still serve them —
// otherwise the node proxy could not verify a single peer until the first pod
// landed.
func TestValidationContextServedWithoutPods(t *testing.T) {
	ca := spiretest.NewCA(t)
	store := &identityStore{}
	source := spiretest.NewPendingIdentity()

	f := startBrokerBridge(t, store, source, ca)

	const nodeID = "spiffe://" + spiretest.TrustDomain + "/ns/aether-system/sa/aether-agent"
	svid := ca.SVID(t, nodeID)
	source.Arrive(svid, ca.Bundle(svid.ID.TrustDomain()))

	require.Eventually(t, func() bool {
		return store.contexts() == 1
	}, 5*time.Second, 10*time.Millisecond,
		"a node with no managed pods must serve the trust bundle from its own Workload API identity")

	f.bridge.mu.RLock()
	_, served := f.bridge.secrets["spiffe://"+spiretest.TrustDomain]
	f.bridge.mu.RUnlock()
	require.True(t, served, "the validation context must be named by the canonical trust-domain URI")
}
