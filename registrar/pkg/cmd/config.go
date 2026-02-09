package cmd

import (
	"github.com/bpalermo/aether/registrar/internal/server"
)

type RegisterConfig struct {
	Debug bool

	srvCfg *server.RegistrarServerConfig
}

func NewRegisterConfig() *RegisterConfig {
	return &RegisterConfig{
		false,
		server.NewRegisterServerConfig(),
	}
}
