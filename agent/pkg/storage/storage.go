package storage

import "google.golang.org/protobuf/proto"

type Storage[T proto.Message] interface {
	AddResource(string, T) error
	RemoveResource(string) error
	GetResource(string) (T, error)
	LoadAll() ([]T, error)
}
