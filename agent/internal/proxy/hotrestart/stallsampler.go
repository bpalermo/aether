package hotrestart

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"
)

// Envoy thread-stall sampler (issue #1093).
//
// On some hot-restart handoffs a node stalled for several seconds: requests to
// every pod on it took 1–3 s and the node-local liveness route (a direct_response
// on the proxy, no upstream) did not answer within 2 s. The 2026-10-01 soak could
// not say WHY, because nothing recorded what the proxy's threads were doing:
//
//   - Pyroscope is on-CPU only. The stalled windows showed ~0.6–1.1 cores per
//     Envoy, nothing unusual — so the threads were not burning CPU, or were
//     waiting for one, or were blocked, and an on-CPU profile cannot tell which.
//   - Envoy's own worker watchdog counted misses (>=200 ms without a loop
//     iteration) on exactly the two nodes that stalled and never on the other
//     three, but a counter says neither when nor in what.
//   - The node has no node-exporter; there is no per-node CPU, PSI or softirq
//     series to set a stall against.
//
// This sampler answers it from /proc at ~zero cost. Every interval it reads, for
// the main thread and every worker ("wrk:*") of each Envoy epoch this supervisor
// runs, the kernel's schedstat (time on a CPU and time RUNNABLE BUT WAITING for
// one) and, for a sleeping thread, the kernel function it sleeps in (wchan). Per
// one-second window it classifies a thread as
//
//   - starved: runnable but not scheduled for >= threshold — CPU contention or a
//     node-level scheduling stall (the runqueue wait is the direct measure);
//   - blocked: asleep for >= threshold somewhere other than its idle epoll wait
//     (a lock, a blocking socket call, the kernel) — the wchan names where;
//   - busy:    on a CPU for >= 90% of the window — its event loop is saturated,
//     so everything queued behind the current callback waits (Pyroscope then
//     says on what);
//
// and logs one "envoy thread stall" line per epoch per flagged window, together
// with the node's own CPU picture for the same second (busy/irq/softirq/steal
// shares, the hottest CPU's irq+softirq share, and the PSI cpu-some and irq-full
// stall time), so a stall every process on the node shares is told apart from
// one only Envoy has. The aether.supervisor.envoy_thread_stalls counter carries
// the same classification for grading a soak.
//
// The threshold defaults to 200 ms, Envoy's own worker watchdog miss threshold,
// so a stall Envoy counts is a stall this attributes.

const (
	// DefaultStallSampleInterval is the --stall-sample-interval default: the
	// resolution of the state/wchan sampling (schedstat is cumulative, so CPU
	// and runqueue time are exact regardless). Per tick it is two or three
	// small file reads per sampled thread — five threads per epoch at Envoy's
	// default --concurrency on the 4-core talos-main workers.
	DefaultStallSampleInterval = 100 * time.Millisecond
	// DefaultStallThreshold is the --stall-threshold default, matching Envoy's
	// default worker watchdog miss_timeout.
	DefaultStallThreshold = 200 * time.Millisecond
	// stallWindow is the accounting and reporting window.
	stallWindow = time.Second
	// stallBusyFraction is the share of a window on a CPU that classifies a
	// thread as busy.
	stallBusyFraction = 0.9
	// envoyWorkerThreadPrefix is how Envoy names its worker threads
	// (source/server/worker_impl.cc: "wrk:" + dispatcher name).
	envoyWorkerThreadPrefix = "wrk:"
)

// Stall classes, the values of attrStallClass.
const (
	stallStarved = "starved"
	stallBlocked = "blocked"
	stallBusy    = "busy"
)

// stallClassValues is the closed set, used to seed the counter at zero.
var stallClassValues = []string{stallStarved, stallBlocked, stallBusy}

// idleWchans are the kernel functions an Envoy event loop sleeps in while it is
// simply idle (libevent's epoll backend). A thread asleep anywhere else is
// blocked.
var idleWchans = []string{"do_epoll_wait", "ep_poll", "do_epoll_pwait"}

func idleWchan(w string) bool {
	for _, idle := range idleWchans {
		if strings.HasPrefix(w, idle) {
			return true
		}
	}
	return false
}

