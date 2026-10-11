// Package hotrestartrevert_test runs REAL proxy supervisors and the pinned
// Envoy as the proxy pods of one node: a shared base id, state directory, admin
// address and listener, which is what hostNetwork and the hostPath give two
// pods of the aether-proxy DaemonSet.
//
// It holds what a stub Envoy cannot: that a roll reverted while its surge pod
// starts is taken back without a request lost, that the recoveries of
// agent/internal/proxy/hotrestart/superseded.go leave exactly one Envoy and one
// Ready pod, and what the pinned Envoy does to a node when it is started at
// epoch 0 beside a live one (the reason those recoveries never use a fork to
// find out whether the node has an Envoy). It began as the harness of the
// adversarial review of that change.
package hotrestartrevert_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/proxy/hotrestart"
	"aethermesh.dev/agent/test/envoybin"
	"github.com/stretchr/testify/require"
)

const bootstrapTmpl = `admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: %d }
static_resources:
  listeners:
  - name: l
    address: { socket_address: { address: 127.0.0.1, port_value: %d } }
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: x
          route_config:
            virtual_hosts:
            - name: v
              domains: ["*"]
              routes:
              - match: { prefix: "/" }
                direct_response: { status: 200, body: { inline_string: "ok" } }
          http_filters:
          - name: envoy.filters.http.router
            typed_config: { "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router }
`

func pinnedEnvoy(t *testing.T) string {
	t.Helper()
	bin, err := envoybin.Path()
	var unsupported *envoybin.ErrUnsupportedArch
	if errors.As(err, &unsupported) {
		t.Skipf("skipping: %v", err)
	}
	require.NoError(t, err)
	return bin
}

// nextPort is where freePort looks next: below the kernel's range for ports it
// assigns itself (32768 and up on Linux), starting at a place of this process's
// own.
var nextPort atomic.Int64

// freePort returns a loopback port nothing listens on, for an Envoy to bind a
// moment later.
//
// Not a port the kernel hands out for ":0". The test's shards run side by side
// and every one of them makes a connection per request from such a port, so a
// port found free that way was sometimes taken before the Envoy bound it: its
// admin failed with "Address already in use", and the test went on to read
// another shard's Envoy on that port as its own.
func freePort(t *testing.T) int {
	t.Helper()
	const lo, span = 10000, 20000
	nextPort.CompareAndSwap(0, int64(lo+(os.Getpid()*7919)%span))
	for range span {
		port := lo + int(nextPort.Add(1))%span
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		require.NoError(t, l.Close())
		return port
	}
	t.Fatal("no free loopback port below the ephemeral range")
	return 0
}

// node is what two proxy pods share on a host.
type node struct {
	t         *testing.T
	envoy     string
	bootstrap string
	baseID    uint32
	stateDir  string
	admin     string
	listener  string
	logMu     sync.Mutex
	logBuf    bytes.Buffer
}

func newNode(t *testing.T) *node {
	t.Helper()
	n := &node{t: t, envoy: pinnedEnvoy(t), stateDir: t.TempDir()}
	adminPort, lPort := freePort(t), freePort(t)
	n.admin = "127.0.0.1:" + strconv.Itoa(adminPort)
	n.listener = "127.0.0.1:" + strconv.Itoa(lPort)
	n.bootstrap = filepath.Join(t.TempDir(), "envoy.yaml")
	require.NoError(t, os.WriteFile(n.bootstrap, []byte(fmt.Sprintf(bootstrapTmpl, adminPort, lPort)), 0o600))
	n.baseID = uint32(20000 + (os.Getpid()*7+adminPort)%20000)
	// Envoy never unlinks its shared memory; base id * 10 is its name.
	t.Cleanup(func() { _ = os.Remove(fmt.Sprintf("/dev/shm/envoy_shared_memory_%d", uint64(n.baseID)*10)) })
	return n
}

type lockedWriter struct {
	n      *node
	prefix string
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.n.logMu.Lock()
	defer w.n.logMu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		ts := time.Now().Format("15:04:05.000")
		fmt.Fprintf(&w.n.logBuf, "%s %s %s\n", ts, w.prefix, line)
		fmt.Fprintf(os.Stderr, "%s %s %s\n", ts, w.prefix, line)
	}
	return len(p), nil
}

