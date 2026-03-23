package xds

import (
	"net/http"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestRequest builds a minimal *http.Request for use in health check tests.
func newTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, err)
	return req
}

func TestHealthzCheck(t *testing.T) {
	tests := []struct {
		name     string
		setLive  bool
		wantErr  bool
		wantMsg  string
	}{
		{
			name:    "returns nil when server is live",
			setLive: true,
			wantErr: false,
		},
		{
			name:    "returns error when server is not live",
			setLive: false,
			wantErr: true,
			wantMsg: "server is not live",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			srv := NewServer(cfg, logr.Discard())
			srv.liveness.Store(tt.setLive)

			err := srv.HealthzCheck(newTestRequest(t))

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.wantMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestReadyzCheck(t *testing.T) {
	tests := []struct {
		name      string
		setReady  bool
		wantErr   bool
		wantMsg   string
	}{
		{
			name:     "returns nil when server is ready",
			setReady: true,
			wantErr:  false,
		},
		{
			name:     "returns error when server is not ready",
			setReady: false,
			wantErr:  true,
			wantMsg:  "server is not ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			srv := NewServer(cfg, logr.Discard())
			srv.readiness.Store(tt.setReady)

			err := srv.ReadyzCheck(newTestRequest(t))

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.wantMsg, err.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
