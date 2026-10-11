// Package procsched reads the kernel's per-thread scheduler accounting for a
// process from procfs and folds it into monotonic per-process totals.
//
// It exists to tell two kinds of idle-CPU overhead apart (issue #1131): a
// process that wakes MORE often (timeslices and voluntary switches per second
// go up) from one whose wakeups COST more (run-queue delay per timeslice, CPU
// migrations and involuntary switches go up at the same wakeup rate). The Go
// runtime's own metrics cannot say which: they see goroutines, not the threads
// the kernel schedules.
//
// The kernel keeps these counters per THREAD — /proc/<pid>/schedstat and the
// ctxt_switches lines of /proc/<pid>/status describe only the thread-group
// leader — so Reader walks /proc/<pid>/task. A thread that exits takes its
// counts with it; Accumulator turns the per-thread readings into totals that
// never go backwards. Everything is a plain file read under a configurable root
// (the tests drive it from a fake tree); nothing needs ptrace or privileges for
// the caller's own process.
//
// The supervisor's stall sampler (agent/internal/proxy/hotrestart/procstat.go,
// #1093/#1100) reads the same files for Envoy's threads. It lists them with
// Reader.ThreadIDs and parses their schedstat with ParseSchedstat, so the two
// cannot disagree about the kernel's format; what it samples besides (thread
// STATE, wait channels, per-CPU node load) is its own and stays there.
package procsched

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ThreadCounters is one reading of one thread's cumulative counters.
type ThreadCounters struct {
	// CPUNs and RunDelayNs are the first two fields of
	// /proc/<pid>/task/<tid>/schedstat: nanoseconds spent on a CPU and
	// nanoseconds spent runnable but waiting for one. Timeslices is the third:
	// how many times the thread was put on a CPU — one per wakeup, plus one per
	// preemption that got it back.
	CPUNs      uint64
	RunDelayNs uint64
	Timeslices uint64
	// Voluntary and Involuntary are the ctxt_switches lines of
	// /proc/<pid>/task/<tid>/status: switches the thread asked for (it blocked or
	// slept) and switches forced on it (preempted).
	Voluntary   uint64
	Involuntary uint64
	// Migrations is se.nr_migrations from /proc/<pid>/task/<tid>/sched: how many
	// times the thread was moved to another CPU. That file exists only on a
	// kernel built with CONFIG_SCHED_DEBUG; HasMigrations says whether it did.
	Migrations    uint64
	HasMigrations bool
}

// Reader reads the scheduler counters of every thread of one process.
type Reader struct {
	// Root is the procfs mount point; empty means "/proc".
	Root string
	// PID is the process directory under Root; empty means "self".
	PID string
}

func (r Reader) taskDir() string {
	root, pid := r.Root, r.PID
	if root == "" {
		root = "/proc"
	}
	if pid == "" {
		pid = "self"
	}
	return filepath.Join(root, pid, "task")
}

// ThreadIDs lists the process's thread IDs: the numeric entries of its task
// directory. A process that is gone yields an error wrapping os.ErrNotExist.
func (r Reader) ThreadIDs() ([]int, error) {
	entries, err := os.ReadDir(r.taskDir())
	if err != nil {
		return nil, err
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		tid, convErr := strconv.Atoi(e.Name())
		if convErr != nil {
			continue
		}
		tids = append(tids, tid)
	}
	return tids, nil
}

// Threads returns the counters of every live thread, keyed by thread ID. A
// thread that exits between the directory listing and its reads is left out.
// It fails only when the task directory cannot be listed, or no thread at all
// could be read.
func (r Reader) Threads() (map[int]ThreadCounters, error) {
	dir := r.taskDir()
	tids, err := r.ThreadIDs()
	if err != nil {
		return nil, err
	}
	out := make(map[int]ThreadCounters, len(tids))
	var lastErr error
	for _, tid := range tids {
		c, readErr := readThread(filepath.Join(dir, strconv.Itoa(tid)))
		if readErr != nil {
			lastErr = readErr
			continue
		}
		out[tid] = c
	}
	if len(out) == 0 && lastErr != nil {
		return nil, fmt.Errorf("no thread of %s readable: %w", dir, lastErr)
	}
	return out, nil
}

