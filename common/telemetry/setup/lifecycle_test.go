package setup

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDetachedTimeout_SurvivesParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, cancelDetached := DetachedTimeout(parent, time.Minute)
	defer cancelDetached()

	cancel()

	if err := ctx.Err(); err != nil {
		t.Fatalf("detached context observed the parent's cancellation: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("detached context was done after the parent was cancelled")
	default:
	}
}

func TestDetachedTimeout_KeepsParentValues(t *testing.T) {
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "value")

	ctx, cancel := DetachedTimeout(parent, time.Minute)
	defer cancel()

	if got := ctx.Value(key{}); got != "value" {
		t.Fatalf("value = %v, want %q", got, "value")
	}
}

func TestDetachedTimeout_IsBounded(t *testing.T) {
	ctx, cancel := DetachedTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("detached context was not bounded by its timeout")
	}
}

// TestBestEffortShutdown_RunsWithCancelledCaller is the regression test for the
// "failed to shutdown telemetry: context canceled" / "failed to flush OTel logs:
// context canceled" pair in issue #662: on SIGTERM the caller's context is
// already cancelled, and the flush must still get a live context.
func TestBestEffortShutdown_RunsWithCancelledCaller(t *testing.T) {
	var got error
	fn := BestEffortShutdown(func(ctx context.Context) error {
		got = ctx.Err()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := fn(ctx); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if got != nil {
		t.Fatalf("flush ran on a dead context: %v", got)
	}
}

func TestBestEffortShutdown_NilPassesThrough(t *testing.T) {
	if fn := BestEffortShutdown(nil); fn != nil {
		t.Fatal("BestEffortShutdown(nil) should return nil")
	}
}

func TestBestEffortShutdown_PropagatesError(t *testing.T) {
	want := errors.New("export failed")
	fn := BestEffortShutdown(func(context.Context) error { return want })

	if err := fn(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
