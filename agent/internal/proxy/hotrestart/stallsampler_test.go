package hotrestart

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProc is a writable stand-in for /proc.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	return &fakeProc{t: t, root: t.TempDir()}
}

func (f *fakeProc) write(rel, content string) {
	f.t.Helper()
	path := filepath.Join(f.root, rel)
	require.NoError(f.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(f.t, os.WriteFile(path, []byte(content), 0o644))
}

// thread writes one thread's comm, stat, schedstat and wchan.
func (f *fakeProc) thread(pid, tid int, comm string, state byte, cpu, runq time.Duration, wchan string) {
	f.t.Helper()
	dir := filepath.Join(strconv.Itoa(pid), "task", strconv.Itoa(tid))
	f.write(filepath.Join(dir, "comm"), comm+"\n")
	// The comm field is deliberately awkward: a ") " inside it must not fool
	// the state parser.
	f.write(filepath.Join(dir, "stat"), fmt.Sprintf("%d (%s) x) %c 1 1 1 0 -1 4194560 0 0\n", tid, comm, state))
	f.write(filepath.Join(dir, "schedstat"), fmt.Sprintf("%d %d 42\n", cpu.Nanoseconds(), runq.Nanoseconds()))
	if wchan == "" {
		wchan = "0"
	}
	f.write(filepath.Join(dir, "wchan"), wchan)
}

// node writes /proc/stat with two CPUs and /proc/pressure/{cpu,irq}.
func (f *fakeProc) node(cpu0, cpu1 cpuTimes, psiCPUSomeUs, psiIRQFullUs uint64) {
	f.t.Helper()
	line := func(name string, c cpuTimes) string {
		return fmt.Sprintf("%s %d %d %d %d %d %d %d %d 0 0\n", name, c.user, c.nice, c.system, c.idle, c.iowait, c.irq, c.softirq, c.steal)
	}
	total := cpuTimes{
		user: cpu0.user + cpu1.user, nice: cpu0.nice + cpu1.nice, system: cpu0.system + cpu1.system,
		idle: cpu0.idle + cpu1.idle, iowait: cpu0.iowait + cpu1.iowait, irq: cpu0.irq + cpu1.irq,
		softirq: cpu0.softirq + cpu1.softirq, steal: cpu0.steal + cpu1.steal,
	}
	f.write("stat", line("cpu", total)+line("cpu0", cpu0)+line("cpu1", cpu1)+"intr 1 2 3\nctxt 99\n")
	f.write("pressure/cpu", fmt.Sprintf("some avg10=1.00 avg60=1.00 avg300=1.00 total=%d\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", psiCPUSomeUs))
	f.write("pressure/irq", fmt.Sprintf("full avg10=0.00 avg60=0.00 avg300=0.00 total=%d\n", psiIRQFullUs))
}

type capturedLog struct {
	buf bytes.Buffer
}

func (c *capturedLog) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (c *capturedLog) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

const (
	testPID   = 4000
	testEpoch = 119
)

func newTestStallSampler(proc *fakeProc, logs *capturedLog, metrics *SupervisorMetrics, targets map[int]int) *stallSampler {
	return newStallSampler(procReader{root: proc.root}, DefaultStallThreshold, logs.logger(), metrics,
		func() map[int]int { return targets },
		func() []any { return []any{"trackedEpochs", len(targets), "handoffPeer", 118} })
}

// TestStallSamplerClassifiesEachThread drives one window with a thread in every
// state the #1093 attribution needs told apart.
func TestStallSamplerClassifiesEachThread(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	metrics, reader := newTestSupervisorMetrics(t)
	s := newTestStallSampler(proc, logs, metrics, map[int]int{testEpoch: testPID})

	// Window start.
	proc.thread(testPID, testPID, "envoy", 'S', time.Second, 0, "do_epoll_wait")
	proc.thread(testPID, 4001, "wrk:worker_0", 'S', time.Second, time.Second, "do_epoll_wait")
	proc.thread(testPID, 4002, "wrk:worker_1", 'S', time.Second, 0, "do_epoll_wait")
	proc.thread(testPID, 4003, "wrk:worker_2", 'S', time.Second, 0, "do_epoll_wait")
	proc.thread(testPID, 4004, "wrk:worker_3", 'S', time.Second, 0, "do_epoll_wait")
	proc.thread(testPID, 4005, "dog:workers", 'S', 0, 0, "hrtimer_nanosleep")
	proc.node(cpuTimes{user: 100, idle: 100}, cpuTimes{user: 100, idle: 100}, 1_000_000, 0)
	t0 := time.Unix(1790821002, 0)
	s.tick(t0) // window start and first per-thread sample

	// One second later:
	//   main thread: on a CPU 950 ms of it                      -> busy
	//   worker_0:    runnable but waiting 600 ms                -> starved
	//   worker_1:    idle in epoll                               -> nothing
	//   worker_2:    asleep in a futex the whole time            -> blocked, wchan named
	//   worker_3:    in D state                                  -> blocked, uninterruptible
	//   dog:workers: not an Envoy event loop; never reported even though its
	//                numbers are terrible.
	proc.thread(testPID, testPID, "envoy", 'R', time.Second+950*time.Millisecond, 0, "")
	proc.thread(testPID, 4001, "wrk:worker_0", 'R', time.Second+100*time.Millisecond, time.Second+600*time.Millisecond, "")
	proc.thread(testPID, 4002, "wrk:worker_1", 'S', time.Second+10*time.Millisecond, 0, "do_epoll_wait")
	proc.thread(testPID, 4003, "wrk:worker_2", 'S', time.Second, 0, "__futex_wait")
	proc.thread(testPID, 4004, "wrk:worker_3", 'D', time.Second, 0, "unix_wait_for_peer")
	proc.thread(testPID, 4005, "dog:workers", 'D', 5*time.Second, 5*time.Second, "something")
	// cpu0: irq+softirq 60 of its 100 jiffies; cpu1 half busy. Node: 130 of
	// 200 jiffies busy. PSI cpu-some +700 ms.
	proc.node(cpuTimes{user: 120, idle: 120, irq: 30, softirq: 30}, cpuTimes{user: 150, idle: 150}, 1_700_000, 0)
	s.tick(t0.Add(time.Second))

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 1, "one line per epoch per flagged window")
	rec := recs[0]
	assert.EqualValues(t, testEpoch, rec["epoch"])
	assert.EqualValues(t, testPID, rec["pid"])
	assert.EqualValues(t, 1000, rec["windowMs"])
	assert.EqualValues(t, 118, rec["handoffPeer"], "the supervisor's handoff context is attached")

	threads := toStrings(t, rec["threads"])
	require.Len(t, threads, 4, "worker_1 is idle and dog:workers is not sampled: %v", threads)
	assert.Contains(t, threads[0], "envoy[busy] cpu=950ms")
	assert.Contains(t, threads[1], "wrk:worker_0[starved] cpu=100ms runq=600ms")
	assert.Contains(t, threads[2], "wrk:worker_2[blocked]")
	assert.Contains(t, threads[2], "wchan=__futex_wait")
	assert.Contains(t, threads[3], "wrk:worker_3[blocked]")
	assert.Contains(t, threads[3], "uninterruptible=")
	assert.Contains(t, threads[3], "wchan=unix_wait_for_peer")

	// Node picture for the same window: 130 of 200 jiffies busy, cpu0 at 60%
	// irq+softirq, 700 ms of PSI cpu-some, irq PSI present but zero.
	assert.InDelta(t, 65.0, rec["nodeBusyPct"], 0.01)
	assert.InDelta(t, 60.0, rec["nodeHottestCPUIrqSoftirqPct"], 0.01)
	assert.EqualValues(t, 700, rec["nodePSICPUSomeMs"])
	assert.EqualValues(t, 0, rec["nodePSIIRQFullMs"])

	assert.Equal(t, map[string]int64{stallStarved: 1, stallBlocked: 2, stallBusy: 1},
		metricSumByAttr(t, reader, "aether.supervisor.envoy_thread_stalls", attrStallClass))
}

