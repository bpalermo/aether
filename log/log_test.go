package log

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultOptions(t *testing.T) {
	opts := DefaultOptions()

	assert.False(t, opts.Development, "development mode should be disabled by default")
	assert.NotNil(t, opts.Encoder, "encoder should be set")
	assert.NotNil(t, opts.TimeEncoder, "TimeEncoder should be set")
}

func TestNewLogger(t *testing.T) {
	tests := []struct {
		name  string
		debug bool
	}{
		{
			name:  "creates logger in production mode",
			debug: false,
		},
		{
			name:  "creates logger in debug mode",
			debug: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := NewLogger(tt.debug)
			require.NotNil(t, logger)
			// Verify the logger is usable without panicking.
			logger.Info("test message", "key", "value")
		})
	}
}

func TestNewLogger_DebugLevelIsUsable(t *testing.T) {
	logger := NewLogger(true)
	require.NotNil(t, logger)
	// Verify V(1) messages are accepted without panicking.
	logger.V(1).Info("debug level message")
}

func TestNewLogger_ProductionLevelIsUsable(t *testing.T) {
	logger := NewLogger(false)
	require.NotNil(t, logger)
	logger.Info("production log message", "component", "test")
}
