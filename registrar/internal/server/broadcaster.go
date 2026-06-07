package server

import (
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
// events on a buffered channel. Events are dropped for slow consumers.
type Broadcaster struct {
	mu       sync.RWMutex
	watchers map[string]chan *registrarv1.WatchEndpointsResponse
	log      logr.Logger
}

// NewBroadcaster creates a Broadcaster.
func NewBroadcaster(log logr.Logger) *Broadcaster {
	return &Broadcaster{
		watchers: make(map[string]chan *registrarv1.WatchEndpointsResponse),
		log:      log.WithName("broadcaster"),
	}
}

// Subscribe registers a new watcher and returns a channel that will receive
// endpoint events. The caller must call Unsubscribe when done.
func (b *Broadcaster) Subscribe(id string) <-chan *registrarv1.WatchEndpointsResponse {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Close any existing channel for this ID (reconnection scenario).
	if ch, exists := b.watchers[id]; exists {
		close(ch)
	}

	ch := make(chan *registrarv1.WatchEndpointsResponse, defaultChannelBuffer)
	b.watchers[id] = ch
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
	b.log.V(1).Info("watcher unsubscribed", "id", id)
}

// Broadcast sends events to all watchers. Events are sent non-blocking;
// if a watcher's channel is full the event is dropped for that watcher.
func (b *Broadcaster) Broadcast(events []*registrarv1.WatchEndpointsResponse) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, event := range events {
		for id, ch := range b.watchers {
			select {
			case ch <- event:
			default:
				b.log.V(1).Info("dropped event for slow watcher", "id", id, "eventType", event.GetType())
			}
		}
	}
}

// WatcherCount returns the number of currently subscribed watchers.
func (b *Broadcaster) WatcherCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.watchers)
}
