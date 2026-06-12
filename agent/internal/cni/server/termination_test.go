package server

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

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
	mu           sync.Mutex
	unregistered int
	registered   int
	lastHealth   registryv1.ServiceEndpoint_Health
}

func (r *unregisterRecordingRegistry) UnregisterEndpoints(_ context.Context, _ string, _ []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unregistered++
	return r.unregisterEndpointsErr
}

func (r *unregisterRecordingRegistry) RegisterEndpoint(_ context.Context, _ string, _ registryv1.Service_Protocol, ep *registryv1.ServiceEndpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registered++
	r.lastHealth = ep.GetHealth()
	return r.registerEndpointErr
}

// Registered and LastHealth are mutex-guarded accessors for assertions that
// race the asynchronous drain phase 2.
func (r *unregisterRecordingRegistry) Registered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registered
}

func (r *unregisterRecordingRegistry) LastHealth() registryv1.ServiceEndpoint_Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastHealth
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
// pod is marked DRAINING in the registry exactly once, persisted as
// terminating, and repeat events (informer resync, force-delete after the
// transition) are no-ops.
func TestHandlePodTerminating(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-a", "default", "container-a")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))
	reg := &unregisterRecordingRegistry{}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), "")

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-a", "default", "test-node"))

	assert.Equal(t, 1, reg.registered, "endpoint must be re-registered as draining")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_DRAINING, reg.lastHealth)
	assert.Zero(t, reg.unregistered, "draining replaces removal; CNI DEL removes later")
	stored, err := store.GetResource(ctx, types.ContainerID("container-a"))
	require.NoError(t, err)
	assert.True(t, stored.GetTerminating(), "terminating flag must be persisted")

	// Second event (resync / DELETE after the transition): idempotent.
	s.handlePodTerminating(ctx, terminatingK8sPod("pod-a", "default", "test-node"))
	assert.Equal(t, 1, reg.registered, "repeat events must not re-mark again")
}

// TestHandlePodTerminatingFallsBackToRemoval: if the DRAINING mark cannot be
// written, removal is the safe fallback (never leave the endpoint selectable).
func TestHandlePodTerminatingFallsBackToRemoval(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-a", "default", "container-a")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))
	reg := &unregisterRecordingRegistry{}
	reg.registerEndpointErr = assert.AnError
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), "")

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-a", "default", "test-node"))

	assert.Equal(t, 1, reg.unregistered, "failed draining mark must fall back to deregistration")
}

// TestHandlePodTerminatingScope: pods on other nodes or unknown to local
// storage are ignored.
func TestHandlePodTerminatingScope(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-a", "default", "container-a")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))
	reg := &unregisterRecordingRegistry{}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), "")

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-a", "default", "other-node"))
	assert.Zero(t, reg.registered+reg.unregistered, "another node's pod must be ignored")

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-unknown", "default", "test-node"))
	assert.Zero(t, reg.registered+reg.unregistered, "a pod absent from local storage must be ignored")
}

// TestHandlePodTerminatingMarksEvenWhenRegistryFails: the terminating flag is
// persisted before any registry call, so transient registry failures cannot
// leave the liveness loop free to resurrect the endpoint (CNI DEL retries the
// removal later).
func TestHandlePodTerminatingMarksEvenWhenRegistryFails(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-a", "default", "container-a")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))
	reg := &unregisterRecordingRegistry{}
	reg.registerEndpointErr = assert.AnError
	reg.unregisterEndpointsErr = assert.AnError
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), "")

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-a", "default", "test-node"))

	stored, err := store.GetResource(ctx, types.ContainerID("container-a"))
	require.NoError(t, err)
	assert.True(t, stored.GetTerminating(), "flag must be set even when the registry is unavailable")
}

// TestReconcileLivenessSkipsTerminatingPod: the liveness loop must never
// re-register a pod whose termination drain has begun — that would resurrect
// the endpoint the termination watch just deregistered.
func TestReconcileLivenessSkipsTerminatingPod(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-a", "default", "container-a")
	pod.Terminating = true

	sock := fakeHealthGateway(t, "health_pod-a", http.StatusServiceUnavailable)

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))
	reg := &unregisterRecordingRegistry{}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), sock)

	s.reconcileLiveness(ctx, newLivenessState())

	assert.Zero(t, reg.registered, "terminating pod must not be re-registered by liveness")
}

// TestHandlePodTerminatingPoolClosePhase: after the drain delay, the endpoint
// is re-registered UNHEALTHY (phase 2 — clients close the by-then-idle pools);
// if CNI DEL removed the pod first, phase 2 must not resurrect the endpoint.
func TestHandlePodTerminatingPoolClosePhase(t *testing.T) {
	ctx := context.Background()
	pod := validCNIPod("pod-2p", "default", "container-2p")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-2p"), pod))
	reg := &unregisterRecordingRegistry{}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), "")

	s.drainPoolCloseDelay = 10 * time.Millisecond

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-2p", "default", "test-node"))
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_DRAINING, reg.LastHealth(), "phase 1: draining")

	assert.Eventually(t, func() bool {
		return reg.LastHealth() == registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
	}, 2*time.Second, 10*time.Millisecond, "phase 2: unhealthy re-registration after drain delay")

	// Resurrection guard: pod removed (CNI DEL) before phase 2 fires.
	pod2 := validCNIPod("pod-2q", "default", "container-2q")
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-2q"), pod2))
	s.handlePodTerminating(ctx, terminatingK8sPod("pod-2q", "default", "test-node"))
	registeredBefore := reg.Registered()
	require.NoError(t, store.RemoveResource(ctx, types.ContainerID("container-2q")))

	time.Sleep(100 * time.Millisecond) // > test drain delay
	assert.Equal(t, registeredBefore, reg.Registered(), "phase 2 must not re-register a removed pod")
}

// TestDrainDelayForPod: phase 2 scales with the workload's sleep preStop
// (close 1s before SIGTERM), capped by the deletion grace, with the
// conservative floor for hookless/opaque pods.
func TestDrainDelayForPod(t *testing.T) {
	s := newTestCNIServer(nil, storage.NewMockStorage[*cniv1.CNIPod](), &unregisterRecordingRegistry{}, cache.NewSnapshotCache("n", logr.Discard()), "")
	s.drainPoolCloseDelay = 2 * time.Second // production floor

	withSleep := func(seconds int64, grace *int64) *corev1.Pod {
		pod := terminatingK8sPod("p", "default", "test-node")
		pod.DeletionGracePeriodSeconds = grace
		if seconds > 0 {
			pod.Spec.Containers = []corev1.Container{{
				Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
					Sleep: &corev1.SleepAction{Seconds: seconds},
				}},
			}}
		}
		return pod
	}
	grace30 := int64(30)

	assert.Equal(t, 2*time.Second, s.drainDelayForPod(withSleep(0, &grace30)), "no preStop: floor")
	assert.Equal(t, 2*time.Second, s.drainDelayForPod(withSleep(3, &grace30)), "sleep 3: 1s before SIGTERM == floor")
	assert.Equal(t, 14*time.Second, s.drainDelayForPod(withSleep(15, &grace30)), "sleep 15: generous window")
	assert.Equal(t, 28*time.Second, s.drainDelayForPod(withSleep(60, &grace30)), "sleep > grace: capped 2s short of hard kill")
	assert.Equal(t, 14*time.Second, s.drainDelayForPod(withSleep(15, nil)), "no grace recorded: sleep governs")
}
