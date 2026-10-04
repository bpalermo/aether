package server

import (
	"context"
	"log/slog"
	"sync"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	commonlog "aethermesh.dev/common/log"
	"google.golang.org/protobuf/proto"
)

const (
	// defaultChannelBuffer is the buffer size for per-watcher event channels.
	defaultChannelBuffer = 256
)

// watcher is one subscribed agent watch stream: its event channel plus the
// service filter the stream asked for (nil = full watch).
type watcher struct {
	ch chan *registrarv1.WatchEndpointsResponse
	// services is the watch filter; nil means full watch (every service).
	// A non-nil empty set watches nothing.
	services map[string]struct{}
}

// Broadcaster fans out endpoint events to connected agent watch streams.
// Each watcher is identified by a string key (typically cluster/node) and
// receives events on a buffered channel. Watchers carrying a service filter
// are indexed by service, so an endpoint change fans out to that service's
// consumers only (demand-scoped distribution) instead of every node.
// A watcher too slow to keep up is forcibly resynced: its channel is closed,
// ending its WatchEndpoints stream, and the agent's reconnect receives a
// fresh (filtered) snapshot — it must never silently miss an event and serve
// stale endpoints until something else triggers a resync.
type Broadcaster struct {
	// publishMu orders publications against watch starts (#1205). A
	// publication -- a snapshot mutation and the broadcast of the batch it
	// produced -- holds it for reading, so publications still run concurrently
	// with each other. A watch start -- Subscribe plus the snapshot read --
	// holds it for writing. Every batch is therefore either wholly before a
	// watch start (in the snapshot it reads, and broadcast before the watcher
	// existed) or wholly after it (absent from that snapshot, and buffered on
	// the watcher's channel): never in the snapshot AND on the channel, whose
	// batch version would then name contents older than the snapshot's.
	// Acquired before mu, never while holding it.
	publishMu sync.RWMutex

	mu       sync.RWMutex
	watchers map[string]*watcher
	// byService indexes filtered watchers by service name for O(consumers)
	// fan-out; fullWatchers holds the watchers with no filter.
	byService    map[string]map[string]*watcher
	fullWatchers map[string]*watcher
	log          *slog.Logger
	metrics      *Metrics
}

// NewBroadcaster creates a Broadcaster. metrics may be nil to disable
// instrumentation.
func NewBroadcaster(log *slog.Logger, metrics *Metrics) *Broadcaster {
	return &Broadcaster{
		watchers:     make(map[string]*watcher),
		byService:    make(map[string]map[string]*watcher),
		fullWatchers: make(map[string]*watcher),
		log:          commonlog.Named(log, "broadcaster"),
		metrics:      metrics,
	}
}

// Subscribe registers a new watcher scoped to the given services (nil =
// full watch; empty = watch nothing) and returns a channel that will receive
// endpoint events. The caller must call Unsubscribe when done.
func (b *Broadcaster) Subscribe(id string, services []string) <-chan *registrarv1.WatchEndpointsResponse {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Close any existing channel for this ID (reconnection scenario).
	existing, replaced := b.watchers[id]
	if replaced {
		b.removeFromIndexLocked(id, existing)
		close(existing.ch)
	}

	w := &watcher{ch: make(chan *registrarv1.WatchEndpointsResponse, defaultChannelBuffer)}
	if services != nil {
		w.services = make(map[string]struct{}, len(services))
		for _, s := range services {
			w.services[s] = struct{}{}
		}
	}
	b.watchers[id] = w
	b.addToIndexLocked(id, w)
	if !replaced {
		// A reconnect replaces the channel but keeps the watcher count.
		b.metrics.watcherSubscribed(context.Background())
	}
	b.metrics.filteredWatchers(context.Background(), b.filteredCountLocked())
	b.log.Debug("watcher subscribed", "id", id, "filtered", w.services != nil, "services", len(services))
	return w.ch
}

