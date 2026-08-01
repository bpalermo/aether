package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertNoMalformedListeners fails the test if any resource returned by
// Listeners() is a nil interface, a typed-nil *Listener, or a Listener with an
// empty Name AND a nil Address. go-control-plane names a nameless Listener with a
// random UUID, and Envoy then NACKs the whole LDS delta with
// "error adding listener named '<UUID>': address is necessary" — dropping the
// good listeners in the same push and wedging a pod added during the window.
func assertNoMalformedListeners(t *testing.T, resources []types.Resource) {
	t.Helper()
	for i, r := range resources {
		require.NotNilf(t, r, "Listeners()[%d] is a nil interface", i)
		l, ok := r.(*listenerv3.Listener)
		if !ok {
			continue
		}
		require.NotNilf(t, l, "Listeners()[%d] is a typed-nil *Listener", i)
		if l.GetName() == "" && l.GetAddress() == nil {
			t.Fatalf("Listeners()[%d] is malformed: empty Name and nil Address (Envoy would NACK with \"address is necessary\")", i)
		}
	}
}

// TestListeners_NoTypedNilUDPCapture is the focused regression guard for the LDS
// "address is necessary" NACK. With capture ENABLED but no UDPRoute backends in
// scope, proxy.GenerateUDPCaptureListener returns a nil *Listener. The cache must
// store an untyped-nil in the listenerEntry — not the nil pointer wrapped in a
// non-nil types.Resource — otherwise Listeners() appends an empty Listener that
// Envoy rejects.
func TestListeners_NoTypedNilUDPCapture(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	// No SetUDPServiceRoutes call: udpServiceRoutes is empty, so
	// GenerateUDPCaptureListener returns (nil, nil) for every pod.

	pod := makeCNIPod("pod-a", "default", "/proc/100/ns/net")
	pod.Ips = []string{"10.0.0.1"}
	require.NoError(t, c.AddPod(context.Background(), pod, "example.org"))

	// The udpCapture field must be an untyped nil, not a typed-nil interface.
	c.listenerMu.RLock()
	entry := c.listeners["/proc/100/ns/net"]
	c.listenerMu.RUnlock()
	assert.Nil(t, entry.udpCapture, "udpCapture must be an untyped nil when there are no UDPRoute backends")

	assertNoMalformedListeners(t, c.Listeners())
}

// TestListeners_NoMalformedUnderChurn drives concurrent AddPod / RemovePod churn
// with capture enabled (the configuration that produced the empty UDP-capture
// listener) and asserts that Listeners() NEVER returns a Listener with an empty
// Name or a nil Address at any observed point during the churn. This is the
// regression guard for the production "address is necessary" NACK seen during
// node-agent rollouts.
func TestListeners_NoMalformedUnderChurn(t *testing.T) {
	const pods = 40

	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)

	var wg sync.WaitGroup

	// Churners: add then remove each pod.
	for i := 0; i < pods; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			netns := fmt.Sprintf("/proc/%d/ns/net", 1000+i)
			pod := makeCNIPod(fmt.Sprintf("pod-%d", i), "default", netns)
			pod.Ips = []string{fmt.Sprintf("10.0.0.%d", i+1)}
			_ = c.AddPod(context.Background(), pod, "example.org")
			_ = c.RemovePod(context.Background(), netns)
		}(i)
	}

	// Readers: continuously snapshot Listeners() and assert well-formedness.
	done := make(chan struct{})
	var rwg sync.WaitGroup
	for r := 0; r < 8; r++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for {
				select {
				case <-done:
					return
				default:
					assertNoMalformedListeners(t, c.Listeners())
				}
			}
		}()
	}

	wg.Wait()
	close(done)
	rwg.Wait()

	// Final state after all churn settles must also be well-formed.
	assertNoMalformedListeners(t, c.Listeners())
}

// TestAppendListener_FiltersMalformed unit-tests the appendListener guard
// directly across the failure modes it must reject and the resources it must keep.
func TestAppendListener_FiltersMalformed(t *testing.T) {
	good := &listenerv3.Listener{Name: "good"}
	var typedNil *listenerv3.Listener

	tests := []struct {
		name string
		in   types.Resource
		keep bool
	}{
		{name: "nil interface dropped", in: nil, keep: false},
		{name: "typed-nil pointer dropped", in: types.Resource(typedNil), keep: false},
		{name: "empty name and nil address dropped", in: &listenerv3.Listener{}, keep: false},
		{name: "named listener kept", in: good, keep: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendListener(nil, tt.in)
			if tt.keep {
				assert.Len(t, got, 1)
			} else {
				assert.Empty(t, got)
			}
		})
	}
}