// TestStallSamplerQuietWindowLogsNothing: an idle proxy produces no lines, and the
// counter is still exported (seeded) so "no stalls" is not "no sampler".
func TestStallSamplerQuietWindowLogsNothing(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	metrics, reader := newTestSupervisorMetrics(t)
	s := newTestStallSampler(proc, logs, metrics, map[int]int{testEpoch: testPID})

	proc.thread(testPID, testPID, "envoy", 'S', time.Second, 0, "do_epoll_wait")
	proc.thread(testPID, 4001, "wrk:worker_0", 'S', time.Second, 0, "do_epoll_wait")
	proc.node(cpuTimes{user: 1, idle: 1}, cpuTimes{user: 1, idle: 1}, 0, 0)
	t0 := time.Unix(1790821002, 0)
	for i := range 25 {
		// Light, healthy load: a few ms on a CPU and a few ms of runqueue wait
		// per tick, idle in epoll at every sample.
		d := time.Duration(i) * 5 * time.Millisecond
		proc.thread(testPID, testPID, "envoy", 'S', time.Second+d, d/2, "do_epoll_wait")
		proc.thread(testPID, 4001, "wrk:worker_0", 'S', time.Second+d, d/2, "do_epoll_wait")
		s.tick(t0.Add(time.Duration(i) * 100 * time.Millisecond))
	}

	assert.Empty(t, logs.records(t, "envoy thread stall"))
	assert.Equal(t, map[string]int64{stallStarved: 0, stallBlocked: 0, stallBusy: 0},
		metricSumByAttr(t, reader, "aether.supervisor.envoy_thread_stalls", attrStallClass))
}

