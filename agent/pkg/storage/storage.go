package storage

import (
	"context"

	"google.golang.org/protobuf/proto"
)

type Storage[T proto.Message] interface {
	AddResource(context.Context, string, T) error
	RemoveResource(context.Context, string) error
	GetResource(context.Context, string) (T, error)
	LoadAll(context.Context) ([]T, error)
}
