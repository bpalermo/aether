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
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
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
	// nackVersion is the version of the resource in the rejected response
	// (empty for a rejected removal).
	nackVersion string
	// version is the version of the resource the proxy holds: the one the
	// acknowledged response carried, or the one the proxy stated. Empty when
	// the resource is not present.
	version string
	// seq is the tracker's sequence number when this was written (Tracker.seq).
	seq uint64
	// stream is the stream whose acknowledgement wrote present and version.
	stream int64
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
	// compared is set while the first response of the type is certain to be
	// the comparison of exactly stated with the snapshot, name by name, so
	// that a stated name it neither adds nor removes was stated at the
	// version the snapshot publishes (statedHeld, noteRequestLocked).
	compared bool
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
	// held is the resources the proxy stated it holds and this response, the
	// first of its type on its stream, neither added nor removed, with the
	// version it stated (#1511). Only an ACK reads it: see statedHeld.
	held map[string]string
	// sentSeq is the tracker's sequence number when the response was sent.
	// Anything written to a resource's state after it is newer than what this
	// response compared (acknowledgedLocked).
	sentSeq uint64
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

// PublishedVersion returns the version of the named resource in the snapshot
// the agent currently publishes, and false when the snapshot does not have it.
// It is the per-resource version go-control-plane sends with the resource and
// compares a proxy's stated version with.
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
	// seq counts the acknowledgements and rejections that wrote state. It
	// orders a write against the moment a response was sent.
	seq uint64
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

// SetPublishedVersion makes WaitListenerPresent version-aware: the listener is
// present when the proxy holds it at the version fn returns, not when it holds
// some version of that name. A pod's listeners are named after the pod, so a
// same-named replacement publishes other content under a name the proxy
// already holds, and the name alone would resolve its wait before the proxy
// has been sent anything.
//
// It is also what allows the tracker to act on what a proxy states it holds
// (statedHeld): a statement is about one version, and is worth nothing once
// another is published. Without fn the tracker keys presence by name and
// resolves no wait from a statement (the AckObserver is told it all the same).
//
// Call it while wiring, before the xDS server serves.
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
// With a PublishedVersion (SetPublishedVersion), which the node agent wires,
// the listener is present when the last thing the proxy acknowledged or stated
// for the name is the version the agent publishes now. So:
//
//   - a listener already ACKed at that version returns immediately;
//
//   - so does one Envoy already holds at that version and that is therefore
//     never sent on the current stream (an agent restart, a stream reset): the
//     proxy states it in its opening request and the ACK of the opening
//     response resolves it (statedHeld, #1511);
//
//   - a listener whose published content changed since (a same-named
//     replacement pod) waits for the ACK of the response that carries the new
//     content, whatever the proxy acknowledged, stated or rejected for the
//     name before: a NACK fails the wait only when it is of the version
//     published now (nackFails).
//
//   - a listener the stream that acknowledged it has since been sent again, or
//     sent the removal of, and has not answered, waits for that answer even
//     when the acknowledged version is the published one again: what was
//     acknowledged is not what was last sent (unansweredLocked).
//
// Like any ACK, that says the proxy accepted the listener, not that it has
// finished warming it. Publish before waiting: the comparison is with what is
// published when the wait looks.
//
// What it does not say, because the state is one per name and not one per
// proxy: during a hot restart the answer may be one generation's. An
// acknowledgement of either generation resolves the wait, and a rejection by
// either fails it, until an acknowledgement of a response sent after that
// rejection (acknowledgedLocked, rejectedLocked).
//
// A wait can still run to the caller's deadline with the listener in place:
// when no proxy is connected, or when the proxy rejected the opening response
// because of another resource. Callers treat the wait as best-effort, exactly
// like the admin config_dump poll this replaces.
//
// Without a PublishedVersion presence is keyed by the name alone: any earlier
// ACK of the name returns immediately, and no statement resolves a wait.
func (t *Tracker) WaitListenerPresent(ctx context.Context, name string) error {
	return t.wait(ctx, resourcev3.ListenerType, name, true)
}