// SubscribeWith subscribes like Subscribe and then runs start (the watch's
// snapshot read), both with every publication excluded (see publishMu): the
// returned channel receives exactly the batches that start's snapshot does not
// contain. start must only read in-memory state -- the snapshot is sent after
// SubscribeWith returns, while new batches buffer on the channel -- and must not
// publish. A buffer that overflows while the snapshot is still being sent closes
// the channel, which ends the stream with DataLoss as for any slow watcher.
func (b *Broadcaster) SubscribeWith(id string, services []string, start func()) <-chan *registrarv1.WatchEndpointsResponse {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	ch := b.Subscribe(id, services)
	start()
	return ch
}

// Publish runs mutate -- which applies a batch to the snapshot and returns the
// batch's events, version-stamped -- and broadcasts the events, as one
// publication with respect to SubscribeWith (see publishMu). Every snapshot
// mutation whose events are broadcast must go through Publish, or a watch
// starting between the mutation and its broadcast receives the batch twice:
// once in its snapshot and once, carrying an older version, after it.
func (b *Broadcaster) Publish(mutate func() []*registrarv1.WatchEndpointsResponse) {
	b.publishMu.RLock()
	defer b.publishMu.RUnlock()
	if events := mutate(); len(events) > 0 {
		b.Broadcast(events)
	}
}

// Unsubscribe removes a watcher and closes its channel. The channel returned
// by the matching Subscribe call must be passed so that a stale caller (whose
// subscription was already replaced by a reconnect with the same id) does not
// close or delete the newer subscription's channel.
func (b *Broadcaster) Unsubscribe(id string, ch <-chan *registrarv1.WatchEndpointsResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()

	existing, exists := b.watchers[id]
	if !exists || (<-chan *registrarv1.WatchEndpointsResponse)(existing.ch) != ch {
		return
	}

	b.removeFromIndexLocked(id, existing)
	close(existing.ch)
	delete(b.watchers, id)
	b.metrics.watcherUnsubscribed(context.Background())
	b.metrics.filteredWatchers(context.Background(), b.filteredCountLocked())
	b.log.Debug("watcher unsubscribed", "id", id)
}

// addToIndexLocked inserts the watcher into the fan-out index. Caller must
// hold mu.
func (b *Broadcaster) addToIndexLocked(id string, w *watcher) {
	if w.services == nil {
		b.fullWatchers[id] = w
		return
	}
	for svc := range w.services {
		m, ok := b.byService[svc]
		if !ok {
			m = make(map[string]*watcher)
			b.byService[svc] = m
		}
		m[id] = w
	}
}

// removeFromIndexLocked removes the watcher from the fan-out index. Caller
// must hold mu.
func (b *Broadcaster) removeFromIndexLocked(id string, w *watcher) {
	if w.services == nil {
		delete(b.fullWatchers, id)
		return
	}
	for svc := range w.services {
		if m, ok := b.byService[svc]; ok {
			delete(m, id)
			if len(m) == 0 {
				delete(b.byService, svc)
			}
		}
	}
}

// filteredCountLocked returns the number of watchers carrying a service
// filter. Caller must hold mu.
func (b *Broadcaster) filteredCountLocked() int {
	return len(b.watchers) - len(b.fullWatchers)
}

