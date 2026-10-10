package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const watchUnansweredMetric = "aether.agent.cni.snapshot_watch_unanswered"

// errUnanswered is what a mutator of the snapshot cache returns for a snapshot
// that was installed while an open watch was not answered: the sentinel,
// wrapped twice, as generateSnapshot and the mutator each wrap it.
func errUnanswered() error {
	build := fmt.Errorf("snapshot 7 is installed, but %w within 5s: %w", cache.ErrWatchNotAnswered, context.DeadlineExceeded)
	return fmt.Errorf("failed to regenerate snapshot: %w", build)
}

// errNotBuilt is any other error of a mutator: the snapshot was not installed.
var errNotBuilt = errors.New("failed to regenerate snapshot: inconsistent snapshot")

// fakeSnapshots is a snapshot cache whose AddPod and RemovePod return what the
// test says. The real cache returns ErrWatchNotAnswered only after waiting out
// a bound this package cannot shorten.
type fakeSnapshots struct {
	addErr, removeErr error
	added, removed    []string
}

func (f *fakeSnapshots) AddPod(_ context.Context, pod *cniv1.CNIPod, _ string) error {
	f.added = append(f.added, pod.GetName())
	return f.addErr
}

func (f *fakeSnapshots) RemovePod(_ context.Context, netns string) error {
	f.removed = append(f.removed, netns)
	return f.removeErr
}

func (f *fakeSnapshots) SetNodeLocality(_, _ string) {}

// watchTestServer is newTestCNIServer over fake snapshots, with its log and
// its metrics readable.
func watchTestServer(t *testing.T, stor storage.Storage[*cniv1.CNIPod], reg *testRegistry, snaps *fakeSnapshots) (*CNIServer, *bytes.Buffer, *sdkmetric.ManualReader) {
	t.Helper()
	k8s := fake.NewClientBuilder().WithObjects(validK8sPod("my-pod", "default")).Build()
	srv := newTestCNIServer(k8s, stor, reg, cache.NewSnapshotCache("test-node", slog.New(slog.DiscardHandler)), "")
	srv.snapshotCache = snaps
	var buf bytes.Buffer
	srv.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	metrics, reader := newTestCNIMetrics(t)
	srv.metrics = metrics
	return srv, &buf, reader
}

// unansweredByCaller reads the counter per caller.
func unansweredByCaller(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != watchUnansweredMetric {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s is %T, want a counter", watchUnansweredMetric, m.Data)
			for _, dp := range sum.DataPoints {
				caller, _ := dp.Attributes.Value(attrSnapshotCaller)
				out[caller.AsString()] = dp.Value
			}
		}
	}
	return out
}

// logLines returns the lines of buf that carry msg.
func logLines(buf *bytes.Buffer, msg string) []string {
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(line, msg) {
			out = append(out, line)
		}
	}
	return out
}

