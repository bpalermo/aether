package plugin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testProber builds a readinessProber against an httptest server, bypassing
// the netns dialer (covered separately) to unit-test the poll loops.
func testProber(srv *httptest.Server) *readinessProber {
	return &readinessProber{
		client: &http.Client{
			Transport: &http.Transport{DisableKeepAlives: true},
			Timeout:   readyProbeRequestTimeout,
		},
		url: srv.URL,
	}
}

func TestWaitServing(t *testing.T) {
	t.Run("returns once the endpoint answers 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, testProber(srv).waitServing(ctx))
	})

	t.Run("retries through 503 (draining epoch) until 200", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, testProber(srv).waitServing(ctx))
		assert.GreaterOrEqual(t, calls.Load(), int32(3))
	})

	t.Run("times out while the endpoint keeps failing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		err := testProber(srv).waitServing(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out waiting for proxy readiness")
	})
}

func TestWaitGone(t *testing.T) {
	t.Run("returns once the listener socket is closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		p := testProber(srv)
		srv.Close() // dialing the freed port now refuses

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, p.waitGone(ctx))
	})

	t.Run("keeps waiting while an envoy still answers", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		err := testProber(srv).waitGone(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out waiting for proxy listener removal")
	})
}

func TestNetnsDialContext_MissingNetns(t *testing.T) {
	dial := netnsDialContext("/nonexistent/netns/path")
	_, err := dial(context.Background(), "tcp", "127.0.0.1:1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist), "missing netns must map to ErrNotExist for waitGone")
}
