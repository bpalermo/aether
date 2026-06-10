package server

import (
	"context"
	"sync"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	"github.com/go-logr/logr"
)

const (
	// defaultChannelBuffer is the buffer size for per-watcher event channels.
	defaultChannelBuffer = 256
)

// Broadcaster fans out endpoint events to all connected agent watch streams.
// Each watcher is identified by a string key (typically node name) and receives
// events on a buffered channel. A watcher too slow to keep up is forcibly
// resynced: its channel is closed, ending its WatchEndpoints stream, and the
// agent's reconnect receives a full snapshot — it must never silently miss an
// event and serve stale endpoints until something else triggers a resync.
type Broadcaster struct {
	mu       sync.RWMutex
	watchers map[string]chan *registrarv1.WatchEndpointsResponse
	log      logr.Logger
	metrics  *Metrics
}

// NewBroadcaster creates a Broadcaster. metrics may be nil to disable
// instrumentation.
func NewBroadcaster(log logr.Logger, metrics *Metrics) *Broadcaster {
	return &Broadcaster{
		watchers: make(map[string]chan *registrarv1.WatchEndpointsResponse),
		log:      log.WithName("broadcaster"),
		metrics:  metrics,
	}
}

// Subscribe registers a new watcher and returns a channel that will receive
// endpoint events. The caller must call Unsubscribe when done.
func (b *Broadcaster) Subscribe(id string) <-chan *registrarv1.WatchEndpointsResponse {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Close any existing channel for this ID (reconnection scenario).
	existing, replaced := b.watchers[id]
	if replaced {
		close(existing)
	}

	ch := make(chan *registrarv1.WatchEndpointsResponse, defaultChannelBuffer)
	b.watchers[id] = ch
	if !replaced {
		// A reconnect replaces the channel but keeps the watcher count.
		b.metrics.watcherSubscribed(context.Background())
	}
	b.log.V(1).Info("watcher subscribed", "id", id)
	return ch
}

// Unsubscribe removes a watcher and closes its channel. The channel returned by
// the matching Subscribe call must be passed so that a stale caller (whose
// subscription was already replaced by a reconnect with the same id) does not
// close or delete the newer subscription's channel.
func (b *Broadcaster) Unsubscribe(id string, ch <-chan *registrarv1.WatchEndpointsResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()

	existing, exists := b.watchers[id]
	if !exists || (<-chan *registrarv1.WatchEndpointsResponse)(existing) != ch {
		return
	}

	close(existing)
	delete(b.watchers, id)
	b.metrics.watcherUnsubscribed(context.Background())
	b.log.V(1).Info("watcher unsubscribed", "id", id)
}

// Broadcast sends events to all watchers. Events are sent non-blocking; a
// watcher whose channel is full has already missed an event, so it is forced
// to resync: its channel is closed, which ends its WatchEndpoints stream, and
// the agent reconnects to receive a fresh full snapshot. The alternative —
// silently dropping the event — leaves the agent serving stale endpoints with
// nothing to correct it until its next reconnect for unrelated reasons.
func (b *Broadcaster) Broadcast(events []*registrarv1.WatchEndpointsResponse) {
	// Collect overflowed watchers under the read lock; closing them requires
	// the write lock, taken afterwards to avoid lock-upgrade deadlocks.
	type droppedWatcher struct {
		id string
		ch chan *registrarv1.WatchEndpointsResponse
	}
	var dropped []droppedWatcher

	b.mu.RLock()
	for _, event := range events {
		for id, ch := range b.watchers {
			select {
			case ch <- event:
				b.metrics.eventBroadcast(context.Background(), event.GetType().String())
			default:
				// This watcher's view has diverged — schedule a force-resync.
				// The counter is the staleness alarm.
				b.metrics.eventDropped(context.Background(), event.GetType().String())
				b.log.Info("event overflowed slow watcher; forcing resync",
					"id", id, "eventType", event.GetType())
				dropped = append(dropped, droppedWatcher{id: id, ch: ch})
			}
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
		if current, ok := b.watchers[d.id]; ok && current == d.ch {
			close(current)
			delete(b.watchers, d.id)
			b.metrics.watcherUnsubscribed(context.Background())
		}
	}
}

// WatcherCount returns the number of currently subscribed watchers.
func (b *Broadcaster) WatcherCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.watchers)
}
