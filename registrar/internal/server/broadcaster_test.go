package server

import (
	"testing"

	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewBroadcaster verifies that NewBroadcaster initializes a broadcaster with
// no watchers.
func TestNewBroadcaster(t *testing.T) {
	b := NewBroadcaster(logr.Discard())

	require.NotNil(t, b)
	assert.Equal(t, 0, b.WatcherCount())
}

// TestBroadcaster_Subscribe verifies that Subscribe returns a channel and increments
// the watcher count.
func TestBroadcaster_Subscribe(t *testing.T) {
	tests := []struct {
		name         string
		ids          []string
		wantCount    int
		wantNilChans bool
	}{
		{
			name:      "single subscriber increments count to one",
			ids:       []string{"node-1"},
			wantCount: 1,
		},
		{
			name:      "multiple distinct subscribers increment count correctly",
			ids:       []string{"node-1", "node-2", "node-3"},
			wantCount: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBroadcaster(logr.Discard())

			for _, id := range tt.ids {
				ch := b.Subscribe(id)
				require.NotNil(t, ch)
			}

			assert.Equal(t, tt.wantCount, b.WatcherCount())
		})
	}
}

// TestBroadcaster_Subscribe_SameID verifies that subscribing with the same ID
// closes the old channel and replaces it with a new one.
func TestBroadcaster_Subscribe_SameID(t *testing.T) {
	b := NewBroadcaster(logr.Discard())

	ch1 := b.Subscribe("node-1")
	require.NotNil(t, ch1)
	assert.Equal(t, 1, b.WatcherCount())

	// Re-subscribing with the same ID should close the old channel.
	ch2 := b.Subscribe("node-1")
	require.NotNil(t, ch2)

	// The watcher count must stay at one (replacement, not addition).
	assert.Equal(t, 1, b.WatcherCount())

	// The old channel must be closed after the re-subscription.
	_, open := <-ch1
	assert.False(t, open, "old channel must be closed after re-subscription")

	// The new channel must be distinct from the old one.
	assert.NotEqual(t, ch1, ch2)
}

// TestBroadcaster_Unsubscribe verifies that Unsubscribe removes the watcher,
// decrements the count, and closes the channel.
func TestBroadcaster_Unsubscribe(t *testing.T) {
	tests := []struct {
		name          string
		subscribeIDs  []string
		unsubscribeID string
		wantCount     int
	}{
		{
			name:          "unsubscribing the only watcher leaves count at zero",
			subscribeIDs:  []string{"node-1"},
			unsubscribeID: "node-1",
			wantCount:     0,
		},
		{
			name:          "unsubscribing one of several watchers decrements count by one",
			subscribeIDs:  []string{"node-1", "node-2", "node-3"},
			unsubscribeID: "node-2",
			wantCount:     2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBroadcaster(logr.Discard())

			var targetCh <-chan *registrarv1.WatchEndpointsResponse
			for _, id := range tt.subscribeIDs {
				ch := b.Subscribe(id)
				if id == tt.unsubscribeID {
					targetCh = ch
				}
			}

			b.Unsubscribe(tt.unsubscribeID)

			assert.Equal(t, tt.wantCount, b.WatcherCount())

			// The channel returned by Subscribe must be closed after Unsubscribe.
			require.NotNil(t, targetCh)
			_, open := <-targetCh
			assert.False(t, open, "channel must be closed after Unsubscribe")
		})
	}
}

// TestBroadcaster_Unsubscribe_UnknownID verifies that Unsubscribe on an unknown
// ID is a no-op and does not panic or alter the watcher count.
func TestBroadcaster_Unsubscribe_UnknownID(t *testing.T) {
	b := NewBroadcaster(logr.Discard())
	b.Subscribe("node-1")

	// Must not panic.
	b.Unsubscribe("does-not-exist")

	assert.Equal(t, 1, b.WatcherCount())
}

// TestBroadcaster_Unsubscribe_EmptyBroadcaster verifies that calling Unsubscribe
// on a broadcaster with no watchers is a no-op.
func TestBroadcaster_Unsubscribe_EmptyBroadcaster(t *testing.T) {
	b := NewBroadcaster(logr.Discard())

	// Must not panic.
	b.Unsubscribe("node-1")

	assert.Equal(t, 0, b.WatcherCount())
}

