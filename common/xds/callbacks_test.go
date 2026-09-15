package xds

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubCallback is a test implementation of ServerCallback that records calls
// and optionally returns a configured error.
//
// PreListen runs on the server's Start goroutine while server_test polls the
// flag from the test goroutine, so called is an atomic.Bool (issue #772,
// race R4).
type stubCallback struct {
	called atomic.Bool
	err    error
}

// Ensure stubCallback satisfies the ServerCallback interface at compile time.
var _ ServerCallback = (*stubCallback)(nil)

func (s *stubCallback) PreListen(_ context.Context) error {
	s.called.Store(true)
	return s.err
}

func TestAddCallback_RegistersCallback(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "AddCallback stores the callback on the server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			srv := NewServer(cfg, slog.New(slog.DiscardHandler))

			cb := &stubCallback{}
			srv.AddCallback(cb)

			require.NotNil(t, srv.callback)
			assert.Equal(t, cb, srv.callback)
		})
	}
}

func TestAddCallback_ReplacesExistingCallback(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "AddCallback replaces a previously registered callback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			srv := NewServer(cfg, slog.New(slog.DiscardHandler))

			first := &stubCallback{}
			second := &stubCallback{}

			srv.AddCallback(first)
			srv.AddCallback(second)

			assert.Equal(t, second, srv.callback)
		})
	}
}

func TestServerCallback_InterfaceSatisfaction(t *testing.T) {
	// This is a compile-time guarantee enforced by var _ above.
	// The test documents that stubCallback satisfies ServerCallback.
	t.Run("stubCallback satisfies ServerCallback interface", func(t *testing.T) {
		var cb ServerCallback = &stubCallback{}
		require.NotNil(t, cb)
	})
}