// sampledThread is one Envoy thread's running window.
type sampledThread struct {
	name string
	// skip marks a thread that is neither the main thread nor a worker
	// (guard dogs, gRPC, file-flush threads): remembered so its comm is read
	// once, not every tick.
	skip bool

	last   threadStat
	lastAt time.Time

	// Window accumulators. span is the wall time the deltas cover: normally
	// the window, longer when the supervisor's own ticks came late (it shares
	// the proxy container's cgroup, so whatever starves Envoy starves the
	// sampler too).
	//
	// The kernel charges a runqueue wait to schedstat when the wait ENDS (the
	// thread gets on a CPU), so runq can exceed the window: a 5 s starvation is
	// reported whole in the window it ended in. That is what a 6 s cpu.max
	// throttle of the proxy container produced on kind:
	//   wrk:worker_1[starved] cpu=0ms runq=5803ms   windowMs=1099
	span          time.Duration
	cpuNs, runqNs uint64
	blocked       time.Duration
	uninterrupt   time.Duration // the part of blocked spent in state D
	wchans        map[string]time.Duration
}

func (t *sampledThread) resetWindow() {
	t.span, t.cpuNs, t.runqNs, t.blocked, t.uninterrupt = 0, 0, 0, 0, 0
	clear(t.wchans)
}

// topWchan is where the thread spent most of its blocked time this window.
func (t *sampledThread) topWchan() string {
	best, bestD := "", time.Duration(-1)
	for _, w := range slices.Sorted(maps.Keys(t.wchans)) {
		if d := t.wchans[w]; d > bestD {
			best, bestD = w, d
		}
	}
	return best
}

// sampledEpoch is one supervised Envoy process.
type sampledEpoch struct {
	pid     int
	threads map[int]*sampledThread
}

// stallSampler is driven by tick; Supervisor.sampleStalls owns the timer.
type stallSampler struct {
	proc      procReader
	window    time.Duration
	threshold time.Duration
	log       *slog.Logger
	metrics   *SupervisorMetrics
	// targets returns the epochs to sample, epoch -> pid.
	targets func() map[int]int
	// logContext returns extra attributes for a stall line (handoff state).
	logContext func() []any

	epochs      map[int]*sampledEpoch
	windowStart time.Time

	node     nodeCPU
	nodeOK   bool
	psiCPU   uint64
	psiCPUOK bool
	psiIRQ   uint64
	psiIRQOK bool
}

func newStallSampler(proc procReader, threshold time.Duration, log *slog.Logger, metrics *SupervisorMetrics,
	targets func() map[int]int, logContext func() []any,
) *stallSampler {
	return &stallSampler{
		proc:       proc,
		window:     stallWindow,
		threshold:  threshold,
		log:        log,
		metrics:    metrics,
		targets:    targets,
		logContext: logContext,
		epochs:     make(map[int]*sampledEpoch),
	}
}

// tick takes one sample of every tracked thread and closes the window once it
// has run its length.
func (s *stallSampler) tick(now time.Time) {
	if s.windowStart.IsZero() {
		s.windowStart = now
		s.readNode()
	}

	targets := s.targets()
	for epoch, e := range s.epochs {
		if pid, ok := targets[epoch]; !ok || pid != e.pid {
			delete(s.epochs, epoch)
		}
	}
	for epoch, pid := range targets {
		e := s.epochs[epoch]
		if e == nil {
			e = &sampledEpoch{pid: pid, threads: make(map[int]*sampledThread)}
			s.epochs[epoch] = e
		}
		s.sampleEpoch(e, now)
	}

	if now.Sub(s.windowStart) >= s.window {
		s.closeWindow(now)
	}
}

func (s *stallSampler) sampleEpoch(e *sampledEpoch, now time.Time) {
	tids, err := s.proc.taskIDs(e.pid)
	if err != nil {
		// The process is gone (reaped between targets() and here) or /proc is
		// unreadable; either way there is nothing to sample this tick.
		clear(e.threads)
		return
	}
	seen := make(map[int]struct{}, len(tids))
	for _, tid := range tids {
		seen[tid] = struct{}{}
		t := e.threads[tid]
		if t == nil {
			name, nameErr := s.proc.threadName(e.pid, tid)
			if nameErr != nil {
				continue
			}
			t = &sampledThread{name: name, wchans: make(map[string]time.Duration)}
			t.skip = tid != e.pid && !strings.HasPrefix(name, envoyWorkerThreadPrefix)
			e.threads[tid] = t
		}
		if t.skip {
			continue
		}
		s.sampleThread(e.pid, tid, t, now)
	}
	for tid := range e.threads {
		if _, ok := seen[tid]; !ok {
			delete(e.threads, tid)
		}
	}
}

