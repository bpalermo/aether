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

// streamTypeState is what the tracker keeps for one resource type on one open
// stream.
type streamTypeState struct {
	// stated is the initial_resource_versions of the first request of the type
	// on the stream: every resource of the type the proxy holds, by the
	// version it accepted it at. Non-nil (possibly empty) from that request
	// until the first response of the type is written, which takes it over.
	stated map[string]string
	// answered is set once a response of the type has been written.
	answered bool
}

// Resource is one resource a delta response carried: its name and the
// per-resource version it was sent at. For go-control-plane that version is a
// hash of the resource's bytes.
type Resource struct {
	Name    string
	Version string
}

// inflightResponse records which resources a delta response added/removed, so
// the matching ACK/NACK (a request echoing the nonce) can be attributed.
type inflightResponse struct {
	typeURL string
	// systemVersion is the response's system_version_info: the version of the
	// snapshot the response was built from. The ACK echoes only the nonce, so
	// it is kept here for the AckObserver.
	systemVersion string
	added         []Resource
	removed       []string
	// opening is set on the first response of its type on its stream when the
	// tracker saw the request it answers; stated is that request's statement.
	opening bool
	stated  map[string]string
}

// Accepted is what one answered delta response says about the resources of
// one type a proxy holds. It never says more than the proxy accepted (#1508).
//
// An acknowledged response says exactly this: the proxy accepted the resources
// the response carried, at the versions it carried them at (Added), and
// dropped the ones it removed (Removed). It says NOTHING about any other
// resource of the snapshot the response was built from. go-control-plane
// treats a resource as delivered the moment it is written, so a resource the
// proxy rejected is not in the next response, and the ACK of that response is
// not an ACK of it.
//
// The answer to the FIRST response of a type on a stream says more, because
// the request that response answers carries the proxy's own statement of every
// resource of the type it holds (initial_resource_versions; a proxy never
// states a version it rejected). Opening is set on it and Stated is that
// statement. Acknowledged, the proxy holds Stated, less Removed, with Added
// over it: its complete set. Rejected (Rejected is set, Added and Removed are
// empty), it holds Stated and nothing else.
//
// Stated, Added and Removed are the tracker's: read them, do not keep or
// change them.
type Accepted struct {
	TypeURL string
	// SystemVersion is the version of the snapshot the response was built
	// from. It names a snapshot the proxy was answering about, not one it
	// holds.
	SystemVersion string
	Opening       bool
	Rejected      bool
	Stated        map[string]string
	Added         []Resource
	Removed       []string
}

// Delivery is the resources of one delta response entering or leaving flight.
// A response is in flight from the moment it is written to its stream until
// the proxy answers it (ACK or NACK) or the stream ends. Ended is false when
// it is written and true when it leaves; every Delivery that is not Ended is
// followed by exactly one that is, with the same Resources.
//
// It exists so that whoever interprets an ACK can keep what it needs to know
// about a version for exactly as long as an ACK of that version can still
// arrive, however long the proxy takes (#1508). An acknowledged response is
// told to the AckObserver first and leaves flight after.
//
// Resources is the tracker's: read it, do not keep or change it.
type Delivery struct {
	TypeURL   string
	Resources []Resource
	Ended     bool
}

// DeliveryObserver is told of every Delivery. It runs on the xDS stream's
// goroutine, after the tracker's own lock is released: it must not block.
type DeliveryObserver func(ctx context.Context, delivery Delivery)

// AckObserver is told what an answered delta response says a proxy holds: once
// per acknowledged response that carried something, and once for the answer,
// ACK or NACK, to the first response of a type on a stream. A later response
// the proxy rejects is not told: it leaves the proxy on what it had.
//
// It runs on the xDS stream's goroutine, after the tracker's own lock is
// released: it must not block.
type AckObserver func(ctx context.Context, accepted Accepted)

// Tracker observes the delta-xDS streams via server callbacks and lets callers
// wait until Envoy has acknowledged the presence or removal of a named resource.
type Tracker struct {
	log     *slog.Logger
	metrics *trackerMetrics // nil disables instrumentation

	mu       sync.Mutex
	state    map[string]resourceState // keyed by typeURL + "/" + name
	inflight map[inflightKey]inflightResponse
	// streams is the (stream, type) pairs a request or a response has been
	// seen for. The first response of a pair is the one computed against what
	// the proxy stated it holds in the pair's first request (onDeltaResponse).
	streams map[streamType]*streamTypeState
	// changed is closed and replaced on every state transition (broadcast).
	changed chan struct{}
	// observer, when set, is told what the proxy accepted (SetAckObserver).
	observer AckObserver
	// deliveries, when set, is told of responses entering and leaving flight
	// (SetDeliveryObserver).
	deliveries DeliveryObserver
}

