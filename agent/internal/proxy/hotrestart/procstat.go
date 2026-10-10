package hotrestart

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procfs readers for the Envoy thread-stall sampler (stallsampler.go, issue
// #1093). Everything here is a plain file read under a configurable root so the
// tests can drive it from a fake tree; nothing needs ptrace. The supervisor is
// the Envoy process's parent and runs as the same user, which is all
// /proc/<pid>/task/<tid>/wchan requires (PTRACE_MODE_READ; Yama's
// ptrace_scope only gates ATTACH).

// threadStat is one sample of one thread.
type threadStat struct {
	// state is the one-letter scheduler state from /proc/<pid>/task/<tid>/stat:
	// R running or runnable, S interruptible sleep, D uninterruptible sleep, T/t
	// stopped, Z zombie.
	state byte
	// cpuNs and runqNs are the first two fields of
	// /proc/<pid>/task/<tid>/schedstat: nanoseconds spent ON a CPU and
	// nanoseconds spent RUNNABLE BUT WAITING for one. The second is what tells a
	// thread starved of CPU apart from one that is busy or blocked, and it needs
	// no tracing: the kernel accounts it whenever CONFIG_SCHED_INFO is on (it is
	// on the Talos kernel; verified on node D, 2026-10-01).
	cpuNs  uint64
	runqNs uint64
}

// nodeCPU is the aggregate and per-CPU jiffy counters from /proc/stat. The
// file is not namespaced, so from inside the proxy container it is the NODE's
// view, which is the point: a stall that every process on the node shares is a
// different problem from one only Envoy has.
type nodeCPU struct {
	total cpuTimes
	cpus  []cpuTimes
}

// cpuTimes is one "cpu" line of /proc/stat, in USER_HZ jiffies.
type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (c cpuTimes) sum() uint64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

// procReader reads the files under root ("/proc" outside tests).
type procReader struct {
	root string
}

// taskIDs lists the thread IDs of pid. A process that is gone yields an error
// wrapping os.ErrNotExist.
func (r procReader) taskIDs(pid int) ([]int, error) {
	entries, err := os.ReadDir(filepath.Join(r.root, strconv.Itoa(pid), "task"))
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

func (r procReader) taskFile(pid, tid int, name string) string {
	return filepath.Join(r.root, strconv.Itoa(pid), "task", strconv.Itoa(tid), name)
}

// threadName reads a thread's comm (Envoy names its workers "wrk:worker_<n>";
// the main thread keeps the executable's name).
func (r procReader) threadName(pid, tid int) (string, error) {
	b, err := os.ReadFile(r.taskFile(pid, tid, "comm"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// thread samples one thread's scheduler state and schedstat counters.
func (r procReader) thread(pid, tid int) (threadStat, error) {
	stat, err := os.ReadFile(r.taskFile(pid, tid, "stat"))
	if err != nil {
		return threadStat{}, err
	}
	state, err := parseTaskState(stat)
	if err != nil {
		return threadStat{}, err
	}
	sched, err := os.ReadFile(r.taskFile(pid, tid, "schedstat"))
	if err != nil {
		return threadStat{}, err
	}
	cpu, runq, err := parseSchedstat(sched)
	if err != nil {
		return threadStat{}, err
	}
	return threadStat{state: state, cpuNs: cpu, runqNs: runq}, nil
}

// wchan reads the kernel function a sleeping thread is blocked in ("0" or
// empty when it is running or the kernel withholds it).
func (r procReader) wchan(pid, tid int) string {
	b, err := os.ReadFile(r.taskFile(pid, tid, "wchan"))
	if err != nil {
		return ""
	}
	w := strings.TrimSpace(string(b))
	if w == "0" {
		return ""
	}
	return w
}

// nodeCPU reads /proc/stat.
func (r procReader) nodeCPU() (nodeCPU, error) {
	b, err := os.ReadFile(filepath.Join(r.root, "stat"))
	if err != nil {
		return nodeCPU{}, err
	}
	return parseProcStat(b)
}

// pressureTotal reads the cumulative stall time, in microseconds, from the
// given line ("some" or "full") of /proc/pressure/<resource>. ok is false when
// the kernel has no PSI or no such resource (irq pressure needs
// CONFIG_IRQ_TIME_ACCOUNTING).
func (r procReader) pressureTotal(resource, line string) (total uint64, ok bool) {
	b, err := os.ReadFile(filepath.Join(r.root, "pressure", resource))
	if err != nil {
		return 0, false
	}
	return parsePressureTotal(b, line)
}

// parseTaskState extracts the state letter from a /proc/<pid>/task/<tid>/stat
// line. The comm field is parenthesized and may itself contain spaces and
// parentheses, so the state is the first field after the LAST ')'.
func parseTaskState(stat []byte) (byte, error) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return 0, fmt.Errorf("malformed stat line %q", truncate(stat))
	}
	rest := bytes.TrimLeft(stat[i+1:], " ")
	if len(rest) == 0 {
		return 0, fmt.Errorf("malformed stat line %q", truncate(stat))
	}
	return rest[0], nil
}

// parseSchedstat returns the on-CPU and runqueue-wait nanoseconds.
func parseSchedstat(b []byte) (cpuNs, runqNs uint64, err error) {
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, 0, fmt.Errorf("malformed schedstat %q", truncate(b))
	}
	if cpuNs, err = strconv.ParseUint(f[0], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("schedstat cpu time: %w", err)
	}
	if runqNs, err = strconv.ParseUint(f[1], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("schedstat runqueue wait: %w", err)
	}
	return cpuNs, runqNs, nil
}

// parseProcStat parses the "cpu" (aggregate) and "cpu<N>" lines of /proc/stat.
func parseProcStat(b []byte) (nodeCPU, error) {
	var n nodeCPU
	found := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 9 {
			continue
		}
		var v [8]uint64
		for i := range v {
			x, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				return nodeCPU{}, fmt.Errorf("/proc/stat %s: %w", f[0], err)
			}
			v[i] = x
		}
		t := cpuTimes{user: v[0], nice: v[1], system: v[2], idle: v[3], iowait: v[4], irq: v[5], softirq: v[6], steal: v[7]}
		if f[0] == "cpu" {
			n.total = t
			found = true
			continue
		}
		n.cpus = append(n.cpus, t)
	}
	if err := sc.Err(); err != nil {
		return nodeCPU{}, err
	}
	if !found {
		return nodeCPU{}, errors.New("/proc/stat has no aggregate cpu line")
	}
	return n, nil
}

// parsePressureTotal finds total=<µs> on the named PSI line.
func parsePressureTotal(b []byte, line string) (uint64, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || f[0] != line {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "total="); ok {
				total, err := strconv.ParseUint(v, 10, 64)
				if err != nil {
					return 0, false
				}
				return total, true
			}
		}
	}
	return 0, false
}

func truncate(b []byte) string {
	const maxLen = 64
	if len(b) > maxLen {
		return string(b[:maxLen]) + "..."
	}
	return string(b)
}
