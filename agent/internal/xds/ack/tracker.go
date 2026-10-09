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
	"log/slog"
	"slices"
	"sync"

	commonlog "aethermesh.dev/common/log"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"go.opentelemetry.io/otel"
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

// streamType is one resource type on one delta stream.
type streamType struct {
	streamID int64
	typeURL  string
}

// inflightResponse records which resources a delta response added/removed, so
// the matching ACK/NACK (a request echoing the nonce) can be attributed.
type inflightResponse struct {
	typeURL string
	// systemVersion is the response's system_version_info: the version of the
	// snapshot the response was built from. The ACK echoes only the nonce, so
	// it is kept here to tell an AckObserver which snapshot was accepted.
	systemVersion string
	added         []string
	removed       []string
	// held is the resources the proxy stated it holds and this response, the
	// first of its type on its stream, neither added nor removed (#1511). Only
	// an ACK reads it: see statedHeld.
	held []string
}

// AckObserver is told, once per acknowledged delta response, the resource type
// and the version of the snapshot the response was built from. It is never
// told about a NACK: a rejected response leaves the proxy on what it had.
//
// A response that changed something is always told. One that carried nothing
// is told only when it was the first of its type on its stream: the answer to
// the resources the proxy stated it holds (onDeltaResponse, #1483).
//
// It runs on the xDS stream's goroutine, after the tracker's own lock is
// released: it must not block.
type AckObserver func(ctx context.Context, typeURL, systemVersion string)

// Tracker observes the delta-xDS streams via server callbacks and lets callers
// wait until Envoy has acknowledged the presence or removal of a named resource.
type Tracker struct {
	log     *slog.Logger
	metrics *trackerMetrics // nil disables instrumentation

	mu       sync.Mutex
	state    map[string]resourceState // keyed by typeURL + "/" + name
	inflight map[inflightKey]inflightResponse
	// answered is the (stream, type) pairs a response has been sent for. The
	// first response of a pair is the one computed against what the proxy
	// stated it holds (onDeltaResponse).
	answered map[streamType]struct{}
	// stated is, per (stream, type) whose first request has arrived and whose
	// first response has not, the names that request's
	// initial_resource_versions carried (noteRequestLocked). The entry is
	// there from the first request on, with no names when nothing may be
	// concluded from them, so that a later request is never taken for the
	// first.
	stated map[streamType][]string
	// changed is closed and replaced on every state transition (broadcast).
	changed chan struct{}
	// observer, when set, is told of every ACK (SetAckObserver).
	observer AckObserver
}

// SetAckObserver registers fn to be told of every acknowledged delta response.
// One observer; a second call replaces the first, nil removes it. Call it
// while wiring, before the xDS server serves: it may also be called later, but
// an ACK being processed at that moment may go to either observer.
func (t *Tracker) SetAckObserver(fn AckObserver) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.observer = fn
}