// SetAckObserver registers fn to be told what each answered delta response
// says the proxy holds (Accepted).
// One observer; a second call replaces the first, nil removes it. Call it
// while wiring, before the xDS server serves: it may also be called later, but
// an ACK being processed at that moment may go to either observer.
func (t *Tracker) SetAckObserver(fn AckObserver) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.observer = fn
}

// SetDeliveryObserver registers fn to be told of every response that carries a
// resource entering and leaving flight (Delivery). One observer; nil removes
// it. Call it while wiring, before the xDS server serves: a response already
// in flight when it is set is reported only as leaving.
func (t *Tracker) SetDeliveryObserver(fn DeliveryObserver) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deliveries = fn
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
		streams:  make(map[streamType]*streamTypeState),
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
// The FIRST response of a type on a stream is the server's answer to the
// proxy's opening request, whose initial_resource_versions state every
// resource of the type the proxy holds (noteOpeningRequestLocked). It is kept
// even when it carries nothing (#1483): go-control-plane answers the first
// wildcard request of a stream whether or not anything is owed, and after an
// agent restart against a proxy that is already in sync the answer to that
// empty response is the only word from the proxy until a resource changes.
// With the statement it is a complete account of what the proxy holds
// (Accepted).
//
// A LATER empty response is dropped. It adds nothing and removes nothing, so
// its ACK says nothing about any resource, whatever snapshot it names:
// go-control-plane answers every wildcard request that carries no nonce (an
// on-demand subscription in the middle of a stream is one), and after a
// rejected update that answer is empty and names the rejected snapshot.
//
// Measured on the pinned proxy by //agent/test/mtlspool
// (TestReconnectingProxyStatesTheClustersItHolds,
// TestReconnectingProxyStatesNoClusterItRejected,
// TestAckAfterARejectedClusterUpdateIsNotAnAckOfTheSnapshot).
func (t *Tracker) onDeltaResponse(streamID int64, _ *discoveryv3.DeltaDiscoveryRequest, resp *discoveryv3.DeltaDiscoveryResponse) {
	if resp.GetNonce() == "" {
		return
	}
	entry := inflightResponse{
		typeURL:       resp.GetTypeUrl(),
		systemVersion: resp.GetSystemVersionInfo(),
		removed:       resp.GetRemovedResources(),
	}
	if resources := resp.GetResources(); len(resources) > 0 {
		entry.added = make([]Resource, 0, len(resources))
		for _, r := range resources {
			entry.added = append(entry.added, Resource{Name: r.GetName(), Version: r.GetVersion()})
		}
	}
	empty := len(entry.added) == 0 && len(entry.removed) == 0

	t.mu.Lock()
	kept, replaced := t.keepLocked(streamID, resp.GetNonce(), &entry, empty)
	deliveries := t.deliveries
	t.mu.Unlock()
	if replaced != nil {
		endDelivery(deliveries, *replaced)
	}
	if kept && deliveries != nil && len(entry.added) > 0 {
		deliveries(context.Background(), Delivery{TypeURL: entry.typeURL, Resources: entry.added})
	}
}

// keepLocked files a written response under its nonce, with the statement it
// answers when it is the first of its type on its stream, and reports whether
// it was kept (a later empty one is not). replaced is the response it took the
// place of, which will now never be answered: go-control-plane's nonces are
// unique per stream, so there is none. Callers hold t.mu.
func (t *Tracker) keepLocked(streamID int64, nonce string, entry *inflightResponse, empty bool) (kept bool, replaced *inflightResponse) {
	pair := streamType{streamID: streamID, typeURL: entry.typeURL}
	st := t.streams[pair]
	if st == nil {
		st = &streamTypeState{}
		t.streams[pair] = st
	}
	if !st.answered {
		// The statement goes with the response that answers it, and is
		// released with it.
		entry.opening, entry.stated = st.stated != nil, st.stated
		st.answered, st.stated = true, nil
	}
	if empty && !entry.opening {
		return false, nil
	}
	key := inflightKey{streamID: streamID, nonce: nonce}
	if old, ok := t.inflight[key]; ok {
		replaced = &old
	}
	t.inflight[key] = *entry
	return true, replaced
}

// noteOpeningRequestLocked keeps the statement of the first request of a type
// on a stream: its initial_resource_versions, the version of every resource of
// the type the proxy holds. Only the first request of a type carries one (the
// xDS protocol), and it has no nonce; a later request without a nonce (an
// on-demand subscription) states nothing and is not read as one. Callers hold
// t.mu.
//
// The map is the request's own: go-control-plane hands each request to the
// callbacks as a fresh message and does not change it afterwards.
func (t *Tracker) noteOpeningRequestLocked(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) {
	pair := streamType{streamID: streamID, typeURL: req.GetTypeUrl()}
	if _, seen := t.streams[pair]; seen {
		return
	}
	stated := req.GetInitialResourceVersions()
	if stated == nil {
		stated = map[string]string{}
	}
	t.streams[pair] = &streamTypeState{stated: stated}
}

