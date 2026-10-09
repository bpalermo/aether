package prober

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"sync"
	"time"
)

// The per-failure log line (#1040). The metrics say HOW MANY probes failed per minute;
// they cannot say WHEN inside that minute, with what error, or how long each attempt
// took, so a burst like #1040's (~142 mesh_dns timeouts from one pod in <=30 s) could not
// be lined up against a proxy hot-restart. Each failed probe therefore prints one line:
//
//	AETHER_PROBE_FAIL {"t":...,"tier":...,"target":...,"result":...,"err":...,"elapsed_ms":...,
//	  "phase":...,"reused":...,"conn_ms":...,"dns_ms":...,"connect_ms":...,"tls_ms":...,
//	  "write_ms":...,"ttfb_ms":...,"dial":...,"remote":...,"local":...,"trace_id":...,
//	  "pod":...,"node":...,"n":...,"truncated":...}
//
// the same shape as the AETHER_FAIL sample of the soak's former k6 load script: one
// JSON object per line behind a fixed greppable marker, with the timestamp as the
// load-bearing field. elapsed_ms separates a probe that burned the whole 2 s budget
// (timeout) from a fast refusal (connection_error); phase and the *_ms fields (#1252, see
// phase.go) say which step of the request the time went to. Every key is always present;
// a *_ms of -1 means that phase never started. pod and node are the prober's own: the
// SOURCE. dial, remote, local and trace_id (#1391, see phaseTimings) are what the
// client knows of where the probe went, and the key that finds the rest in the proxies'
// access logs.
//
// It is bounded so a burst cannot flood the log pipeline: at most failLogCap detail lines
// per (tier, result) per failLogWindow. Past the cap the failures are only counted, and
// when the window closes ONE summary line carries the count, under the same marker:
//
//	AETHER_PROBE_FAIL {"t":...,"tier":...,"result":...,"suppressed":122,"window_s":60,"window_start":...,"pod":...,"node":...}
//
// A summary's t is the time it was written, which is when its window was closed; the
// failures it counts lie between window_start (the window's first failure) and t. A
// window is closed by the first once-a-window flush that finds it a full window old, so
// t is less than two window_s after window_start; a prober that is stopping closes every
// open window at once, at the time it stops (#1463: it used to stamp that summary one
// window in the future).
//
// The budget renews every window (it is not a lifetime cap), so the next burst, weeks
// later, is still attributable.
const (
	failLinePrefix = "AETHER_PROBE_FAIL "
	failLogCap     = 20
	failLogWindow  = time.Minute
)

// errSaturated is the cause logged for a probe that was never sent because
// Config.MaxConcurrent probes to the target were already in flight.
var errSaturated = errors.New("max in-flight probes reached; probe not sent")

type failKey struct{ tier, result string }

type failWindow struct {
	start      time.Time
	logged     int
	suppressed int
}

// failLine is one per-failure detail line. Field order is the JSON key order.
type failLine struct {
	T         string  `json:"t"`
	Tier      string  `json:"tier"`
	Target    string  `json:"target"`
	Result    string  `json:"result"`
	Err       string  `json:"err"`
	ElapsedMS float64 `json:"elapsed_ms"`
	phaseTimings
	Pod       string `json:"pod"`
	Node      string `json:"node"`
	N         int    `json:"n"`
	Truncated bool   `json:"truncated"`
}

// failSummary is the once-per-window count of the failures the cap suppressed. T is
// when the summary was written; WindowStart is when its window opened (the window's
// first failure), so the failures it counts lie in [WindowStart, T]. WindowS is the
// cap's period, not the length of this window: a window closed at shutdown is shorter.
type failSummary struct {
	T           string  `json:"t"`
	Tier        string  `json:"tier"`
	Result      string  `json:"result"`
	Suppressed  int     `json:"suppressed"`
	WindowS     float64 `json:"window_s"`
	WindowStart string  `json:"window_start"`
	Pod         string  `json:"pod"`
	Node        string  `json:"node"`
}

// failLog writes the bounded AETHER_PROBE_FAIL lines. It is safe for concurrent use:
// every probe goroutine of every target shares one.
type failLog struct {
	mu     sync.Mutex
	out    io.Writer
	pod    string
	node   string
	cap    int
	window time.Duration
	keys   map[failKey]*failWindow
}

func newFailLog(out io.Writer, pod, node string, capPerWindow int, window time.Duration) *failLog {
	return &failLog{
		out: out, pod: pod, node: node, cap: capPerWindow, window: window,
		keys: make(map[failKey]*failWindow),
	}
}

// log records one failed probe observed at now, with the phase timings of its trace
// (noPhase when it never reached the transport).
func (f *failLog) log(now time.Time, t target, result string, elapsedSeconds float64, err error, pt phaseTimings) {
	if f == nil || f.out == nil {
		return
	}
	k := failKey{tier: t.tier, result: result}
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.keys[k]
	if w != nil && now.Sub(w.start) >= f.window {
		f.closeWindow(k, w, now)
		w = nil
	}
	if w == nil {
		w = &failWindow{start: now}
		f.keys[k] = w
	}
	if w.logged >= f.cap {
		w.suppressed++
		return
	}
	w.logged++
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	f.write(failLine{
		T:            now.UTC().Format(time.RFC3339Nano),
		Tier:         t.tier,
		Target:       t.name,
		Result:       result,
		Err:          errStr,
		ElapsedMS:    math.Round(elapsedSeconds*1e4) / 10, // 0.1 ms resolution
		phaseTimings: pt,
		Pod:          f.pod,
		Node:         f.node,
		N:            w.logged,
		Truncated:    w.logged == f.cap,
	})
}

// flush closes every window that has run its full length by now, printing the summary
// for any that suppressed failures. Run calls it once per window so the tail of a burst
// is reported even when no further failure arrives to close its window.
func (f *failLog) flush(now time.Time) { f.closeWindows(now, false) }

// flushAll closes every open window, however young, at now: the prober is stopping and
// nothing will close them later. now is the real time, and it is what the summaries
// carry. Which windows to close and what time it is are separate arguments (#1463: the
// shutdown path used to call flush with a now one window in the future, to get the
// first, and so printed that future time).
func (f *failLog) flushAll(now time.Time) { f.closeWindows(now, true) }

func (f *failLog) closeWindows(now time.Time, all bool) {
	if f == nil || f.out == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]failKey, 0, len(f.keys))
	for k := range f.keys {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b failKey) int {
		return cmp.Or(cmp.Compare(a.tier, b.tier), cmp.Compare(a.result, b.result))
	})
	for _, k := range keys {
		if w := f.keys[k]; all || now.Sub(w.start) >= f.window {
			f.closeWindow(k, w, now)
		}
	}
}

// closeWindow prints the summary for w (only when it suppressed anything) and forgets it.
// f.mu must be held.
func (f *failLog) closeWindow(k failKey, w *failWindow, now time.Time) {
	if w.suppressed > 0 {
		f.write(failSummary{
			T:           now.UTC().Format(time.RFC3339Nano),
			Tier:        k.tier,
			Result:      k.result,
			Suppressed:  w.suppressed,
			WindowS:     f.window.Seconds(),
			WindowStart: w.start.UTC().Format(time.RFC3339Nano),
			Pod:         f.pod,
			Node:        f.node,
		})
	}
	delete(f.keys, k)
}

// write prints one marker-prefixed JSON line as a single Write. f.mu must be held.
func (f *failLog) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return // the structs above hold only strings and numbers; cannot fail
	}
	line := append([]byte(failLinePrefix), b...)
	line = append(line, '\n')
	_, _ = f.out.Write(line)
}
