package server

import (
	"log/slog"
	"testing"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	"github.com/stretchr/testify/assert"
)

// TestWatcherIDOf: an agent that names its instance gets a key of its own; one
// that predates the field keeps the cluster/node key it always had.
func TestWatcherIDOf(t *testing.T) {
	assert.Equal(t, "c/n", watcherIDOf(&registrarv1.WatchEndpointsRequest{ClusterName: "c", NodeName: "n"}))
	assert.Equal(t, "c/n/aether-agent-x1", watcherIDOf(&registrarv1.WatchEndpointsRequest{ClusterName: "c", NodeName: "n", Instance: "aether-agent-x1"}))
}

// TestSurgeAgentsOnOneNodeKeepBothWatches is the registry-watch half of
// proposal 041's overlap: the standby and the agent it replaces both watch for
// the same node. Keyed by node alone, each Subscribe closes the other's channel
// (DataLoss, a full resync) and the two reconnect into each other for the whole
// overlap — the reconnect loop the cluster/node key itself was introduced to
// end between nodes. Keyed by instance, both streams live.
func TestSurgeAgentsOnOneNodeKeepBothWatches(t *testing.T) {
	b := NewBroadcaster(slog.New(slog.DiscardHandler), nil)
	old := b.Subscribe(watcherIDOf(&registrarv1.WatchEndpointsRequest{ClusterName: "c", NodeName: "n", Instance: "agent-old"}), nil)
	standby := b.Subscribe(watcherIDOf(&registrarv1.WatchEndpointsRequest{ClusterName: "c", NodeName: "n", Instance: "agent-new"}), nil)

	b.Broadcast([]*registrarv1.WatchEndpointsResponse{{Type: registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED, ServiceName: "s"}})
	for name, ch := range map[string]<-chan *registrarv1.WatchEndpointsResponse{"old": old, "standby": standby} {
		ev, open := <-ch
		assert.True(t, open, "%s's watch was closed by the other agent's", name)
		assert.Equal(t, "s", ev.GetServiceName(), name)
	}

	// The control: without an instance the second subscription replaces the first.
	a := b.Subscribe(watcherIDOf(&registrarv1.WatchEndpointsRequest{ClusterName: "c", NodeName: "m"}), nil)
	_ = b.Subscribe(watcherIDOf(&registrarv1.WatchEndpointsRequest{ClusterName: "c", NodeName: "m"}), nil)
	_, open := <-a
	assert.False(t, open)
}