func (n *node) note(format string, args ...any) {
	_, _ = lockedWriter{n, "[test]"}.Write([]byte(fmt.Sprintf(format, args...)))
}

// pod is one proxy pod: a real supervisor and whatever Envoys it forks.
type pod struct {
	name    string
	n       *node
	sup     *hotrestart.Supervisor
	cancel  context.CancelFunc
	done    chan error
	marker  string
	pidFile string
}

type podOpts struct {
	adminUnresponsive time.Duration
	terminationGrace  time.Duration
	concurrency       int
	// envoyNeverStarts makes every Envoy the pod's supervisor forks a process
	// that never becomes one: it holds no hot-restart socket, binds nothing
	// and answers nothing, until it is signalled.
	envoyNeverStarts bool
}

// start launches the pod's supervisor. Envoy is run through a wrapper that
// records each child's PID, so the test can SIGKILL "the container".
func (n *node) start(name string, o podOpts) *pod {
	n.t.Helper()
	dir := n.t.TempDir()
	p := &pod{name: name, n: n, marker: filepath.Join(dir, "ready"), pidFile: filepath.Join(dir, "pids"), done: make(chan error, 1)}
	wrapper := filepath.Join(dir, "envoy-wrapper.sh")
	run := "exec " + n.envoy + " \"$@\""
	if o.envoyNeverStarts {
		run = "exec sleep 3600"
	}
	require.NoError(n.t, os.WriteFile(wrapper, []byte("#!/bin/sh\necho $$ >> "+p.pidFile+"\n"+run+"\n"), 0o755))
	if o.adminUnresponsive == 0 {
		o.adminUnresponsive = 5 * time.Second
	}
	if o.terminationGrace == 0 {
		o.terminationGrace = 60 * time.Second
	}
	log := slog.New(slog.NewTextHandler(lockedWriter{n, "[sup " + name + "]"}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p.sup = hotrestart.New(hotrestart.Config{
		EnvoyPath:                 wrapper,
		ConfigPath:                n.bootstrap,
		BaseID:                    n.baseID,
		DrainTime:                 3 * time.Second,
		ParentShutdownTime:        6 * time.Second,
		ExtraArgs:                 []string{"--concurrency", strconv.Itoa(max(o.concurrency, 1)), "-l", "warn"},
		StateDir:                  n.stateDir,
		ReadyMarkerPath:           p.marker,
		AdminAddress:              n.admin,
		AdminUnresponsiveDeadline: o.adminUnresponsive,
		TerminationGrace:          o.terminationGrace,
		PodName:                   name,
	}, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go func() {
		err := p.sup.Run(ctx)
		n.note("pod %s: Run returned: %v", name, err)
		p.done <- err
	}()
	n.t.Cleanup(func() { cancel(); p.killEnvoys() })
	return p
}

func (p *pod) pids() []int {
	b, _ := os.ReadFile(p.pidFile)
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

func (p *pod) killEnvoys() {
	for _, pid := range p.pids() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func (p *pod) signalEnvoys(sig syscall.Signal) {
	for _, pid := range p.pids() {
		_ = syscall.Kill(pid, sig)
	}
}

func (n *node) logged(sub string) bool {
	n.logMu.Lock()
	defer n.logMu.Unlock()
	return strings.Contains(n.logBuf.String(), sub)
}

func (n *node) waitLog(sub string, within time.Duration) {
	n.t.Helper()
	require.Eventually(n.t, func() bool { return n.logged(sub) }, within, 10*time.Millisecond, "never logged: "+sub)
}

// listenerAccepts reports whether the node's data-plane listener serves.
func (n *node) listenerAccepts() bool {
	c := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://" + n.listener + "/")
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// strayThenRoll: the kept pod A is superseded by a healthy successor B whose
// admin then goes silent for longer than A's bound (its main thread is stalled:
// SIGSTOP), so A's successor_gone recovery starts a fresh epoch 0. B's Envoy
// resumes. The NEXT roll's pod C must still be able to hot-restart from B.
// totalLiveEnvoys counts the Envoy processes of every pod.
func totalLiveEnvoys(pods ...*pod) int {
	n := 0
	for _, p := range pods {
		n += p.liveEnvoys()
	}
	return n
}

// TestThreeGenerations: A superseded by B, B superseded by C, neither A
// nor B deleted yet (a controller that is slow to delete), then C is deleted.
// Both A and B read C's request. Exactly one Envoy must end up holding the
// node, and a later ordinary roll must still work.
func TestThreeGenerations(t *testing.T) {
	n := newNode(t)
	a, b := rollUntilSuperseded(t, n, podOpts{}, podOpts{})
	require.Eventually(t, b.ready, 30*time.Second, 50*time.Millisecond, "B never Ready")
	c := n.start("C", podOpts{})
	n.waitAdminEpoch(2, 20*time.Second, "C's envoy never went LIVE at epoch 2")
	require.Eventually(t, c.ready, 30*time.Second, 50*time.Millisecond, "C never Ready")
	require.Zero(t, a.liveEnvoys()+b.liveEnvoys())
	tr := n.startTraffic()

	n.note("deleting pod C; A and B are both superseded and not deleted")
	c.cancel()
	n.waitAdminEpoch(3, 40*time.Second, "nobody took the node back at epoch 3")
	require.Eventually(t, func() bool { _, ok := c.returned(); return ok }, 40*time.Second, 50*time.Millisecond,
		"C's supervisor never exited")
	require.Eventually(t, func() bool { return a.ready() || b.ready() }, 30*time.Second, 50*time.Millisecond)
	time.Sleep(3 * time.Second)
	n.note("A: ready=%v envoys=%d; B: ready=%v envoys=%d; admin LIVE epoch %d",
		a.ready(), a.liveEnvoys(), b.ready(), b.liveEnvoys(), n.adminEpoch())
	require.Equal(t, 1, totalLiveEnvoys(a, b, c), "exactly one envoy must hold the node")
	require.False(t, a.ready() && b.ready(), "two pods are Ready with one envoy between them")
	for _, p := range []*pod{a, b} {
		_, exited := p.returned()
		require.False(t, exited, "pod %s's supervisor exited", p.name)
		require.Equal(t, p.liveEnvoys() == 1, p.ready(), "pod %s: Ready must mean it runs the envoy", p.name)
	}

	// An ordinary roll afterwards.
	d := n.start("D", podOpts{})
	n.waitAdminEpoch(4, 40*time.Second, "the next roll's pod never attached at epoch 4")
	require.Eventually(t, d.ready, 30*time.Second, 50*time.Millisecond, "D never Ready")
	time.Sleep(2 * time.Second)
	require.Equal(t, 1, totalLiveEnvoys(a, b, c, d))
	require.Equal(t, 1, d.liveEnvoys())
	require.False(t, a.ready() || b.ready(), "an old pod is Ready after the roll: A=%v B=%v", a.ready(), b.ready())
	ok, failed, first, last := tr.end()
	t.Logf("traffic: %d ok, %d failed (first %q last %q)", ok, failed, first, last)
	require.Zero(t, failed)
	// The roll deletes the old pods: they must exit at once.
	a.cancel()
	b.cancel()
	for _, p := range []*pod{a, b} {
		require.Eventually(t, func() bool { _, ok := p.returned(); return ok }, 10*time.Second, 50*time.Millisecond,
			"pod %s did not exit on deletion", p.name)
	}
	require.Equal(t, 4, n.adminEpoch())
}

// TestNewPodDuringTheTerminatingPodsDrain: a pod is deleted and no successor
// comes within its wait budget (12 s here, 155 s at the defaults), so it
// drains. A new pod that starts during that drain reads the node epoch
// heartbeat. A terminating supervisor refreshes it while it asks for a
// successor, and must have stopped long enough before the drain for it to be
// stale by then: with a fresh one the new pod hot-restarted from an Envoy that
// was SIGTERMed under it, and its own Envoy looped on "hot restart sendmsg()
// connection refused" until the admin watchdog killed it.
func TestNewPodDuringTheTerminatingPodsDrain(t *testing.T) {
	n := newNode(t)
	// budget = grace - (drain 3s + 5s) - 10s = 12s
	a := n.start("A", podOpts{terminationGrace: 30 * time.Second})
	n.waitAdminEpoch(0, 20*time.Second, "A's envoy never went LIVE at epoch 0")
	require.Eventually(t, a.ready, 10*time.Second, 50*time.Millisecond, "A never Ready")
	n.note("deleting pod A; no successor for the whole budget")
	a.cancel()
	n.waitLog("no successor within budget; draining listeners", 30*time.Second)
	time.Sleep(300 * time.Millisecond)
	n.note("a new pod D starts during A's drain; heartbeat fresh=%v", n.heartbeatFresh())
	started := time.Now()
	d := n.start("D", podOpts{})
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && !(d.ready() && n.listenerAccepts()) {
		if err, ok := d.returned(); ok {
			t.Fatalf("D's supervisor exited %s after it started: %v", time.Since(started).Round(100*time.Millisecond), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	n.note("D ready=%v listener=%v after %s; admin LIVE epoch %d", d.ready(), n.listenerAccepts(),
		time.Since(started).Round(100*time.Millisecond), n.adminEpoch())
	require.True(t, d.ready() && n.listenerAccepts(), "the new pod never served")
	// It must stay that way: its own envoy, alive, supervisor running, once the
	// terminating pod is gone.
	require.Eventually(t, func() bool { _, ok := a.returned(); return ok }, 20*time.Second, 50*time.Millisecond)
	end := time.Now().Add(15 * time.Second)
	for time.Now().Before(end) {
		if err, ok := d.returned(); ok {
			t.Fatalf("D's supervisor exited %s after it started: %v (its envoy was SIGKILLed with it)",
				time.Since(started).Round(100*time.Millisecond), err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	n.note("at the end: D ready=%v envoys=%d listener=%v admin LIVE epoch %d", d.ready(), d.liveEnvoys(), n.listenerAccepts(), n.adminEpoch())
	require.Equal(t, 1, d.liveEnvoys())
	require.True(t, d.ready() && n.listenerAccepts())
}

func (n *node) heartbeatFresh() bool {
	b, err := os.ReadFile(filepath.Join(n.stateDir, "epoch"))
	f := strings.Fields(string(b))
	if err != nil || len(f) != 2 {
		return false
	}
	ms, err := strconv.ParseInt(f[1], 10, 64)
	return err == nil && time.Since(time.UnixMilli(ms)) < 2*time.Second
}

func (p *pod) liveEnvoys() int {
	n := 0
	for _, pid := range p.pids() {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err == nil && !strings.Contains(string(b), ") Z ") {
			n++
		}
	}
	return n
}

func (p *pod) ready() bool { _, err := os.Stat(p.marker); return err == nil }

func (p *pod) returned() (error, bool) {
	select {
	case err := <-p.done:
		p.done <- err
		return err, true
	default:
		return nil, false
	}
}

type serverInfo struct {
	State              string `json:"state"`
	CommandLineOptions struct {
		RestartEpoch int `json:"restart_epoch"`
	} `json:"command_line_options"`
}

// adminEpoch returns the epoch LIVE on the node's admin, or -1.
func (n *node) adminEpoch() int {
	c := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://" + n.admin + "/server_info")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var si serverInfo
	if json.Unmarshal(b, &si) != nil || si.State != "LIVE" {
		return -1
	}
	return si.CommandLineOptions.RestartEpoch
}

func (n *node) waitAdminEpoch(epoch int, within time.Duration, why string) {
	n.t.Helper()
	require.Eventually(n.t, func() bool { return n.adminEpoch() == epoch }, within, 50*time.Millisecond, why)
}

// traffic sends one request per interval to the node's listener on a fresh
// connection and records every failure.
type traffic struct {
	ok, failed atomic.Int64
	mu         sync.Mutex
	failures   []string
	stop       chan struct{}
	done       chan struct{}
}

func (n *node) startTraffic() *traffic {
	tr := &traffic{stop: make(chan struct{}), done: make(chan struct{})}
	c := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	go func() {
		defer close(tr.done)
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-tr.stop:
				return
			case <-tk.C:
			}
			resp, err := c.Get("http://" + n.listener + "/")
			if err != nil {
				tr.failed.Add(1)
				tr.mu.Lock()
				tr.failures = append(tr.failures, time.Now().Format("15:04:05.000")+" "+err.Error())
				tr.mu.Unlock()
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			tr.ok.Add(1)
		}
	}()
	return tr
}

func (tr *traffic) end() (ok, failed int64, first, last string) {
	close(tr.stop)
	<-tr.done
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.failures) > 0 {
		first, last = tr.failures[0], tr.failures[len(tr.failures)-1]
	}
	return tr.ok.Load(), tr.failed.Load(), first, last
}

// rollUntilSuperseded brings the node to the state the reverted roll leaves:
// kept pod A (epoch 0) and surge pod B, whose Envoy (epoch 1) has taken the
// node.
func rollUntilSuperseded(t *testing.T, n *node, oa, ob podOpts) (a, b *pod) {
	t.Helper()
	a = n.start("A", oa)
	n.waitAdminEpoch(0, 20*time.Second, "A's envoy never went LIVE at epoch 0")
	require.Eventually(t, a.ready, 10*time.Second, 50*time.Millisecond, "A never Ready")
	b = n.start("B", ob)
	n.waitAdminEpoch(1, 20*time.Second, "B's envoy never went LIVE at epoch 1")
	return a, b
}

// TestRevertedRollIsTakenBackWithoutLosingARequest is the roll reverted while
// its surge pod starts (nightly e2e, 2026-10-11): the surge pod is deleted
// right after its Envoy took the node, and the kept pod hot-restarts from it
// (successor_terminating). One request every 20 ms throughout, none may fail.
func TestRevertedRollIsTakenBackWithoutLosingARequest(t *testing.T) {
	n := newNode(t)
	a, b := rollUntilSuperseded(t, n, podOpts{}, podOpts{})
	tr := n.startTraffic()
	// The DaemonSet deletes the surge pod: SIGTERM, long grace.
	n.note("deleting surge pod B")
	b.cancel()

	n.waitAdminEpoch(2, 40*time.Second, "A never took the node back at epoch 2")
	require.Eventually(t, a.ready, 30*time.Second, 50*time.Millisecond, "A never went Ready again")
	require.Eventually(t, func() bool { _, ok := b.returned(); return ok }, 30*time.Second, 50*time.Millisecond,
		"B's supervisor never exited")
	time.Sleep(2 * time.Second)
	ok, failed, first, last := tr.end()
	t.Logf("traffic: %d ok, %d failed (first %q last %q)", ok, failed, first, last)
	_, aReturned := a.returned()
	require.False(t, aReturned, "A's supervisor exited")
	require.Equal(t, 1, a.liveEnvoys(), "A must run exactly one envoy")
	require.Zero(t, b.liveEnvoys(), "B must run none")
	require.Zero(t, failed, "requests failed during the take-back")
	require.Positive(t, ok, "no request was sent")
	require.True(t, n.logged("trigger=successor_terminating"), "the node was not taken back by the terminating successor's request")
}

// TestSuccessorKilledOutrightIsReplacedByAFreshEnvoy: the surge pod is
// SIGKILLed after it took the node, so nobody asks for a successor
// (successor_gone). The kept pod must end up Ready with one Envoy and its
// supervisor still running: the Envoy it starts is not to be killed by the
// admin watchdog for the silence that preceded it.
func TestSuccessorKilledOutrightIsReplacedByAFreshEnvoy(t *testing.T) {
	n := newNode(t)
	a, b := rollUntilSuperseded(t, n, podOpts{}, podOpts{})
	// Wait until B's envoy has terminated A's.
	require.Eventually(t, func() bool { return a.liveEnvoys() == 0 }, 30*time.Second, 50*time.Millisecond)
	// The surge pod's container is killed outright.
	n.note("SIGKILLing surge pod B")
	b.cancel()
	b.killEnvoys()
	gone := time.Now()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err, ok := a.returned(); ok {
			t.Fatalf("A's supervisor exited %s after the successor was killed: %v\n(the container restarts; "+
				"the envoy the successor_gone recovery had started was killed by A's own admin watchdog)",
				time.Since(gone).Round(100*time.Millisecond), err)
		}
		if a.ready() && n.adminEpoch() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, a.ready(), "A never went Ready with a fresh envoy")
	t.Logf("node had a proxy again %s after the successor was killed", time.Since(gone).Round(100*time.Millisecond))
	time.Sleep(3 * time.Second)
	if err, ok := a.returned(); ok {
		t.Fatalf("A's supervisor exited after going Ready: %v", err)
	}
	require.Equal(t, 1, a.liveEnvoys())
	require.True(t, n.logged("trigger=successor_gone"))
	require.False(t, n.logged("liveness watchdog fired"))
}

// runEnvoy starts the pinned Envoy directly at the given epoch.
func (n *node) runEnvoy(epoch int, out io.Writer) *exec.Cmd {
	cmd := exec.Command(n.envoy, "-c", n.bootstrap, "--base-id", strconv.Itoa(int(n.baseID)),
		"--restart-epoch", strconv.Itoa(epoch), "--drain-time-s", "2", "--parent-shutdown-time-s", "4",
		"--concurrency", "1", "-l", "warn")
	cmd.Stdout, cmd.Stderr = out, out
	require.NoError(n.t, cmd.Start())
	n.t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// TestNoFreshEnvoyIsStartedBesideAStoppedSuccessor: the successor serves the
// node, then stops answering anything (SIGSTOP: its admin is silent and its
// supervisor's heartbeat goes stale) for longer than the kept pod's bound. The
// node looks empty and is not. The kept pod must start nothing: the stopped
// Envoy still holds its hot-restart sockets. When it answers again, the next
// roll must attach to it as to any healthy Envoy.
// TestEnvoyStartedAtEpochZeroBesideALiveOnePoisonsItsHotRestart shows what the
// start that must not happen does.
func TestNoFreshEnvoyIsStartedBesideAStoppedSuccessor(t *testing.T) {
	n := newNode(t)
	a, b := rollUntilSuperseded(t, n, podOpts{adminUnresponsive: 3 * time.Second}, podOpts{adminUnresponsive: 5 * time.Minute})
	require.Eventually(t, func() bool { return a.liveEnvoys() == 0 }, 30*time.Second, 50*time.Millisecond)
	require.Eventually(t, b.ready, 30*time.Second, 50*time.Millisecond, "B never Ready")

	n.note("stalling B's envoy (SIGSTOP)")
	b.signalEnvoys(syscall.SIGSTOP)
	// The heartbeat is stale after 8 s and A's bound is 3 s: 14 s is past both.
	time.Sleep(14 * time.Second)
	require.False(t, n.heartbeatFresh(), "the stopped successor's heartbeat must be stale for this test to mean anything")
	b.signalEnvoys(syscall.SIGCONT)
	n.note("B's envoy resumed (SIGCONT)")
	require.False(t, n.logged("trigger=successor_gone"), "a fresh envoy was started beside the stopped successor's")
	require.False(t, n.logged("starting fresh at epoch 0\" statePresent=true"))
	require.Zero(t, a.liveEnvoys())
	n.waitAdminEpoch(1, 20*time.Second, "B's envoy did not come back LIVE at epoch 1")
	require.True(t, n.listenerAccepts(), "B's envoy must serve again")
	require.Eventually(t, n.heartbeatFresh, 10*time.Second, 50*time.Millisecond)

	n.note("the next roll: starting pod C")
	c := n.start("C", podOpts{adminUnresponsive: 5 * time.Minute})
	n.waitAdminEpoch(2, 40*time.Second, "the next roll's pod could not hot-restart from the successor")
	require.Eventually(t, c.ready, 30*time.Second, 50*time.Millisecond, "C never Ready")
	_, aReturned := a.returned()
	require.False(t, aReturned, "A's supervisor exited")
	require.False(t, a.ready())
}

// TestEnvoyStartedAtEpochZeroBesideALiveOnePoisonsItsHotRestart pins what the
// PINNED ENVOY does, not what the supervisor does: it is why the supervisor
// asks the kernel whether an Envoy holds the base id before a fresh start
// (hotrestart.envoyHoldsBaseID), instead of forking and seeing whether the fork
// survives.
//
// An Envoy binds its two hot-restart sockets, named by restart epoch modulo 3,
// before it attaches the shared memory. Started at epoch 0 beside a live epoch
// 1 it therefore does not collide: it unlinks the base id's shared memory,
// creates a new one, sets the "initializing" flag in it, and exits on the
// admin address the live Envoy holds, flag still set. The live Envoy goes on
// serving, and nothing can hot-restart from it again.
//
// If a pin bump makes this test fail because epoch 2 goes LIVE, Envoy no
// longer does that, and the guard can be reconsidered.
func TestEnvoyStartedAtEpochZeroBesideALiveOnePoisonsItsHotRestart(t *testing.T) {
	n := newNode(t)
	e0 := n.runEnvoy(0, lockedWriter{n, "[envoy 0]"})
	n.waitAdminEpoch(0, 20*time.Second, "epoch 0 never LIVE")
	n.runEnvoy(1, lockedWriter{n, "[envoy 1]"})
	n.waitAdminEpoch(1, 20*time.Second, "epoch 1 never LIVE")
	_ = e0.Wait() // terminated by epoch 1 at parent-shutdown time
	require.Equal(t, 1, n.adminEpoch())

	n.note("starting a stray epoch 0 beside the live epoch 1")
	stray := n.runEnvoy(0, lockedWriter{n, "[stray 0]"})
	strayDone := make(chan error, 1)
	go func() { strayDone <- stray.Wait() }()
	select {
	case err := <-strayDone:
		require.Error(t, err, "the stray epoch 0 must fail")
	case <-time.After(20 * time.Second):
		t.Fatal("the stray epoch 0 is still running beside the live epoch 1")
	}
	require.False(t, n.logged("unable to bind domain socket"),
		"the stray epoch 0 collided on the domain socket: with a live epoch 1 it should not have")
	require.Equal(t, 1, n.adminEpoch(), "the node's envoy at epoch 1 must be untouched")
	require.True(t, n.listenerAccepts())

	n.note("hot-restarting from the live epoch 1 at epoch 2")
	var out2 bytes.Buffer
	e2 := n.runEnvoy(2, io.MultiWriter(&out2, lockedWriter{n, "[envoy 2]"}))
	e2Done := make(chan error, 1)
	go func() { e2Done <- e2.Wait() }()
	select {
	case err := <-e2Done:
		require.Error(t, err)
	case <-time.After(20 * time.Second):
		t.Fatalf("epoch 2 neither exited nor was expected to go LIVE (admin LIVE epoch %d):\n%s", n.adminEpoch(), out2.String())
	}
	require.Contains(t, out2.String(), "previous envoy process is still initializing",
		"the pinned Envoy no longer refuses to hot-restart after a stray epoch 0: see this test's comment")
	require.Equal(t, 1, n.adminEpoch())
}

// holdHotRestartSocket binds the hot-restart domain socket an Envoy of the
// node's base id at the given restart epoch would bind (role "parent"), as
// Envoy does: the abstract name NUL-padded to all of sun_path. An Envoy started
// at that epoch then exits on the bind. The returned function releases it.
func (n *node) holdHotRestartSocket(epoch int) (release func()) {
	n.t.Helper()
	name := fmt.Sprintf("@envoy_domain_socket_parent_%d", uint64(n.baseID)*10+uint64(epoch%3))
	name += strings.Repeat("\x00", 108-len(name))
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	require.NoError(n.t, err)
	var once sync.Once
	release = func() { once.Do(func() { _ = conn.Close() }) }
	n.t.Cleanup(release)
	return release
}

// TestTakeBackThatCannotWorkEndsInAFreshEnvoy (from the second review pass):
// the successor's Envoy cannot be hot-restarted from, and the successor is
// then deleted. Every answer to its request exits at once, so the kept pod
// spends its budget of answers while the successor still asks. That must stop
// the answers and nothing else: when the successor has drained and exited the
// node is empty, and the kept pod must start a fresh Envoy, which serves.
//
// What makes the hot restart impossible here is the next epoch's hot-restart
// socket, held by another process, so the Envoy forked at that epoch exits on
// the bind. It used to be a shared memory left marked "initializing" by a
// stray epoch 0; the supervisor repairs that now
// (TestStaleInitializingFlagIsClearedBeforeAHotRestart), and the take-back
// from such a node simply works.
func TestTakeBackThatCannotWorkEndsInAFreshEnvoy(t *testing.T) {
	n := newNode(t)
	// B: budget = 56 - (3 + 5) - 10 = 38 s, of which it asks 38 - (8 + 6) = 24 s:
	// room for more than five answers, one every 4 s or so.
	a, b := rollUntilSuperseded(t, n, podOpts{}, podOpts{terminationGrace: 56 * time.Second})
	require.Eventually(t, func() bool { return a.liveEnvoys() == 0 }, 30*time.Second, 50*time.Millisecond)
	require.Eventually(t, b.ready, 30*time.Second, 50*time.Millisecond, "B never Ready")

	n.note("something holds the hot-restart socket of epoch 2: no envoy can start at it")
	release := n.holdHotRestartSocket(2)
	require.Equal(t, 1, n.adminEpoch())

	n.note("deleting pod B")
	b.cancel()
	require.Eventually(t, func() bool { _, ok := b.returned(); return ok }, 90*time.Second, 50*time.Millisecond,
		"B's supervisor never exited")
	release()
	gone := time.Now()
	n.note("B exited; the node has no envoy")
	require.True(t, n.logged("no longer answering its requests"), "A did not spend its budget of answers: this test no longer tests that")

	require.Eventually(t, func() bool { return a.ready() && a.liveEnvoys() == 1 && n.listenerAccepts() },
		40*time.Second, 100*time.Millisecond,
		"the node is empty and stays without a proxy: the kept pod gave up after its lost relaunches, "+
			"although the successor that made them fail is gone")
	t.Logf("node had a proxy again %s after the successor exited", time.Since(gone).Round(100*time.Millisecond))
	require.True(t, n.logged("trigger=successor_gone"))
	_, aReturned := a.returned()
	require.False(t, aReturned, "A's supervisor exited")
}

// TestTwoFreshStartsCollideOnTheSocketAndHarmNothing pins the other half of
// what the supervisor relies on in the pinned Envoy. A fresh start is only
// made on a node where no process holds the base id, so the one thing it can
// still meet is another fresh start in the same instant. Both are epoch 0:
// the second fails on the bind of the socket of id 0, which comes before the
// shared memory is touched, and the first can be hot-restarted from as if
// nothing had happened.
func TestTwoFreshStartsCollideOnTheSocketAndHarmNothing(t *testing.T) {
	n := newNode(t)
	n.runEnvoy(0, lockedWriter{n, "[envoy 0]"})
	n.waitAdminEpoch(0, 20*time.Second, "epoch 0 never LIVE")

	var out bytes.Buffer
	second := n.runEnvoy(0, io.MultiWriter(&out, lockedWriter{n, "[second 0]"}))
	done := make(chan error, 1)
	go func() { done <- second.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("a second epoch 0 is running beside the first")
	}
	require.Contains(t, out.String(), "unable to bind domain socket",
		"the second epoch 0 did not fail on the hot-restart socket: it got further, see this test's comment")
	require.Equal(t, 0, n.adminEpoch())
	require.True(t, n.listenerAccepts())

	n.runEnvoy(1, lockedWriter{n, "[envoy 1]"})
	n.waitAdminEpoch(1, 20*time.Second, "the first epoch 0 could not be hot-restarted from after the collision")
}