// WaitListenerAbsent blocks until Envoy has ACKed the removal of the named
// listener (or it was never known to be present), the context ends, or Envoy
// NACKs the removal.
//
// "Never known to be present" is read as absent, and after an agent restart
// that is every listener until a proxy's opening Listener exchange has been
// acknowledged (statedHeld, #1511). A removal waited for before that moment,
// while no proxy is connected or between its opening request and its ACK of
// the opening response, returns at once although the proxy may hold the
// listener. Like the rest of this wait it is best-effort.
func (t *Tracker) WaitListenerAbsent(ctx context.Context, name string) error {
	return t.wait(ctx, resourcev3.ListenerType, name, false)
}

func (t *Tracker) wait(ctx context.Context, typeURL, name string, wantPresent bool) error {
	key := typeURL + "/" + name
	for {
		t.mu.Lock()
		st := t.state[key]
		ch := t.changed
		published := t.published
		unanswered := t.unansweredLocked(st.stream, typeURL, name)
		t.mu.Unlock()

		if st.nackFails(wantPresent, published, typeURL, name) {
			t.metrics.waitFailed(ctx, wantPresent, reasonNack)
			return fmt.Errorf("envoy rejected config for %s: %w", name, st.nackErr)
		}
		if !wantPresent && !st.present {
			return nil
		}
		if wantPresent && st.present && !unanswered && atPublishedVersion(published, typeURL, name, st.version) {
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

// unansweredLocked reports whether the stream has been sent a response that
// adds or removes the resource and has not answered it. Callers must hold t.mu.
//
// The state is what a proxy last ACKNOWLEDGED, and the server may have sent it
// something else since. While it has, the acknowledged version is not known to
// be what the proxy holds, even when it is the published one again (the agent
// went back to it): the proxy may have applied the newer one, and the server
// has yet to send the published one again. Only the stream that wrote the
// state is looked at: another proxy generation's unanswered response says
// nothing about what this one holds.
func (t *Tracker) unansweredLocked(streamID int64, typeURL, name string) bool {
	for key, entry := range t.inflight {
		if key.streamID != streamID || entry.typeURL != typeURL {
			continue
		}
		if slices.ContainsFunc(entry.added, func(r Resource) bool { return r.Name == name }) || slices.Contains(entry.removed, name) {
			return true
		}
	}
	return false
}

// nackFails reports whether the recorded rejection fails a wait. A rejection
// is of the version the proxy was sent: with a PublishedVersion it fails a
// wait for the resource to be present only when that is the version published
// now, so neither the rejection of a same-named predecessor nor a rejected
// removal fails the wait of a replacement. The removal wait, and a tracker
// with no PublishedVersion, are failed by any rejection, as they always were.
func (st resourceState) nackFails(wantPresent bool, published PublishedVersion, typeURL, name string) bool {
	if st.nackErr == nil {
		return false
	}
	if !wantPresent {
		return true
	}
	// With no PublishedVersion every version is the published one.
	return atPublishedVersion(published, typeURL, name, st.nackVersion)
}

// atPublishedVersion reports whether held is the version published for the
// resource. With no PublishedVersion every version is.
func atPublishedVersion(published PublishedVersion, typeURL, name, held string) bool {
	if published == nil {
		return true
	}
	version, ok := published(typeURL, name)
	return ok && version == held
}

// onDeltaResponse records the resources carried by an outgoing delta response
// under its nonce, so the eventual ACK/NACK can be attributed to them.
//
// The FIRST response of a type on a stream is the server's answer to the
// proxy's opening request, whose initial_resource_versions state every
// resource of the type the proxy holds (noteRequestLocked). It is kept
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
		// Without a published version to hold it to, a statement resolves
		// no wait (SetPublishedVersion).
		if st.compared && t.published != nil {
			entry.held = statedHeld(st.stated, entry.added, entry.removed)
		}
		st.answered, st.stated = true, nil
	}
	entry.sentSeq = t.seq
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

// noteRequestLocked keeps the statement of the first request of a type on a
// stream: its initial_resource_versions, the version of every resource of the
// type the proxy holds. Only the first request of a type carries one (the xDS
// protocol), and it has no nonce; a later request without a nonce (an
// on-demand subscription) states nothing and is not read as one. Callers hold
// t.mu.
//
// The map is the request's own: go-control-plane hands each request to the
// callbacks as a fresh message.
//
// The statement is kept for the AckObserver whatever the request subscribes
// to. Whether the first response can also be read name by name against it
// (compared, for statedHeld) is narrower, and mirrors go-control-plane, which
// seeds a type's subscription from the first request of the type on the
// stream and from no other:
//
//   - it opens a wildcard subscription and unsubscribes from nothing
//     (opensWildcard);
//   - no other request of the type arrives before the first response. A second
//     one can change the subscription the response is computed for.
//
// A request that carries a nonce before anything was seen of its type on the
// stream is not kept at all, as nothing is kept for a request that arrives
// for a stream already closed. It cannot come from a proxy that follows the
// protocol: a nonce echoes a response of the same stream.
func (t *Tracker) noteRequestLocked(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) {
	pair := streamType{streamID: streamID, typeURL: req.GetTypeUrl()}
	st, seen := t.streams[pair]
	switch {
	case seen && !st.answered:
		st.compared = false
	case seen || req.GetResponseNonce() != "":
	default:
		stated := req.GetInitialResourceVersions()
		if stated == nil {
			stated = map[string]string{}
		}
		t.streams[pair] = &streamTypeState{stated: stated, compared: opensWildcard(req)}
	}
}

// wildcard is the resource name that subscribes to every resource of a type.
const wildcard = "*"

// opensWildcard reports whether a first request subscribes to every resource
// of its type and to nothing else: it names no resource (the legacy form) or
// names "*", and unsubscribes from nothing.
func opensWildcard(req *discoveryv3.DeltaDiscoveryRequest) bool {
	if len(req.GetResourceNamesUnsubscribe()) > 0 {
		return false
	}
	subscribed := req.GetResourceNamesSubscribe()
	return len(subscribed) == 0 || slices.Contains(subscribed, wildcard)
}

// statedHeld returns, with the version stated, the stated names that the first
// response of their type on their stream neither added nor removed (#1511).
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
//
// The comparison is with the snapshot of the moment the response was
// computed, and the snapshot moves on: by the ACK, or any time after it, the
// name may publish other content. So the stated VERSION is what is recorded,
// and a wait compares it with what is published when it looks
// (atPublishedVersion). The name alone would resolve the wait of a same-named
// replacement before the proxy had been sent it.
func statedHeld(stated map[string]string, added []Resource, removed []string) map[string]string {
	if len(stated) == 0 {
		return nil
	}
	changed := make(map[string]struct{}, len(added)+len(removed))
	for _, r := range added {
		changed[r.Name] = struct{}{}
	}
	for _, name := range removed {
		changed[name] = struct{}{}
	}
	held := make(map[string]string, len(stated))
	for name, version := range stated {
		if _, ok := changed[name]; !ok {
			held[name] = version
		}
	}
	return held
}

// onDeltaRequest resolves an inflight response when the request echoes its
// nonce: without an error detail it is an ACK (resources accepted), with one it
// is a NACK (whole response rejected, error recorded against each resource).
// A request without a nonce answers nothing; the first of its type on the
// stream is the proxy's statement of what it holds (noteRequestLocked).
func (t *Tracker) onDeltaRequest(streamID int64, req *discoveryv3.DeltaDiscoveryRequest) error {
	nonce := req.GetResponseNonce()

	t.mu.Lock()
	t.noteRequestLocked(streamID, req)
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

	// nackErr is Envoy's error detail when the request is a NACK.
	var nackErr error
	if detail := req.GetErrorDetail(); detail != nil {
		nackErr = fmt.Errorf("%s", detail.GetMessage())
	}
	t.resolveLocked(streamID, entry, nackErr)
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
func (t *Tracker) resolveLocked(streamID int64, entry inflightResponse, nackErr error) {
	t.seq++
	if nackErr != nil {
		t.rejectedLocked(entry, nackErr)
		return
	}
	t.acknowledgedLocked(streamID, entry)
}

// rejectedLocked records a NACK against every resource of the response.
// Callers must hold t.mu and have advanced t.seq.
//
// Unlike an acknowledgement (acknowledgedLocked) a rejection is recorded
// whatever was acknowledged since its response was sent. One rejection is kept
// per name, though, and an older one does not replace a newer one: the newer
// is of the later version, the one a wait is more likely for.
func (t *Tracker) rejectedLocked(entry inflightResponse, nackErr error) {
	reject := func(name, version string) {
		key := entry.typeURL + "/" + name
		st := t.state[key]
		if st.nackErr != nil && st.seq > entry.sentSeq {
			return
		}
		st.nackErr, st.nackVersion, st.seq = nackErr, version, t.seq
		t.state[key] = st
	}
	for _, r := range entry.added {
		reject(r.Name, r.Version)
	}
	for _, name := range entry.removed {
		reject(name, "")
	}
}

// acknowledgedLocked records an ACK: what the response added and removed, and
// what the proxy had stated it holds (statedHeld). Callers must hold t.mu and
// have advanced t.seq.
//
// The state is one per name, and two proxy generations write it during a hot
// restart. An ACK is the answer to a response computed when it was SENT, so
// it is older than anything written to the name since: it is recorded only
// when nothing was. Otherwise a draining generation's late ACK of an older
// version, or of an add, would replace what the generation taking over has
// acknowledged since (a newer version, or the removal), and nothing would
// ever correct it: that generation is sent nothing more for the name.
//
// On one stream the rule never bites: go-control-plane has at most one
// response of a type outstanding per stream, so nothing is written to a name
// between a response and its answer except by another stream.
func (t *Tracker) acknowledgedLocked(streamID int64, entry inflightResponse) {
	record := func(name string, st resourceState) {
		key := entry.typeURL + "/" + name
		if t.state[key].seq > entry.sentSeq {
			return
		}
		st.seq, st.stream = t.seq, streamID
		t.state[key] = st
	}
	for _, r := range entry.added {
		record(r.Name, resourceState{present: true, version: r.Version})
	}
	for _, name := range entry.removed {
		record(name, resourceState{})
	}
	// A statement, besides, never clears a rejection of the very version it
	// states, whenever that was: one generation holding it does not make the
	// other accept it.
	for name, version := range entry.held {
		if st := t.state[entry.typeURL+"/"+name]; st.nackErr != nil && st.nackVersion == version {
			continue
		}
		record(name, resourceState{present: true, version: version})
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

// onDeltaStreamClosed drops inflight responses for the closed stream; their
// ACKs will never arrive. Acknowledged state is kept: Envoy retains its config
// across stream reconnects. What is kept per type of the stream goes too (a
// statement not yet answered included): stream IDs are never reused.
func (t *Tracker) onDeltaStreamClosed(streamID int64, _ *corev3.Node) {
	t.mu.Lock()
	var ended []inflightResponse
	for key, entry := range t.inflight {
		if key.streamID == streamID {
			t.unansweredForgottenLocked(streamID, entry)
			ended = append(ended, entry)
			delete(t.inflight, key)
		}
	}
	for key := range t.streams {
		if key.streamID == streamID {
			delete(t.streams, key)
		}
	}
	// A wait held up only by a response this stream left unanswered can be
	// answered now (unansweredLocked).
	t.broadcastLocked()
	deliveries := t.deliveries
	t.mu.Unlock()
	for _, entry := range ended {
		endDelivery(deliveries, entry)
	}
}

// unansweredForgottenLocked is called for a response its stream closed
// without answering. The proxy may or may not have applied it, so for every
// resource it carried whose state this stream wrote, the version the proxy
// holds is no longer known: it is still present (a removal waits for its
// acknowledgement), at no version (no wait for it to be present is answered).
// The proxy says which it holds when it reconnects. Callers must hold t.mu.
func (t *Tracker) unansweredForgottenLocked(streamID int64, entry inflightResponse) {
	forget := func(name, sent string) {
		key := entry.typeURL + "/" + name
		st := t.state[key]
		// Sent the version it had acknowledged: it holds that one either way.
		if !st.present || st.stream != streamID || st.version == sent {
			return
		}
		st.version = ""
		t.state[key] = st
	}
	for _, r := range entry.added {
		forget(r.Name, r.Version)
	}
	for _, name := range entry.removed {
		forget(name, "")
	}
}

// broadcastLocked wakes all waiters. Callers must hold t.mu.
func (t *Tracker) broadcastLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}
