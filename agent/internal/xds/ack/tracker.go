// Package ack tracks Envoy's delta-xDS ACK/NACKs per resource, replacing admin
// /config_dump polling (which serializes config on Envoy's main thread) as the
// agent's confirmation that a config update reached the proxy.
//
// What a proxy holds is kept per xDS stream, one stream being one proxy
// process, and dropped with the stream (Tracker).
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
	"maps"
	"slices"
	"sync"

	commonlog "aethermesh.dev/common/log"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"go.opentelemetry.io/otel"
)

// rejection is a proxy's refusal of the last response that added a resource on
// one stream.
type rejection struct {
	err error
	// version is the version the rejected response carried the resource at.
	version string
}

// typeState is what the tracker keeps for one resource type on one open
// stream.
type typeState struct {
	// stated is the initial_resource_versions of the first request of the type
	// on the stream: every resource of the type the proxy holds, by the
	// version it accepted it at. Non-nil (possibly empty) from that request
	// until the first response of the type is written, which takes it over.
	stated map[string]string
	// answered is set once a response of the type has been written.
	answered bool

	// The rest is kept only for a type somebody can wait on (holdingsOf).

	// known is set when held is everything the proxy on this stream holds of
	// the type: from the moment the response to its statement is written.
	// Until then a name missing from held says nothing. It is never set on a
	// stream whose first response of the type answered no statement.
	known bool
	// held is the resources the proxy on this stream holds, by version: what
	// it stated, with every response it acknowledged since applied in the
	// order it acknowledged them.
	held map[string]string
	// rejected is the resources the last response to add which, on this
	// stream, was rejected. The next answered response that carries the
	// resource clears it or replaces it.
	rejected map[string]rejection
}

// stream is one open delta stream: one connection of one proxy process. A hot
// restart opens a second one while the first drains, and a proxy that
// reconnects opens a new one.
type stream struct {
	// openedFor is the one type of a stream opened for a single type, and
	// empty for an ADS stream, which carries the types it is asked for.
	openedFor string
	types     map[string]*typeState
	// inflight is the responses written to the stream and not answered, by
	// nonce. Nonces are unique per stream in go-control-plane.
	inflight map[string]inflightResponse
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
// wait until the connected proxies hold, or no longer hold, a named listener.
//
// What a proxy holds is kept per stream (#1624): a stream is one connection
// of one proxy process, a proxy answers the responses of a stream in the
// order they were written, and so each stream's account is the proxy's own
// history. Nothing is kept across streams, and nothing one stream says can
// change what another said. What the node holds is read off the open streams
// when a wait asks (holdersLocked).
type Tracker struct {
	log     *slog.Logger
	metrics *trackerMetrics // nil disables instrumentation

	mu sync.Mutex
	// streams is the open delta streams. Everything the tracker knows of a
	// proxy is in its stream and goes with it (onDeltaStreamClosed).
	streams map[int64]*stream
	// closedThrough is the highest ID of a stream that closed. go-control-plane
	// numbers streams upwards and announces each before anything else happens
	// on it, so a callback for a stream that was never announced and is not
	// above this mark is a straggler of a closed one and is dropped.
	closedThrough int64
	// changed is closed and replaced on every state transition (broadcast).
	changed chan struct{}
	// observer, when set, is told what the proxy accepted (SetAckObserver).
	observer AckObserver
	// deliveries, when set, is told of responses entering and leaving flight
	// (SetDeliveryObserver).
	deliveries DeliveryObserver
	// published, when set, makes "present" mean "at the published version"
	// (SetPublishedVersion).
	published PublishedVersion
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

// PublishedVersion returns the version of the named resource in the snapshot
// the agent currently publishes, and false when the snapshot does not have it.
// It is the per-resource version go-control-plane sends with the resource and
// a proxy states it holds the resource at.
//
// It is called from a waiter's goroutine with no tracker lock held.
type PublishedVersion func(typeURL, name string) (version string, ok bool)

// SnapshotVersions is a PublishedVersion that reads the snapshot a
// go-control-plane snapshot cache serves to nodeID: the same per-resource
// version map the cache's delta responses are computed from.
func SnapshotVersions(snapshots interface {
	GetSnapshot(nodeID string) (cachev3.ResourceSnapshot, error)
}, nodeID string,
) PublishedVersion {
	return func(typeURL, name string) (string, bool) {
		snapshot, err := snapshots.GetSnapshot(nodeID)
		if err != nil {
			return "", false
		}
		version := snapshot.GetVersionMap(typeURL)[name]
		return version, version != ""
	}
}

// SetPublishedVersion makes WaitListenerPresent a wait for a version: the
// listener is present when a proxy holds it at the version fn returns, not
// when it holds some listener of that name. A pod's listeners are named after
// the pod, so a same-named replacement publishes other content under a name
// the proxy already holds, and the name alone would resolve its wait before
// the proxy had been sent anything.
//
// Without fn every version of a name is the published one. Call it while
// wiring, before the xDS server serves.
func (t *Tracker) SetPublishedVersion(fn PublishedVersion) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.published = fn
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
		log:     log,
		metrics: metrics,
		streams: make(map[int64]*stream),
		changed: make(chan struct{}),
	}
}

