package prober

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/go-logr/logr"
)

func TestClassifyErr(t *testing.T) {
	t.Run("connection refused -> connection_error", func(t *testing.T) {
		err := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
		if got := classifyErr(context.Background(), err); got != resultConnectionError {
			t.Fatalf("got %q, want %q", got, resultConnectionError)
		}
	})
	t.Run("deadline -> timeout", func(t *testing.T) {
		if got := classifyErr(context.Background(), context.DeadlineExceeded); got != resultTimeout {
			t.Fatalf("got %q, want %q", got, resultTimeout)
		}
	})
}

func TestNewTargets(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReachabilityTargets = []string{"svc-1", "svc-2"}
	p, err := New(context.Background(), cfg, logr.Discard(), "test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(p.targets) != 3 {
		t.Fatalf("want 3 targets (1 liveness + 2 reachability), got %d", len(p.targets))
	}
	if p.targets[0].tier != tierLiveness {
		t.Fatalf("first target tier = %q, want liveness", p.targets[0].tier)
	}
	if want := "http://127.0.0.1:18081/-/-/live"; p.targets[0].url != want {
		t.Fatalf("liveness url = %q, want %q", p.targets[0].url, want)
	}
	if want := "svc-1.aether.internal"; p.targets[1].authority != want {
		t.Fatalf("reachability authority = %q, want %q", p.targets[1].authority, want)
	}
}
