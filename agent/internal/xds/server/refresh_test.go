package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

// notifyRegistry wraps mockRegistry to also implement registry.ChangeNotifier.
type notifyRegistry struct {
	*mockRegistry
	ch chan struct{}
}

func (n *notifyRegistry) Changes() <-chan struct{} { return n.ch }

var _ registry.ChangeNotifier = (*notifyRegistry)(nil)

func TestRegistryRefresher_ReloadsOnChange(t *testing.T) {
	c := cache.NewSnapshotCache("node-1", logr.Discard())

	// A local pod declaring "echo" as an upstream puts it in the node
	// dependency set — only in-scope services are distributed.
	require.NoError(t, c.AddPod(context.Background(), &cniv1.CNIPod{
		Name:             "client-1",
		Namespace:        "default",
		ServiceAccount:   "client",
		NetworkNamespace: "/proc/100/ns/net",
		Annotations:      map[string]string{constants.AnnotationConfigUpstreams: "echo"},
	}, "example.org"))

	var mu sync.Mutex
	data := map[string][]*registryv1.ServiceEndpoint{}
	reg := &notifyRegistry{
		mockRegistry: &mockRegistry{
			listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
				mu.Lock()
				defer mu.Unlock()
				return data, nil
			},
		},
		ch: make(chan struct{}, 1),
	}

	r := NewRegistryRefresher("cluster-1", "node-1", c, reg, logr.Discard())
	r.debounce = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// No clusters until a change is published.
	require.Nil(t, c.Endpoints("echo"))

	mu.Lock()
	data = map[string][]*registryv1.ServiceEndpoint{
		"echo": {{Ip: "10.0.0.1", ClusterName: "cluster-1", Port: 8080}},
	}
	mu.Unlock()
	reg.ch <- struct{}{}

	require.Eventually(t, func() bool {
		return c.Endpoints("echo") != nil
	}, 2*time.Second, 10*time.Millisecond, "refresher should rebuild the cluster after a change signal")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("refresher did not return after context cancel")
	}
}

// TestRegistryRefresher_ReloadsOnDependencyChange verifies a dependency-set
// change (pod added with a new declared upstream) triggers a scoped reload
// even when the registry itself reported no change.
func TestRegistryRefresher_ReloadsOnDependencyChange(t *testing.T) {
	c := cache.NewSnapshotCache("node-1", logr.Discard())

	data := map[string][]*registryv1.ServiceEndpoint{
		"echo": {{Ip: "10.0.0.1", ClusterName: "cluster-1", Port: 8080}},
	}
	reg := &notifyRegistry{
		mockRegistry: &mockRegistry{
			listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
				return data, nil
			},
		},
		ch: make(chan struct{}, 1),
	}

	r := NewRegistryRefresher("cluster-1", "node-1", c, reg, logr.Discard())
	r.debounce = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// "echo" is in the registry but not in the (empty) dependency set.
	require.Nil(t, c.Endpoints("echo"))

	// A pod declaring echo lands: the dependency change alone must surface it.
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name:             "client-1",
		Namespace:        "default",
		ServiceAccount:   "client",
		NetworkNamespace: "/proc/100/ns/net",
		Annotations:      map[string]string{constants.AnnotationConfigUpstreams: "echo"},
	}, "example.org"))

	require.Eventually(t, func() bool {
		return c.Endpoints("echo") != nil
	}, 2*time.Second, 10*time.Millisecond, "refresher should rebuild the scoped snapshot after a dependency change")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("refresher did not return after context cancel")
	}
}

func TestRegistryRefresher_NoNotifier_StopsOnContext(t *testing.T) {
	c := cache.NewSnapshotCache("node-1", logr.Discard())
	// mockRegistry does not implement registry.ChangeNotifier.
	r := NewRegistryRefresher("cluster-1", "node-1", c, &mockRegistry{}, logr.Discard())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("refresher did not return after context cancel")
	}
}