// Callbacks returns the go-control-plane server callbacks feeding this tracker.
// Pass the result to serverv3.NewServer (the agent's proxy speaks delta ADS, so
// only the delta hooks are wired).
func (t *Tracker) Callbacks() serverv3.Callbacks {
	return serverv3.CallbackFuncs{
		DeltaStreamOpenFunc:     t.onDeltaStreamOpen,
		StreamDeltaResponseFunc: t.onDeltaResponse,
		StreamDeltaRequestFunc:  t.onDeltaRequest,
		DeltaStreamClosedFunc:   t.onDeltaStreamClosed,
	}
}

// WaitListenerPresent blocks until a connected proxy holds the named listener
// at the version the agent publishes (SetPublishedVersion), the context ends,
// or a connected proxy rejected that version (returned as the error).
//
// What a proxy holds is kept per stream, and a stream is one proxy process: a
// hot restart has two connected for a while. On each, the listener is held at
// the published version when that is the last version the proxy stated or
// acknowledged for the name and nothing that adds or removes the name is
// written to the stream and unanswered. So:
//
//   - a listener acknowledged at the published version returns immediately;
//
//   - so does one the proxy already holds at that version and that is
//     therefore never sent on its stream (an agent restart, a stream reset):
//     the proxy states it in its opening request (#1511);
//
//   - a listener whose published content changed since (a same-named
//     replacement pod) waits for the ACK of the response that carries the new
//     content, whatever the proxy acknowledged, stated or rejected for the
//     name before;
//
//   - a NACK fails the wait only when it is of the version published now.
//
// One proxy holding the listener answers the wait; any proxy rejecting it
// fails it. That is a choice and not a proof that the listener serves: the
// tracker cannot tell which generation of a hot restart will serve the pod.
// In particular a proxy that has stopped its listeners (the parent, once the
// child has taken over) acknowledges a listener it does not add, and that ACK
// answers the wait while the child has yet to acknowledge (measured,
// //agent/test/mtlspool,
// TestADrainingProxyAcknowledgesAListenerItDoesNotAdd). The data-plane proof
// stays the CNI plugin's in-netns probe.
//
// Like any ACK, that says the proxy accepted the listener, not that it has
// finished warming it. Publish before waiting: the comparison is with what is
// published when the wait looks.
//
// With no proxy that has asked for its listeners nothing is known and the wait
// runs to the caller's deadline. Callers treat it as best-effort.
func (t *Tracker) WaitListenerPresent(ctx context.Context, name string) error {
	return t.wait(ctx, resourcev3.ListenerType, name, true)
}

// WaitListenerAbsent blocks until every proxy that has asked for its listeners
// is known not to hold the named listener, or the context ends.
//
// Known, not assumed (#1572): a proxy is known not to hold a listener when it
// did not state it (or was sent its removal and answered since), nothing that
// adds or removes the name is unanswered on its stream, and the last answer
// about the name was not the rejection of an add. A proxy whose opening
// Listener request has not been answered is not known, and neither is the
// node when no proxy has asked for its listeners at all: after an agent
// restart the proxy holds every listener it had, and the agent learns that
// only when the proxy reconnects. The wait then runs until it does, or to the
// caller's deadline.
//
// Every such proxy, because each generation of a hot restart has its own
// sockets in the pod's network namespace. A proxy counts from its Listener
// request and not from the moment its stream opens: a new generation that is
// still loading its clusters holds no listener, and counting it would hold
// every pod DEL of a hot restart for the whole wait. The cost is the moment
// between a reconnecting proxy's stream opening and its Listener request,
// which is shorter than the time its stream was closed, when nothing was
// known of it either.
func (t *Tracker) WaitListenerAbsent(ctx context.Context, name string) error {
	return t.wait(ctx, resourcev3.ListenerType, name, false)
}