// TestBroadcaster_Broadcast_DeliveredToAll verifies that Broadcast sends events
// to all subscribed watchers.
func TestBroadcaster_Broadcast_DeliveredToAll(t *testing.T) {
	tests := []struct {
		name       string
		watcherIDs []string
		events     []*registrarv1.WatchEndpointsResponse
	}{
		{
			name:       "single event delivered to single watcher",
			watcherIDs: []string{"node-1"},
			events: []*registrarv1.WatchEndpointsResponse{
				{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "svc-a"},
			},
		},
		{
			name:       "single event delivered to multiple watchers",
			watcherIDs: []string{"node-1", "node-2", "node-3"},
			events: []*registrarv1.WatchEndpointsResponse{
				{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "svc-a"},
			},
		},
		{
			name:       "multiple events delivered in order to all watchers",
			watcherIDs: []string{"node-1", "node-2"},
			events: []*registrarv1.WatchEndpointsResponse{
				{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "svc-a"},
				{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED, ServiceName: "svc-b"},
				{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_UPDATED, ServiceName: "svc-c"},
			},
		},
		{
			name:       "full snapshot event delivered to all watchers",
			watcherIDs: []string{"node-1", "node-2"},
			events: []*registrarv1.WatchEndpointsResponse{
				{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, ServiceName: "svc-a"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBroadcaster(logr.Discard())

			channels := make(map[string]<-chan *registrarv1.WatchEndpointsResponse, len(tt.watcherIDs))
			for _, id := range tt.watcherIDs {
				channels[id] = b.Subscribe(id)
			}

			b.Broadcast(tt.events)

			for _, id := range tt.watcherIDs {
				ch := channels[id]
				for _, want := range tt.events {
					select {
					case got, open := <-ch:
						require.True(t, open, "channel for watcher %q must remain open", id)
						assert.Equal(t, want, got)
					default:
						t.Errorf("watcher %q: expected event %v but channel was empty", id, want)
					}
				}
			}
		})
	}
}

// TestBroadcaster_Broadcast_NoWatchers verifies that Broadcast with no subscribed
// watchers does not panic.
func TestBroadcaster_Broadcast_NoWatchers(t *testing.T) {
	b := NewBroadcaster(logr.Discard())

	// Must not panic.
	b.Broadcast([]*registrarv1.WatchEndpointsResponse{
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "svc-a"},
	})
}

// TestBroadcaster_Broadcast_EmptyEventList verifies that broadcasting an empty
// event slice is a no-op and does not panic.
func TestBroadcaster_Broadcast_EmptyEventList(t *testing.T) {
	b := NewBroadcaster(logr.Discard())
	b.Subscribe("node-1")

	// Must not panic.
	b.Broadcast([]*registrarv1.WatchEndpointsResponse{})

	assert.Equal(t, 1, b.WatcherCount())
}

// TestBroadcaster_Broadcast_DropsEventsForSlowConsumer verifies that Broadcast
// does not block and drops events when a watcher's channel is full.
//
// Broadcast uses a non-blocking select internally, so it must return immediately
// even when the watcher channel is at capacity.
func TestBroadcaster_Broadcast_DropsEventsForSlowConsumer(t *testing.T) {
	b := NewBroadcaster(logr.Discard())
	ch := b.Subscribe("slow-node")

	// Fill the watcher's channel to capacity by broadcasting one event at a time.
	// No goroutine drains the channel so after defaultChannelBuffer calls it is full.
	fillEvent := &registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-fill",
	}
	for i := 0; i < defaultChannelBuffer; i++ {
		b.Broadcast([]*registrarv1.WatchEndpointsResponse{fillEvent})
	}
	require.Equal(t, defaultChannelBuffer, len(ch), "channel must be full before overflow test")

	// Broadcasting onto a full channel must be a non-blocking no-op (the event is
	// dropped via the default branch in Broadcast). Verify this by checking that
	// the channel length is unchanged after the extra call.
	overflowEvent := &registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-overflow",
	}
	b.Broadcast([]*registrarv1.WatchEndpointsResponse{overflowEvent})

	// Channel must still hold exactly defaultChannelBuffer events; overflow dropped.
	assert.Equal(t, defaultChannelBuffer, len(ch))
}

// TestBroadcaster_WatcherCount verifies WatcherCount through subscribe/unsubscribe
// sequences.
func TestBroadcaster_WatcherCount(t *testing.T) {
	tests := []struct {
		name      string
		actions   func(b *Broadcaster)
		wantCount int
	}{
		{
			name:      "zero watchers on fresh broadcaster",
			actions:   func(_ *Broadcaster) {},
			wantCount: 0,
		},
		{
			name: "count reflects subscriptions",
			actions: func(b *Broadcaster) {
				b.Subscribe("node-1")
				b.Subscribe("node-2")
			},
			wantCount: 2,
		},
		{
			name: "count reflects unsubscriptions",
			actions: func(b *Broadcaster) {
				b.Subscribe("node-1")
				b.Subscribe("node-2")
				b.Unsubscribe("node-1")
			},
			wantCount: 1,
		},
		{
			name: "re-subscribing same ID does not increase count",
			actions: func(b *Broadcaster) {
				b.Subscribe("node-1")
				b.Subscribe("node-1")
			},
			wantCount: 1,
		},
		{
			name: "unsubscribe of unknown ID does not decrement count",
			actions: func(b *Broadcaster) {
				b.Subscribe("node-1")
				b.Unsubscribe("node-99")
			},
			wantCount: 1,
		},
		{
			name: "subscribe then unsubscribe all returns to zero",
			actions: func(b *Broadcaster) {
				b.Subscribe("node-1")
				b.Subscribe("node-2")
				b.Unsubscribe("node-1")
				b.Unsubscribe("node-2")
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBroadcaster(logr.Discard())
			tt.actions(b)
			assert.Equal(t, tt.wantCount, b.WatcherCount())
		})
	}
}
