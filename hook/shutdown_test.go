package hook

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
)

type mockShutdownHook struct {
	shutdownCalled bool
	shutdownErr    error
	shutdownDelay  time.Duration
	mu             sync.Mutex
}

func (m *mockShutdownHook) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdownCalled = true
	if m.shutdownDelay > 0 {
		select {
		case <-time.After(m.shutdownDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.shutdownErr
}

func (m *mockShutdownHook) wasCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shutdownCalled
}

func TestAddShutdownHook_CallsAllHooks(t *testing.T) {
	log := testr.New(t)

	hook1 := &mockShutdownHook{}
	hook2 := &mockShutdownHook{}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		AddShutdownHook(ctx, 5*time.Second, log, hook1, hook2)
		close(done)
	}()

	// Give time for the signal handler to be set up
	time.Sleep(50 * time.Millisecond)

	// Trigger shutdown via context cancellation instead of signal
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for shutdown")
	}

	if !hook1.wasCalled() {
		t.Error("hook1 Shutdown was not called")
	}
	if !hook2.wasCalled() {
		t.Error("hook2 Shutdown was not called")
	}
}

func TestAddShutdownHook_HandlesHookError(t *testing.T) {
	log := testr.New(t)

	hook1 := &mockShutdownHook{shutdownErr: errors.New("shutdown error")}
	hook2 := &mockShutdownHook{}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		AddShutdownHook(ctx, 5*time.Second, log, hook1, hook2)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for shutdown")
	}

	// Both hooks should still be called even with one error
	if !hook1.wasCalled() {
		t.Error("hook1 Shutdown was not called")
	}
	if !hook2.wasCalled() {
		t.Error("hook2 Shutdown was not called")
	}
}

func TestAddShutdownHook_RespectsContextCancellation(t *testing.T) {
	log := testr.New(t)

	ctx, cancel := context.WithCancel(context.Background())
	hook := &mockShutdownHook{}

	done := make(chan struct{})
	go func() {
		AddShutdownHook(ctx, 5*time.Second, log, hook)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for shutdown")
	}

	if !hook.wasCalled() {
		t.Error("hook Shutdown was not called on context cancellation")
	}
}

func TestAddShutdownHook_NoHooks(t *testing.T) {
	log := testr.New(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		AddShutdownHook(ctx, 5*time.Second, log)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for shutdown with no hooks")
	}
}
