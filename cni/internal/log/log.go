package log

import (
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

const defaultOutputPath = "/var/log/aether-cni/plugin.log"

func NewLogger() (*zap.Logger, error) {
	// Ensure the log directory exists
	logDir := filepath.Dir(defaultOutputPath)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		// Fallback to stderr if we can't create the log directory
		return zap.NewProduction()
	}

	// Configure encoder for JSON output
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	encoder := zapcore.NewJSONEncoder(encoderConfig)

	// Configure file-only output with rotation
	fileWriteSyncer := zapcore.AddSync(&lumberjack.Logger{
		Filename:   defaultOutputPath,
		MaxSize:    10, // MB
		MaxBackups: 5,
		MaxAge:     30, // days
		Compress:   true,
		LocalTime:  true,
	})

	// Single core writing to file only
	core := zapcore.NewCore(
		encoder,
		fileWriteSyncer, // File output only
		zap.DebugLevel,
	)

	return zap.New(core, zap.AddCaller()), nil
}