func (s *stallSampler) sampleThread(pid, tid int, t *sampledThread, now time.Time) {
	st, err := s.proc.thread(pid, tid)
	if err != nil {
		// Exited between the task listing and this read; the next listing
		// drops it.
		return
	}
	if t.lastAt.IsZero() {
		t.last, t.lastAt = st, now
		return
	}
	elapsed := now.Sub(t.lastAt)
	dCPU := counterDelta(st.cpuNs, t.last.cpuNs)
	dRunq := counterDelta(st.runqNs, t.last.runqNs)
	t.span += elapsed
	t.cpuNs += dCPU
	t.runqNs += dRunq
	if off := elapsed - time.Duration(dCPU+dRunq); off > 0 {
		s.chargeSleep(pid, tid, t, st.state, off)
	}
	t.last, t.lastAt = st, now
}

// chargeSleep charges off — the part of the last interval the thread was
// neither on a CPU nor waiting for one — as blocked time when the thread is
// asleep somewhere other than its idle epoll wait, keyed by where it sleeps. A
// thread is sampled at the end of the interval, so this is an approximation at
// --stall-sample-interval resolution; the CPU and runqueue times are exact.
//
// An interruptible sleep whose wchan the kernel withholds is NOT counted: it is
// far more likely the idle epoll wait than a stall, and counting it would flag
// every idle worker every window. Uninterruptible sleep (D) is never idle for an
// event loop, wchan or not.
func (s *stallSampler) chargeSleep(pid, tid int, t *sampledThread, state byte, off time.Duration) {
	if state != 'S' && state != 'D' {
		return
	}
	w := s.proc.wchan(pid, tid)
	if state == 'S' && (w == "" || idleWchan(w)) {
		return
	}
	t.blocked += off
	if state == 'D' {
		t.uninterrupt += off
	}
	if w == "" {
		w = "?"
	}
	t.wchans[w] += off
}

// counterDelta is cur-prev for a monotonic kernel counter, 0 if it went
// backwards (a recycled TID).
func counterDelta(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

// threadVerdict is one flagged thread in a window.
type threadVerdict struct {
	name    string
	classes []string
	cpu     time.Duration
	runq    time.Duration
	blocked time.Duration
	inD     time.Duration
	wchan   string
}

func (v threadVerdict) String() string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "%s[%s] cpu=%dms runq=%dms blocked=%dms",
		v.name, strings.Join(v.classes, ","), v.cpu.Milliseconds(), v.runq.Milliseconds(), v.blocked.Milliseconds())
	if v.inD > 0 {
		fmt.Fprintf(b, " uninterruptible=%dms", v.inD.Milliseconds())
	}
	if v.wchan != "" {
		fmt.Fprintf(b, " wchan=%s", v.wchan)
	}
	return b.String()
}

// classify flags a thread's window, or returns ok=false. Busy is measured
// against the span the thread's own deltas cover.
func (s *stallSampler) classify(t *sampledThread) (threadVerdict, bool) {
	v := threadVerdict{
		name:    t.name,
		cpu:     time.Duration(t.cpuNs),
		runq:    time.Duration(t.runqNs),
		blocked: t.blocked,
		inD:     t.uninterrupt,
	}
	if v.runq >= s.threshold {
		v.classes = append(v.classes, stallStarved)
	}
	if v.blocked >= s.threshold {
		v.classes = append(v.classes, stallBlocked)
		v.wchan = t.topWchan()
	}
	if t.span > 0 && float64(v.cpu) >= stallBusyFraction*float64(t.span) {
		v.classes = append(v.classes, stallBusy)
	}
	return v, len(v.classes) > 0
}

func (s *stallSampler) closeWindow(now time.Time) {
	wall := now.Sub(s.windowStart)
	node := s.nodeAttrs()

	for _, epoch := range slices.Sorted(maps.Keys(s.epochs)) {
		e := s.epochs[epoch]
		flagged, span := s.closeEpochWindow(e)
		if len(flagged) == 0 {
			continue
		}
		attrs := []any{
			"epoch", epoch,
			"pid", e.pid,
			"windowMs", max(wall, span).Milliseconds(),
			"thresholdMs", s.threshold.Milliseconds(),
			"threads", flagged,
		}
		attrs = append(attrs, node...)
		if s.logContext != nil {
			attrs = append(attrs, s.logContext()...)
		}
		s.log.Warn("envoy thread stall", attrs...)
	}
	s.windowStart = now
}

// closeEpochWindow classifies and resets every sampled thread of one epoch,
// counts the flagged ones and returns their descriptions and the longest span a
// flagged thread's deltas cover.
func (s *stallSampler) closeEpochWindow(e *sampledEpoch) (flagged []string, span time.Duration) {
	for _, tid := range slices.Sorted(maps.Keys(e.threads)) {
		t := e.threads[tid]
		if t.skip {
			continue
		}
		if v, ok := s.classify(t); ok {
			flagged = append(flagged, v.String())
			span = max(span, t.span)
			for _, class := range v.classes {
				s.metrics.envoyThreadStalled(class)
			}
		}
		t.resetWindow()
	}
	return flagged, span
}

