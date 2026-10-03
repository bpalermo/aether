package drain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func withSleep(sleep int64, grace *int64) *corev1.Pod {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	if sleep > 0 {
		pod.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: sleep}}}
	}
	pod.DeletionGracePeriodSeconds = grace
	return pod
}

func TestPoolCloseDelay(t *testing.T) {
	grace30 := int64(30)
	assert.Equal(t, 2*time.Second, PoolCloseDelay(withSleep(0, &grace30), PoolCloseFloor), "no preStop: floor")
	assert.Equal(t, 2*time.Second, PoolCloseDelay(withSleep(3, &grace30), PoolCloseFloor), "sleep 3: 1s before SIGTERM == floor")
	assert.Equal(t, 14*time.Second, PoolCloseDelay(withSleep(15, &grace30), PoolCloseFloor), "sleep 15: 1s before SIGTERM")
	assert.Equal(t, 28*time.Second, PoolCloseDelay(withSleep(60, &grace30), PoolCloseFloor), "sleep > grace: capped 2s short of hard kill")
	assert.Equal(t, 14*time.Second, PoolCloseDelay(withSleep(15, nil), PoolCloseFloor), "no grace recorded: sleep governs")
}

// TestPoolCloseAt: the request time is deletionTimestamp minus the grace the
// API server recorded; phase 2 is PoolCloseDelay after it.
func TestPoolCloseAt(t *testing.T) {
	requested := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	grace := int64(30)
	pod := withSleep(15, &grace)
	dt := metav1.NewTime(requested.Add(30 * time.Second))
	pod.DeletionTimestamp = &dt

	at, ok := PoolCloseAt(pod)
	assert.True(t, ok)
	assert.Equal(t, requested.Add(14*time.Second), at, "1 s before the 15 s preStop ends")

	_, ok = PoolCloseAt(withSleep(15, &grace))
	assert.False(t, ok, "no deletion requested")

	noGrace := withSleep(15, nil)
	noGrace.DeletionTimestamp = &dt
	_, ok = PoolCloseAt(noGrace)
	assert.False(t, ok, "no grace recorded: the request time is unknown")
}
