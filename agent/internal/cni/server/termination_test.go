package server

import (
	"context"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/storage"
	"github.com/bpalermo/aether/agent/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// unregisterRecordingRegistry counts UnregisterEndpoints and RegisterEndpoint
// calls on top of testRegistry.
type unregisterRecordingRegistry struct {
	testRegistry
	unregistered int
	registered   int
}

func (r *unregisterRecordingRegistry) UnregisterEndpoints(_ context.Context, _ string, _ []string) error {
	r.unregistered++
	return r.unregisterEndpointsErr
}

func (r *unregisterRecordingRegistry) RegisterEndpoint(_ context.Context, _ string, _ registryv1.Service_Protocol, _ *registryv1.ServiceEndpoint) error {
	r.registered++
	return nil
}

// terminatingK8sPod returns a corev1.Pod on the given node with
// deletionTimestamp set (deletion requested).
func terminatingK8sPod(name, namespace, node string) *corev1.Pod {
	now := metav1.Now()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			DeletionTimestamp: &now,
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
}

// TestHandlePodTerminating covers the early-drain path: a deletion-requested
// pod is deregistered exactly once, persisted as terminating, and repeat events
// (informer resync, force-delete after the transition) are no-ops.
func TestHandlePodTerminating(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-a", "default", "container-a")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))
	reg := &unregisterRecordingRegistry{}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), "127.0.0.1:1")

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-a", "default", "test-node"))

	assert.Equal(t, 1, reg.unregistered, "endpoint must be deregistered")
	stored, err := store.GetResource(ctx, types.ContainerID("container-a"))
	require.NoError(t, err)
	assert.True(t, stored.GetTerminating(), "terminating flag must be persisted")

	// Second event (resync / DELETE after the transition): idempotent.
	s.handlePodTerminating(ctx, terminatingK8sPod("pod-a", "default", "test-node"))
	assert.Equal(t, 1, reg.unregistered, "repeat events must not deregister again")
}

// TestHandlePodTerminatingScope: pods on other nodes or unknown to local
// storage are ignored.
func TestHandlePodTerminatingScope(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-a", "default", "container-a")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))
	reg := &unregisterRecordingRegistry{}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), "127.0.0.1:1")

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-a", "default", "other-node"))
	assert.Zero(t, reg.unregistered, "another node's pod must be ignored")

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-unknown", "default", "test-node"))
	assert.Zero(t, reg.unregistered, "a pod absent from local storage must be ignored")
}

// TestHandlePodTerminatingMarksEvenWhenUnregisterFails: the terminating flag is
// persisted before the registry call, so a transient unregister failure cannot
// leave the liveness loop free to resurrect the endpoint (CNI DEL retries the
// unregister later).
func TestHandlePodTerminatingMarksEvenWhenUnregisterFails(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-a", "default", "container-a")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))
	reg := &unregisterRecordingRegistry{}
	reg.unregisterEndpointsErr = assert.AnError
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), "127.0.0.1:1")

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-a", "default", "test-node"))

	stored, err := store.GetResource(ctx, types.ContainerID("container-a"))
	require.NoError(t, err)
	assert.True(t, stored.GetTerminating(), "flag must be set even when unregister fails")
}

// TestReconcileLivenessSkipsTerminatingPod: the liveness loop must never
// re-register a pod whose termination drain has begun — that would resurrect
// the endpoint the termination watch just deregistered.
func TestReconcileLivenessSkipsTerminatingPod(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-a", "default", "container-a")
	pod.Terminating = true

	srv := fakeClustersAdmin(t, "health_pod-a")
	defer srv.Close()

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))
	reg := &unregisterRecordingRegistry{}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), srv.Listener.Addr().String())

	s.reconcileLiveness(ctx, map[string]registryv1.ServiceEndpoint_Health{})

	assert.Zero(t, reg.registered, "terminating pod must not be re-registered by liveness")
}
