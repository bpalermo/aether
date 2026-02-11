package cmd

import (
	"github.com/bpalermo/aether/registrar/internal/server"
)

const defaultClusterName = "unknown"

type RegistrarConfig struct {
	Debug bool

	ClusterName  string
	registrarCfg *server.RegistrarServerConfig
}

func NewRegistrarConfig() *RegistrarConfig {
	return &RegistrarConfig{
		false,
		defaultClusterName,
		server.NewRegisterServerConfig(),
	}
}
