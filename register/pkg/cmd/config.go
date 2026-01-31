package cmd

import "time"

const (
	defaultShutdownTimeout = 30 * time.Second
)

type RegisterConfig struct {
	Debug bool

	ShutdownTimeout time.Duration
}

func NewRegisterConfig() *RegisterConfig {
	return &RegisterConfig{
		false,
		defaultShutdownTimeout,
	}
}