func (t *Tracker) wait(ctx context.Context, typeURL, name string, wantPresent bool) error {
	for {
		t.mu.Lock()
		holders := t.holdersLocked(typeURL, name)
		ch := t.changed
		published := t.published
		t.mu.Unlock()

		// The published version is read with no lock held. If the tracker
		// changed meanwhile, the two were never true together: look again.
		answered, rejectedBy := answer(holders, wantPresent, publishedAt(published, typeURL, name))
		if (answered || rejectedBy != nil) && isClosed(ch) {
			continue
		}
		if rejectedBy != nil {
			t.metrics.waitFailed(ctx, wantPresent, reasonNack)
			return fmt.Errorf("envoy rejected config for %s: %w", name, rejectedBy)
		}
		if answered {
			return nil
		}

		select {
		case <-ctx.Done():
			return t.expired(ctx, name, wantPresent, len(holders) == 0)
		case <-ch:
		}
	}
}

// expired counts and returns the failure of a wait whose context ended. A
// wait that ends with no proxy to ask is not a proxy that did not answer, and
// is counted apart (reasonNoProxy).
func (t *Tracker) expired(ctx context.Context, name string, wantPresent, noProxy bool) error {
	reason := reasonTimeout
	if noProxy {
		reason = reasonNoProxy
	}
	t.metrics.waitFailed(ctx, wantPresent, reason)
	if wantPresent {
		return fmt.Errorf("timed out waiting for envoy to ack %s", name)
	}
	return fmt.Errorf("timed out waiting for envoy to ack removal of %s", name)
}

// isClosed reports whether ch is closed, without waiting.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// holder is what one connected proxy says about one resource.
type holder struct {
	// known: the proxy's whole set of the type is known (typeState.known).
	known bool
	// held and version: the proxy holds the resource, at that version.
	held    bool
	version string
	// unanswered: a response that adds or removes the resource is written to
	// the stream and not answered. The proxy may have applied it.
	unanswered bool
	// rejected is the proxy's refusal of the last response that added the
	// resource, when that was its last answer about it.
	rejected *rejection
}

// wanted is the version a wait for a resource to be present is for.
type wanted struct {
	// any: no PublishedVersion is set, so every version is the published one.
	any bool
	// version is the published version; ok is false when the snapshot does
	// not have the resource, and then no proxy holds "the published version".
	version string
	ok      bool
}

func (w wanted) is(version string) bool {
	return w.any || (w.ok && w.version == version)
}

func publishedAt(published PublishedVersion, typeURL, name string) wanted {
	if published == nil {
		return wanted{any: true}
	}
	version, ok := published(typeURL, name)
	return wanted{version: version, ok: ok}
}

// answer decides a wait from what every connected proxy says: answered, or
// failed by the rejection it returns, or neither yet.
func answer(holders []holder, wantPresent bool, want wanted) (answered bool, rejectedBy error) {
	if wantPresent {
		return answerPresent(holders, want)
	}
	return answerAbsent(holders), nil
}

// answerPresent: one proxy holds the wanted version with nothing unanswered
// about the name, and none rejected that version.
func answerPresent(holders []holder, want wanted) (bool, error) {
	for _, h := range holders {
		if r := h.rejected; r != nil && want.is(r.version) {
			return false, r.err
		}
	}
	for _, h := range holders {
		if h.held && !h.unanswered && want.is(h.version) {
			return true, nil
		}
	}
	return false, nil
}

// answerAbsent: there is a proxy, and every one is known not to hold the name.
//
// A rejected add does not make the name absent: the proxy applies a rejected
// Listener response in part and keeps the listeners of it that it could build
// (//agent/test/mtlspool, TestRejectedListenerResponseIsAppliedInPart). The
// wait is for the removal, which go-control-plane sends because it counts a
// resource as delivered when it is written. No rejection fails this wait: a
// removal is done whatever the answer to the response that carried it
// (typeState.resolve).
func answerAbsent(holders []holder) bool {
	for _, h := range holders {
		if !h.known || h.held || h.unanswered || h.rejected != nil {
			return false
		}
	}
	return len(holders) > 0
}