// seeded is the counter before anything happened: every caller, at zero.
func seeded(over map[string]int64) map[string]int64 {
	out := map[string]int64{snapshotCallerCNIAdd: 0, snapshotCallerCNIDel: 0, snapshotCallerTakeover: 0, snapshotCallerGhostSweep: 0}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// TestSnapshotWatchUnansweredIsSeeded: the counter is not expected to move, so
// every caller's series exists at zero from the start.
func TestSnapshotWatchUnansweredIsSeeded(t *testing.T) {
	_, reader := newTestCNIMetrics(t)
	assert.Equal(t, seeded(nil), unansweredByCaller(t, reader))
}

// TestAddPodSucceedsWhenTheSnapshotIsInstalledButAWatchWasNotAnswered is
// #1620. The pod's listeners are in the snapshot the agent serves; failing the
// ADD made the runtime tear the sandbox down and retry a pod that was meshed.
func TestAddPodSucceedsWhenTheSnapshotIsInstalledButAWatchWasNotAnswered(t *testing.T) {
	snaps := &fakeSnapshots{addErr: errUnanswered()}
	srv, buf, reader := watchTestServer(t, storage.NewMockStorage[*cniv1.CNIPod](), &testRegistry{}, snaps)

	got, err := srv.AddPod(context.Background(), &cniv1.AddPodRequest{Pod: validCNIPod("my-pod", "default", "abc123")})

	require.NoError(t, err, "the snapshot is installed: the ADD is served")
	assert.Equal(t, cniv1.AddPodResponse_RESULT_SUCCESS, got.GetResult())
	assert.Equal(t, []string{"my-pod"}, snaps.added)
	assert.Equal(t, seeded(map[string]int64{snapshotCallerCNIAdd: 1}), unansweredByCaller(t, reader))

	lines := logLines(buf, snapshotWatchUnansweredMsg)
	require.Len(t, lines, 1, "one line says what happened")
	assert.Contains(t, lines[0], `"level":"WARN"`)
	assert.Contains(t, lines[0], `"pod":"my-pod"`)
	assert.Contains(t, lines[0], `"namespace":"default"`)
	assert.Contains(t, lines[0], `"caller":"cni_add"`)
	assert.Contains(t, lines[0], cache.ErrWatchNotAnswered.Error(), "the build's own error is on the line")
}

// TestAddPodStillFailsWhenTheSnapshotWasNotBuilt: only the sentinel is
// forgiven. Any other error of the cache is an ADD that failed, with the code
// it has always had.
func TestAddPodStillFailsWhenTheSnapshotWasNotBuilt(t *testing.T) {
	snaps := &fakeSnapshots{addErr: errNotBuilt}
	srv, buf, reader := watchTestServer(t, storage.NewMockStorage[*cniv1.CNIPod](), &testRegistry{}, snaps)

	got, err := srv.AddPod(context.Background(), &cniv1.AddPodRequest{Pod: validCNIPod("my-pod", "default", "abc123")})

	require.Error(t, err)
	assert.Nil(t, got)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.Contains(t, err.Error(), "failed to add listener")
	assert.Contains(t, err.Error(), errNotBuilt.Error())
	assert.Equal(t, seeded(nil), unansweredByCaller(t, reader))
	assert.Empty(t, logLines(buf, snapshotWatchUnansweredMsg))
}

// TestRemovePodDoesNotCallAnInstalledSnapshotAFailedRemoval: a CNI DEL
// succeeds whatever the cache returns (the removal is best-effort), but its
// ERROR line says the listener was not removed. For the sentinel it was.
func TestRemovePodDoesNotCallAnInstalledSnapshotAFailedRemoval(t *testing.T) {
	const failedMsg = "failed to remove listener"
	tests := []struct {
		name       string
		removeErr  error
		wantCount  int64
		wantWarn   int
		wantFailed int
	}{
		{name: "watch not answered", removeErr: errUnanswered(), wantCount: 1, wantWarn: 1},
		{name: "snapshot not built", removeErr: errNotBuilt, wantFailed: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			stor := storage.NewMockStorage[*cniv1.CNIPod]()
			pod := validCNIPod("my-pod", "default", "abc123")
			require.NoError(t, stor.AddResource(ctx, types.ContainerID("abc123"), pod))
			snaps := &fakeSnapshots{removeErr: tt.removeErr}
			srv, buf, reader := watchTestServer(t, stor, &testRegistry{}, snaps)

			got, err := srv.RemovePod(ctx, &cniv1.RemovePodRequest{Name: "my-pod", Namespace: "default", ContainerId: "abc123"})

			require.NoError(t, err)
			assert.Equal(t, cniv1.RemovePodResponse_RESULT_SUCCESS, got.GetResult())
			assert.Equal(t, []string{pod.GetNetworkNamespace()}, snaps.removed)
			assert.Equal(t, seeded(map[string]int64{snapshotCallerCNIDel: tt.wantCount}), unansweredByCaller(t, reader))
			warns := logLines(buf, snapshotWatchUnansweredMsg)
			assert.Len(t, warns, tt.wantWarn)
			for _, line := range warns {
				assert.Contains(t, line, `"level":"WARN"`)
				assert.Contains(t, line, `"caller":"cni_del"`)
			}
			assert.Len(t, logLines(buf, failedMsg), tt.wantFailed)
		})
	}
}

