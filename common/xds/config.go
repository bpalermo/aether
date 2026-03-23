package xds

import (
	"time"

	"google.golang.org/grpc"
)

const (
	defaultServerShutdownTimeout = time.Second * 30
)

// ServerConfig holds configuration for a Server.
//
// Network and Address specify the network type and address for the server to bind to.
// For Unix domain sockets, use WithUDS. For TCP, the Address should be ":port" format.
// ShutdownTimeout specifies how long to wait for graceful shutdown before forcefully
// stopping the server.
type ServerConfig struct {
	// Network is the network type: "tcp" or "unix"
	Network string
	// Address is the address to bind to (e.g., ":50051" for TCP or "/tmp/server.sock" for Unix domain socket)
	Address string
	// ShutdownTimeout is the maximum time to wait for graceful shutdown
	ShutdownTimeout time.Duration

	grpcServerOptions []grpc.ServerOption
}

// ServerConfigOpt is a functional option for configuring ServerConfig.
type ServerConfigOpt func(*ServerConfig)

// WithUDS configures the server to use a Unix domain socket at the given address.
func WithUDS(address string) ServerConfigOpt {
	return func(c *ServerConfig) {
		c.Network = "unix"
		c.Address = address
	}
}

// NewServerConfig creates a new ServerConfig with default values.
// The default network is TCP on port 50051 with a 30-second shutdown timeout.
// Options can be provided to override defaults.
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