// onDeltaRequest resolves an inflight response when the request echoes its
// nonce: without an error detail it is an ACK (resources accepted), with one it
// is a NACK (whole response rejected, error recorded against each resource).
// A request without a nonce answers nothing; the first of its type on the
// stream is the proxy's statement of what it holds (noteOpeningRequestLocked).
func (t *Tracker) onDeltaRequest(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) error {
	nonce := req.GetResponseNonce()
	if nonce == "" {
		t.mu.Lock()
		t.noteOpeningRequestLocked(streamID, req)
		t.mu.Unlock()
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

	// nackErr is Envoy's error detail when the request is a NACK.
	var nackErr error
	if detail := req.GetErrorDetail(); detail != nil {
		nackErr = fmt.Errorf("%s", detail.GetMessage())
	}
	t.resolveLocked(entry, nackErr)
	t.broadcastLocked()
	observer, deliveries := t.observer, t.deliveries
	t.mu.Unlock()

	accepted, told := t.answered(entry, nackErr)
	if told && observer != nil {
		observer(context.Background(), accepted)
	}
	// After the AckObserver: what was kept for the versions in flight is
	// there for it to read the ACK with.
	endDelivery(deliveries, entry)
	return nil
}

// endDelivery tells the DeliveryObserver that a response left flight.
func endDelivery(deliveries DeliveryObserver, entry inflightResponse) {
	if deliveries != nil && len(entry.added) > 0 {
		deliveries(context.Background(), Delivery{TypeURL: entry.typeURL, Resources: entry.added, Ended: true})
	}
}

// resolveLocked records an answered response against each resource it carried:
// present or absent on an ACK, Envoy's error on a NACK (nackErr is non-nil).
// Callers hold t.mu.
func (t *Tracker) resolveLocked(entry inflightResponse, nackErr error) {
	if nackErr != nil {
		for _, r := range entry.added {
			t.nackLocked(entry.typeURL, r.Name, nackErr)
		}
		for _, name := range entry.removed {
			t.nackLocked(entry.typeURL, name, nackErr)
		}
		return
	}
	for _, r := range entry.added {
		t.state[entry.typeURL+"/"+r.Name] = resourceState{present: true}
	}
	for _, name := range entry.removed {
		t.state[entry.typeURL+"/"+name] = resourceState{present: false}
	}
}

// answered is what an answered response says the proxy holds, and whether
// there is anything to tell an AckObserver. A NACK (nackErr is non-nil) is
// counted and logged here; it is told only when it answers an opening
// response, whose statement stands whatever the answer.
func (t *Tracker) answered(entry inflightResponse, nackErr error) (Accepted, bool) {
	accepted := Accepted{
		TypeURL:       entry.typeURL,
		SystemVersion: entry.systemVersion,
		Opening:       entry.opening,
		Stated:        entry.stated,
	}
	if nackErr == nil {
		accepted.Added, accepted.Removed = entry.added, entry.removed
		return accepted, true
	}
	t.metrics.nacked(context.Background(), entry.typeURL)
	names := make([]string, 0, len(entry.added))
	for _, r := range entry.added {
		names = append(names, r.Name)
	}
	t.log.Info("envoy NACKed delta response",
		"typeURL", entry.typeURL, "added", names, "removed", entry.removed, "error", nackErr.Error())
	// A later response the proxy rejects leaves it on what it had, and
	// nothing here says what that is.
	accepted.Rejected = true
	return accepted, entry.opening
}

// nackLocked records Envoy's error against one resource of a rejected
// response. Callers hold t.mu.
func (t *Tracker) nackLocked(typeURL, name string, nackErr error) {
	st := t.state[typeURL+"/"+name]
	st.nackErr = nackErr
	t.state[typeURL+"/"+name] = st
}

// onDeltaStreamClosed drops inflight responses for the closed stream; their
// ACKs will never arrive. Acknowledged state is kept: Envoy retains its config
// across stream reconnects. What is kept per type of the stream goes too (a
// statement not yet answered included): stream IDs are never reused.
func (t *Tracker) onDeltaStreamClosed(streamID int64, _ *corev3.Node) {
	t.mu.Lock()
	var ended []inflightResponse
	for key, entry := range t.inflight {
		if key.streamID == streamID {
			ended = append(ended, entry)
			delete(t.inflight, key)
		}
	}
	for key := range t.streams {
		if key.streamID == streamID {
			delete(t.streams, key)
		}
	}
	deliveries := t.deliveries
	t.mu.Unlock()
	for _, entry := range ended {
		endDelivery(deliveries, entry)
	}
}

// broadcastLocked wakes all waiters. Callers must hold t.mu.
func (t *Tracker) broadcastLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}
