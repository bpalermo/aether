package log

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLogger(t *testing.T) {
	for _, debug := range []bool{false, true} {
		l := NewLogger(debug)
		require.NotNil(t, l)
	}
}

// captureHandler records the records it receives, for asserting fanout behaviour.
type captureHandler struct {
	slog.Handler
	got *[]slog.Record
}

func (c captureHandler) Handle(ctx context.Context, r slog.Record) error {
	*c.got = append(*c.got, r)
	return nil
}

func TestNewLoggerWithHandler_FansOut(t *testing.T) {
	var buf bytes.Buffer
	var captured []slog.Record
	extra := captureHandler{Handler: slog.NewJSONHandler(&buf, options(slog.LevelInfo)), got: &captured}

	// Build a logger that tees to the extra handler (and stderr); assert the extra
	// sink receives the record.
	l := slog.New(newFanout(slog.NewJSONHandler(&buf, options(slog.LevelInfo)), extra))
	l.InfoContext(context.Background(), "hello", "k", "v")

	require.Len(t, captured, 1)
	assert.Equal(t, "hello", captured[0].Message)
}

func TestFormatParity(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, options(slog.LevelInfo)))
	l.InfoContext(context.Background(), "msg")

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	assert.Contains(t, m, "timestamp")
	assert.Contains(t, m, "message")
	assert.Contains(t, m, "caller")
	assert.Equal(t, "INFO", m["level"])
}

func TestNamed(t *testing.T) {
	var buf bytes.Buffer
	l := Named(slog.New(slog.NewJSONHandler(&buf, options(slog.LevelInfo))), "syncer")
	l.InfoContext(context.Background(), "msg")

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	assert.Equal(t, "syncer", m["logger"])
}

func TestNamed_NestedDotJoinsSingleKey(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, options(slog.LevelInfo)))
	l := Named(Named(base, "aether-agent"), "cache")
	l.InfoContext(context.Background(), "msg")

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	assert.Equal(t, "aether-agent.cache", m["logger"])
	// Exactly one "logger" key in the raw JSON (json.Unmarshal would hide dupes).
	assert.Equal(t, 1, strings.Count(buf.String(), `"logger":`))
}

func TestLevelGate(t *testing.T) {
	var buf bytes.Buffer
	// Production threshold: V(1)/V(2)-equivalent debug records are dropped.
	l := slog.New(slog.NewJSONHandler(&buf, options(slog.LevelInfo)))
	l.DebugContext(context.Background(), "debug line")
	l.Log(context.Background(), LevelTrace, "trace line")
	assert.Empty(t, buf.String())
}
