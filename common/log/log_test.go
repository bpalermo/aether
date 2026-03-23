package log

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultOptions(t *testing.T) {
	opts := DefaultOptions()

	assert.False(t, opts.Development)
	assert.NotNil(t, opts.Encoder)
	assert.NotNil(t, opts.TimeEncoder)
}

func TestNewLogger(t *testing.T) {
	tests := []struct {
		name  string
		debug bool
	}{
		{name: "production mode", debug: false},
		{name: "debug mode", debug: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := NewLogger(tt.debug)
			require.NotNil(t, logger)
			assert.True(t, logger.GetSink() != nil)
		})
	}
}
