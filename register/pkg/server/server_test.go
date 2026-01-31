package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/go-logr/logr/testr"
)

func TestNewRegisterServer(t *testing.T) {
	log := testr.New(t)

	rs, err := NewRegisterServer(log)
	if err != nil {
		t.Fatalf("NewRegisterServer failed: %v", err)
	}

	if rs == nil {
		t.Fatal("expected non-nil RegisterServer")
	}
	if rs.e == nil {
		t.Error("expected non-nil actor engine")
	}
	if rs.clients == nil {
		t.Error("expected non-nil clients map")
	}
	if rs.agents == nil {
		t.Error("expected non-nil agents map")
	}
}

func TestRegisterServer_Shutdown_NoClients(t *testing.T) {
	log := testr.New(t)

	rs, err := NewRegisterServer(log)
	if err != nil {
		t.Fatalf("NewRegisterServer failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = rs.Shutdown(ctx)
	if err != nil {
		t.Errorf("Shutdown with no clients should succeed, got: %v", err)
	}
}

func TestRegisterServer_Shutdown_WithClients(t *testing.T) {
	log := testr.New(t)

	rs, err := NewRegisterServer(log)
	if err != nil {
		t.Fatalf("NewRegisterServer failed: %v", err)
	}

	// Spawn a test actor
	pid := rs.e.Spawn(newTestActor, "test-actor")
	rs.clients["test-node"] = pid

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = rs.Shutdown(ctx)
	if err != nil {
		t.Errorf("Shutdown should succeed, got: %v", err)
	}
}

func TestRegisterServer_Shutdown_ContextTimeout(t *testing.T) {
	log := testr.New(t)

	rs, err := NewRegisterServer(log)
	if err != nil {
		t.Fatalf("NewRegisterServer failed: %v", err)
	}

	// Spawn a slow actor
	pid := rs.e.Spawn(newSlowActor, "slow-actor")
	rs.clients["slow-node"] = pid

	// Use very short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	err = rs.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got: %v", err)
	}
}

// Test actor that stops immediately
type testActor struct{}

func newTestActor() actor.Receiver {
	return &testActor{}
}

func (a *testActor) Receive(c *actor.Context) {}

// Test actor that delays stopping
type slowActor struct{}

func newSlowActor() actor.Receiver {
	return &slowActor{}
}

func (a *slowActor) Receive(c *actor.Context) {
	switch c.Message().(type) {
	case actor.Stopped:
		time.Sleep(5 * time.Second)
	}
}