// TestStallSamplerWithheldWchanIsNotABlock: a kernel that answers "0" for a
// sleeping thread's wchan must not turn every idle worker into a "blocked" line
// every second; a D-state sleep still counts without one.
func TestStallSamplerWithheldWchanIsNotABlock(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID})

	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "")
	proc.thread(testPID, 4001, "wrk:worker_0", 'S', 0, 0, "")
	t0 := time.Unix(1790821002, 0)
	s.tick(t0)
	proc.thread(testPID, 4001, "wrk:worker_0", 'D', 0, 0, "")
	s.tick(t0.Add(time.Second))

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 1)
	threads := toStrings(t, recs[0]["threads"])
	require.Len(t, threads, 1, "the main thread's wchan-less S sleep is not a stall: %v", threads)
	assert.Contains(t, threads[0], "wrk:worker_0[blocked]")
	assert.Contains(t, threads[0], "uninterruptible=1000ms wchan=?")
}

// TestStallSamplerLateTickReportsTheSpan: when the supervisor itself was starved
// and its next tick comes seconds late, the line's windowMs covers the span the
// deltas were taken over, and busy is judged against that span.
func TestStallSamplerLateTickReportsTheSpan(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID})

	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "ep_poll")
	proc.thread(testPID, 4001, "wrk:worker_0", 'S', 0, 0, "ep_poll")
	t0 := time.Unix(1790821002, 0)
	s.tick(t0)
	// 6 s later: the worker waited 5.5 s for a CPU; the main thread was on one
	// for 1 s of the 6 (not busy over the span, although it would be over a
	// nominal 1 s window).
	proc.thread(testPID, testPID, "envoy", 'S', time.Second, 0, "ep_poll")
	proc.thread(testPID, 4001, "wrk:worker_0", 'R', 10*time.Millisecond, 5500*time.Millisecond, "")
	s.tick(t0.Add(6 * time.Second))

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 1)
	assert.EqualValues(t, 6000, recs[0]["windowMs"])
	threads := toStrings(t, recs[0]["threads"])
	require.Len(t, threads, 1, "%v", threads)
	assert.Contains(t, threads[0], "wrk:worker_0[starved] cpu=10ms runq=5500ms")
}

