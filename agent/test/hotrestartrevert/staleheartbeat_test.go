package hotrestartrevert_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

// leftTheFlag is what a supervisor logs when it finds the flag set and may not
// clear it, and clearedTheFlag what it logs when it has.
const (
	leftTheFlag    = "leaving the flag"
	clearedTheFlag = "cleared a stale initializing flag"
)

// startEnvoyHeldWhileItInitializes starts the pinned Envoy at the given epoch
// and holds it where an Envoy legitimately has the "initializing" flag set:
// after it has bound its hot-restart sockets and attached the shared memory,
// before it has read its bootstrap. The bootstrap is a named pipe nobody
// writes to, so it stays there until it is killed.
//
// That is earlier than the point at which a hot-restart child asks its parent
// to close its admin, so the parent goes on answering LIVE at its own epoch:
// the node as a supervisor sees it while another pod's Envoy starts.
func (n *node) startEnvoyHeldWhileItInitializes(epoch int) *exec.Cmd {
	n.t.Helper()
	fifo := filepath.Join(n.t.TempDir(), "envoy.yaml")
	require.NoError(n.t, syscall.Mkfifo(fifo, 0o600))
	cmd := exec.Command(n.envoy, "-c", fifo, "--base-id", strconv.Itoa(int(n.baseID)),
		"--restart-epoch", strconv.Itoa(epoch), "--drain-time-s", "2", "--parent-shutdown-time-s", "4",
		"--concurrency", "1", "-l", "warn")
	out := lockedWriter{n, "[held " + strconv.Itoa(epoch) + "]"}
	cmd.Stdout, cmd.Stderr = out, out
	require.NoError(n.t, cmd.Start())
	n.t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd
}

// running reports whether the process is alive and not a zombie.
func running(cmd *exec.Cmd) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
	return err == nil && !bytes.Contains(b, []byte(") Z "))
}

// TestInitializingFlagOfAnEnvoyThatIsInitializingIsLeft is the other half of
// TestStaleInitializingFlagIsClearedBeforeAHotRestart, against the pinned
// Envoy: the flag is set and it is TRUE. An Envoy of the base id is
// initializing (another pod's hot-restart child, slow to start), the node's
// Envoy answers LIVE on the admin at the parent epoch, and a supervisor is
// about to fork at the same epoch.
//
// Clearing the flag then would take away the one thing that keeps a second
// Envoy from hot-restarting from a parent that is in the middle of a handoff.
// The supervisor must leave it, every time it looks: what tells it so is the
// hot-restart socket the initializing Envoy holds, the admin cannot.
//
// When that Envoy is then killed where it stands, the same flag is stale, and
// the supervisor's next look clears it: the case sharedmemory.go names second
// ("a hot-restart child killed during its initialization").
func TestInitializingFlagOfAnEnvoyThatIsInitializingIsLeft(t *testing.T) {
	n := newNode(t)
	a := n.start("A", podOpts{})
	n.waitAdminEpoch(0, 20*time.Second, "A's envoy never went LIVE at epoch 0")
	require.Eventually(t, a.ready, 10*time.Second, 50*time.Millisecond, "A never Ready")
	require.False(t, n.shmInitializing(), "a LIVE envoy with no successor leaves the flag clear")

	n.note("another pod's envoy starts at epoch 1 and is held while it initializes")
	held := n.startEnvoyHeldWhileItInitializes(1)
	require.Eventually(t, n.shmInitializing, 10*time.Second, 10*time.Millisecond,
		"the held envoy never set the initializing flag: the pinned Envoy no longer attaches the shared memory "+
			"before it reads its bootstrap, and this test needs another way to hold it")
	require.True(t, running(held), "the held envoy exited")
	require.Equal(t, 0, n.adminEpoch(), "the parent must still answer LIVE at epoch 0: the admin alone says the flag is stale")

	n.note("pod C starts: it will fork at epoch 1 with the flag set and true")
	c := n.start("C", podOpts{})
	// The Envoy C forks exits on the socket the held one has bound, and C
	// looks again on its retry, 3 s later: two looks, the flag left at both.
	n.waitLog(lostAtOnce, 20*time.Second)
	time.Sleep(4 * time.Second)
	require.True(t, running(held), "the held envoy exited")
	require.True(t, n.shmInitializing(),
		"the supervisor cleared the initializing flag of an envoy that is initializing")
	require.False(t, n.logged(clearedTheFlag), "the supervisor cleared the initializing flag of an envoy that is initializing")
	require.True(t, n.logged(leftTheFlag), "the supervisor did not look at the flag before it forked")
	require.Equal(t, 0, n.adminEpoch(), "the node's envoy must be untouched")
	require.True(t, n.listenerAccepts())
	_, cExited := c.returned()
	require.False(t, cExited, "C's supervisor exited")

	n.note("the held envoy is killed while it initializes")
	require.NoError(t, held.Process.Kill())
	_ = held.Wait()
	require.True(t, n.shmInitializing(), "nothing clears the flag for an envoy that dies while it initializes: "+
		"if the pinned Envoy now does, the repair in sharedmemory.go can be reconsidered")

	n.waitAdminEpoch(1, 30*time.Second, "C could not hot-restart from the node's envoy once the flag was stale")
	require.Eventually(t, c.ready, 30*time.Second, 50*time.Millisecond, "C never Ready")
	require.True(t, n.logged(clearedTheFlag))
	require.False(t, n.shmInitializing())
	require.True(t, n.listenerAccepts())
	require.Equal(t, 1, c.liveEnvoys())
	_, cExited = c.returned()
	require.False(t, cExited, "C's supervisor exited")
}
