package xds

import "context"

// ServerCallback provides lifecycle hooks for the Server.
// Implementations can perform setup tasks before the server starts accepting connections.
type ServerCallback interface {
	// PreListen is called before the server starts listening on the configured address.
	// It is called after the context is created but before net.Listen is invoked.
	// Implementations should return an error if setup fails; the server will not start.
	PreListen(ctx context.Context) error
}
