package hotrestartrevert_test

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// neverReadyWithoutItsEnvoy samples, until stop is closed, that no pod stays
// Ready with no Envoy of its own, and returns the longest such stretch. A pod
// clears its marker on the liveness tick after its Envoy exits (one second
// apart), so a short stretch is expected and a long one is the defect: a pod
// marked Ready by another pod's Envoy answering at its epoch number.
func neverReadyWithoutItsEnvoy(stop <-chan struct{}, pods ...*pod) <-chan time.Duration {
	out := make(chan time.Duration, 1)
	go func() {
		var longest time.Duration
		since := make([]time.Time, len(pods))
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				out <- longest
				return
			case now := <-tk.C:
				for i, p := range pods {
					if !(p.ready() && p.liveEnvoys() == 0) {
						since[i] = time.Time{}
						continue
					}
					if since[i].IsZero() {
						since[i] = now
					}
					longest = max(longest, now.Sub(since[i]))
				}
			}
		}
	}()
	return out
}

// TestRevertedWorkerCountRollLeavesTheNodeServed is issue #1679: a roll that
// changes the Envoy worker count, reverted while its surge pod starts.
//
// The surge pod does not hot-restart across a worker-count change (#1136): it
// asks the kept pod's Envoy to drain its listeners, waits the drain time,
// stops it and starts a fresh one. The DaemonSet deletes the surge pod during
// that wait. It used to stop there: the kept pod's Envoy closed its listeners
// a moment later and stayed LIVE on its admin, the kept pod stayed Ready, and
// the node served nothing ("after 25s: A ready=true, admin LIVE epoch=0,
// listener serves=false").
//
// A supervisor that has asked a predecessor to drain finishes the replacement
// before it obeys its own termination. The node then has the surge pod's
// Envoy, in a pod that is terminating, and the kept pod takes it back as after
// any reverted roll.
func TestRevertedWorkerCountRollLeavesTheNodeServed(t *testing.T) {
	n := newNode(t)
	a := n.start("A", podOpts{concurrency: 1})
	n.waitAdminEpoch(0, 20*time.Second, "A's envoy never went LIVE at epoch 0")
	require.Eventually(t, a.ready, 10*time.Second, 50*time.Millisecond, "A never Ready")

	b := n.start("B", podOpts{concurrency: 2})
	n.waitLog("NOT hot-restarting: draining the predecessor", 20*time.Second)
	stopSampling := make(chan struct{})
	readyWithoutEnvoy := neverReadyWithoutItsEnvoy(stopSampling, a, b)
	n.note("deleting surge pod B during its drain-time wait")
	b.cancel()
	deleted := time.Now()

	served := false
	for end := time.Now().Add(50 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		_, bExited := b.returned()
		if bExited && a.ready() && a.liveEnvoys() == 1 && b.liveEnvoys() == 0 && n.listenerAccepts() {
			served = true
			break
		}
	}
	n.note("%s after B's deletion: A ready=%v envoys=%d; B envoys=%d; admin LIVE epoch %d; listener serves=%v",
		time.Since(deleted).Round(100*time.Millisecond), a.ready(), a.liveEnvoys(), b.liveEnvoys(), n.adminEpoch(), n.listenerAccepts())
	require.True(t, served, "the node is not served by the kept pod: its envoy was drained by a surge pod that was then deleted")

	// And it stays that way.
	time.Sleep(3 * time.Second)
	close(stopSampling)
	require.True(t, a.ready() && a.liveEnvoys() == 1 && n.listenerAccepts(),
		"the kept pod's envoy closed its listeners after the surge pod that had asked it to drain was deleted: "+
			"A ready=%v, listener serves=%v", a.ready(), n.listenerAccepts())
	_, aExited := a.returned()
	require.False(t, aExited, "A's supervisor exited")
	require.Equal(t, 1, totalLiveEnvoys(a, b), "exactly one envoy must hold the node")
	require.True(t, n.logged("finishing the replacement first"), "the surge pod did not finish the replacement it had started")
	require.LessOrEqual(t, <-readyWithoutEnvoy, 2500*time.Millisecond,
		"a pod was Ready with no envoy of its own: another lineage LIVE at its epoch number made it so")
}

// TestDrainedEnvoyLeftBehindIsHotRestartedInPlace is the other side of issue
// #1679: the pod whose Envoy was asked to drain by a supervisor that then
// never stopped it (killed outright mid-way, or one older than the fix above).
// The Envoy answers LIVE on its admin and serves nothing.
//
// The pod must stop reporting Ready when its listeners close, and restore them
// itself when nobody replaces the Envoy: an in-place hot restart, whose child
// binds the listeners again.
func TestDrainedEnvoyLeftBehindIsHotRestartedInPlace(t *testing.T) {
	n := newNode(t)
	a := n.start("A", podOpts{})
	n.waitAdminEpoch(0, 20*time.Second, "A's envoy never went LIVE at epoch 0")
	require.Eventually(t, a.ready, 10*time.Second, 50*time.Millisecond, "A never Ready")
	// The listener watch takes its baseline once the pod is Ready.
	time.Sleep(3 * time.Second)

	n.note("a supervisor that will never come back asks A's envoy to drain")
	resp, err := http.Post("http://"+n.admin+"/drain_listeners?graceful", "", nil)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Eventually(t, func() bool { return !n.listenerAccepts() }, 10*time.Second, 50*time.Millisecond,
		"the drained envoy still serves")
	closed := time.Now()
	require.Equal(t, 0, n.adminEpoch(), "the drained envoy must still answer LIVE: that is the trap")

	require.Eventually(t, func() bool { return !a.ready() }, 6*time.Second, 50*time.Millisecond,
		"the pod reports Ready with an envoy that has closed its listeners")
	n.note("A not Ready %s after its listeners closed", time.Since(closed).Round(100*time.Millisecond))

	require.Eventually(t, func() bool { return n.listenerAccepts() && a.ready() && n.adminEpoch() == 1 },
		30*time.Second, 50*time.Millisecond, "the pod never restored its listeners")
	n.note("node served again %s after its listeners closed", time.Since(closed).Round(100*time.Millisecond))
	require.True(t, n.logged("trigger=listeners_stopped"))
	// The drained parent is terminated by its child at parent-shutdown time,
	// and nothing asks for a second restart.
	require.Eventually(t, func() bool { return a.liveEnvoys() == 1 }, 15*time.Second, 50*time.Millisecond)
	time.Sleep(4 * time.Second)
	_, aExited := a.returned()
	require.False(t, aExited, "A's supervisor exited")
	require.True(t, a.ready() && n.listenerAccepts() && a.liveEnvoys() == 1)
	require.Equal(t, 1, n.adminEpoch(), "the restored envoy was restarted again")
}