// readNode refreshes the node CPU and PSI baselines.
func (s *stallSampler) readNode() {
	if n, err := s.proc.nodeCPU(); err == nil {
		s.node, s.nodeOK = n, true
	} else {
		s.nodeOK = false
	}
	s.psiCPU, s.psiCPUOK = s.proc.pressureTotal("cpu", "some")
	s.psiIRQ, s.psiIRQOK = s.proc.pressureTotal("irq", "full")
}

// nodeAttrs returns the node's CPU picture since the previous window and
// advances the baselines.
func (s *stallSampler) nodeAttrs() []any {
	prevNode, prevNodeOK := s.node, s.nodeOK
	prevCPU, prevCPUOK := s.psiCPU, s.psiCPUOK
	prevIRQ, prevIRQOK := s.psiIRQ, s.psiIRQOK
	s.readNode()

	var attrs []any
	if prevNodeOK && s.nodeOK {
		d := cpuDelta(s.node.total, prevNode.total)
		if total := d.sum(); total > 0 {
			attrs = append(attrs,
				"nodeBusyPct", pct(total-d.idle-d.iowait, total),
				"nodeIrqPct", pct(d.irq, total),
				"nodeSoftirqPct", pct(d.softirq, total),
				"nodeStealPct", pct(d.steal, total),
			)
		}
		hottest := 0.0
		for i := range min(len(s.node.cpus), len(prevNode.cpus)) {
			c := cpuDelta(s.node.cpus[i], prevNode.cpus[i])
			if sum := c.sum(); sum > 0 {
				hottest = max(hottest, pct(c.irq+c.softirq, sum))
			}
		}
		attrs = append(attrs, "nodeHottestCPUIrqSoftirqPct", hottest)
	}
	if prevCPUOK && s.psiCPUOK {
		attrs = append(attrs, "nodePSICPUSomeMs", counterDelta(s.psiCPU, prevCPU)/1000)
	}
	if prevIRQOK && s.psiIRQOK {
		attrs = append(attrs, "nodePSIIRQFullMs", counterDelta(s.psiIRQ, prevIRQ)/1000)
	}
	return attrs
}

func cpuDelta(cur, prev cpuTimes) cpuTimes {
	return cpuTimes{
		user:    counterDelta(cur.user, prev.user),
		nice:    counterDelta(cur.nice, prev.nice),
		system:  counterDelta(cur.system, prev.system),
		idle:    counterDelta(cur.idle, prev.idle),
		iowait:  counterDelta(cur.iowait, prev.iowait),
		irq:     counterDelta(cur.irq, prev.irq),
		softirq: counterDelta(cur.softirq, prev.softirq),
		steal:   counterDelta(cur.steal, prev.steal),
	}
}

// pct is part/total as a percentage rounded to one decimal.
func pct(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part*1000/total) / 10
}

// sampleStalls runs the stall sampler until ctx is done. It never touches
// Envoy: every read is a /proc file of a process this supervisor forked.
func (s *Supervisor) sampleStalls(ctx context.Context) {
	threshold := s.cfg.StallThreshold
	if threshold <= 0 {
		threshold = DefaultStallThreshold
	}
	sampler := newStallSampler(procReader{root: "/proc"}, threshold, s.log, s.metrics, s.childPIDs, s.stallLogContext)
	s.log.InfoContext(ctx, "envoy thread-stall sampler running",
		"interval", s.cfg.StallSampleInterval, "threshold", threshold, "window", stallWindow)

	ticker := time.NewTicker(s.cfg.StallSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case now := <-ticker.C:
			sampler.tick(now)
		}
	}
}

// childPIDs returns the supervised Envoy processes, epoch -> pid.
func (s *Supervisor) childPIDs() map[int]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	pids := make(map[int]int, len(s.children))
	for epoch, cmd := range s.children {
		if cmd != nil && cmd.Process != nil {
			pids[epoch] = cmd.Process.Pid
		}
	}
	return pids
}

// stallLogContext places a stall in the hot-restart lifecycle: how many epochs
// this supervisor runs, and the cross-pod predecessor while a handoff is in
// flight (-1: none).
func (s *Supervisor) stallLogContext() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []any{"trackedEpochs", len(s.children), "handoffPeer", s.handoffPeer}
}
