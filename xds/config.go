package xds

import (
	"time"

	"google.golang.org/grpc"
)

const (
	defaultServerShutdownTimeout = time.Second * 30
)

type ServerConfig struct {
	Network         string
	Address         string
	ShutdownTimeout time.Duration

	grpcServerOptions []grpc.ServerOption
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
		Network:           "tcp",
		Address:           ":50051",
		ShutdownTimeout:   defaultServerShutdownTimeout,
		grpcServerOptions: make([]grpc.ServerOption, 0),
	}

	for _, opt := range opts {
		opt(cfg)
	}

	return cfg
}
