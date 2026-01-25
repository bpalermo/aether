package storage

import (
	"context"

	"github.com/bpalermo/aether/agent/pkg/types"
	"google.golang.org/protobuf/proto"
)

type Storage[T proto.Message] interface {
	AddResource(ctx context.Context, key types.ContainerID, resource T) error
	RemoveResource(ctx context.Context, key types.ContainerID) error
	GetResource(ctx context.Context, key types.ContainerID) (T, error)
	LoadAll(ctx context.Context) ([]T, error)
}
