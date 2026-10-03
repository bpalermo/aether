package k8s

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
)

// aether#1124: on this backend the agent's DRAINING mark lands only on the
// registrar replica that received it (writes are no-ops). Every other replica
// has to learn the drain from the Pod itself, and promptly.

func withReady(pod *corev1.Pod, ready corev1.ConditionStatus) *corev1.Pod {
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}
	return pod
}

func deleting(pod *corev1.Pod) *corev1.Pod {
	now := metav1.NewTime(time.Now())
	pod.DeletionTimestamp = &now
	// The fake client refuses an object carrying a deletionTimestamp without a
	// finalizer; a real terminating pod has none but is still served.
	pod.Finalizers = []string{"test.aether.io/hold"}
	return pod
}

func TestPodHealth_DeletionRequested(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want registryv1.ServiceEndpoint_Health
	}{
		{"ready", withReady(managedPod("p", "ns", "sa", "10.0.0.1", "n"), corev1.ConditionTrue), registryv1.ServiceEndpoint_HEALTH_HEALTHY},
		{"not ready", withReady(managedPod("p", "ns", "sa", "10.0.0.1", "n"), corev1.ConditionFalse), registryv1.ServiceEndpoint_HEALTH_UNHEALTHY},
		{"no ready condition", managedPod("p", "ns", "sa", "10.0.0.1", "n"), registryv1.ServiceEndpoint_HEALTH_UNSPECIFIED},
		// The preStop window: deletion requested, the kubelet still reports Ready.
		{"deleting, still ready", deleting(withReady(managedPod("p", "ns", "sa", "10.0.0.1", "n"), corev1.ConditionTrue)), registryv1.ServiceEndpoint_HEALTH_DRAINING},
		{"deleting, no longer ready", deleting(withReady(managedPod("p", "ns", "sa", "10.0.0.1", "n"), corev1.ConditionFalse)), registryv1.ServiceEndpoint_HEALTH_UNHEALTHY},
		{"deleting, no ready condition", deleting(managedPod("p", "ns", "sa", "10.0.0.1", "n")), registryv1.ServiceEndpoint_HEALTH_UNHEALTHY},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, podHealth(tt.pod, time.Now()))
		})
	}
}

// terminatingAt makes pod a deletion requested at requested with a grace of
// grace s and a sleep preStop of sleep s, as the API server records it.
func terminatingAt(pod *corev1.Pod, requested time.Time, grace, sleep int64) *corev1.Pod {
	dt := metav1.NewTime(requested.Add(time.Duration(grace) * time.Second))
	pod.DeletionTimestamp = &dt
	pod.DeletionGracePeriodSeconds = &grace
	pod.Finalizers = []string{"test.aether.io/hold"}
	pod.Spec.Containers = []corev1.Container{{
		Name:      "app",
		Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: sleep}}},
	}}
	return pod
}

// TestPodHealth_PoolCloseFromThePod is aether#1144: the drain's second phase,
// UNHEALTHY about 1 s before SIGTERM, derived from the Pod so every registrar
// replica lists it, not only the one the node agent's mark reached. A 15 s
// sleep preStop, 30 s grace: DRAINING until 14 s after the request, UNHEALTHY
// from then on, while the kubelet still reports the pod Ready.
func TestPodHealth_PoolCloseFromThePod(t *testing.T) {
	requested := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	pod := terminatingAt(withReady(managedPod("p", "ns", "sa", "10.0.0.1", "n"), corev1.ConditionTrue), requested, 30, 15)

	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_DRAINING, podHealth(pod, requested))
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_DRAINING, podHealth(pod, requested.Add(14*time.Second-time.Millisecond)))
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_UNHEALTHY, podHealth(pod, requested.Add(14*time.Second)),
		"phase 2 is due 1 s before the preStop ends")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_UNHEALTHY,
		podHealth(withReady(pod.DeepCopy(), corev1.ConditionFalse), requested), "no longer Ready: UNHEALTHY whatever the time")
}

