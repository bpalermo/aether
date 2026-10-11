package hotrestartrevert_test

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// lostAtOnce is what a supervisor logs when the Envoy it forked exited within
// seconds, which is how "previous envoy process is still initializing" and a
// start beside a live Envoy both end. (The pods' Envoys write to the test's
// own output, not to the node's log.)
const lostAtOnce = "retrying epoch detection in-process"

// shm returns the node's hot-restart shared memory as Envoy laid it out.
func (n *node) shm() []byte {
	n.t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/dev/shm/envoy_shared_memory_%d", uint64(n.baseID)*10))
	require.NoError(n.t, err)
	return b
}

// shmInitializing reads the "initializing" flag where the supervisor reads it:
// bit 0 of the segment's last 8 bytes.
func (n *node) shmInitializing() bool {
	b := n.shm()
	return binary.NativeEndian.Uint64(b[len(b)-8:])&1 != 0
}

// TestStaleHeartbeatStartDoesNotPoisonTheLivePredecessor is issue #1680.
//
// The node's Envoy (epoch 1) is alive and its heartbeat is stale: here its
// main thread is stopped, so its supervisor's LIVE-gated heartbeat stops; on a
// node it is a stalled supervisor or admin probes timing out under load. A new
// pod starts then. It used to read "no live predecessor" and fork an Envoy at
// epoch 0, which does not collide with a live epoch 1 on the hot-restart
// socket: it replaced the shared memory, died on the admin address and left
// the "initializing" flag set. When the predecessor answered again, every hot
// restart from it exited with "previous envoy process is still initializing".
//
// The new pod must fork nothing while an Envoy holds the base id, and
// hot-restart from the predecessor once its heartbeat is back.
func TestStaleHeartbeatStartDoesNotPoisonTheLivePredecessor(t *testing.T) {
	n := newNode(t)
	a, b := rollUntilSuperseded(t, n, podOpts{adminUnresponsive: 5 * time.Minute}, podOpts{adminUnresponsive: 5 * time.Minute})
	require.Eventually(t, func() bool { return a.liveEnvoys() == 0 }, 30*time.Second, 50*time.Millisecond)
	require.Eventually(t, b.ready, 30*time.Second, 50*time.Millisecond, "B never Ready")
	// The old pod is deleted, as a completed roll does.
	a.cancel()

	n.note("stalling B's envoy (SIGSTOP) until the heartbeat is stale")
	b.signalEnvoys(syscall.SIGSTOP)
	time.Sleep(10 * time.Second)
	require.False(t, n.heartbeatFresh(), "the heartbeat must be stale for this test to mean anything")

	n.note("the next roll: pod C starts beside the live envoy whose heartbeat is stale")
	c := n.start("C", podOpts{adminUnresponsive: 5 * time.Minute})
	time.Sleep(4 * time.Second)
	b.signalEnvoys(syscall.SIGCONT)
	n.note("B's envoy resumed (SIGCONT)")

	attached := false
	for end := time.Now().Add(40 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if err, exited := c.returned(); exited {
			t.Fatalf("C's supervisor exited: %v", err)
		}
		if n.adminEpoch() == 2 && c.ready() {
			attached = true
			break
		}
	}
	n.note("C ready=%v envoys=%d; admin LIVE epoch %d; an envoy of C's exited at once=%v",
		c.ready(), c.liveEnvoys(), n.adminEpoch(), n.logged(lostAtOnce))
	require.False(t, n.logged(lostAtOnce),
		"C started an envoy that exited at once: at epoch 0 beside the live one, which leaves its shared memory marked "+
			"initializing, or into that flag afterwards")
	require.True(t, attached, "the next roll's pod never took over from the live predecessor")
	require.True(t, n.logged("not starting at epoch 0 beside it"))
	require.True(t, n.listenerAccepts())
	require.Equal(t, 1, c.liveEnvoys())
}

// TestStaleInitializingFlagIsClearedBeforeAHotRestart: the node is already in
// the state above (a supervisor older than that fix, or a hot-restart child
// killed while it initialized): the healthy Envoy serves, and the shared
// memory says an Envoy is initializing that no longer exists. The next pod
// must be able to take over without anyone deleting the old pod by hand.
//
// It also holds the supervisor to the pinned Envoy's shared-memory layout: the
// flag it clears is bit 0 of the segment's last 8 bytes.
func TestStaleInitializingFlagIsClearedBeforeAHotRestart(t *testing.T) {
	n := newNode(t)
	a, b := rollUntilSuperseded(t, n, podOpts{}, podOpts{})
	require.Eventually(t, func() bool { return a.liveEnvoys() == 0 }, 30*time.Second, 50*time.Millisecond)
	require.Eventually(t, b.ready, 30*time.Second, 50*time.Millisecond, "B never Ready")
	a.cancel()
	require.False(t, n.shmInitializing(), "a LIVE envoy with no successor leaves the flag clear")
	if runtime.GOARCH == "amd64" {
		require.Len(t, n.shm(), 104, "the pinned Envoy's shared-memory segment changed size: see sharedmemory.go")
	}

	n.note("a stray epoch 0 beside B's live epoch 1")
	stray := n.runEnvoy(0, lockedWriter{n, "[stray 0]"})
	require.Error(t, stray.Wait())
	require.Equal(t, 1, n.adminEpoch())
	require.True(t, n.shmInitializing(), "the stray epoch 0 did not leave the flag set where the supervisor reads it")

	n.note("the next roll: starting pod C")
	c := n.start("C", podOpts{})
	n.waitAdminEpoch(2, 30*time.Second, "the next roll's pod could not hot-restart from the predecessor")
	require.Eventually(t, c.ready, 30*time.Second, 50*time.Millisecond, "C never Ready")
	require.True(t, n.logged("cleared a stale initializing flag"))
	require.False(t, n.logged(lostAtOnce), "an envoy was forked into the stale flag")
	require.False(t, n.shmInitializing())
	require.True(t, n.listenerAccepts())
	_, cExited := c.returned()
	require.False(t, cExited, "C's supervisor exited")
}
