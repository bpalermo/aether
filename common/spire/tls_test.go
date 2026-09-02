package spire

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNewSourceWithTimeout_BoundsFirstSVIDWait is the regression test for the
// startup stall in issue #662: the Workload API client retries an unreachable
// agent forever, so without a bound the first-SVID wait never returns and the
// binary is killed by its liveness probe before it ever serves /healthz.
func TestNewSourceWithTimeout_BoundsFirstSVIDWait(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "nothing-listens-here.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		src, err := NewSourceWithTimeout(ctx, socket, 200*time.Millisecond)
		if src != nil {
			_ = src.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("NewSourceWithTimeout() succeeded against a socket nobody serves")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline-exceeded error", err)
		}
		// The error must name the socket: the old one said only
		// "context canceled", which pointed at the wrong subsystem entirely.
		if !strings.Contains(err.Error(), socket) {
			t.Fatalf("err = %v, want it to name the socket %q", err, socket)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("NewSourceWithTimeout() did not honour its timeout")
	}

	// The caller's context must survive the failure: it is the startup context
	// the rest of the binary hangs off.
	if err := ctx.Err(); err != nil {
		t.Fatalf("NewSourceWithTimeout() disturbed the caller's context: %v", err)
	}
}

// TestNewSourceWithTimeout_HonoursCallerCancellation keeps the pre-existing
// behaviour: a cancelled caller context still aborts the wait.
func TestNewSourceWithTimeout_HonoursCallerCancellation(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "nothing-listens-here.sock")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		src, err := NewSourceWithTimeout(ctx, socket, time.Minute)
		if src != nil {
			_ = src.Close()
		}
		done <- err
	}()

	time.AfterFunc(50*time.Millisecond, cancel)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want a cancellation error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("NewSourceWithTimeout() ignored the caller's cancellation")
	}
}

func TestSocketAddr(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "bare path", path: "/run/spire/socket", want: "unix:///run/spire/socket"},
		{name: "unix endpoint", path: "unix:///run/spire/socket", want: "unix:///run/spire/socket"},
		{name: "tcp endpoint", path: "tcp://127.0.0.1:8081", want: "tcp://127.0.0.1:8081"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := socketAddr(tt.path); got != tt.want {
				t.Errorf("socketAddr(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
