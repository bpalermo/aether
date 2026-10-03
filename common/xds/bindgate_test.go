package xds

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// startServer runs srv.Start in the background and returns its result channel.
func startServer(ctx context.Context, srv *Server) <-chan error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()
	return errCh
}

func waitErr(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Start to return")
		return nil
	}
}

// TestBindGate_HoldsTheBindUntilItOpens: a surge standby (proposal 041) runs
// PreListen but binds nothing until the gate opens. The path must not exist —
// not even briefly — while the gate is shut, or the standby would have
// displaced the owning agent's socket.
func TestBindGate_HoldsTheBindUntilItOpens(t *testing.T) {
	cfg := newUDSConfig(t)
	srv := NewServer(cfg, slog.New(slog.DiscardHandler), WithGRPCServer(grpc.NewServer()))
	cb := &stubCallback{}
	srv.AddCallback(cb)
	gate := make(chan struct{})
	srv.SetBindGate(func(ctx context.Context) error {
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := startServer(ctx, &srv)

	require.Eventually(t, cb.called.Load, time.Second, time.Millisecond, "PreListen runs before the gate")
	time.Sleep(50 * time.Millisecond)
	assert.NoFileExists(t, cfg.Address, "nothing is bound while the gate is shut")
	assert.False(t, srv.readiness.Load())

	close(gate)
	require.Eventually(t, func() bool { return srv.readiness.Load() }, time.Second, time.Millisecond)
	conn, err := net.Dial("unix", cfg.Address)
	require.NoError(t, err, "bound once the gate opened")
	_ = conn.Close()

	cancel()
	require.NoError(t, waitErr(t, errCh))
	assert.NoFileExists(t, cfg.Address, "its own socket is removed on shutdown")
}

// TestBindGate_ShutdownWhileHeldIsNotAFailure: a standby stopped before it
// ever owned the node (a stuck roll undone) stops cleanly and binds nothing.
func TestBindGate_ShutdownWhileHeldIsNotAFailure(t *testing.T) {
	cfg := newUDSConfig(t)
	srv := NewServer(cfg, slog.New(slog.DiscardHandler), WithGRPCServer(grpc.NewServer()))
	srv.SetBindGate(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })

	ctx, cancel := context.WithCancel(context.Background())
	errCh := startServer(ctx, &srv)
	time.Sleep(20 * time.Millisecond)
	cancel()
	require.NoError(t, waitErr(t, errCh))
	assert.NoFileExists(t, cfg.Address)
}

// TestBindGate_ErrorFailsStart: a gate that fails while ctx is live is a fault.
func TestBindGate_ErrorFailsStart(t *testing.T) {
	cfg := newUDSConfig(t)
	srv := NewServer(cfg, slog.New(slog.DiscardHandler), WithGRPCServer(grpc.NewServer()))
	boom := errors.New("boom")
	srv.SetBindGate(func(context.Context) error { return boom })
	assert.ErrorIs(t, srv.Start(context.Background()), boom)
}

// TestShutdownLeavesASuccessorsSocketAlone is the "Socket ownership" row: Go's
// UnixListener.Close unlinks its path by name, so a server exiting after
// another process re-bound the path would delete THAT process's live socket.
// The server unlinks only the inode it bound.
func TestShutdownLeavesASuccessorsSocketAlone(t *testing.T) {
	cfg := newUDSConfig(t)
	srv := NewServer(cfg, slog.New(slog.DiscardHandler), WithGRPCServer(grpc.NewServer()))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := startServer(ctx, &srv)
	require.Eventually(t, func() bool { return srv.readiness.Load() }, time.Second, time.Millisecond)

	// A successor unlinks and re-binds the path while this server still runs.
	require.NoError(t, os.Remove(cfg.Address))
	successor, err := net.Listen("unix", cfg.Address)
	require.NoError(t, err)
	t.Cleanup(func() { _ = successor.Close() })

	cancel()
	require.NoError(t, waitErr(t, errCh))

	conn, err := net.Dial("unix", cfg.Address)
	require.NoError(t, err, "the successor's socket must survive this server's shutdown")
	_ = conn.Close()
}
