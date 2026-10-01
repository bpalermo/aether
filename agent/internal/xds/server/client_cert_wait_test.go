package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #1103: the first serve after an agent restart must not open the xDS
// socket on a snapshot that still lacks the local workloads' client
// certificates. Such a snapshot carries none of their `quic:` twins (the #1049
// gate), so a proxy that reconnects to it is told to remove every twin it
// holds and routes into the gap: on kind, with the proxy's ADS reconnect
// capped at 1 s, 1-9.6 s of cluster_not_found per restart.

const (
	certWaitTD         = "example.org"
	certWaitNodeSVID   = "spiffe://example.org/ns/aether-system/sa/aether-agent"
	certWaitClientSVID = "spiffe://example.org/ns/demo/sa/client"
)

// newCertWaitServer builds a node-agent xDS server (identity gate present and
// already satisfied, registry answering) whose storage holds one local pod
// under ServiceAccount demo/client, with the SPIRE bridge having delivered the
// node's own SVID but not yet the pod's.
func newCertWaitServer(t *testing.T, gate IdentityGate) (*AgentXdsServer, *cache.SnapshotCache) {
	t.Helper()
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}
	pod := &cniv1.CNIPod{
		Name:             "client-0",
		Namespace:        "demo",
		ServiceAccount:   "client",
		NetworkNamespace: "/proc/self/ns/net", // must exist: LoadListenersFromStorage skips a missing netns
		ContainerId:      "c0ffee",
		Ips:              []string{"10.0.0.7"},
	}
	store := storage.NewMockStorageWithGetAll(func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{pod}, nil
	})
	c := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	require.NoError(t, c.SetSecrets(t.Context(), []*tlsv3.Secret{{Name: certWaitNodeSVID}}))

	srv, err := NewAgentXdsServer(t.Context(), "cluster-1", "node-1", certWaitTD, reg, store, c, nil, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	srv.SetIdentityGate(gate)
	return srv, c
}

func arrivedIdentity() *fakeIdentity {
	g := newFakeIdentity()
	g.arrive()
	return g
}

// The socket opens only once the snapshot carries the local pod's certificate.
//
// Red on main: PreListen returns as soon as the registry load is done, with
// the pod's identity still awaiting its certificate.
func TestPreListen_WaitsForLocalClientCertificates(t *testing.T) {
	srv, c := newCertWaitServer(t, arrivedIdentity())
	srv.clientCertTimeout = 30 * time.Second

	done := make(chan error, 1)
	go func() { done <- srv.PreListen(t.Context()) }()

	select {
	case err := <-done:
		t.Fatalf("PreListen returned (err=%v) while the snapshot still held %d local identity out of east-west QUIC for want of its certificate: a reconnecting proxy would be told to remove its twins",
			err, c.AwaitingClientCertificates())
	case <-time.After(500 * time.Millisecond):
	}
	require.Equal(t, 1, c.AwaitingClientCertificates(), "the pod's identity is the one awaiting its certificate")

	// The SPIRE bridge delivers the pod's SVID: its snapshot carries the twins,
	// and the socket may open.
	require.NoError(t, c.SetSecrets(t.Context(), []*tlsv3.Secret{{Name: certWaitNodeSVID}, {Name: certWaitClientSVID}}))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("PreListen did not proceed after the certificate arrived")
	}
	assert.Zero(t, c.AwaitingClientCertificates())
}

// A certificate that never comes costs the bound, not the node's xDS.
func TestPreListen_ClientCertificateWaitIsBounded(t *testing.T) {
	srv, c := newCertWaitServer(t, arrivedIdentity())
	srv.clientCertTimeout = 300 * time.Millisecond

	start := time.Now()
	require.NoError(t, srv.PreListen(t.Context()))
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 300*time.Millisecond, "the wait must run while a certificate is missing")
	assert.Less(t, elapsed, 10*time.Second, "the wait must end at its bound")
	assert.Equal(t, 1, c.AwaitingClientCertificates(), "served without the certificate, as before the wait existed")
}

// No identity gate (SPIRE off, the edge): no wait, whatever the cache says.
func TestPreListen_NoIdentityGateNoClientCertificateWait(t *testing.T) {
	srv, _ := newCertWaitServer(t, nil)
	srv.clientCertTimeout = 30 * time.Second

	start := time.Now()
	require.NoError(t, srv.PreListen(t.Context()))
	assert.Less(t, time.Since(start), 10*time.Second)
}

// Shutdown ends the wait without an error.
func TestPreListen_ClientCertificateWaitEndsOnShutdown(t *testing.T) {
	srv, _ := newCertWaitServer(t, arrivedIdentity())
	srv.clientCertTimeout = 30 * time.Second

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.PreListen(ctx) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the client-certificate wait did not end when the context was cancelled")
	}
}
