package registrar

import (
	"context"
	"io"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chanWatchStream feeds processStream from a channel, so a test can act between
// events; closing the channel ends the stream with EOF.
type chanWatchStream struct {
	registrarv1.RegistrarService_WatchEndpointsClient
	events chan *registrarv1.WatchEndpointsResponse
}

func (c *chanWatchStream) Recv() (*registrarv1.WatchEndpointsResponse, error) {
	e, ok := <-c.events
	if !ok {
		return nil, io.EOF
	}
	return e, nil
}

// TestProcessStream_VersionMarkerAdvancesTheToken (#1241): a SNAPSHOT_COMPLETE
// after the initial exchange is a version-only marker. The token moves to it;
// the cache, the catalog and the readiness are untouched, and consumers are not
// woken (nothing they derive from changed).
func TestProcessStream_VersionMarkerAdvancesTheToken(t *testing.T) {
	r := newTestRegistry()
	stream := &chanWatchStream{events: make(chan *registrarv1.WatchEndpointsResponse, 4)}
	type result struct {
		version string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		v, err := r.processStream(context.Background(), stream, "")
		done <- result{v, err}
	}()

	stream.events <- watchEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_FULL_SNAPSHOT, "default/a", "")
	stream.events <- watchEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SERVICE_ADDED, "default/a", "")
	stream.events <- watchEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, "", "40.0123456789abcdef")
	require.NoError(t, r.WaitReady(t.Context()))
	require.Eventually(t, func() bool { return len(r.Changes()) == 1 }, 5*time.Second, time.Millisecond)
	<-r.Changes()

	stream.events <- watchEvent(registrarv1.WatchEndpointsResponse_EVENT_TYPE_SNAPSHOT_COMPLETE, "", "41.0123456789abcdef")
	close(stream.events)
	got := <-done
	require.NoError(t, got.err)
	assert.Equal(t, "41.0123456789abcdef", got.version, "the marker's version is the resume token")
	assert.Empty(t, r.Changes(), "a version marker wakes no consumer")
	assert.Equal(t, []string{"default/a"}, cacheServices(r))
	assert.True(t, r.HasService("default/a"), "the catalog is untouched")
}