// TestStallSamplerFollowsEpochs: a reaped epoch is dropped without a panic, and a
// new one is picked up — the cross-pod handoff case, where the set changes under
// the sampler.
func TestStallSamplerFollowsEpochs(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	targets := map[int]int{testEpoch: testPID}
	s := newStallSampler(procReader{root: proc.root}, DefaultStallThreshold, logs.logger(), nil,
		func() map[int]int { return targets }, nil)

	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "do_epoll_wait")
	t0 := time.Unix(1790821002, 0)
	s.tick(t0)
	s.tick(t0.Add(100 * time.Millisecond))
	require.Contains(t, s.epochs, testEpoch)

	// The epoch exits and is reaped: its /proc directory goes away.
	require.NoError(t, os.RemoveAll(filepath.Join(proc.root, strconv.Itoa(testPID))))
	s.tick(t0.Add(200 * time.Millisecond))
	assert.Empty(t, s.epochs[testEpoch].threads, "a vanished process leaves nothing to report")

	targets = map[int]int{testEpoch + 1: testPID + 10}
	proc.thread(testPID+10, testPID+10, "envoy", 'S', 0, 0, "do_epoll_wait")
	proc.thread(testPID+10, testPID+11, "wrk:worker_0", 'S', 0, 0, "do_epoll_wait")
	s.tick(t0.Add(300 * time.Millisecond))
	assert.NotContains(t, s.epochs, testEpoch, "the reaped epoch is forgotten")
	require.Contains(t, s.epochs, testEpoch+1)
	assert.Len(t, s.epochs[testEpoch+1].threads, 2)

	// With no node files at all (nil metrics, no /proc/stat) the window still
	// closes and logs nothing.
	s.tick(t0.Add(1500 * time.Millisecond))
	assert.Empty(t, logs.records(t, "envoy thread stall"))
}

func TestParseTaskState(t *testing.T) {
	for _, tc := range []struct {
		line string
		want byte
	}{
		{"4001 (wrk:worker_0) S 1 2 3", 'S'},
		{"4001 (a) b (c)) R 1", 'R'},
		{"4001 (envoy) D 1", 'D'},
	} {
		got, err := parseTaskState([]byte(tc.line))
		require.NoError(t, err, tc.line)
		assert.Equal(t, string(tc.want), string(got), tc.line)
	}
	for _, bad := range []string{"", "4001 (envoy", "4001 (envoy)", "4001 (envoy) "} {
		_, err := parseTaskState([]byte(bad))
		assert.Error(t, err, "%q", bad)
	}
}

func TestParseSchedstat(t *testing.T) {
	cpu, runq, err := parseSchedstat([]byte("807472818 798485462 3329\n"))
	require.NoError(t, err)
	assert.EqualValues(t, 807472818, cpu)
	assert.EqualValues(t, 798485462, runq)

	for _, bad := range []string{"", "1", "x 1 1", "1 y 1"} {
		_, _, err := parseSchedstat([]byte(bad))
		assert.Error(t, err, "%q", bad)
	}
}

