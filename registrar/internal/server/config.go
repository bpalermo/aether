package server

import "time"

const (
	defaultClusterName           = "unknown"
	defaultServerShutdownTimeout = time.Second * 30
)

type RegistrarServerConfig struct {
	Network string
	Address string

	ShutdownTimeout time.Duration
}

func NewRegisterServerConfig() *RegistrarServerConfig {
	return &RegistrarServerConfig{
		Network:         "tcp",
		Address:         ":50051",
		ShutdownTimeout: defaultServerShutdownTimeout,
	}
}