// holdersLocked is what each connected proxy says about one resource, in
// stream order. A stream counts when it carries the type (stream.carries).
// Callers hold t.mu.
func (t *Tracker) holdersLocked(typeURL, name string) []holder {
	ids := make([]int64, 0, len(t.streams))
	for id, s := range t.streams {
		if s.carries(typeURL) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	holders := make([]holder, 0, len(ids))
	for _, id := range ids {
		s := t.streams[id]
		h := holder{unanswered: s.unanswered(typeURL, name)}
		if ts := s.types[typeURL]; ts != nil {
			h.known = ts.known
			h.version, h.held = ts.held[name]
			if r, ok := ts.rejected[name]; ok {
				h.rejected = &r
			}
		}
		holders = append(holders, h)
	}
	return holders
}

// carries reports whether resources of the type travel on the stream: the type
// a single-type stream was opened for, and on an ADS stream every type that
// has been asked for or sent. An ADS stream that has not asked for a type yet
// is not counted for it (WaitListenerAbsent says why).
func (s *stream) carries(typeURL string) bool {
	return (s.openedFor != "" && s.openedFor == typeURL) || s.types[typeURL] != nil
}

// unanswered reports whether a response that adds or removes the resource is
// written to the stream and not answered.
func (s *stream) unanswered(typeURL, name string) bool {
	for _, entry := range s.inflight {
		if entry.typeURL != typeURL {
			continue
		}
		if slices.ContainsFunc(entry.added, func(r Resource) bool { return r.Name == name }) || slices.Contains(entry.removed, name) {
			return true
		}
	}
	return false
}

// holdingsOf reports whether the tracker keeps what a proxy holds of a type.
// Only for the type somebody can wait on: a stream's clusters and endpoints
// would be kept for nobody.
func holdingsOf(typeURL string) bool {
	return typeURL == resourcev3.ListenerType
}

// streamLocked returns the open stream with that ID. A stream the tracker was
// not told the opening of is taken as an ADS stream opening now, unless its ID
// says it is a closed one (closedThrough): nil then. Callers hold t.mu.
func (t *Tracker) streamLocked(streamID int64) *stream {
	if s := t.streams[streamID]; s != nil {
		return s
	}
	if streamID <= t.closedThrough {
		return nil
	}
	return t.openLocked(streamID, "")
}

// openLocked adds a stream opened for typeURL, which is empty for ADS.
func (t *Tracker) openLocked(streamID int64, typeURL string) *stream {
	s := &stream{
		openedFor: typeURL,
		types:     make(map[string]*typeState),
		inflight:  make(map[string]inflightResponse),
	}
	t.streams[streamID] = s
	return s
}

// onDeltaStreamOpen records a proxy connecting. typeURL is empty for an ADS
// stream, which counts for a type from its first request of it; a stream
// opened for the Listener type alone counts from here, and until it is
// answered no removal wait is (WaitListenerAbsent).
func (t *Tracker) onDeltaStreamOpen(_ context.Context, streamID int64, typeURL string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.streams[streamID] == nil {
		t.openLocked(streamID, typeURL)
		t.broadcastLocked()
	}
	return nil
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
	s := t.streamLocked(streamID)
	if s == nil {
		t.mu.Unlock()
		return
	}
	kept, replaced := s.keep(resp.GetNonce(), &entry, empty)
	// A response in flight, and a type answered for the first time, change
	// what a wait may answer: one that is looking now must look again.
	t.broadcastLocked()
	deliveries := t.deliveries
	t.mu.Unlock()
	if replaced != nil {
		endDelivery(deliveries, *replaced)
	}
	if kept && deliveries != nil && len(entry.added) > 0 {
		deliveries(context.Background(), Delivery{TypeURL: entry.typeURL, Resources: entry.added})
	}
}

// keep files a written response under its nonce, with the statement it
// answers when it is the first of its type on its stream, and reports whether
// it was kept (a later empty one is not). replaced is the response it took the
// place of, which will now never be answered: go-control-plane's nonces are
// unique per stream, so there is none. Callers hold the tracker's lock.
func (s *stream) keep(nonce string, entry *inflightResponse, empty bool) (kept bool, replaced *inflightResponse) {
	ts := s.types[entry.typeURL]
	if ts == nil {
		ts = &typeState{}
		s.types[entry.typeURL] = ts
	}
	if !ts.answered {
		// The statement goes with the response that answers it, and is
		// released with it.
		entry.opening, entry.stated = ts.stated != nil, ts.stated
		ts.answered, ts.stated = true, nil
		if holdingsOf(entry.typeURL) {
			ts.opened(entry.opening, entry.stated)
		}
	}
	if empty && !entry.opening {
		return false, nil
	}
	if old, ok := s.inflight[nonce]; ok {
		replaced = &old
	}
	s.inflight[nonce] = *entry
	return true, replaced
}

// opened starts the account of what the proxy on this stream holds of the
// type, as the first response of the type is written: what the proxy stated,
// which is everything it holds (#1511). A proxy never states a version it
// rejected (//agent/test/mtlspool,
// TestReconnectingProxyStatesNoClusterItRejected).
//
// The account is complete from this moment and not from the statement: the
// response that answers the statement was computed from the snapshot of a
// moment ago and may add a name that has left the snapshot since. Written, it
// is in flight and holds a wait for that name; before, nothing would.
//
// With no statement (the tracker did not see the request the response
// answers) the account starts empty and is never complete.
func (ts *typeState) opened(stated bool, statement map[string]string) {
	ts.known = stated
	// The statement is the request's own, and is handed to the AckObserver.
	ts.held = maps.Clone(statement)
	if ts.held == nil {
		ts.held = map[string]string{}
	}
	ts.rejected = map[string]rejection{}
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
func (t *Tracker) noteOpeningRequestLocked(s *stream, req *discoveryv3.DeltaDiscoveryRequest) {
	if _, seen := s.types[req.GetTypeUrl()]; seen {
		return
	}
	stated := req.GetInitialResourceVersions()
	if stated == nil {
		stated = map[string]string{}
	}
	s.types[req.GetTypeUrl()] = &typeState{stated: stated}
	t.broadcastLocked()
}

// onDeltaRequest resolves an inflight response when the request echoes its
// nonce: without an error detail it is an ACK (resources accepted), with one it
// is a NACK (the error is recorded against each resource the response added;
// typeState.resolve).
// A request without a nonce answers nothing; the first of its type on the
// stream is the proxy's statement of what it holds (noteOpeningRequestLocked).
//
// The request answers a response of its own stream and its own type. A nonce
// of this stream echoed under another type answers nothing: it would write one
// type's resources as acknowledged on the word of a request about another.
func (t *Tracker) onDeltaRequest(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) error {
	nonce := req.GetResponseNonce()

	t.mu.Lock()
	s := t.streamLocked(streamID)
	if s == nil {
		t.mu.Unlock()
		return nil
	}
	if nonce == "" {
		t.noteOpeningRequestLocked(s, req)
		t.mu.Unlock()
		return nil
	}
	entry, ok := s.inflight[nonce]
	if !ok || entry.typeURL != req.GetTypeUrl() {
		t.mu.Unlock()
		return nil
	}
	delete(s.inflight, nonce)

	// nackErr is Envoy's error detail when the request is a NACK.
	var nackErr error
	if detail := req.GetErrorDetail(); detail != nil {
		nackErr = fmt.Errorf("%s", detail.GetMessage())
	}
	if ts := s.types[entry.typeURL]; ts != nil && holdingsOf(entry.typeURL) {
		ts.resolve(entry, nackErr)
	}
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

// resolve applies an answered response to what the proxy on this stream
// holds.
//
// What the response removed, the proxy no longer holds, whatever the answer:
// the proxy does every removal of a Listener response before any add, and
// rejects the response at the end for an add it could not build (measured,
// //agent/test/mtlspool, TestRemovalCarriedByARejectedResponseIsDone). A
// removed name is forgotten, not kept as absent (#1573).
//
// What the response added, the proxy holds at the version it carried when it
// acknowledged the response. When it rejected it (nackErr is non-nil) the
// rejection is kept against each added name, and what was held of the name is
// left as it was: the proxy may or may not have built it
// (TestRejectedListenerResponseIsAppliedInPart).
//
// A proxy answers the responses of a stream in the order they were written,
// so applying the answers in the order they arrive is the proxy's own history
// and needs no other ordering. Callers hold the tracker's lock.
func (ts *typeState) resolve(entry inflightResponse, nackErr error) {
	for _, name := range entry.removed {
		delete(ts.held, name)
		delete(ts.rejected, name)
	}
	for _, r := range entry.added {
		if nackErr != nil {
			ts.rejected[r.Name] = rejection{err: nackErr, version: r.Version}
			continue
		}
		ts.held[r.Name] = r.Version
		delete(ts.rejected, r.Name)
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

// onDeltaStreamClosed forgets the stream and everything known through it: the
// responses it left unanswered, whose ACKs will never arrive, and what its
// proxy stated, acknowledged and rejected (#1585). A proxy that reconnects
// says what it holds in its opening request; a new proxy process holds
// nothing and says so the same way. Until one does, nothing is known.
func (t *Tracker) onDeltaStreamClosed(streamID int64, _ *corev3.Node) {
	t.mu.Lock()
	var ended []inflightResponse
	if s := t.streams[streamID]; s != nil {
		for _, entry := range s.inflight {
			ended = append(ended, entry)
		}
		delete(t.streams, streamID)
	}
	t.closedThrough = max(t.closedThrough, streamID)
	// A wait held up by this stream alone can be answered now.
	t.broadcastLocked()
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
