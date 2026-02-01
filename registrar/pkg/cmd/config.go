package cmd

import (
	"time"

	"github.com/bpalermo/aether/registrar/pkg/server"
)

const (
	defaultShutdownTimeout = 30 * time.Second
)

type RegisterConfig struct {
	Debug bool

	ShutdownTimeout time.Duration

	srvCfg *server.RegistrarServerConfig
}

func NewRegisterConfig() *RegisterConfig {
	return &RegisterConfig{
		false,
		defaultShutdownTimeout,
		server.NewRegisterServerConfig(),
	}
}
