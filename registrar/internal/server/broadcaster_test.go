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
	b := NewBroadcaster(logr.Discard(), nil)

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
			b := NewBroadcaster(logr.Discard(), nil)

			for _, id := range tt.ids {
				ch := b.Subscribe(id, nil)
				require.NotNil(t, ch)
			}

			assert.Equal(t, tt.wantCount, b.WatcherCount())
		})
	}
}

// TestBroadcaster_Subscribe_SameID verifies that subscribing with the same ID
// closes the old channel and replaces it with a new one.
func TestBroadcaster_Subscribe_SameID(t *testing.T) {
	b := NewBroadcaster(logr.Discard(), nil)

	ch1 := b.Subscribe("node-1", nil)
	require.NotNil(t, ch1)
	assert.Equal(t, 1, b.WatcherCount())

	// Re-subscribing with the same ID should close the old channel.
	ch2 := b.Subscribe("node-1", nil)
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
			b := NewBroadcaster(logr.Discard(), nil)

			var targetCh <-chan *registrarv1.WatchEndpointsResponse
			for _, id := range tt.subscribeIDs {
				ch := b.Subscribe(id, nil)
				if id == tt.unsubscribeID {
					targetCh = ch
				}
			}

			b.Unsubscribe(tt.unsubscribeID, targetCh)

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
	b := NewBroadcaster(logr.Discard(), nil)
	b.Subscribe("node-1", nil)

	// Must not panic.
	b.Unsubscribe("does-not-exist", nil)

	assert.Equal(t, 1, b.WatcherCount())
}

// TestBroadcaster_Unsubscribe_EmptyBroadcaster verifies that calling Unsubscribe
// on a broadcaster with no watchers is a no-op.
func TestBroadcaster_Unsubscribe_EmptyBroadcaster(t *testing.T) {
	b := NewBroadcaster(logr.Discard(), nil)

	// Must not panic.
	b.Unsubscribe("node-1", nil)

	assert.Equal(t, 0, b.WatcherCount())
}

// TestBroadcaster_Unsubscribe_StaleChannel verifies that a stale caller (whose
// subscription was already replaced by a reconnect with the same id) cannot close
// or remove the newer subscription's channel. This guards against a same-id
// reconnect race that would otherwise drop the live watcher and double-close.
func TestBroadcaster_Unsubscribe_StaleChannel(t *testing.T) {
	b := NewBroadcaster(logr.Discard(), nil)

	ch1 := b.Subscribe("node-1", nil) // first connection; channel closed by re-Subscribe below
	ch2 := b.Subscribe("node-1", nil) // reconnect with the same id replaces ch1

	// The stale caller unsubscribing with its old channel must be a no-op: ch2 is
	// still the live subscription and must remain registered.
	b.Unsubscribe("node-1", ch1)
	assert.Equal(t, 1, b.WatcherCount(), "live subscription must survive a stale unsubscribe")

	// ch2 must still be open (not closed by the stale unsubscribe).
	select {
	case _, open := <-ch2:
		assert.True(t, open, "live channel must not be closed by a stale unsubscribe")
	default:
	}

	// The live caller unsubscribing with the current channel removes it.
	b.Unsubscribe("node-1", ch2)
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
			b := NewBroadcaster(logr.Discard(), nil)

			channels := make(map[string]<-chan *registrarv1.WatchEndpointsResponse, len(tt.watcherIDs))
			for _, id := range tt.watcherIDs {
				channels[id] = b.Subscribe(id, nil)
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
	b := NewBroadcaster(logr.Discard(), nil)

	// Must not panic.
	b.Broadcast([]*registrarv1.WatchEndpointsResponse{
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "svc-a"},
	})
}

// TestBroadcaster_Broadcast_EmptyEventList verifies that broadcasting an empty
// event slice is a no-op and does not panic.
func TestBroadcaster_Broadcast_EmptyEventList(t *testing.T) {
	b := NewBroadcaster(logr.Discard(), nil)
	b.Subscribe("node-1", nil)

	// Must not panic.
	b.Broadcast([]*registrarv1.WatchEndpointsResponse{})

	assert.Equal(t, 1, b.WatcherCount())
}

// TestBroadcaster_Broadcast_ForcesResyncOnSlowConsumer verifies that Broadcast
// does not block when a watcher's channel is full, and that the overflowed
// watcher is force-resynced: removed from the broadcaster with its channel
// closed, so its WatchEndpoints stream ends and the agent reconnects for a
// full snapshot instead of silently missing the event.
func TestBroadcaster_Broadcast_ForcesResyncOnSlowConsumer(t *testing.T) {
	b := NewBroadcaster(logr.Discard(), nil)
	ch := b.Subscribe("slow-node", nil)

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

	// Overflow by several events in one batch: the channel must be closed
	// exactly once (no panic) and the watcher removed.
	overflowEvent := &registrarv1.WatchEndpointsResponse{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-overflow",
	}
	b.Broadcast([]*registrarv1.WatchEndpointsResponse{overflowEvent, overflowEvent})

	assert.Equal(t, 0, b.WatcherCount(), "overflowed watcher must be removed")

	// The buffered events remain readable, then the channel reports closed.
	for i := 0; i < defaultChannelBuffer; i++ {
		_, ok := <-ch
		require.True(t, ok, "buffered event %d must still be readable", i)
	}
	_, ok := <-ch
	assert.False(t, ok, "channel must be closed after the buffer drains")

	// The stream handler's deferred Unsubscribe must be a stale no-op.
	b.Unsubscribe("slow-node", ch)
	assert.Equal(t, 0, b.WatcherCount())
}