func readThread(dir string) (ThreadCounters, error) {
	var c ThreadCounters
	schedstat, err := os.ReadFile(filepath.Join(dir, "schedstat"))
	if err != nil {
		return c, err
	}
	if c.CPUNs, c.RunDelayNs, c.Timeslices, err = ParseSchedstat(schedstat); err != nil {
		return c, err
	}
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return c, err
	}
	c.Voluntary, c.Involuntary = parseStatus(status)
	if sched, schedErr := os.ReadFile(filepath.Join(dir, "sched")); schedErr == nil {
		c.Migrations, c.HasMigrations = parseSchedField(sched, "se.nr_migrations")
	}
	return c, nil
}

// ParseSchedstat parses the content of a /proc/<pid>/task/<tid>/schedstat file,
// "<cpu_ns> <runq_ns> <timeslices>": nanoseconds on a CPU, nanoseconds runnable
// but waiting for one, and the number of timeslices run. The kernel always
// writes all three (three zeros when scheduler accounting is off), so fewer is
// an error.
func ParseSchedstat(b []byte) (cpu, runq, slices uint64, err error) {
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return 0, 0, 0, fmt.Errorf("schedstat: want 3 fields, got %q", strings.TrimSpace(string(b)))
	}
	vals := [3]uint64{}
	for i := range vals {
		if vals[i], err = strconv.ParseUint(f[i], 10, 64); err != nil {
			return 0, 0, 0, fmt.Errorf("schedstat field %d: %w", i, err)
		}
	}
	return vals[0], vals[1], vals[2], nil
}

// parseStatus extracts the two ctxt_switches lines of a status file. A missing
// line reads as zero.
func parseStatus(b []byte) (voluntary, involuntary uint64) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "voluntary_ctxt_switches":
			voluntary = n
		case "nonvoluntary_ctxt_switches":
			involuntary = n
		}
	}
	return voluntary, involuntary
}

// parseSchedField finds "<name>   :   <value>" in a /proc/<pid>/sched file.
func parseSchedField(b []byte, name string) (uint64, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// Totals are process-wide counters summed over every thread ever observed.
// They never decrease.
type Totals struct {
	CPUNs       uint64
	RunDelayNs  uint64
	Timeslices  uint64
	Voluntary   uint64
	Involuntary uint64
	Migrations  uint64
	// HasMigrations is whether any thread's migrations were readable.
	HasMigrations bool
	// Threads is the number of threads in the latest reading.
	Threads int
}

// Accumulator folds successive per-thread readings into monotonic Totals: it
// adds each thread's growth since its previous reading and so keeps the
// contribution of threads that have since exited. It is safe for concurrent use
// (an OTel callback runs once per metric reader).
type Accumulator struct {
	mu     sync.Mutex
	last   map[int]ThreadCounters
	totals Totals
}

// growth is cur-prev for a counter, or cur when the counter went backwards (the
// thread ID was reused by a new thread, whose counts start from zero).
func growth(cur, prev uint64) uint64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// Update adds a reading and returns the new totals.
func (a *Accumulator) Update(threads map[int]ThreadCounters) Totals {
	a.mu.Lock()
	defer a.mu.Unlock()
	for tid, cur := range threads {
		prev := a.last[tid]
		a.totals.CPUNs += growth(cur.CPUNs, prev.CPUNs)
		a.totals.RunDelayNs += growth(cur.RunDelayNs, prev.RunDelayNs)
		a.totals.Timeslices += growth(cur.Timeslices, prev.Timeslices)
		a.totals.Voluntary += growth(cur.Voluntary, prev.Voluntary)
		a.totals.Involuntary += growth(cur.Involuntary, prev.Involuntary)
		if cur.HasMigrations {
			a.totals.HasMigrations = true
			a.totals.Migrations += growth(cur.Migrations, prev.Migrations)
		}
	}
	a.last = threads
	a.totals.Threads = len(threads)
	return a.totals
}

// ErrUnsupported reports that procfs scheduler accounting is not available here
// (not Linux, or /proc not mounted).
var ErrUnsupported = errors.New("procfs scheduler accounting unavailable")

// Probe checks that r can read at least one thread, wrapping ErrUnsupported
// when the task directory does not exist.
func (r Reader) Probe() error {
	if _, err := r.Threads(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %w", ErrUnsupported, err)
		}
		return err
	}
	return nil
}
