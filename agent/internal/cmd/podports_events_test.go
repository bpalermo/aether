package cmd

import (
	"testing"
)

type hookHolder struct{ fn func() }

func (h *hookHolder) OnPodPortsChanged(fn func()) { h.fn = fn }

// TestPodPortsEvents: the cache's "my pods' ports changed" reaches the route
// reconciler as an event, and the hook, which the cache calls with its lock
// held, never blocks: changes that pile up before the reconciler runs are one
// event, because one reconcile judges every route.
func TestPodPortsEvents(t *testing.T) {
	h := &hookHolder{}
	events := podPortsEvents(h)
	if h.fn == nil {
		t.Fatal("no hook was registered with the cache")
	}
	select {
	case <-events:
		t.Fatal("an event before any change")
	default:
	}
	for i := 0; i < 5; i++ {
		h.fn() // must not block
	}
	select {
	case <-events:
	default:
		t.Fatal("the cache said its pods' ports changed and no event was queued")
	}
	select {
	case <-events:
		t.Fatal("five changes before one reconcile are one event")
	default:
	}
	h.fn()
	select {
	case <-events:
	default:
		t.Fatal("a change after the event was taken queued nothing")
	}
}
