// Package ack tracks Envoy's delta-xDS ACK/NACKs per resource, replacing admin
// /config_dump polling (which serializes config on Envoy's main thread) as the
// agent's confirmation that a config update reached the proxy.
//
// Semantics: a delta ACK means Envoy validated and *accepted* the update — a bad
// config or a failed listener socket bind (including the per-pod netns bind) is
// a NACK carrying Envoy's error detail. ACK does not by itself mean the listener
// is active on workers; the data-plane proof is the CNI plugin's in-netns probe
// of the readiness health_check filter. The tracker is the diagnostic layer.
package ack

import (
	"context"
	"fmt"
	"sync"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
)

// resourceState is the last word Envoy gave about one resource.
type resourceState struct {
	// present is true when the most recent ACK covering this resource added or
	// updated it, false when it removed it (or nothing is known yet).
	present bool
	// nackErr holds Envoy's error detail when the most recent response covering
	// this resource was rejected. Cleared by a subsequent ACK.
	nackErr error
}

// inflightKey identifies one unacknowledged delta response. Nonces are unique
// per stream in go-control-plane, so the pair is unambiguous.
type inflightKey struct {
	streamID int64
	nonce    string
}

// inflightResponse records which resources a delta response added/removed, so
// the matching ACK/NACK (a request echoing the nonce) can be attributed.
type inflightResponse struct {
	typeURL string
	added   []string
	removed []string
}

// Tracker observes the delta-xDS streams via server callbacks and lets callers
// wait until Envoy has acknowledged the presence or removal of a named resource.
type Tracker struct {
	log logr.Logger

	mu       sync.Mutex
	state    map[string]resourceState // keyed by typeURL + "/" + name
	inflight map[inflightKey]inflightResponse
	// changed is closed and replaced on every state transition (broadcast).
	changed chan struct{}
}

// NewTracker creates an empty Tracker.
func NewTracker(log logr.Logger) *Tracker {
	return &Tracker{
		log:      log.WithName("xds-ack"),
		state:    make(map[string]resourceState),
		inflight: make(map[inflightKey]inflightResponse),
		changed:  make(chan struct{}),
	}
}

// Callbacks returns the go-control-plane server callbacks feeding this tracker.
// Pass the result to serverv3.NewServer (the agent's proxy speaks delta ADS, so
// only the delta hooks are wired).
func (t *Tracker) Callbacks() serverv3.Callbacks {
	return serverv3.CallbackFuncs{
		StreamDeltaResponseFunc: t.onDeltaResponse,
		StreamDeltaRequestFunc:  t.onDeltaRequest,
		DeltaStreamClosedFunc:   t.onDeltaStreamClosed,
	}
}

// WaitListenerPresent blocks until Envoy has ACKed an update containing the
// named listener, the context ends, or Envoy NACKs it (returned as the error).
//
// A listener already ACKed earlier returns immediately. A listener Envoy
// already holds but that was never sent on the current stream (agent restart
// with initial_resource_versions match) is never ACKed by name and waits out
// the caller's deadline — callers treat the wait as best-effort, exactly like
// the admin config_dump poll this replaces.
func (t *Tracker) WaitListenerPresent(ctx context.Context, name string) error {
	return t.wait(ctx, resourcev3.ListenerType, name, true)
}

// WaitListenerAbsent blocks until Envoy has ACKed the removal of the named
// listener (or it was never known to be present), the context ends, or Envoy
// NACKs the removal.
func (t *Tracker) WaitListenerAbsent(ctx context.Context, name string) error {
	return t.wait(ctx, resourcev3.ListenerType, name, false)
}

func (t *Tracker) wait(ctx context.Context, typeURL, name string, wantPresent bool) error {
	key := typeURL + "/" + name
	for {
		t.mu.Lock()
		st := t.state[key]
		ch := t.changed
		t.mu.Unlock()

		if st.nackErr != nil {
			return fmt.Errorf("envoy rejected config for %s: %w", name, st.nackErr)
		}
		if st.present == wantPresent {
			return nil
		}

		select {
		case <-ctx.Done():
			if wantPresent {
				return fmt.Errorf("timed out waiting for envoy to ack %s", name)
			}
			return fmt.Errorf("timed out waiting for envoy to ack removal of %s", name)
		case <-ch:
		}
	}
}

// onDeltaResponse records the resources carried by an outgoing delta response
// under its nonce, so the eventual ACK/NACK can be attributed to them.
func (t *Tracker) onDeltaResponse(streamID int64, _ *discoveryv3.DeltaDiscoveryRequest, resp *discoveryv3.DeltaDiscoveryResponse) {
	if resp.GetNonce() == "" {
		return
	}
	entry := inflightResponse{
		typeURL: resp.GetTypeUrl(),
		removed: resp.GetRemovedResources(),
	}
	for _, r := range resp.GetResources() {
		entry.added = append(entry.added, r.GetName())
	}
	if len(entry.added) == 0 && len(entry.removed) == 0 {
		return
	}

	t.mu.Lock()
	t.inflight[inflightKey{streamID: streamID, nonce: resp.GetNonce()}] = entry
	t.mu.Unlock()
}

// onDeltaRequest resolves an inflight response when the request echoes its
// nonce: without an error detail it is an ACK (resources applied), with one it
// is a NACK (whole response rejected, error recorded against each resource).
func (t *Tracker) onDeltaRequest(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) error {
	nonce := req.GetResponseNonce()
	if nonce == "" {
		return nil
	}

	t.mu.Lock()
	key := inflightKey{streamID: streamID, nonce: nonce}
	entry, ok := t.inflight[key]
	if !ok {
		t.mu.Unlock()
		return nil
	}
	delete(t.inflight, key)

	if detail := req.GetErrorDetail(); detail != nil {
		nackErr := fmt.Errorf("%s", detail.GetMessage())
		for _, name := range append(entry.added, entry.removed...) {
			st := t.state[entry.typeURL+"/"+name]
			st.nackErr = nackErr
			t.state[entry.typeURL+"/"+name] = st
		}
	} else {
		for _, name := range entry.added {
			t.state[entry.typeURL+"/"+name] = resourceState{present: true}
		}
		for _, name := range entry.removed {
			t.state[entry.typeURL+"/"+name] = resourceState{present: false}
		}
	}
	t.broadcastLocked()
	t.mu.Unlock()

	if detail := req.GetErrorDetail(); detail != nil {
		t.log.Info("envoy NACKed delta response",
			"typeURL", entry.typeURL, "added", entry.added, "removed", entry.removed, "error", detail.GetMessage())
	}
	return nil
}

// onDeltaStreamClosed drops inflight responses for the closed stream; their
// ACKs will never arrive. Acknowledged state is kept: Envoy retains its config
// across stream reconnects.
func (t *Tracker) onDeltaStreamClosed(streamID int64, _ *corev3.Node) {
	t.mu.Lock()
	for key := range t.inflight {
		if key.streamID == streamID {
			delete(t.inflight, key)
		}
	}
	t.mu.Unlock()
}

// broadcastLocked wakes all waiters. Callers must hold t.mu.
func (t *Tracker) broadcastLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}