// Broadcast sends each event to the watchers subscribed to its service (the
// service's consumers plus full watchers). Events are sent non-blocking; a
// watcher whose channel is full has already missed an event, so it is forced
// to resync: its channel is closed, which ends its WatchEndpoints stream, and
// the agent reconnects to receive a fresh filtered snapshot. The alternative —
// silently dropping the event — leaves the agent serving stale endpoints with
// nothing to correct it until its next reconnect for unrelated reasons.
//
// Version stamping (#1203): the caller stamps the batch's version on its
// events, but each watcher receives it on the LAST event of the batch that
// watcher is sent, and on no other. The agent adopts every non-empty version as
// its resume token, and a stream cut between two events of a batch would
// otherwise leave it presenting the post-batch version while missing the rest
// of the batch -- which a reconnect then treats as current. Filtered watchers
// receive different subsets, so "last" is per watcher. The unversioned copies
// are built once per event, not per watcher.
func (b *Broadcaster) Broadcast(events []*registrarv1.WatchEndpointsResponse) {
	// Collect overflowed watchers under the read lock; closing them requires
	// the write lock, taken afterwards to avoid lock-upgrade deadlocks.
	var dropped []droppedWatcher
	// Tally the enqueued events per type and record them once the read lock is
	// released: the fan-out is O(events × watchers) and the counter is only
	// meaningful in aggregate, so the instrument call does not belong inside it.
	broadcast := make(map[registrarv1.WatchEndpointsResponse_EventType]int64, 4)

	send := func(id string, w *watcher, event *registrarv1.WatchEndpointsResponse) {
		select {
		case w.ch <- event:
			broadcast[event.GetType()]++
		default:
			// This watcher's view has diverged — schedule a force-resync.
			// The counter is the staleness alarm.
			b.metrics.eventDropped(context.Background(), event.GetType().String())
			b.log.Info("event overflowed slow watcher; forcing resync",
				"id", id, "eventType", event.GetType().String())
			dropped = append(dropped, droppedWatcher{id: id, w: w})
		}
	}

	b.mu.RLock()
	// Pass 1: the index of the last event each watcher receives.
	last := make(map[*watcher]int)
	for i, event := range events {
		b.forEachRecipientLocked(event, func(_ string, w *watcher) { last[w] = i })
	}
	// Pass 2: fan out, versioned only at each watcher's last event.
	unversioned := make([]*registrarv1.WatchEndpointsResponse, len(events))
	for i, event := range events {
		b.forEachRecipientLocked(event, func(id string, w *watcher) {
			if last[w] == i || event.GetVersion() == "" {
				send(id, w, event)
				return
			}
			if unversioned[i] == nil {
				c := proto.CloneOf(event)
				c.Version = ""
				unversioned[i] = c
			}
			send(id, w, unversioned[i])
		})
	}
	b.mu.RUnlock()

	if len(broadcast) > 0 {
		b.metrics.eventsBroadcast(context.Background(), broadcast)
	}

	if len(dropped) > 0 {
		b.closeDroppedWatchers(dropped)
	}
}

// forEachRecipientLocked calls fn for every watcher that receives event: all
// watchers for a service-catalog event (catalog events bypass the watch filter:
// every agent keeps the full service-name index -- rare, deploy-time
// transitions), else the service's consumers plus the full watchers. Caller
// must hold mu (read or write).
func (b *Broadcaster) forEachRecipientLocked(event *registrarv1.WatchEndpointsResponse, fn func(id string, w *watcher)) {
	if event.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED ||
		event.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_REMOVED {
		for id, w := range b.watchers {
			fn(id, w)
		}
		return
	}
	for id, w := range b.byService[event.GetServiceName()] {
		fn(id, w)
	}
	for id, w := range b.fullWatchers {
		fn(id, w)
	}
}

type droppedWatcher struct {
	id string
	w  *watcher
}

// closeDroppedWatchers acquires the write lock and closes the channels of
// overflowed watchers, forcing them to reconnect for a fresh snapshot.
// Re-checks identity before closing (watcher may have reconnected or been
// unsubscribed between the read and write locks).
func (b *Broadcaster) closeDroppedWatchers(dropped []droppedWatcher) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, d := range dropped {
		// Re-check identity: the watcher may have reconnected (replacing its
		// channel) or unsubscribed between the locks; later events in this batch
		// may also have queued the same channel more than once.
		if current, ok := b.watchers[d.id]; ok && current == d.w {
			b.removeFromIndexLocked(d.id, current)
			close(current.ch)
			delete(b.watchers, d.id)
			b.metrics.watcherUnsubscribed(context.Background())
		}
	}
	b.metrics.filteredWatchers(context.Background(), b.filteredCountLocked())
}

// WatcherCount returns the number of currently subscribed watchers.
func (b *Broadcaster) WatcherCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.watchers)
}
