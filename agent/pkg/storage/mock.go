package storage

import (
	"context"
	"errors"

	"github.com/bpalermo/aether/agent/pkg/types"
	"google.golang.org/protobuf/proto"
)

// MockStorage is an in-memory Storage implementation intended for use in tests
// outside the storage package. Because Storage contains the unexported loadAll
// method, types in other packages cannot implement the interface directly;
// MockStorage bridges that gap by living in package storage while exposing
// configurable behaviour via exported function fields.
type MockStorage[T proto.Message] struct {
	// GetAllFunc is called by GetAll. If nil, GetAll returns an empty slice.
	GetAllFunc func(ctx context.Context) ([]T, error)

	// resources holds items added via AddResource.
	resources map[types.ContainerID]T
}

// NewMockStorage returns a MockStorage with an empty resource map.
func NewMockStorage[T proto.Message]() *MockStorage[T] {
	return &MockStorage[T]{
		resources: make(map[types.ContainerID]T),
	}
}

// NewMockStorageWithGetAll returns a MockStorage whose GetAll method is backed
// by the provided function. Use this when test cases need precise control over
// the data returned by GetAll, including error injection.
func NewMockStorageWithGetAll[T proto.Message](fn func(ctx context.Context) ([]T, error)) *MockStorage[T] {
	return &MockStorage[T]{
		GetAllFunc: fn,
		resources:  make(map[types.ContainerID]T),
	}
}

// Compile-time assertion that MockStorage satisfies Storage.
var _ Storage[proto.Message] = (*MockStorage[proto.Message])(nil)

func (m *MockStorage[T]) Initialize(_ context.Context) error { return nil }

func (m *MockStorage[T]) WaitUntilReady(_ context.Context) error { return nil }

func (m *MockStorage[T]) AddResource(_ context.Context, key types.ContainerID, resource T) error {
	m.resources[key] = resource
	return nil
}

func (m *MockStorage[T]) RemoveResource(_ context.Context, key types.ContainerID) error {
	delete(m.resources, key)
	return nil
}

func (m *MockStorage[T]) GetResource(_ context.Context, key types.ContainerID) (T, error) {
	r, ok := m.resources[key]
	if !ok {
		var zero T
		return zero, errors.New("not found")
	}
	return r, nil
}

func (m *MockStorage[T]) GetAll(ctx context.Context) ([]T, error) {
	if m.GetAllFunc != nil {
		return m.GetAllFunc(ctx)
	}
	result := make([]T, 0, len(m.resources))
	for _, v := range m.resources {
		result = append(result, v)
	}
	return result, nil
}

// loadAll satisfies the unexported Storage method. It delegates to GetAll so
// that the in-memory resources map is the single source of truth.
func (m *MockStorage[T]) loadAll(ctx context.Context) ([]T, error) {
	return m.GetAll(ctx)
}
