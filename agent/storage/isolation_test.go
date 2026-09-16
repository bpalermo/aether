package storage

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// benchPod returns a CNIPod the size of a real node-local record: a handful of
// IPs and the label/annotation maps a meshed pod actually carries (the managed
// label, the health-check-mode annotation, plus the kubectl/checksum noise every
// Deployment pod picks up). Clone cost is dominated by those two maps.
func benchPod(i int) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "workload-deadbeef-" + strconv.Itoa(i),
		Namespace:        "default",
		NetworkNamespace: "/var/run/netns/cni-0f9a1b2c-3d4e-5f60-7182-93a4b5c6d7e8",
		ContainerId:      fmt.Sprintf("containerd://%064d", i),
		Ips:              []string{"10.244.3." + strconv.Itoa(i%250), "fd00::" + strconv.Itoa(i%250)},
		ServiceAccount:   "workload",
		Uid:              "0f9a1b2c-3d4e-5f60-7182-93a4b5c6d7e8",
		Labels: map[string]string{
			"aether.io/managed":            "true",
			"app.kubernetes.io/name":       "workload",
			"app.kubernetes.io/instance":   "workload-prod",
			"pod-template-hash":            "7d9fbb6c84",
			"app.kubernetes.io/component":  "api",
			"app.kubernetes.io/managed-by": "Helm",
		},
		Annotations: map[string]string{
			"endpoint.aether.io/health-check-mode":     "eds",
			"kubectl.kubernetes.io/default-container":  "workload",
			"checksum/config":                          "9f2b1c4d6e8a0b2c4d6e8a0b2c4d6e8a0b2c4d6e",
			"prometheus.io/scrape":                     "true",
			"prometheus.io/port":                       "9090",
			"capture.aether.io/exclude-outbound-ports": "9090",
			"endpoint.aether.io/uds-socket":            "sockets/app.sock",
		},
	}
}

func newCNIPod() *cniv1.CNIPod { return &cniv1.CNIPod{} }

// TestGetResourceReturnsIsolatedCopy pins the ownership contract: what
// GetResource hands back is the caller's own copy, so mutating it cannot reach
// into the cache (the copy-on-write half of the S1 fix — the CNI termination
// path mutates and then writes back with AddResource).
func TestGetResourceReturnsIsolatedCopy(t *testing.T) {
	ctx := context.Background()
	s := NewCachedLocalStorage[*cniv1.CNIPod](t.TempDir(), newCNIPod)
	require.NoError(t, s.AddResource(ctx, types.ContainerID("c1"), benchPod(1)))

	got, err := s.GetResource(ctx, types.ContainerID("c1"))
	require.NoError(t, err)
	got.Terminating = true
	got.Labels["mutated"] = "yes"

	again, err := s.GetResource(ctx, types.ContainerID("c1"))
	require.NoError(t, err)
	assert.False(t, again.GetTerminating(), "mutating a returned value must not reach the cache")
	assert.NotContains(t, again.GetLabels(), "mutated", "maps must be deep-copied too")

	// And the write-back path still works.
	require.NoError(t, s.AddResource(ctx, types.ContainerID("c1"), got))
	after, err := s.GetResource(ctx, types.ContainerID("c1"))
	require.NoError(t, err)
	assert.True(t, after.GetTerminating())
}

// TestGetAllReturnsIsolatedCopies is the same contract for the bulk read the
// liveness loop and the ghost sweep use.
func TestGetAllReturnsIsolatedCopies(t *testing.T) {
	ctx := context.Background()
	s := NewCachedLocalStorage[*cniv1.CNIPod](t.TempDir(), newCNIPod)
	require.NoError(t, s.AddResource(ctx, types.ContainerID("c1"), benchPod(1)))

	all, err := s.GetAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	all[0].Terminating = true

	again, err := s.GetAll(ctx)
	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.False(t, again[0].GetTerminating())
}