// NewTracker creates an empty Tracker.
func NewTracker(log *slog.Logger) *Tracker {
	log = commonlog.Named(log, "xds-ack")
	// Instruments ride the global MeterProvider (no-op unless --otel-enabled);
	// a registration failure only disables instrumentation, never the tracker.
	metrics, err := newTrackerMetrics(otel.Meter(meterName))
	if err != nil {
		log.Error("failed to create xds ack metrics; continuing without instrumentation", "error", err)
	}
	return &Tracker{
		log:      log,
		metrics:  metrics,
		state:    make(map[string]resourceState),
		inflight: make(map[inflightKey]inflightResponse),
		answered: make(map[streamType]struct{}),
		stated:   make(map[streamType][]string),
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
// A listener already ACKed earlier returns immediately. So does one Envoy
// already holds at the published version and that is therefore never sent on
// the current stream (an agent restart, a stream reset): the proxy states it
// in its opening request and the ACK of the opening response resolves it
// (statedHeld, #1511). Like an ACK, that says the proxy accepted the listener,
// not that it has finished warming it.
//
// A wait can still run to the caller's deadline with the listener in place:
// when no proxy is connected, or when the proxy rejected the opening response
// because of another resource. Callers treat the wait as best-effort, exactly
// like the admin config_dump poll this replaces.
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
			t.metrics.waitFailed(ctx, wantPresent, reasonNack)
			return fmt.Errorf("envoy rejected config for %s: %w", name, st.nackErr)
		}
		if st.present == wantPresent {
			return nil
		}

		select {
		case <-ctx.Done():
			t.metrics.waitFailed(ctx, wantPresent, reasonTimeout)
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
//
// A response that carries nothing is kept only when it is the FIRST response
// of its type on its stream (#1483). That one is the server's answer to the
// proxy's opening request, whose initial_resource_versions state every
// resource of the type the proxy holds: go-control-plane seeds the
// subscription with them, compares them with the snapshot, and for a wildcard
// subscription answers even when there is nothing to add and nothing to
// remove. So an empty first response reads "the snapshot with this
// system_version_info is exactly what you stated", and its ACK is told to the
// AckObserver like any other. After an agent restart against a proxy that is
// already in sync it is the only acknowledgement there is until a resource
// changes.
//
// A LATER empty response is dropped, and must be. From its first response on,
// go-control-plane compares the snapshot with what it has SENT on the stream,
// whether the proxy accepted it or not, and it answers every wildcard request
// that carries no nonce, which an on-demand subscription in the middle of a
// stream is. After a rejected update such a response is empty, names the
// rejected snapshot, and is ACKed: it says nothing about what the proxy holds.
//
// Both halves are measured on the pinned proxy by //agent/test/mtlspool
// (TestReconnectingProxyStatesTheClustersItHolds,
// TestReconnectingProxyStatesNoClusterItRejected).
func (t *Tracker) onDeltaResponse(streamID int64, _ *discoveryv3.DeltaDiscoveryRequest, resp *discoveryv3.DeltaDiscoveryResponse) {
	if resp.GetNonce() == "" {
		return
	}
	entry := inflightResponse{
		typeURL:       resp.GetTypeUrl(),
		systemVersion: resp.GetSystemVersionInfo(),
		removed:       resp.GetRemovedResources(),
	}
	for _, r := range resp.GetResources() {
		entry.added = append(entry.added, r.GetName())
	}
	empty := len(entry.added) == 0 && len(entry.removed) == 0

	t.mu.Lock()
	defer t.mu.Unlock()
	opening := streamType{streamID: streamID, typeURL: entry.typeURL}
	_, later := t.answered[opening]
	t.answered[opening] = struct{}{}
	if !later {
		entry.held = statedHeld(t.stated[opening], entry.added, entry.removed)
		delete(t.stated, opening)
	}
	if empty && later {
		return
	}
	t.inflight[inflightKey{streamID: streamID, nonce: resp.GetNonce()}] = entry
}

// onDeltaRequest resolves an inflight response when the request echoes its
// nonce: without an error detail it is an ACK (resources applied), with one it
// is a NACK (whole response rejected, error recorded against each resource).
//
// It also keeps what the first request of a type on a stream states the proxy
// holds, for the first response to be read against (noteRequestLocked).
func (t *Tracker) onDeltaRequest(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) error {
	nonce := req.GetResponseNonce()

	t.mu.Lock()
	t.noteRequestLocked(streamType{streamID: streamID, typeURL: req.GetTypeUrl()}, req)
	if nonce == "" {
		t.mu.Unlock()
		return nil
	}
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
		for _, name := range entry.held {
			t.state[entry.typeURL+"/"+name] = resourceState{present: true}
		}
	}
	t.broadcastLocked()
	observer := t.observer
	t.mu.Unlock()

	if detail := req.GetErrorDetail(); detail != nil {
		t.metrics.nacked(context.Background(), entry.typeURL)
		t.log.Info("envoy NACKed delta response",
			"typeURL", entry.typeURL, "added", entry.added, "removed", entry.removed, "error", detail.GetMessage())
		return nil
	}
	if observer != nil {
		observer(context.Background(), entry.typeURL, entry.systemVersion)
	}
	return nil
}

// noteRequestLocked keeps the names a stream's FIRST request of a type states
// in initial_resource_versions, until the first response of that type.
// Callers must hold t.mu.
//
// It mirrors go-control-plane, which seeds a type's subscription from the
// first request of the type on the stream and reads the field from no other.
// The names are kept only when the first response is certain to be the
// comparison of exactly those statements with the snapshot:
//
//   - the request opens a wildcard subscription. For a subscription by name
//     go-control-plane compares only the subscribed names, so a stated
//     resource that is missing from the response may not have been looked at;
//   - no other request of the type arrives before the first response. A second
//     one can change the subscription the response is computed for, so it
//     drops what the first stated.
func (t *Tracker) noteRequestLocked(key streamType, req *discoveryv3.DeltaDiscoveryRequest) {
	if _, answered := t.answered[key]; answered {
		return
	}
	if _, second := t.stated[key]; second {
		t.stated[key] = nil
		return
	}
	var names []string
	if opensWildcard(req) {
		for name := range req.GetInitialResourceVersions() {
			names = append(names, name)
		}
	}
	t.stated[key] = names
}

// wildcard is the resource name that subscribes to every resource of a type.
const wildcard = "*"

// opensWildcard reports whether a first request subscribes to every resource
// of its type: it names no resource (the legacy form) or names "*", and does
// not unsubscribe from "*".
func opensWildcard(req *discoveryv3.DeltaDiscoveryRequest) bool {
	if slices.Contains(req.GetResourceNamesUnsubscribe(), wildcard) {
		return false
	}
	subscribed := req.GetResourceNamesSubscribe()
	return len(subscribed) == 0 || slices.Contains(subscribed, wildcard)
}

// statedHeld returns the stated names that the first response of their type
// on their stream neither added nor removed (#1511).
//
// go-control-plane computes that response by comparing every resource of the
// snapshot with the version the proxy stated for it: a resource whose version
// differs, or that the proxy did not state, is added, and a stated resource
// the snapshot does not have is removed. So a stated name that is in neither
// list was stated at exactly the version the snapshot publishes, and nothing
// is sent for it on this stream until it changes.
//
// What that proves: the proxy holds the resource at the published version and
// accepted it. Envoy records a resource's version only once the update that
// carried it has been applied without error, so it never states a version it
// rejected (//agent/test/mtlspool,
// TestReconnectingProxyStatesNoClusterItRejected). That is what the ACK of a
// response carrying the resource proves, and no more: not that a listener has
// finished warming.
//
// The names are acted on only when the proxy ACKs the response
// (onDeltaRequest). A rejected opening response resolves nothing, though the
// proxy still holds what it stated: the agent then waits for the next
// acknowledgement instead of reasoning about a proxy that is rejecting config.
func statedHeld(stated, added, removed []string) []string {
	if len(stated) == 0 {
		return nil
	}
	changed := make(map[string]struct{}, len(added)+len(removed))
	for _, name := range added {
		changed[name] = struct{}{}
	}
	for _, name := range removed {
		changed[name] = struct{}{}
	}
	var held []string
	for _, name := range stated {
		if _, ok := changed[name]; !ok {
			held = append(held, name)
		}
	}
	return held
}

// onDeltaStreamClosed drops inflight responses for the closed stream; their
// ACKs will never arrive. Acknowledged state is kept: Envoy retains its config
// across stream reconnects. The record of which types the stream had been
// answered for goes too, with what its opening requests stated: stream IDs are
// never reused.
func (t *Tracker) onDeltaStreamClosed(streamID int64, _ *corev3.Node) {
	t.mu.Lock()
	for key := range t.inflight {
		if key.streamID == streamID {
			delete(t.inflight, key)
		}
	}
	for key := range t.answered {
		if key.streamID == streamID {
			delete(t.answered, key)
		}
	}
	for key := range t.stated {
		if key.streamID == streamID {
			delete(t.stated, key)
		}
	}
	t.mu.Unlock()
}

// broadcastLocked wakes all waiters. Callers must hold t.mu.
func (t *Tracker) broadcastLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}