// TestArmPoolClose_WakesTheSyncWhenPhaseTwoIsDue: nothing about the Pod
// changes at the phase-2 moment, so the deletion event arms one wake-up for
// it, and the wake-up signals a re-list.
func TestArmPoolClose_WakesTheSyncWhenPhaseTwoIsDue(t *testing.T) {
	informer := controllertest.NewFakeInformer()
	r := NewKubernetesRegistry(slog.New(slog.DiscardHandler), fake.NewClientBuilder().Build(),
		Config{ClusterName: "c", Informers: fakeInformerSource{informer}})
	requested := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return requested.Add(100 * time.Millisecond) }
	type armed struct {
		wait time.Duration
		fire func()
	}
	var timers []armed
	r.afterFunc = func(d time.Duration, f func()) { timers = append(timers, armed{d, f}) }
	require.NoError(t, r.Initialize(context.Background()))
	changes := r.Changes()

	ready := withReady(managedPod("p", "ns", "sa", "10.0.0.1", "n"), corev1.ConditionTrue)
	ready.UID = "uid-p"
	draining := terminatingAt(ready.DeepCopy(), requested, 30, 15)
	informer.Update(ready, draining)
	require.True(t, signalled(changes), "the deletion request")
	require.Len(t, timers, 1)
	assert.Equal(t, 14*time.Second-100*time.Millisecond, timers[0].wait, "due 14 s after the request")

	churn := draining.DeepCopy()
	churn.Annotations = map[string]string{"endpoint.aether.io/weight": "5"}
	informer.Update(draining, churn)
	assert.True(t, signalled(changes))
	assert.Len(t, timers, 1, "one wake-up per pod and due time")

	timers[0].fire()
	assert.True(t, signalled(changes), "phase 2 due: re-list")

	notReady := withReady(churn.DeepCopy(), corev1.ConditionFalse)
	notReady.Annotations = nil
	informer.Update(churn, notReady)
	assert.Len(t, timers, 1, "a pod no longer Ready is UNHEALTHY already; nothing to wake for")
}

// TestListAllEndpoints_TerminatingPodListsDraining: a terminating pod is still
// Running with its IP, so it is still listed, but as DRAINING.
func TestListAllEndpoints_TerminatingPodListsDraining(t *testing.T) {
	r := newTestRegistry("c",
		topologyNode("n", "r", "z"),
		deleting(withReady(managedPod("victim", "ns", "svc", "10.0.0.1", "n"), corev1.ConditionTrue)),
		withReady(managedPod("peer", "ns", "svc", "10.0.0.2", "n"), corev1.ConditionTrue),
	)
	got, err := r.ListAllEndpoints(context.Background(), registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	health := map[string]registryv1.ServiceEndpoint_Health{}
	for _, ep := range got["ns/svc"] {
		health[ep.GetIp()] = ep.GetHealth()
	}
	assert.Equal(t, map[string]registryv1.ServiceEndpoint_Health{
		"10.0.0.1": registryv1.ServiceEndpoint_HEALTH_DRAINING,
		"10.0.0.2": registryv1.ServiceEndpoint_HEALTH_HEALTHY,
	}, health)
}

type fakeInformerSource struct{ informer *controllertest.FakeInformer }

func (f fakeInformerSource) GetInformer(context.Context, client.Object, ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	return f.informer, nil
}

func signalled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestChanges_NilWithoutInformers(t *testing.T) {
	r := newTestRegistry("c")
	require.NoError(t, r.Initialize(context.Background()))
	assert.Nil(t, r.Changes(), "a backend without an informer source is poll-only")
}

// TestChanges_SignalsWhenAManagedPodsEndpointCanChange: the deletion request is
// what a peer replica must hear at watch speed; status churn that cannot change
// an endpoint, and unmanaged pods, must not trigger a re-list.
func TestChanges_SignalsWhenAManagedPodsEndpointCanChange(t *testing.T) {
	informer := controllertest.NewFakeInformer()
	r := NewKubernetesRegistry(slog.New(slog.DiscardHandler), fake.NewClientBuilder().Build(),
		Config{ClusterName: "c", Informers: fakeInformerSource{informer}})
	require.NoError(t, r.Initialize(context.Background()))
	changes := r.Changes()
	require.NotNil(t, changes)

	ready := withReady(managedPod("p", "ns", "sa", "10.0.0.1", "n"), corev1.ConditionTrue)

	informer.Add(ready)
	assert.True(t, signalled(changes), "a managed pod appearing")

	churn := ready.DeepCopy()
	churn.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1}}
	informer.Update(ready, churn)
	assert.False(t, signalled(changes), "status churn no endpoint field reads")

	draining := deleting(ready.DeepCopy())
	informer.Update(ready, draining)
	assert.True(t, signalled(changes), "the deletion request (the drain)")

	notReady := withReady(draining.DeepCopy(), corev1.ConditionFalse)
	informer.Update(draining, notReady)
	assert.True(t, signalled(changes), "readiness lost")

	annotated := ready.DeepCopy()
	annotated.Annotations = map[string]string{"endpoint.aether.io/weight": "5"}
	informer.Update(ready, annotated)
	assert.True(t, signalled(changes), "an endpoint annotation")

	unmanaged := ready.DeepCopy()
	unmanaged.Labels = nil
	unmanagedDeleting := deleting(unmanaged.DeepCopy())
	informer.Add(unmanaged)
	informer.Update(unmanaged, unmanagedDeleting)
	assert.False(t, signalled(changes), "an unmanaged pod is never listed")

	informer.Update(ready, unmanaged)
	assert.True(t, signalled(changes), "a pod leaving the mesh")

	informer.Delete(notReady)
	assert.True(t, signalled(changes), "a managed pod gone")
}