// TestReconcileStorageDoesNotFailTheTakeoverStepOnAnInstalledSnapshot: the
// takeover step's error is logged as "takeover step failed". Listeners that
// are installed are not a failed step; listeners that were not built are.
func TestReconcileStorageDoesNotFailTheTakeoverStepOnAnInstalledSnapshot(t *testing.T) {
	// overlap leaves one pod ADDed and one DELed by the previous agent since
	// the standby loaded its storage.
	overlap := func(t *testing.T) *storage.CachedLocalStorage[*cniv1.CNIPod] {
		t.Helper()
		ctx := context.Background()
		podDir, storeDir := t.TempDir(), t.TempDir()
		gone := overlapPod(t, podDir, "gone", "10.0.0.2")
		added := overlapPod(t, podDir, "added", "10.0.0.3")
		old := newStorage(t, storeDir)
		require.NoError(t, old.AddResource(ctx, types.ContainerID(gone.GetContainerId()), gone))
		standby := newStorage(t, storeDir)
		require.NoError(t, old.AddResource(ctx, types.ContainerID(added.GetContainerId()), added))
		require.NoError(t, old.RemoveResource(ctx, types.ContainerID(gone.GetContainerId())))
		return standby
	}

	t.Run("watch not answered", func(t *testing.T) {
		snaps := &fakeSnapshots{addErr: errUnanswered(), removeErr: errUnanswered()}
		srv, buf, reader := watchTestServer(t, overlap(t), &testRegistry{}, snaps)

		require.NoError(t, srv.ReconcileStorage(context.Background()))

		assert.Equal(t, []string{"added"}, snaps.added)
		assert.Len(t, snaps.removed, 1)
		assert.Equal(t, seeded(map[string]int64{snapshotCallerTakeover: 2}), unansweredByCaller(t, reader))
		warns := logLines(buf, snapshotWatchUnansweredMsg)
		require.Len(t, warns, 2)
		for _, line := range warns {
			assert.Contains(t, line, `"level":"WARN"`)
			assert.Contains(t, line, `"caller":"takeover"`)
		}
	})

	t.Run("snapshot not built", func(t *testing.T) {
		snaps := &fakeSnapshots{addErr: errNotBuilt, removeErr: errNotBuilt}
		srv, buf, reader := watchTestServer(t, overlap(t), &testRegistry{}, snaps)

		err := srv.ReconcileStorage(context.Background())

		require.ErrorIs(t, err, errNotBuilt)
		assert.Contains(t, err.Error(), "building listeners of default/added")
		assert.Contains(t, err.Error(), "removing listeners of default/gone")
		assert.Equal(t, seeded(nil), unansweredByCaller(t, reader))
		assert.Empty(t, logLines(buf, snapshotWatchUnansweredMsg))
	})
}

// TestGhostSweepDoesNotCallAnInstalledSnapshotAFailedPrune: the sweep prunes
// the pod either way; its ERROR line says the listener was not dropped, which
// for the sentinel is untrue.
func TestGhostSweepDoesNotCallAnInstalledSnapshotAFailedPrune(t *testing.T) {
	const failedMsg = "ghost sweep: failed to drop listener for pruned pod"
	tests := []struct {
		name       string
		removeErr  error
		wantCount  int64
		wantWarn   int
		wantFailed int
	}{
		{name: "watch not answered", removeErr: errUnanswered(), wantCount: 1, wantWarn: 1},
		{name: "snapshot not built", removeErr: errNotBuilt, wantFailed: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			// The orphan's Kubernetes pod is gone (TestSweepPrunesOrphanedPods).
			orphan := validCNIPod("pod-orphan", "default", "container-orphan")
			orphan.Ips = []string{"10.0.0.9"}
			stor := storage.NewMockStorage[*cniv1.CNIPod]()
			require.NoError(t, stor.AddResource(ctx, types.ContainerID("container-orphan"), orphan))
			snaps := &fakeSnapshots{removeErr: tt.removeErr}
			srv, buf, reader := watchTestServer(t, stor, &testRegistry{}, snaps)
			srv.registry = &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{}}

			srv.sweepGhostEndpoints(ctx)

			_, err := stor.GetResource(ctx, types.ContainerID("container-orphan"))
			require.Error(t, err, "the orphan is pruned from storage either way")
			assert.Equal(t, []string{orphan.GetNetworkNamespace()}, snaps.removed)
			assert.Equal(t, seeded(map[string]int64{snapshotCallerGhostSweep: tt.wantCount}), unansweredByCaller(t, reader))
			warns := logLines(buf, snapshotWatchUnansweredMsg)
			assert.Len(t, warns, tt.wantWarn)
			for _, line := range warns {
				assert.Contains(t, line, `"level":"WARN"`)
				assert.Contains(t, line, `"caller":"ghost_sweep"`)
				assert.Contains(t, line, `"pod":"pod-orphan"`)
			}
			assert.Len(t, logLines(buf, failedMsg), tt.wantFailed)
		})
	}
}
