// Package log provides structured logging configuration for Aether components.
//
// It wraps controller-runtime's zap logger to provide consistent JSON-formatted
// logging across the agent, CNI plugin, and other Aether services. Logs include
// structured fields (timestamp, level, logger name, caller, message) for easier
// parsing and analysis in production environments.
//
// Log levels can be controlled via the debug flag:
//   - debug=false: Info level (default production setting)
//   - debug=true: Debug level (for troubleshooting)
package log

import (
	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// DefaultOptions returns the default Zap logger options for Aether.
// Logs are formatted as JSON with ISO8601 timestamps, capital log levels,
// and short caller information.
func DefaultOptions() zap.Options {
	opts := zap.Options{
		Development: false, // Set to false for production
		TimeEncoder: zapcore.ISO8601TimeEncoder,
		EncoderConfigOptions: []zap.EncoderConfigOption{
			func(ec *zapcore.EncoderConfig) {
				ec.EncodeLevel = zapcore.CapitalLevelEncoder
				ec.EncodeDuration = zapcore.StringDurationEncoder
				ec.EncodeCaller = zapcore.ShortCallerEncoder
			},
		},
	}

	// Use JSON encoder
	opts.Encoder = zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "timestamp",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    "function",
		MessageKey:     "message",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	})

	return opts
}

// NewLogger creates a new structured logger with Aether's default configuration.
// If debug is true, the logger is configured for debug-level output; otherwise,
// it logs at info level. Returns a logr.Logger interface compatible with
// controller-runtime components.
func NewLogger(debug bool) logr.Logger {
	opts := DefaultOptions()
	if debug {
		opts.Level = zapcore.DebugLevel
	}
	return zap.New(zap.UseFlagOptions(&opts))
}
