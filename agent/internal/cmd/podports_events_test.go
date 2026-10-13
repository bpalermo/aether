package cmd

import (
	"testing"

	"aethermesh.dev/agent/internal/gamma"
)

type hookHolder struct{ fn func() }

// fakeGammaCache is a snapshot cache as the GAMMA reconciler sees it.
type fakeGammaCache struct {
	gamma.RouteSink
	gamma.PodPorts
	hookHolder
}

// TestNewGammaReconciler_IsGivenTheCachesEvents: the reconciler the agent
// runs is built over the cache in every role, the events included.
func TestNewGammaReconciler_IsGivenTheCachesEvents(t *testing.T) {
	c := &fakeGammaCache{}
	r := newGammaReconciler(nil, nil, c, "mesh.example")
	if r.Sink != gamma.RouteSink(c) || r.PodPorts != gamma.PodPorts(c) {
		t.Fatal("the reconciler's sink and its pod-port answers are not the cache")
	}
	if r.MeshDomain != "mesh.example" {
		t.Fatalf("mesh domain %q", r.MeshDomain)
	}
	if r.PodPortsChanged == nil || c.fn == nil {
		t.Fatal("the reconciler is not given the cache's pod-port events: nothing would make it judge a status again when no API object changed")
	}
	c.fn()
	select {
	case <-r.PodPortsChanged:
	default:
		t.Fatal("the cache said its pods' ports changed and the reconciler's channel carries no event")
	}
}

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
