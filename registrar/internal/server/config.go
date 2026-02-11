package server

import (
	"github.com/bpalermo/aether/xds"
)

type RegistrarServerConfig struct {
	SrvCfg *xds.ServerConfig
}

func NewRegisterServerConfig() *RegistrarServerConfig {
	return &RegistrarServerConfig{
		SrvCfg: xds.NewServerConfig(),
	}
}
