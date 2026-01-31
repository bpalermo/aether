package log

import (
	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

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

func NewLogger(debug bool) logr.Logger {
	opts := DefaultOptions()
	if debug {
		opts.Development = true
		opts.Level = zapcore.DebugLevel
	}
	return zap.New(zap.UseFlagOptions(&opts))
}
