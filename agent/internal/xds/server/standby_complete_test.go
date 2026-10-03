package server

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flakyRegistryServer is a server whose registry fails until healthy is set.
func flakyRegistryServer(t *testing.T) (*AgentXdsServer, *atomic.Bool) {
	t.Helper()
	var healthy atomic.Bool
	reg := &mockRegistry{
		listAllEndpointsFunc: func(context.Context, registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if !healthy.Load() {
				return nil, errors.New("registrar unreachable")
			}
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}
	store := storage.NewMockStorageWithGetAll(func(context.Context) ([]*cniv1.CNIPod, error) { return nil, nil })
	sc := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	srv, err := NewAgentXdsServer(t.Context(), "cluster-1", "node-1", "example.org", reg, store, sc, nil, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	srv.retryBackoff = 10 * time.Millisecond
	return srv, &healthy
}

// TestStandbyComplete_ATimedOutRegistryGateIsNotComplete is the #740 clobber a
// surge roll could otherwise cause (proposal 041 review): a standby that
// cannot reach the registrar falls back to local-only config — right for a
// node nobody serves, wrong for a standby, whose Ready would let the
// DaemonSet delete the healthy old agent and the takeover then publish
// local-only CDS/EDS. FirstServeReady still closes (the uncontended path is
// unchanged); StandbyComplete waits until the registry actually loads.
func TestStandbyComplete_ATimedOutRegistryGateIsNotComplete(t *testing.T) {
	srv, healthy := flakyRegistryServer(t)

	require.NoError(t, srv.PreListen(t.Context()))
	assert.True(t, isClosed(srv.FirstServeReady()), "a lone agent still serves local-only, as before")
	time.Sleep(100 * time.Millisecond)
	assert.False(t, isClosed(srv.StandbyComplete()), "a standby whose registry gate timed out must stay NotReady")

	healthy.Store(true)
	require.Eventually(t, func() bool { return isClosed(srv.StandbyComplete()) }, 10*time.Second, 5*time.Millisecond,
		"the background retry's success completes the standby")
}

// TestStandbyComplete_ATimedOutCaptureGateIsNotComplete: likewise, a capture
// listener without its projected TCP chains is a degraded first serve.
func TestStandbyComplete_ATimedOutCaptureGateIsNotComplete(t *testing.T) {
	srv, healthy := flakyRegistryServer(t)
	healthy.Store(true)
	projected := make(chan struct{})
	srv.SetCaptureGate(projected)
	srv.captureTimeout = 20 * time.Millisecond

	require.NoError(t, srv.PreListen(t.Context()))
	assert.True(t, isClosed(srv.FirstServeReady()))
	time.Sleep(100 * time.Millisecond)
	assert.False(t, isClosed(srv.StandbyComplete()), "a standby whose capture gate timed out must stay NotReady")

	close(projected)
	require.Eventually(t, func() bool { return isClosed(srv.StandbyComplete()) }, 5*time.Second, 5*time.Millisecond)
}

// TestStandbyComplete_AllGatesPassed: the ordinary standby.
func TestStandbyComplete_AllGatesPassed(t *testing.T) {
	srv, healthy := flakyRegistryServer(t)
	healthy.Store(true)
	projected := make(chan struct{})
	close(projected)
	srv.SetCaptureGate(projected)

	require.NoError(t, srv.PreListen(t.Context()))
	require.Eventually(t, func() bool { return isClosed(srv.StandbyComplete()) }, 5*time.Second, 5*time.Millisecond)
}

// TestTakeoverCertificateWait_AStuckPodDoesNotTaxEveryTakeover: a pod whose
// SVID never comes is in the backlog before the takeover; the bind must not
// wait out the full bound for it again on every surge roll. Only an increase
// over the pre-takeover baseline is waited for.
func TestTakeoverCertificateWait_AStuckPodDoesNotTaxEveryTakeover(t *testing.T) {
	srv, c := newCertWaitServer(t, arrivedIdentity())
	srv.clientCertTimeout = 50 * time.Millisecond
	require.NoError(t, srv.PreListen(t.Context()))
	require.Equal(t, 1, c.AwaitingClientCertificates(), "one pod whose certificate never comes")

	require.NoError(t, srv.MarkTakeoverBaseline(t.Context()))
	started := time.Now()
	srv.waitForTakeoverCertificates(t.Context())
	assert.Less(t, time.Since(started), 200*time.Millisecond, "no increase over the baseline: nothing to wait for")
}

// TestTakeoverCertificateWait_WaitsForAnIncrease: a takeover that added a pod
// still waiting for its certificate holds the bind, bounded.
func TestTakeoverCertificateWait_WaitsForAnIncrease(t *testing.T) {
	srv, c := newCertWaitServer(t, arrivedIdentity())
	srv.clientCertTimeout = 50 * time.Millisecond
	require.NoError(t, srv.PreListen(t.Context()))
	require.Equal(t, 1, c.AwaitingClientCertificates())

	srv.takeoverAwaiting.Store(0) // as if marked before the reconcile added this pod
	started := time.Now()
	srv.waitForTakeoverCertificates(t.Context())
	elapsed := time.Since(started)
	assert.GreaterOrEqual(t, elapsed, takeoverClientCertTimeout, "an added pod's certificate is waited for")
	assert.Less(t, elapsed, 5*time.Second, "bounded")
}
