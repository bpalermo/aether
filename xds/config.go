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

type ServerConfigOpt func(*ServerConfig)

func WithUDS(address string) ServerConfigOpt {
	return func(c *ServerConfig) {
		c.Network = "unix"
		c.Address = address
	}
}

func NewServerConfig(opts ...ServerConfigOpt) *ServerConfig {
	cfg := &ServerConfig{
		Network:         "tcp",
		Address:         ":50051",
		ShutdownTimeout: defaultServerShutdownTimeout,
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return cfg
}
