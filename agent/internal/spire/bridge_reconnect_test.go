package spire

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/go-logr/logr"
	delegatedidentityv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/agent/delegatedidentity/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeDelegatedIdentity is a SPIRE delegated-identity server whose first
// subscription stream of each kind sends one empty response and then ends,
// simulating a SPIRE agent restart. Subsequent streams stay open until the
// test ends so the bridge can settle in a healthy reconnected state.
type fakeDelegatedIdentity struct {
	delegatedidentityv1.UnimplementedDelegatedIdentityServer

	mu         sync.Mutex
	bundleSubs int
	svidSubs   int
}

// counts returns the number of bundle and SVID subscriptions seen so far.
func (f *fakeDelegatedIdentity) counts() (bundle, svid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bundleSubs, f.svidSubs
}

func (f *fakeDelegatedIdentity) SubscribeToX509Bundles(_ *delegatedidentityv1.SubscribeToX509BundlesRequest, stream grpc.ServerStreamingServer[delegatedidentityv1.SubscribeToX509BundlesResponse]) error {
	f.mu.Lock()
	f.bundleSubs++
	n := f.bundleSubs
	f.mu.Unlock()

	if err := stream.Send(&delegatedidentityv1.SubscribeToX509BundlesResponse{}); err != nil {
		return err
	}
	if n == 1 {
		return nil // simulated SPIRE agent restart: stream ends after one update
	}
	<-stream.Context().Done()
	return nil
}

func (f *fakeDelegatedIdentity) SubscribeToX509SVIDs(_ *delegatedidentityv1.SubscribeToX509SVIDsRequest, stream grpc.ServerStreamingServer[delegatedidentityv1.SubscribeToX509SVIDsResponse]) error {
	f.mu.Lock()
	f.svidSubs++
	n := f.svidSubs
	f.mu.Unlock()

	if err := stream.Send(&delegatedidentityv1.SubscribeToX509SVIDsResponse{}); err != nil {
		return err
	}
	if n == 1 {
		return nil // simulated SPIRE agent restart: stream ends after one update
	}
	<-stream.Context().Done()
	return nil
}

// nopStore is a SecretStore that accepts and discards all pushes.
type nopStore struct{}

func (nopStore) SetSecrets(context.Context, []*tlsv3.Secret) error { return nil }

// startFakeSpire serves a fakeDelegatedIdentity on a temporary UDS and returns
// it with the socket path. It uses os.MkdirTemp under /tmp to keep the path
// within the ~108-byte UDS limit (Bazel sandbox paths can exceed it).
func startFakeSpire(t *testing.T) (*fakeDelegatedIdentity, string) {
	t.Helper()

	dir, err := os.MkdirTemp("", "spire")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")

	lis, err := net.Listen("unix", sock)
	require.NoError(t, err)

	fake := &fakeDelegatedIdentity{}
	srv := grpc.NewServer()
	delegatedidentityv1.RegisterDelegatedIdentityServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return fake, sock
}

// newReconnectTestBridge creates a bridge against the fake SPIRE socket with
// backoff shortened so reconnects happen within the test budget.
func newReconnectTestBridge(sock string) *Bridge {
	b := NewBridge(sock, nopStore{}, nil, logr.Discard())
	b.backoffInitial = 10 * time.Millisecond
	b.backoffMax = 50 * time.Millisecond
	return b
}

// TestBundleStreamReconnects verifies that a bundle subscription stream ending
// (e.g. a SPIRE agent restart) does not fail the bridge runnable — which would
// take down the whole agent — but is re-subscribed with backoff.
func TestBundleStreamReconnects(t *testing.T) {
	fake, sock := startFakeSpire(t)
	b := newReconnectTestBridge(sock)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()

	require.Eventually(t, func() bool {
		bundle, _ := fake.counts()
		return bundle >= 2
	}, 10*time.Second, 10*time.Millisecond, "bridge must re-subscribe to bundles after the stream ends")

	select {
	case err := <-done:
		t.Fatalf("Start returned instead of retrying the bundle stream: %v", err)
	default:
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "Start must return nil on context cancellation")
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
}

// TestSVIDStreamResubscribes verifies that a pod's SVID stream ending is
// re-subscribed instead of silently freezing the pod's SVID until expiry, and
// that the subscription stays tracked so UnsubscribePod still works.
func TestSVIDStreamResubscribes(t *testing.T) {
	fake, sock := startFakeSpire(t)
	b := newReconnectTestBridge(sock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Start(ctx) }()

	select {
	case <-b.Started():
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not start")
	}

	const netns = "/proc/42/ns/net"
	require.NoError(t, b.SubscribePod(netns, "spiffe://example.org/sa", PodSelectors("ns", "sa", "pod", "uid")))

	require.Eventually(t, func() bool {
		_, svid := fake.counts()
		return svid >= 2
	}, 10*time.Second, 10*time.Millisecond, "bridge must re-subscribe to SVIDs after the stream ends")

	b.subsMu.Lock()
	_, exists := b.subscriptions[netns]
	b.subsMu.Unlock()
	require.True(t, exists, "subscription must remain tracked across reconnects")

	select {
	case err := <-done:
		t.Fatalf("Start returned unexpectedly: %v", err)
	default:
	}
}
