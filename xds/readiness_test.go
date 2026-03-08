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
		name    string
		wantErr bool
	}{
		{
			name:    "returns nil when server is running",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			srv := NewServer(cfg, logr.Discard())

			err := srv.HealthzCheck(newTestRequest(t))

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestReadyzCheck(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{
			name:    "returns nil when server is running",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			srv := NewServer(cfg, logr.Discard())

			err := srv.ReadyzCheck(newTestRequest(t))

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
