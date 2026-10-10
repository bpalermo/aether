package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPreListen_HoldsUntilCaptureProjection is the first-serve half of #1094.
//
// The capture listener's per-VIP and per-port TCP chains come from the
// mesh-Service projection, which arrives from a controller on its own schedule.
// If the socket opened before it, a restarted agent would replace Envoy's
// still-correct capture listener with one carrying none of those chains. On
// the reference cluster the registry wait happened to outlast the reconciler; nothing made it.
func TestPreListen_HoldsUntilCaptureProjection(t *testing.T) {
	srv, registryCalls := newHoldServer(t, nil)
	srv.SetCaptureGate(srv.cache.CaptureProjected())

	done := make(chan error, 1)
	go func() { done <- srv.PreListen(t.Context()) }()

	select {
	case err := <-done:
		t.Fatalf("PreListen returned before the mesh-Service projection (err=%v): the first snapshot would carry no capture TCP chains", err)
	case <-time.After(250 * time.Millisecond):
	}
	assert.Zero(t, registryCalls.Load(),
		"the registry load publishes the snapshot the socket opens on; it must come after the projection")

	// An EMPTY projection releases it too: a mesh with no Services still
	// projects once (capture.initialProjection), and must not sit out the bound.
	srv.cache.SetCaptureTCPServices(nil)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("PreListen did not proceed after the projection arrived")
	}
	assert.Positive(t, registryCalls.Load())
}

// TestPreListen_CaptureGateIsBounded: a kube API that cannot be listed must not
// keep the node's xDS socket closed forever. The projection still rebuilds the
// listeners whenever it lands.
func TestPreListen_CaptureGateIsBounded(t *testing.T) {
	srv, registryCalls := newHoldServer(t, nil)
	srv.SetCaptureGate(make(chan struct{})) // never closes
	srv.captureTimeout = 100 * time.Millisecond

	start := time.Now()
	require.NoError(t, srv.PreListen(t.Context()))
	assert.GreaterOrEqual(t, time.Since(start), 100*time.Millisecond)
	assert.Positive(t, registryCalls.Load())
}

// TestPreListen_CaptureGateEndsOnShutdown: like the identity hold, the capture
// hold is not a way to wedge a shutdown.
func TestPreListen_CaptureGateEndsOnShutdown(t *testing.T) {
	srv, registryCalls := newHoldServer(t, nil)
	srv.SetCaptureGate(make(chan struct{}))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.PreListen(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the capture hold did not end when the context was cancelled")
	}
	assert.Zero(t, registryCalls.Load(), "a cancelled hold must not fall through to the snapshot build")
}