// TestAddResourceDoesNotAliasCaller closes the other direction: the CNI ADD
// handler keeps using the request proto after storing it, so the cache must not
// be a window onto it.
func TestAddResourceDoesNotAliasCaller(t *testing.T) {
	ctx := context.Background()
	s := NewCachedLocalStorage[*cniv1.CNIPod](t.TempDir(), newCNIPod)

	pod := benchPod(1)
	require.NoError(t, s.AddResource(ctx, types.ContainerID("c1"), pod))
	pod.Terminating = true
	pod.Ips = append(pod.Ips, "10.0.0.99")

	got, err := s.GetResource(ctx, types.ContainerID("c1"))
	require.NoError(t, err)
	assert.False(t, got.GetTerminating())
	assert.NotContains(t, got.GetIps(), "10.0.0.99")
}

// TestMockStorageIsolatesValues holds the double to the same contract as the
// real implementation — otherwise a test of the CNI server's concurrent paths
// would pass on a double that hands two goroutines the same pointer.
func TestMockStorageIsolatesValues(t *testing.T) {
	ctx := context.Background()
	m := NewMockStorage[*cniv1.CNIPod]()

	pod := benchPod(1)
	require.NoError(t, m.AddResource(ctx, types.ContainerID("c1"), pod))
	pod.Terminating = true

	got, err := m.GetResource(ctx, types.ContainerID("c1"))
	require.NoError(t, err)
	assert.False(t, got.GetTerminating())
	got.Terminating = true

	all, err := m.GetAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.False(t, all[0].GetTerminating())
}

// TestConcurrentReadWhileValueMutated is the storage-level form of S1: one
// goroutine reads every stored value while another takes a value, mutates it
// and writes it back. Before the clone-on-read/write fix the two goroutines
// touched the same proto and `--config=race` failed here deterministically.
func TestConcurrentReadWhileValueMutated(t *testing.T) {
	ctx := context.Background()
	s := NewCachedLocalStorage[*cniv1.CNIPod](t.TempDir(), newCNIPod)
	for i := range 8 {
		require.NoError(t, s.AddResource(ctx, types.ContainerID(strconv.Itoa(i)), benchPod(i)))
	}

	// The reader is in-memory and cheap; the writer is not — every AddResource
	// marshals and fsyncs a file — so the write loop stays short and the reader
	// runs for as long as the writer does rather than for a fixed count. A reader
	// that finishes first would leave the writes unobserved and prove nothing.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			pods, err := s.GetAll(ctx)
			if err != nil {
				continue
			}
			for _, p := range pods {
				_ = p.GetTerminating()
				_ = len(p.GetLabels())
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer close(done)
		for i := range 40 {
			key := types.ContainerID(strconv.Itoa(i % 8))
			cur, err := s.GetResource(ctx, key)
			if err != nil {
				continue
			}
			cur.Terminating = !cur.GetTerminating()
			_ = s.AddResource(ctx, key, cur)
		}
	}()
	wg.Wait()
}

// BenchmarkGetAll measures the clone cost the fix adds to the bulk read taken
// by the 5s liveness tick and the 60s ghost sweep, at a realistic node-local
// pod count.
func BenchmarkGetAll(b *testing.B) {
	for _, n := range []int{1, 10, 50} {
		b.Run(strconv.Itoa(n)+"pods", func(b *testing.B) {
			ctx := context.Background()
			s := NewCachedLocalStorage[*cniv1.CNIPod](b.TempDir(), newCNIPod)
			for i := range n {
				require.NoError(b, s.AddResource(ctx, types.ContainerID(strconv.Itoa(i)), benchPod(i)))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				pods, err := s.GetAll(ctx)
				if err != nil || len(pods) != n {
					b.Fatal("unexpected result")
				}
			}
		})
	}
}

// BenchmarkGetResource measures the single-record read taken on every CNI
// ADD/DEL and every liveness re-check.
func BenchmarkGetResource(b *testing.B) {
	ctx := context.Background()
	s := NewCachedLocalStorage[*cniv1.CNIPod](b.TempDir(), newCNIPod)
	require.NoError(b, s.AddResource(ctx, types.ContainerID("c1"), benchPod(1)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := s.GetResource(ctx, types.ContainerID("c1")); err != nil {
			b.Fatal(err)
		}
	}
}
