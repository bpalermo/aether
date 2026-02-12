package storage

import (
	"context"

	"github.com/bpalermo/aether/agent/pkg/types"
	"google.golang.org/protobuf/proto"
)

type Storage[T proto.Message] interface {
	Initialize(ctx context.Context) error
	WaitUntilReady(ctx context.Context) error
	AddResource(ctx context.Context, key types.ContainerID, resource T) error
	RemoveResource(ctx context.Context, key types.ContainerID) error
	GetResource(ctx context.Context, key types.ContainerID) (T, error)
	GetAll(_ context.Context) ([]T, error)
	loadAll(ctx context.Context) ([]T, error)
}
