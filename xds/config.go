package xds

import "time"

const (
	defaultServerShutdownTimeout = time.Second * 30
)

type ServerConfig struct {
	Network         string
	Address         string
	ShutdownTimeout time.Duration
}

func NewServerConfig() *ServerConfig {
	return &ServerConfig{
		Network:         "tcp",
		Address:         ":50051",
		ShutdownTimeout: defaultServerShutdownTimeout,
	}
}
