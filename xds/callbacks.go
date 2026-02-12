package xds

import "context"

type ServerCallback interface {
	PreListen(ctx context.Context) error
}
