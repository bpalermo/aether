package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/ownership"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type scriptedRunnable struct {
	calls atomic.Int32
	errs  []error
}

func (s *scriptedRunnable) Start(context.Context) error {
	n := int(s.calls.Add(1)) - 1
	if n < len(s.errs) {
		return s.errs[n]
	}
	return nil
}

func ownedNow(t *testing.T) *ownership.Node {
	t.Helper()
	o := ownership.New("", nil, nil)
	o.Claim()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = o.Start(ctx) }()
	require.NoError(t, o.WaitOwned(ctx))
	return o
}

// TestOwnedRunnableWaitsOutThePredecessorsPort: the lock can be ours a moment
// before the old agent's metrics socket is closed (descriptors close in order
// on exit). EADDRINUSE then is a wait, never a fatal runnable error — which
// would stop the manager and take CNI and xDS down with the metrics port.
func TestOwnedRunnableWaitsOutThePredecessorsPort(t *testing.T) {
	inUse := fmt.Errorf("failed to start metrics server: failed to create listener: %w",
		&net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRINUSE})
	inner := &scriptedRunnable{errs: []error{inUse, inUse}}
	r := ownedRunnable{owner: ownedNow(t), inner: inner}

	done := make(chan error, 1)
	go func() { done <- r.Start(t.Context()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("never bound")
	}
	assert.EqualValues(t, 3, inner.calls.Load())
}

// TestOwnedRunnableOtherErrorsStayFatal: only the predecessor's port is
// waited out.
func TestOwnedRunnableOtherErrorsStayFatal(t *testing.T) {
	boom := errors.New("boom")
	r := ownedRunnable{owner: ownedNow(t), inner: &scriptedRunnable{errs: []error{boom}}}
	assert.ErrorIs(t, r.Start(t.Context()), boom)
}

// TestOwnedRunnableNeverStartsOnAStandby: a standby stopped before it owns
// the node never binds the port.
func TestOwnedRunnableNeverStartsOnAStandby(t *testing.T) {
	inner := &scriptedRunnable{}
	r := ownedRunnable{owner: ownership.New("", nil, nil), inner: inner} // never Started: never owned
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, r.Start(ctx))
	assert.Zero(t, inner.calls.Load())
}
