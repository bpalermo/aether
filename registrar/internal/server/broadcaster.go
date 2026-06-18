package server

import (
	"context"
	"log/slog"
	"sync"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	commonlog "github.com/bpalermo/aether/common/log"
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
func (b *Broadcaster) Broadcast(events []*registrarv1.WatchEndpointsResponse) {
	// Collect overflowed watchers under the read lock; closing them requires
	// the write lock, taken afterwards to avoid lock-upgrade deadlocks.
	type droppedWatcher struct {
		id string
		w  *watcher
	}
	var dropped []droppedWatcher

	send := func(id string, w *watcher, event *registrarv1.WatchEndpointsResponse) {
		select {
		case w.ch <- event:
			b.metrics.eventBroadcast(context.Background(), event.GetType().String())
		default:
			// This watcher's view has diverged — schedule a force-resync.
			// The counter is the staleness alarm.
			b.metrics.eventDropped(context.Background(), event.GetType().String())
			b.log.Info("event overflowed slow watcher; forcing resync",
				"id", id, "eventType", event.GetType())
			dropped = append(dropped, droppedWatcher{id: id, w: w})
		}
	}

	b.mu.RLock()
	for _, event := range events {
		// Service-catalog events bypass the watch filter: every agent keeps
		// the full service-name index (rare, deploy-time transitions).
		if event.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED ||
			event.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_REMOVED {
			for id, w := range b.watchers {
				send(id, w, event)
			}
			continue
		}
		for id, w := range b.byService[event.GetServiceName()] {
			send(id, w, event)
		}
		for id, w := range b.fullWatchers {
			send(id, w, event)
		}
	}
	b.mu.RUnlock()

	if len(dropped) == 0 {
		return
	}

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
