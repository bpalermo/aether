package snapshot

import (
	"context"
	"fmt"
	"testing"
	"time"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewXdsSnapshot(t *testing.T) {
	nodeID := "test-node"
	log := logr.Discard()

	registry := NewXdsSnapshot(nodeID, log)

	assert.NotNil(t, registry)
	assert.Equal(t, nodeID, registry.nodeID)
	assert.NotNil(t, registry.listenerCache)
	assert.NotNil(t, registry.eventChan)
	assert.NotNil(t, registry.snapshot)
}

func TestXdsRegistry_Start(t *testing.T) {
	t.Run("processes events until context canceled", func(t *testing.T) {
		registry := NewXdsSnapshot("test-node", logr.Discard())
		ctx, cancel := context.WithCancel(context.Background())

		// Send test event
		event := &registryv1.Event{
			Operation: registryv1.Event_CREATED,
			Resource: &registryv1.Event_K8SPod{
				K8SPod: &registryv1.KubernetesPod{
					Name:        "test-pod",
					Namespace:   "default",
					ServiceName: "test-service",
					Ip:          "10.0.0.1",
				},
			},
		}

		done := make(chan error, 1)
		go func() {
			done <- registry.Start(ctx)
		}()

		// Send event and wait for processing
		registry.eventChan <- event
		time.Sleep(50 * time.Millisecond)

		// Cancel context
		cancel()

		err := <-done
		assert.Equal(t, context.Canceled, err)
	})

	t.Run("handles nil events", func(t *testing.T) {
		registry := NewXdsSnapshot("test-node", logr.Discard())
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		go registry.Start(ctx)

		// Send nil event
		registry.eventChan <- nil
		time.Sleep(50 * time.Millisecond)

		// Should not panic and continue processing
		assert.NotPanics(t, func() {
			registry.eventChan <- &registryv1.Event{
				Operation: registryv1.Event_CREATED,
				Resource: &registryv1.Event_K8SPod{
					K8SPod: &registryv1.KubernetesPod{
						ServiceName: "fake",
					},
				},
			}
		})
	})
}

func TestXdsRegistry_processEvent(t *testing.T) {
	tests := []struct {
		name  string
		event *registryv1.Event
		want  error
	}{
		{
			name: "pod event",
			event: &registryv1.Event{
				Operation: registryv1.Event_CREATED,
				Resource: &registryv1.Event_K8SPod{
					K8SPod: &registryv1.KubernetesPod{
						Name:        "test-pod",
						Namespace:   "default",
						ServiceName: "test-service",
						Ip:          "10.0.0.1",
					},
				},
			},
			want: nil,
		},
		{
			name: "network namespace event",
			event: &registryv1.Event{
				Operation: registryv1.Event_CREATED,
				Resource: &registryv1.Event_RegistryPod{
					RegistryPod: &registryv1.RegistryPod{
						CniPod: &cniv1.CNIPod{
							Name:             "test-pod",
							Namespace:        "default",
							NetworkNamespace: "/var/run/netns/test",
						},
					},
				},
			},
			want: nil,
		},
		{
			name: "unknown resource type",
			event: &registryv1.Event{
				Operation: registryv1.Event_CREATED,
				Resource:  nil,
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := NewXdsSnapshot("test-node", logr.Discard())
			ctx := context.Background()

			err := registry.processEvent(ctx, tt.event)
			assert.Equal(t, tt.want, err)
		})
	}
}

func TestXdsRegistry_processPodEvent(t *testing.T) {
	ctx := context.Background()

	t.Run("CREATED operation", func(t *testing.T) {
		registry := NewXdsSnapshot("test-node", logr.Discard())
		pod := &registryv1.KubernetesPod{
			Name:        "test-pod",
			Namespace:   "default",
			ServiceName: "test-service",
			Ip:          "10.0.0.1",
		}

		err := registry.processPodEvent(ctx, registryv1.Event_CREATED, pod)
		require.NoError(t, err)
	})

	t.Run("UPDATED operation", func(t *testing.T) {
		registry := NewXdsSnapshot("test-node", logr.Discard())
		pod := &registryv1.KubernetesPod{
			Name:        "test-pod",
			Namespace:   "default",
			ServiceName: "test-service",
			Ip:          "10.0.0.2",
		}

		err := registry.processPodEvent(ctx, registryv1.Event_UPDATED, pod)
		require.NoError(t, err)
	})

	t.Run("DELETED operation", func(t *testing.T) {
		registry := NewXdsSnapshot("test-node", logr.Discard())

		// First add a pod
		pod := &registryv1.KubernetesPod{
			Name:        "test-pod",
			Namespace:   "default",
			ServiceName: "test-service",
			Ip:          "10.0.0.1",
		}
		registry.processPodEvent(ctx, registryv1.Event_CREATED, pod)

		// Then delete it
		err := registry.processPodEvent(ctx, registryv1.Event_DELETED, pod)
		require.NoError(t, err)
	})
}

func TestXdsRegistry_processNetworkNs(t *testing.T) {
	ctx := context.Background()

	t.Run("CREATED operation", func(t *testing.T) {
		registry := NewXdsSnapshot("test-node", logr.Discard())
		cniPod := &registryv1.RegistryPod{
			CniPod: &cniv1.CNIPod{
				Name:             "test-pod",
				Namespace:        "default",
				NetworkNamespace: "/var/run/netns/test",
			},
		}

		err := registry.processRegistryPod(ctx, registryv1.Event_CREATED, cniPod)
		require.NoError(t, err)
	})

	t.Run("DELETED operation", func(t *testing.T) {
		registry := NewXdsSnapshot("test-node", logr.Discard())
		registryPod := &registryv1.RegistryPod{
			CniPod: &cniv1.CNIPod{
				Name:             "test-pod",
				Namespace:        "default",
				NetworkNamespace: "/var/run/netns/test",
			},
		}

		// First add
		registry.processRegistryPod(ctx, registryv1.Event_CREATED, registryPod)

		// Then delete
		err := registry.processRegistryPod(ctx, registryv1.Event_DELETED, registryPod)
		require.NoError(t, err)
	})
}

func TestXdsRegistry_generateSnapshot(t *testing.T) {
	ctx := context.Background()
	registry := NewXdsSnapshot("test-node", logr.Discard())

	err := registry.generateSnapshot(ctx)
	require.NoError(t, err)

	// Verify the snapshot was set
	snapshot, err := registry.snapshot.GetSnapshot(registry.nodeID)
	require.NoError(t, err)
	assert.NotNil(t, snapshot)

	// Verify the snapshot contains expected resource types
	resources := snapshot.GetResources(resource.EndpointType)
	assert.NotNil(t, resources)
}

func TestXdsRegistry_GetSnapshot(t *testing.T) {
	registry := NewXdsSnapshot("test-node", logr.Discard())

	snapshot := registry.GetSnapshot()
	assert.NotNil(t, snapshot)
}

func TestXdsRegistry_GetEventChan(t *testing.T) {
	registry := NewXdsSnapshot("test-node", logr.Discard())

	eventChan := registry.GetEventChan()
	assert.NotNil(t, eventChan)

	// Verify we can send events
	assert.NotPanics(t, func() {
		select {
		case eventChan <- &registryv1.Event{}:
		default:
		}
	})
}

func TestXdsRegistry_ConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	registry := NewXdsSnapshot("test-node", logr.Discard())

	// Start registry
	go registry.Start(ctx)

	// Simulate concurrent pod events
	for i := 0; i < 10; i++ {
		go func(id int) {
			pod := &registryv1.KubernetesPod{
				Name:        fmt.Sprintf("pod-%d", id),
				Namespace:   "default",
				ServiceName: fmt.Sprintf("service-%d", id),
				Ip:          fmt.Sprintf("10.0.0.%d", id),
			}

			event := &registryv1.Event{
				Operation: registryv1.Event_CREATED,
				Resource: &registryv1.Event_K8SPod{
					K8SPod: pod,
				},
			}

			registry.GetEventChan() <- event
		}(i)
	}

	// Let events process
	time.Sleep(100 * time.Millisecond)

	// Verify no data races and registry is in valid state
	assert.NotPanics(t, func() {
		_ = registry.generateSnapshot(ctx)
	})
}