func TestParsePressureTotal(t *testing.T) {
	psi := []byte("some avg10=15.76 avg60=17.57 avg300=18.45 total=184615611461\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	got, ok := parsePressureTotal(psi, "some")
	require.True(t, ok)
	assert.EqualValues(t, 184615611461, got)
	got, ok = parsePressureTotal(psi, "full")
	require.True(t, ok)
	assert.EqualValues(t, 0, got)
	_, ok = parsePressureTotal([]byte("full avg10=5.33 total=56374668813\n"), "some")
	assert.False(t, ok, "irq pressure has no 'some' line")
}

func TestParseProcStat(t *testing.T) {
	// Verbatim head of node D's /proc/stat (2026-10-01).
	n, err := parseProcStat([]byte("cpu  81259081 0 52506391 207860896 12266 10811280 15188217 0 0 0\n" +
		"cpu0 17021116 0 13834353 49829478 2674 3099526 8314454 0 0 0\n" +
		"cpu1 21458363 0 12867111 52647812 3210 2565697 2311839 0 0 0\n" +
		"intr 1 2\n"))
	require.NoError(t, err)
	assert.EqualValues(t, 81259081, n.total.user)
	assert.EqualValues(t, 15188217, n.total.softirq)
	require.Len(t, n.cpus, 2)
	assert.EqualValues(t, 8314454, n.cpus[0].softirq)

	_, err = parseProcStat([]byte("intr 1 2\n"))
	assert.Error(t, err)
}

func TestIdleWchan(t *testing.T) {
	for _, idle := range []string{"do_epoll_wait", "ep_poll", "do_epoll_pwait"} {
		assert.True(t, idleWchan(idle), idle)
	}
	for _, blocked := range []string{"", "__futex_wait", "futex_wait_queue", "unix_wait_for_peer", "do_nanosleep"} {
		assert.False(t, idleWchan(blocked), blocked)
	}
}

// TestProcReaderReadsLiveThreads checks the reader against the real /proc of
// this test process — the files the supervisor reads in production.
func TestProcReaderReadsLiveThreads(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	r := procReader{root: "/proc"}
	pid := os.Getpid()
	tids, err := r.taskIDs(pid)
	require.NoError(t, err)
	require.Contains(t, tids, pid, "the main thread is listed under its own pid")

	st, err := r.thread(pid, pid)
	require.NoError(t, err)
	assert.Contains(t, "RSDtTZI", string(st.state))

	n, err := r.nodeCPU()
	require.NoError(t, err)
	assert.NotZero(t, n.total.sum())
	assert.NotEmpty(t, n.cpus)
}

// TestStallSamplerIsTickedWithTheTimeOfTheObservation: the timestamp a
// time.Ticker delivers is when the tick was DUE. The supervisor shares the
// proxy container's cgroup, so whatever starves Envoy can keep the supervisor
// off the CPU for seconds too, and when it runs again the tick it receives is
// that old. What it then reads from /proc is the state NOW: the sampler has to
// be told the time of the reading, or a 3 s wait is placed 3 s too early (its
// start is the reading minus the wait) and the consumers are taken over
// seconds that had nothing to do with it.
func TestStallSamplerIsTickedWithTheTimeOfTheObservation(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID})
	fs := newFakeStatFS()
	fs.talosNode()
	s.consumers = newTestConsumers(fs, 5)
	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "do_epoll_wait")

	clock := consumersT0
	wake := make(chan time.Time)
	ticked := make(chan time.Time)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		driveStallSampler(context.Background(), done, wake, func() time.Time { return clock },
			func(now time.Time) { s.tick(now); ticked <- now })
	}()
	t.Cleanup(func() {
		close(done)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("the sampler loop did not end")
		}
	})
	// step delivers the tick that was due at `due` while the clock reads
	// `observed`, and returns the time the sampler was ticked with.
	step := func(due, observed time.Duration) time.Time {
		clock = consumersT0.Add(observed)
		wake <- consumersT0.Add(due)
		select {
		case now := <-ticked:
			return now
		case <-time.After(5 * time.Second):
			t.Fatal("no tick")
			return time.Time{}
		}
	}

	for _, at := range []time.Duration{0, time.Second, 2 * time.Second} {
		require.Equal(t, consumersT0.Add(at), step(at, at))
	}

	// From 2.1 s to 5.1 s neither Envoy nor the supervisor runs; /init has the
	// CPUs. The tick due at 2.1 s is received at 5.1 s.
	fs.cgroup("/init", 2900*time.Millisecond)
	fs.cgroup("/", 2900*time.Millisecond)
	proc.thread(testPID, testPID, "envoy", 'R', 0, 3*time.Second, "")
	assert.Equal(t, consumersT0.Add(5100*time.Millisecond), step(2100*time.Millisecond, 5100*time.Millisecond),
		"ticked with the time of the reading, not the time the tick was due")

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 1, "the window closes on the reading that found the wait")
	assert.Contains(t, toStrings(t, recs[0]["threads"])[0], "envoy[starved] cpu=0ms runq=3000ms")
	assert.Equal(t, "/init=2900ms", recs[0][attrTopCgroups])
	assert.EqualValues(t, 3100, recs[0][attrTopCgroupsOverMs], "from the sample at 2 s, the last one before the wait began")
	assert.NotContains(t, recs[0], attrTopCgroupsTruncated)
}

func toStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	require.True(t, ok, "%T", v)
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		s, ok := x.(string)
		require.True(t, ok, "%T", x)
		out = append(out, s)
	}
	return out
}
