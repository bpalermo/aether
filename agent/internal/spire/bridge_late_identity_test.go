package spire

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/stretchr/testify/require"
)

// lateNodeSource is a node SVID source that has NO identity yet and announces
// one later — the shape a WaitingSource has while SPIRE is still coming up
// (issue #740).
type lateNodeSource struct {
	updated chan struct{}

	mu   sync.Mutex
	svid *x509svid.SVID
}

func newLateNodeSource() *lateNodeSource {
	return &lateNodeSource{updated: make(chan struct{}, 1)}
}

func (s *lateNodeSource) GetX509SVID() (*x509svid.SVID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.svid == nil {
		return nil, errors.New("no SVID yet")
	}
	return s.svid, nil
}

func (s *lateNodeSource) Updated() <-chan struct{} { return s.updated }

// arrive publishes the SVID and wakes whoever is watching, as go-spiffe does on
// the first Workload API update.
func (s *lateNodeSource) arrive(svid *x509svid.SVID) {
	s.mu.Lock()
	s.svid = svid
	s.mu.Unlock()
	select {
	case s.updated <- struct{}{}:
	default:
	}
}

// identityStore records what the bridge pushed into the xDS cache.
type identityStore struct {
	mu     sync.Mutex
	nodeID string
	pushes int
}

func (s *identityStore) SetSecrets(context.Context, []*tlsv3.Secret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushes++
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

// TestNodeSVIDServedWhenIdentityArrivesLate is the bridge half of #740. The
// agent no longer blocks on SPIRE, so the bridge routinely starts with a node
// source that has nothing to give. It must (a) not treat that as a failure, and
// (b) serve the node identity the moment the source announces one — not up to
// 30s later on the refresh tick, which is the only thing that used to wake it.
func TestNodeSVIDServedWhenIdentityArrivesLate(t *testing.T) {
	_, sock := startFakeSpire(t)

	store := &identityStore{}
	source := newLateNodeSource()

	b := NewBridge(sock, store, source, slog.New(slog.DiscardHandler))
	b.backoffInitial = 10 * time.Millisecond
	b.backoffMax = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()

	select {
	case <-b.Started():
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not start")
	}

	// No identity yet: nothing is claimed, and the bridge is still running.
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, store.identity(), "the bridge must not invent a node identity before SPIRE issues one")
	select {
	case err := <-done:
		t.Fatalf("a node source without an SVID must not fail the bridge: %v", err)
	default:
	}

	const nodeID = "spiffe://example.org/ns/aether-system/sa/aether-agent"
	source.arrive(newTestX509SVID(t, nodeID))

	// One second, deliberately: the refresh ticker is 30s, so anything that
	// passes here passed because of the update wake.
	require.Eventually(t, func() bool {
		return store.identity() == nodeID
	}, time.Second, 10*time.Millisecond,
		"the node identity must be served as soon as the SVID arrives, not on the next 30s tick")
}
