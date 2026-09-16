package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowHealthGateway serves the health gateway contract answering 404 (pod not
// programmed yet) to every probe after a per-request delay. The delay stretches
// one liveness pass over many milliseconds so a concurrent write can land in
// the middle of it — which is the whole point of TestLivenessReadRacesTerminationWrite.
func slowHealthGateway(t *testing.T, delay time.Duration) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hg-slow")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusNotFound)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// TestLivenessReadRacesTerminationWrite is the regression test for S1 (#772).
//
// The termination watch marking a pod terminating and the liveness tick reading
// that same flag used to touch the SAME proto: agent/storage handed both
// goroutines the cache's own pointer, because its RWMutex protects the map, not
// the values. The two accesses are
//
//	liveness.go    `if isIgnorablePod(pod) || pod.GetTerminating()`  (read, no lock —
//	               the tick released storage's RLock inside GetAll)
//	termination.go `cur.Terminating = true`                          (write, under
//	               lifecycleMu, which the tick's read does not take)
//
// and nothing orders them. Under `--config=race` this test failed on the pre-fix
// tree and passes now that storage deep-copies every value it hands out.
//
// Shape of the interleave: the health gateway answers 404 for every probe (so
// the tick walks the whole GetAll snapshot instead of aborting on the first pod)
// and does so slowly, so one pass spans hundreds of milliseconds. The writer
// marks batches of pods part-way into a pass: the reader took its RLock at the
// start of the pass, so the reads it performs after the write are not ordered
// against it by any mutex.
func TestLivenessReadRacesTerminationWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	const (
		pods       = 16
		batch      = 4
		probeDelay = 5 * time.Millisecond
		writeAfter = 25 * time.Millisecond
	)

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	for i := range pods {
		p := validCNIPod("pod-"+strconv.Itoa(i), "default", "container-"+strconv.Itoa(i))
		require.NoError(t, store.AddResource(ctx, types.ContainerID(p.GetContainerId()), p))
	}

	s := newTestCNIServer(nil, store, &unregisterRecordingRegistry{},
		cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), slowHealthGateway(t, probeDelay))

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Reader: the liveness tick, reading Terminating off every pod of its snapshot.
	go func() {
		defer wg.Done()
		state := newLivenessState()
		for {
			select {
			case <-done:
				return
			default:
			}
			s.reconcileLiveness(ctx, state)
		}
	}()

	// Writer: the termination watch, marking pods terminating in place. Each pod
	// is marked exactly once (handlePodTerminating is a no-op on an
	// already-terminating pod), so every write lands on a pointer that has been
	// in the cache — and in the reader's snapshots — since setup.
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < pods; i += batch {
			time.Sleep(writeAfter)
			for j := i; j < i+batch && j < pods; j++ {
				s.handlePodTerminating(ctx, terminatingK8sPod("pod-"+strconv.Itoa(j), "default", "test-node"))
			}
		}
	}()

	wg.Wait()

	// The drain marking still works while all that is going on.
	stored, err := store.GetAll(ctx)
	require.NoError(t, err)
	require.Len(t, stored, pods)
	for _, p := range stored {
		assert.True(t, p.GetTerminating(), "every pod must be persisted as terminating: %s", p.GetName())
	}
}
