// Package log provides structured logging configuration for Aether components.
//
// Logging is built on the standard library's log/slog. Records are emitted as
// JSON to stderr (parsed via kubectl logs) and, when an OTLP handler is supplied,
// fanned out to the OpenTelemetry logs bridge as well. slog's context-aware
// Handle is what lets the otelslog bridge populate trace_id/span_id natively from
// the ctx passed to the *Context log methods.
//
// Log levels can be controlled via the debug flag:
//   - debug=false: Info level (default production setting)
//   - debug=true: Trace level (enables the V(1)/V(2)-equivalent debug output)
package log

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// LevelTrace is the most verbose level, mapping the old logr V(2) calls. logr
// V(1) maps to slog.LevelDebug (-4); V(2) needs something lower.
const LevelTrace slog.Level = -8

// loggerKey is the attribute key carrying a component name (replacing logr's
// WithName), preserving the "logger" field in the JSON output.
const loggerKey = "logger"

// levelFor returns the handler threshold: Info in production, Trace under --debug
// so the V(1)/V(2)-equivalent records are emitted.
func levelFor(debug bool) slog.Level {
	if debug {
		return LevelTrace
	}
	return slog.LevelInfo
}

// options returns the slog handler options shared by every sink: source location
// plus key/format parity with the previous zap JSON output
// (timestamp/level/logger/caller/message, uppercase levels).
func options(level slog.Level) *slog.HandlerOptions {
	return &slog.HandlerOptions{
		AddSource: true,
		Level:     level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				a.Key = "timestamp"
			case slog.MessageKey:
				a.Key = "message"
			case slog.LevelKey:
				// Default slog levels already render uppercase (DEBUG/INFO/...);
				// only the sub-debug Trace level needs a friendly name.
				if lv, ok := a.Value.Any().(slog.Level); ok && lv < slog.LevelDebug {
					a.Value = slog.StringValue("TRACE")
				}
			case slog.SourceKey:
				if src, ok := a.Value.Any().(*slog.Source); ok {
					a.Key = "caller"
					a.Value = slog.StringValue(shortCaller(src))
				}
			}
			return a
		},
	}
}

// shortCaller renders a zap-style "<dir>/<file>:<line>" caller.
func shortCaller(src *slog.Source) string {
	if src == nil || src.File == "" {
		return ""
	}
	dir, file := filepath.Split(src.File)
	return filepath.Base(filepath.Clean(dir)) + "/" + file + ":" + strconv.Itoa(src.Line)
}

// NewLogger creates a structured logger writing JSON to stderr.
func NewLogger(debug bool) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, options(levelFor(debug))))
}

// NewLoggerWithHandler is like NewLogger but, when extra is non-nil, fans every
// record out to that additional handler (the otelslog OTLP bridge) in addition to
// the stderr JSON sink. The extra sink inherits the same level threshold so both
// outputs stay consistent.
func NewLoggerWithHandler(debug bool, extra slog.Handler) *slog.Logger {
	level := levelFor(debug)
	stderr := slog.NewJSONHandler(os.Stderr, options(level))
	if extra == nil {
		return slog.New(stderr)
	}
	return slog.New(newFanout(stderr, leveled{handler: extra, level: level}))
}

// Named returns a child logger tagged with a component name under the "logger"
// field, replacing logr's WithName. Nested naming keeps the most recent name.
func Named(l *slog.Logger, name string) *slog.Logger {
	return l.With(loggerKey, name)
}