// TestBroadcaster_Broadcast_ResyncedWatcherCanResubscribe verifies the
// force-resync recovery path: the agent reconnects under the same watcher ID
// and receives events normally.
func TestBroadcaster_Broadcast_ResyncedWatcherCanResubscribe(t *testing.T) {
	b := NewBroadcaster(logr.Discard(), nil)
	b.Subscribe("slow-node", nil)

	event := &registrarv1.WatchEndpointsResponse{
		Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
	}
	for i := 0; i < defaultChannelBuffer+1; i++ {
		b.Broadcast([]*registrarv1.WatchEndpointsResponse{event})
	}
	require.Equal(t, 0, b.WatcherCount(), "watcher must be force-resynced")

	ch2 := b.Subscribe("slow-node", nil)
	b.Broadcast([]*registrarv1.WatchEndpointsResponse{event})
	require.Equal(t, 1, b.WatcherCount())
	select {
	case got, ok := <-ch2:
		require.True(t, ok)
		assert.Equal(t, event.GetType(), got.GetType())
	default:
		t.Fatal("resubscribed watcher did not receive the event")
	}
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
				b.Subscribe("node-1", nil)
				b.Subscribe("node-2", nil)
			},
			wantCount: 2,
		},
		{
			name: "count reflects unsubscriptions",
			actions: func(b *Broadcaster) {
				ch1 := b.Subscribe("node-1", nil)
				b.Subscribe("node-2", nil)
				b.Unsubscribe("node-1", ch1)
			},
			wantCount: 1,
		},
		{
			name: "re-subscribing same ID does not increase count",
			actions: func(b *Broadcaster) {
				b.Subscribe("node-1", nil)
				b.Subscribe("node-1", nil)
			},
			wantCount: 1,
		},
		{
			name: "unsubscribe of unknown ID does not decrement count",
			actions: func(b *Broadcaster) {
				b.Subscribe("node-1", nil)
				b.Unsubscribe("node-99", nil)
			},
			wantCount: 1,
		},
		{
			name: "subscribe then unsubscribe all returns to zero",
			actions: func(b *Broadcaster) {
				ch1 := b.Subscribe("node-1", nil)
				ch2 := b.Subscribe("node-2", nil)
				b.Unsubscribe("node-1", ch1)
				b.Unsubscribe("node-2", ch2)
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBroadcaster(logr.Discard(), nil)
			tt.actions(b)
			assert.Equal(t, tt.wantCount, b.WatcherCount())
		})
	}
}

// TestBroadcaster_FilteredFanout verifies demand-scoped fan-out: an endpoint
// event reaches the service's consumers and full watchers only — never
// watchers filtered to other services or to nothing.
func TestBroadcaster_FilteredFanout(t *testing.T) {
	b := NewBroadcaster(logr.Discard(), nil)

	chA := b.Subscribe("consumer-a", []string{"svc-a"})
	chB := b.Subscribe("consumer-b", []string{"svc-b"})
	chFull := b.Subscribe("full-watcher", nil)
	chNone := b.Subscribe("empty-node", []string{})

	b.Broadcast([]*registrarv1.WatchEndpointsResponse{{
		Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
		ServiceName: "svc-a",
	}})

	select {
	case e := <-chA:
		assert.Equal(t, "svc-a", e.GetServiceName())
	default:
		t.Fatal("svc-a consumer must receive the svc-a event")
	}
	select {
	case e := <-chFull:
		assert.Equal(t, "svc-a", e.GetServiceName())
	default:
		t.Fatal("full watcher must receive every event")
	}
	select {
	case <-chB:
		t.Fatal("svc-b consumer must not receive svc-a events")
	default:
	}
	select {
	case <-chNone:
		t.Fatal("empty-filter watcher must receive nothing")
	default:
	}
}

// TestBroadcaster_ResubscribeReplacesFilter verifies a reconnect with a new
// filter fully replaces the old index entries (no events from the old scope).
func TestBroadcaster_ResubscribeReplacesFilter(t *testing.T) {
	b := NewBroadcaster(logr.Discard(), nil)

	chOld := b.Subscribe("node-1", []string{"svc-a"})
	chNew := b.Subscribe("node-1", []string{"svc-b"}) // reconnect, new scope

	// Old channel is closed by the replacement.
	_, open := <-chOld
	assert.False(t, open, "replaced subscription's channel must be closed")

	b.Broadcast([]*registrarv1.WatchEndpointsResponse{
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "svc-a"},
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "svc-b"},
	})

	select {
	case e := <-chNew:
		assert.Equal(t, "svc-b", e.GetServiceName(), "only the new scope's events may arrive")
	default:
		t.Fatal("new subscription must receive svc-b")
	}
	select {
	case e := <-chNew:
		t.Fatalf("unexpected extra event for %s", e.GetServiceName())
	default:
	}

	// Unsubscribe cleans the index: broadcasting afterwards reaches nobody.
	b.Unsubscribe("node-1", chNew)
	assert.Zero(t, b.WatcherCount())
	b.Broadcast([]*registrarv1.WatchEndpointsResponse{
		{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "svc-b"},
	})
}
