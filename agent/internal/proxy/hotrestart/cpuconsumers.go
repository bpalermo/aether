package hotrestart

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Who had the CPU while Envoy was starved of it (issue #1392).
//
// A [starved] stall line (stallsampler.go) says an Envoy thread was runnable and
// not scheduled, and that the node was busy (nodeBusyPct, nodePSICPUSomeMs). It
// did not say WHO was on the CPUs, and two investigations (#1320, #1389) ended
// there: nothing on the node records CPU per consumer at one-second resolution.
// This file adds the top few consumers over the stall interval to that line.
//
// Two sources, by what the proxy container can read:
//
//   - cgroup v2 cpu.stat (usage_usec) under /sys/fs/cgroup. The proxy container
//     is privileged, and the container runtime gives a privileged container the
//     HOST's cgroup namespace (containerd: "cgroupns is not used when running in
//     cgroup v1 mode or in privileged"), so the node's whole hierarchy is
//     visible with no extra mount: on Talos /init, /system/*, /podruntime/kubelet,
//     /podruntime/runtime and /kubepods/... down to one entry per pod. The kernel
//     does this accounting anyway, it includes processes that lived and died
//     inside the interval (an exec probe, a runc shim), and one file covers a
//     whole pod. This is the source that works today.
//   - /proc/<pid>/stat (utime+stime, with the process comm). It names a process
//     rather than a cgroup, but it only sees the node when the pod shares the
//     host PID namespace, which the chart does not ask for (no hostPID). Without
//     it /proc lists this container's own processes, which the line already
//     describes, so the scan is skipped and the field says why.
//
// Cost model. A delta needs a "before", and a stall is only known once it is
// over (the kernel charges a runqueue wait when it ends), so the baseline cannot
// be taken lazily at the start of a stall. Instead:
//
//   - cgroups: one sample per one-second window, always: one small file per
//     cgroup down to pod level, about what the thread sampler itself reads per
//     second, for an exact per-window delta. A short history of these samples
//     lets a starvation that lasted several seconds (reported whole in the
//     window it ended in) be measured over its whole length. Listing the
//     directories costs more than reading the files, so the set of cgroups is
//     listed every cgroupListInterval and reused in between: a pod created
//     since the last listing is counted in its parent's entry until the next
//     one. The interval holds in a starved window too (a node starved for a
//     minute would otherwise never see the pods created in it): the sample a
//     stall line is made from is then the listing, inside the same scan
//     budget.
//   - processes: a full /proc scan is several times dearer (one file per
//     process), so the rolling baseline is taken every procBaselineInterval and
//     a second scan only when a window reports a starved thread. The delta then
//     covers up to that interval before the stall, which dilutes a short burst;
//     the line says over how long it was taken. The scan made for one starved
//     window is the baseline of the next, so a stall that lasts gets exact
//     per-window deltas from its second line on.
//
// Nothing here can fail or hold back the stall line: every error becomes an
// "unavailable: <reason>" value of the field, and a scan that runs past
// consumerScanBudget is abandoned.

const (
	// DefaultStallTopConsumers is how many consumers a [starved] line names per
	// source (--stall-top-consumers).
	DefaultStallTopConsumers = 5
	// maxStallTopConsumers bounds the flag, and with it the line.
	maxStallTopConsumers = 20

	// cgroupMaxDepth is how far below the cgroup root the walk goes. Four
	// reaches a pod under both kubelet cgroup drivers: cgroupfs (Talos)
	// /kubepods/burstable/pod<uid> at depth 3, systemd
	// /kubelet.slice/kubelet-kubepods.slice/…-burstable.slice/…-pod<uid>.slice at
	// depth 4. A pod's cgroup is never descended into: one entry per pod.
	cgroupMaxDepth = 4
	// cgroupMaxCount and procMaxCount bound one sample. A walk that would
	// exceed its bound is not reported (a partial sample makes the entries it
	// missed look new, with their whole lifetime's CPU as the delta).
	cgroupMaxCount = 1024
	procMaxCount   = 8192

	// procBaselineInterval is how often the rolling /proc baseline is taken
	// while nothing is starved.
	procBaselineInterval = 10 * time.Second
	// cgroupListInterval is how often the cgroup directories are listed
	// again; between listings a sample reads cpu.stat of the cgroups already
	// known.
	cgroupListInterval = 10 * time.Second
	// cgroupHistoryLen and procHistoryLen are how many samples are kept to
	// pick a baseline from: about half a minute of each.
	cgroupHistoryLen = 32
	procHistoryLen   = 4
	// consumerScanBudget is the wall time one scan may take before it is
	// abandoned, so a slow /proc or cgroupfs cannot hold the stall line back.
	// It is checked before every file, before every directory listing and
	// once after the last, so a scan overruns it by at most one read or one
	// listing (never one on top of the other), and a scan that finished late
	// is not reported either.
	consumerScanBudget = 250 * time.Millisecond

	// Names are truncated so N entries bound the line. A cgroup path keeps its
	// tail (the most specific part: "/kubepods/burstable/pod<uid>" is 59
	// bytes and fits whole); a process comm keeps its head.
	cgroupNameMax = 64
	procNameMax   = 24

	// usecPerTick converts /proc/<pid>/stat's utime/stime: they are in USER_HZ
	// ticks, which the Linux ABI fixes at 100 per second.
	usecPerTick = 10_000
	// statReadMax is the read buffer for one file. /proc/<pid>/stat and a
	// cgroup's cpu.stat are a few hundred bytes; what is needed from each sits
	// at the front (the 22nd field; the first line).
	statReadMax = 1024

	attrTopCgroups       = "topCgroups"
	attrTopCgroupsOverMs = "topCgroupsOverMs"
	attrTopProcs         = "topProcs"
	attrTopProcsOverMs   = "topProcsOverMs"
	// The truncated fields are on the line (as true) only when the stall
	// began before the oldest sample kept, so the figures cover its END only.
	attrTopCgroupsTruncated = "topCgroupsTruncated"
	attrTopProcsTruncated   = "topProcsTruncated"
)

var (
	errScanBudget   = errors.New("scan exceeded its time budget")
	errTooMany      = errors.New("too many entries to sample")
	errNoBaseline   = errors.New("no earlier sample yet")
	errPodPIDNS     = errors.New("/proc shows only this pod's PID namespace (no hostPID)")
	errNoCgroupV2   = errors.New("no cgroup v2 hierarchy")
	errOwnCgroup    = errors.New("only this container's cgroup is visible")
	errMalformed    = errors.New("malformed")
	errRootNotInOld = errors.New("the earlier sample has no root entry")
)

// statFS is the little of a filesystem the consumer sampler needs, so the
// tests can drive it from an in-memory tree (and inject the errors a real
// procfs produces: a process that exits mid-scan, an entry it may not read).
type statFS interface {
	// subdirs lists the names of the directories in dir. more is true when
	// the listing was cut at limit.
	subdirs(dir string, limit int) (names []string, more bool, err error)
	// readFile returns the head of a small file (at most statReadMax bytes).
	// The bytes are only valid until the next call.
	readFile(name string) ([]byte, error)
}

// osStatFS reads the real procfs and cgroupfs. It is not safe for concurrent
// use: every read shares one buffer, so a sample of a few hundred files
// allocates nothing per file.
type osStatFS struct {
	buf [statReadMax]byte
}

func (*osStatFS) subdirs(dir string, limit int) ([]string, bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, false, unwrapPathError(err)
	}
	defer func() { _ = f.Close() }()
	var names []string
	for {
		// The d_type the kernel returns with each entry tells a directory
		// from a file without a stat (procfs and cgroupfs both fill it in).
		entries, readErr := f.ReadDir(256)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if len(names) >= limit {
				return names, true, nil
			}
			names = append(names, e.Name())
		}
		if errors.Is(readErr, io.EOF) {
			return names, false, nil
		}
		if readErr != nil {
			return names, false, unwrapPathError(readErr)
		}
	}
}

// readFile is open, one read, close: three system calls, where os.ReadFile
// makes five (an fstat and a second read to see EOF). These files are
// generated whole on the first read.
func (fs *osStatFS) readFile(name string) ([]byte, error) {
	var fd int
	for {
		var err error
		fd, err = syscall.Open(name, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EINTR) {
			return nil, err
		}
	}
	defer func() { _ = syscall.Close(fd) }()
	for {
		n, err := syscall.Read(fd, fs.buf[:])
		if err == nil {
			return fs.buf[:n], nil
		}
		if !errors.Is(err, syscall.EINTR) {
			return nil, err
		}
	}
}

// unwrapPathError drops the path from an *os.PathError: the reason is logged,
// and the path adds length without adding anything the field name does not say.
func unwrapPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// consumer is one entry of a top list: CPU time over the interval.
type consumer struct {
	name string
	cpu  time.Duration
}

// topConsumers orders by CPU time, largest first (name breaks a tie, so the
// line is deterministic), drops what would print as 0ms and keeps n. The line
// reports whole milliseconds, so anything under one would take a slot to say
// `name=0ms`.
func topConsumers(all []consumer, n int) []consumer {
	all = slices.DeleteFunc(all, func(c consumer) bool { return c.cpu < time.Millisecond })
	slices.SortFunc(all, func(a, b consumer) int {
		return cmp.Or(cmp.Compare(b.cpu, a.cpu), cmp.Compare(a.name, b.name))
	})
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// formatConsumers renders "name=<n>ms name=<n>ms", or "none" when nothing used
// a millisecond.
func formatConsumers(top []consumer) string {
	if len(top) == 0 {
		return "none"
	}
	b := &strings.Builder{}
	for i, c := range top {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(b, "%s=%dms", c.name, c.cpu.Milliseconds())
	}
	return b.String()
}

// cleanName makes a kernel-supplied name safe as one token of the line: a comm
// may hold spaces, quotes or "=" ("tmux: server", "(sd-pam)"), and the entries
// are separated by spaces and split at "=".
func cleanName(s string) string {
	return strings.Map(func(r rune) rune {
		if r <= ' ' || r == '=' || r == '"' || r == 0x7f {
			return '_'
		}
		return r
	}, s)
}

// headName keeps the first limit bytes; tailName the last limit bytes.
func headName(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit-3] + "..."
}

func tailName(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return "..." + s[len(s)-limit+3:]
}

// ---- cgroups ---------------------------------------------------------------

// cgroupSample is cpu.stat's usage_usec for every sampled cgroup, keyed by its
// path under the cgroup root ("/" is the root itself). usage_usec is inclusive:
// a cgroup's figure contains its descendants'.
type cgroupSample struct {
	at    time.Time
	usage map[string]uint64
	// unknown is the cgroups that are there but whose cpu.stat could not be
	// read or parsed in this sample (anything but "it is gone"). Like a
	// process's unread stat: a later sample that can read it has no delta to
	// take from this one, and must not take the cgroup for a new one.
	unknown map[string]struct{}
}

func (s cgroupSample) when() time.Time { return s.at }

// isPodCgroup reports whether a cgroup directory is one pod's: "pod<uid>"
// (cgroupfs driver) or "…-pod<uid>.slice" (systemd driver). "podruntime" and
// "kubepods" are not.
func isPodCgroup(name string) bool {
	i := strings.LastIndex(name, "pod")
	if i < 0 {
		return false
	}
	rest := name[i+len("pod"):]
	const uidHead = 8 // a UID starts with eight hex digits
	if len(rest) < uidHead {
		return false
	}
	for _, c := range []byte(rest[:uidHead]) {
		if !isHex(c) {
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// parseCgroupUsage finds usage_usec in a cpu.stat.
func parseCgroupUsage(b []byte) (uint64, error) {
	for len(b) > 0 {
		var line []byte
		line, b, _ = bytes.Cut(b, []byte{'\n'})
		if v, ok := bytes.CutPrefix(line, []byte("usage_usec ")); ok {
			return strconv.ParseUint(string(bytes.TrimSpace(v)), 10, 64)
		}
	}
	return 0, fmt.Errorf("cpu.stat: %w", errMalformed)
}

// cgroupDeltas turns two samples into CPU time per cgroup over the interval,
// EXCLUSIVE of the sampled cgroups below it, so the entries do not overlap and
// the list can be ranked: "/kubepods" is what ran in /kubepods outside every
// pod that has its own entry, and "/" is what was charged to the root cgroup
// itself once every sampled cgroup below it is taken out (commonly kernel
// threads, but also any userspace task attached directly to the root; every
// task is in some cgroup). A pod's entry, and any cgroup at the depth limit,
// is its whole subtree.
//
// A cgroup that is only in the earlier sample was removed: the kernel keeps its
// usage in its ancestors, so what it used in the interval is reported by the
// nearest sampled ancestor. A cgroup that is only in the later sample was first
// listed inside the interval: its usage counts, but it may be up to
// cgroupListInterval older than the listing that found it, so it gets at most
// what is left of its parent's delta once the siblings that are in both samples
// have taken theirs. The entries therefore never add up to more than the root's
// delta, whatever was listed when.
//
// A cgroup the earlier sample knew of but could not read is neither: its
// counter is its lifetime's, so it gets no entry and what it used stays in its
// parent's (nothing below it gets one either). And whatever went wrong with a
// baseline, no cgroup can have used more than the interval on every CPU of the
// node, so each delta is held to that as well as to its parent's. ncpu is the
// NODE's CPU count (cpuConsumers.ncpu); 0, not known, holds nothing.
func cgroupDeltas(base, cur cgroupSample, ncpu int) ([]consumer, error) {
	baseRoot, ok := base.usage["/"]
	if !ok {
		return nil, errRootNotInOld
	}
	curRoot, ok := cur.usage["/"]
	if !ok {
		return nil, errNoCgroupV2
	}
	// Parents before children (by depth), and among the cgroups of one depth
	// those in both samples before the newly listed ones, so an exact delta is
	// never squeezed by a sibling's estimate.
	paths := make([]string, 0, len(cur.usage))
	for p := range cur.usage {
		if p != "/" {
			paths = append(paths, p)
		}
	}
	isNew := func(p string) bool { _, seen := base.usage[p]; return !seen }
	slices.SortFunc(paths, func(a, b string) int {
		return cmp.Or(
			cmp.Compare(strings.Count(a, "/"), strings.Count(b, "/")),
			cmp.Compare(btoi(isNew(a)), btoi(isNew(b))),
			cmp.Compare(a, b),
		)
	})

	// left[p] starts as p's delta and ends as the part of it no sampled child
	// accounts for: p's exclusive time.
	limit := uint64(math.MaxUint64)
	if span := cur.at.Sub(base.at); span > 0 && ncpu > 0 {
		limit = uint64(span.Microseconds()) * uint64(ncpu)
	}
	left := make(map[string]uint64, len(paths)+1)
	left["/"] = min(counterDelta(curRoot, baseRoot), limit)
	for _, p := range paths {
		parent := path.Dir(p)
		d := cur.usage[p]
		if before, seen := base.usage[p]; seen {
			d = counterDelta(d, before)
		} else if _, unread := base.unknown[p]; unread {
			continue
		}
		// Also the floor for read skew: the files are not read atomically, so
		// known children can add up to a little more than their parent.
		d = min(d, left[parent])
		left[p] = d
		left[parent] -= d
	}
	paths = append(paths, "/")

	out := make([]consumer, 0, len(paths))
	for _, p := range paths {
		out = append(out, consumer{
			name: tailName(cleanName(p), cgroupNameMax),
			cpu:  usecDuration(left[p]),
		})
	}
	return out, nil
}

// usecDuration converts a microsecond count read from the kernel to a
// Duration, saturating instead of overflowing: the counters are unsigned 64-bit
// and a Duration is signed nanoseconds, so anything past about 292 years of CPU
// time would otherwise wrap to a negative figure.
func usecDuration(usec uint64) time.Duration {
	const maxUsec = math.MaxInt64 / int64(time.Microsecond)
	if usec > uint64(maxUsec) {
		return math.MaxInt64
	}
	return time.Duration(usec) * time.Microsecond
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- processes -------------------------------------------------------------

// procKey identifies a process across samples. The start time (in ticks since
// boot) tells a PID from the next process to be given the same number.
type procKey struct {
	pid   int
	start uint64
}

type procUse struct {
	comm string
	usec uint64
}

type procSample struct {
	at    time.Time
	procs map[procKey]procUse
	// unknown is the PIDs that were listed but whose stat could not be read or
	// parsed for a reason other than the process having exited. The process
	// is there and its counters are not known, so a later sample that can
	// read it has nothing to take a delta from.
	unknown map[int]struct{}
}

func (s procSample) when() time.Time { return s.at }

// parseProcCPU reads comm, utime+stime and starttime from a /proc/<pid>/stat
// line. The comm is parenthesized and may itself hold spaces and parentheses,
// so the numbered fields are counted from the LAST ')': state is field 3,
// utime 14, stime 15, starttime 22.
func parseProcCPU(b []byte) (comm string, usec, start uint64, err error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return "", 0, 0, fmt.Errorf("stat: %w", errMalformed)
	}
	const (
		utimeAfterComm = 14 - 3 // index into the fields after the comm
		stimeAfterComm = 15 - 3
		startAfterComm = 22 - 3
	)
	f := bytes.Fields(b[end+1:])
	if len(f) <= startAfterComm {
		return "", 0, 0, fmt.Errorf("stat: %w", errMalformed)
	}
	utime, err := strconv.ParseUint(string(f[utimeAfterComm]), 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("stat utime: %w", errMalformed)
	}
	stime, err := strconv.ParseUint(string(f[stimeAfterComm]), 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("stat stime: %w", errMalformed)
	}
	if start, err = strconv.ParseUint(string(f[startAfterComm]), 10, 64); err != nil {
		return "", 0, 0, fmt.Errorf("stat starttime: %w", errMalformed)
	}
	return string(b[open+1 : end]), ticksToUsec(utime, stime), start, nil
}

// ticksToUsec is (utime+stime) in microseconds, saturating: a garbled stat
// line must not wrap into a small, plausible figure.
func ticksToUsec(utime, stime uint64) uint64 {
	const maxTicks = math.MaxUint64 / usecPerTick
	if utime > maxTicks || stime > maxTicks-utime {
		return math.MaxUint64
	}
	return (utime + stime) * usecPerTick
}

// procDeltas is CPU time per process over the interval. A process that is only
// in the later sample (a new PID, or a reused PID with another start time)
// started inside the interval, so all of its CPU time counts. One that exited
// is not listed: what it used after the earlier sample is not recorded
// anywhere this can read (its cgroup's entry has it).
//
// A process the earlier scan listed but could not read is not listed either:
// it is not new, and charging it as new would put its whole lifetime's CPU on
// the line. (Failing the whole scan instead would let one entry that is never
// readable turn the field off for good.) And whatever the cause, no process can
// have used more than the interval on every CPU of the node, so each figure is
// held to that: a wrong baseline can then mislead by at most what was possible
// (ncpu as in cgroupDeltas: the node's count, 0 holds nothing).
func procDeltas(base, cur procSample, ncpu int) []consumer {
	limit := uint64(math.MaxUint64)
	if span := cur.at.Sub(base.at); span > 0 && ncpu > 0 {
		limit = uint64(span.Microseconds()) * uint64(ncpu)
	}
	out := make([]consumer, 0, len(cur.procs))
	for k, now := range cur.procs {
		d := now.usec
		if before, seen := base.procs[k]; seen {
			d = counterDelta(d, before.usec)
		} else if _, unread := base.unknown[k.pid]; unread {
			continue
		}
		d = min(d, limit)
		out = append(out, consumer{
			name: headName(cleanName(now.comm), procNameMax) + "(" + strconv.Itoa(k.pid) + ")",
			cpu:  usecDuration(d),
		})
	}
	return out
}

// ---- sampler ---------------------------------------------------------------

// sampleHistory keeps the last few samples, oldest first.
type sampleHistory[S interface{ when() time.Time }] struct {
	limit   int
	samples []S
}

func (h *sampleHistory[S]) push(s S) {
	if len(h.samples) >= h.limit {
		h.samples = slices.Delete(h.samples, 0, len(h.samples)-h.limit+1)
	}
	h.samples = append(h.samples, s)
}

// baseline returns the newest sample taken at or before since: the latest
// "before" that still covers the whole stall. When the stall began before all
// of them it returns the oldest one kept with truncated set: only the END of
// the stall is covered, and the reported interval is shorter than the stall.
func (h *sampleHistory[S]) baseline(since time.Time) (base S, truncated, ok bool) {
	if len(h.samples) == 0 {
		return base, false, false
	}
	for _, s := range slices.Backward(h.samples) {
		if !s.when().After(since) {
			return s, false, true
		}
	}
	return h.samples[0], true, true
}

// topList is one source's part of a stall line.
type topList struct {
	consumers []consumer
	// over is the interval the figures were taken over; truncated says it
	// starts after the stall did.
	over      time.Duration
	truncated bool
	err       error
}

// cpuConsumers samples the two sources and reports the top consumers of a
// stall. It is driven by the stall sampler's window: roll at a window with no
// starved thread, report at one with.
type cpuConsumers struct {
	fs         statFS
	procRoot   string
	cgroupRoot string
	topN       int
	// clock is the wall clock the scan budget is measured on.
	clock func() time.Time
	// ncpu is the NODE's online CPU count (onlineCPUsPath), which bounds what
	// any consumer can have used over an interval; 0 when it could not be
	// read (ncpuErr says why), and then nothing is bounded by it. It is not
	// runtime.NumCPU: that is the CPUs THIS process may run on, and both
	// sources are node-wide, so under a cpuset it would cut real usage down
	// and leave the wrong consumers on top. Read again every
	// cgroupListInterval (ncpuReadAt), for a CPU brought online later.
	ncpu       int
	ncpuErr    error
	ncpuReadAt time.Time

	cgroups sampleHistory[cgroupSample]
	procs   sampleHistory[procSample]

	// cgroupPaths is the set of cgroups found by the last listing that
	// succeeded (without the root), reused until cgroupListedAt is
	// cgroupListInterval old. cgroupListedAt is the last ATTEMPT, so a listing
	// that fails (too many cgroups, over its budget) is not tried again every
	// window: with no set to fall back on, cgroupListErr is what a sample
	// returns until the next attempt is due.
	cgroupPaths    []string
	cgroupListedAt time.Time
	cgroupListErr  error

	// procErr is why processes are not sampled at all (the PID namespace),
	// decided once: a process cannot change namespace.
	procChecked bool
	procErr     error
	lastProcAt  time.Time
}

func newCPUConsumers(fs statFS, procRoot, cgroupRoot string, topN int) *cpuConsumers {
	return &cpuConsumers{
		fs:         fs,
		procRoot:   procRoot,
		cgroupRoot: cgroupRoot,
		topN:       min(topN, maxStallTopConsumers),
		clock:      time.Now,
		cgroups:    sampleHistory[cgroupSample]{limit: cgroupHistoryLen},
		procs:      sampleHistory[procSample]{limit: procHistoryLen},
	}
}

// nodeCPUConsumers is the sampler over the real /proc and /sys/fs/cgroup, or nil
// when topN (--stall-top-consumers) is 0 or less: the sampling is off and
// nothing is read.
func nodeCPUConsumers(topN int) *cpuConsumers {
	if topN <= 0 {
		return nil
	}
	return newCPUConsumers(&osStatFS{}, "/proc", "/sys/fs/cgroup", topN)
}

// refreshNodeCPUs reads the node's online CPU count when the last reading is
// cgroupListInterval old. A count that cannot be read or parsed is no count:
// the deltas are then not held to "the interval on every CPU" at all, which is
// better than holding them to a number that describes something else.
func (c *cpuConsumers) refreshNodeCPUs(now time.Time) {
	if !c.ncpuReadAt.IsZero() && now.Sub(c.ncpuReadAt) < cgroupListInterval {
		return
	}
	c.ncpuReadAt = now
	c.ncpu, c.ncpuErr = 0, nil
	b, err := c.fs.readFile(onlineCPUsPath)
	if err == nil {
		c.ncpu, err = parseCPUList(strings.TrimSpace(string(b)))
	}
	if err != nil {
		c.ncpu, c.ncpuErr = 0, err
	}
}

// nodeCPUs is the node's CPU count as the startup line reports it: the number,
// or why there is none and what that means for the figures.
func (c *cpuConsumers) nodeCPUs(now time.Time) any {
	c.refreshNodeCPUs(now)
	if c.ncpuErr != nil {
		return "unknown (" + c.ncpuErr.Error() + "): consumer figures are not capped at the interval on every CPU"
	}
	return c.ncpu
}

// cgroupListDue reports whether the cgroup directories are to be listed again.
func (c *cpuConsumers) cgroupListDue(now time.Time) bool {
	return now.Sub(c.cgroupListedAt) >= cgroupListInterval
}

// roll takes the rolling baselines: cgroups every call (once per window),
// processes every procBaselineInterval.
func (c *cpuConsumers) roll(now time.Time) {
	c.refreshNodeCPUs(now)
	if s, err := c.sampleCgroups(now, c.cgroupListDue(now)); err == nil {
		c.cgroups.push(s)
	}
	if c.procsVisible() != nil || now.Sub(c.lastProcAt) < procBaselineInterval {
		return
	}
	c.lastProcAt = now
	if s, err := c.sampleProcs(now); err == nil {
		c.procs.push(s)
	}
}

// report samples both sources once, now, and returns one set of stall-line
// attributes per span: the top consumers since the newest baseline that is at
// least that old. Two epochs starved in the same window (a handoff) each get
// the consumers over their own stall. A source that cannot be reported says so
// in its own field.
//
// The cgroups are those of the baseline, which is what makes a delta exact,
// except when the listing is due: a node starved in every window never rolls,
// so without a listing here the set would never be refreshed for as long as the
// incident lasts, and every pod created in it would stay inside its parent's
// entry. The listing is paid once per cgroupListInterval (not per line), inside
// this sample's one scan budget; a cgroup it finds is "first listed inside the
// interval" to cgroupDeltas, held to what its parent has left. If the listing
// fails, this one line has no cgroups and the next reads the known set again.
func (c *cpuConsumers) report(now time.Time, spans ...time.Duration) [][]any {
	c.refreshNodeCPUs(now)
	cg, cgErr := c.sampleCgroups(now, c.cgroupListDue(now))
	var ps procSample
	procErr := c.procsVisible()
	if procErr == nil {
		ps, procErr = c.sampleProcs(now)
	}

	out := make([][]any, 0, len(spans))
	for _, span := range spans {
		since := now.Add(-span)
		attrs := make([]any, 0, 10)
		attrs = appendTop(attrs, attrTopCgroups, attrTopCgroupsOverMs, attrTopCgroupsTruncated, c.cgroupTop(cg, cgErr, since))
		out = append(out, appendTop(attrs, attrTopProcs, attrTopProcsOverMs, attrTopProcsTruncated, c.procTop(ps, procErr, since)))
	}

	if cgErr == nil {
		c.cgroups.push(cg)
	}
	if procErr == nil {
		// This scan is the next window's baseline: a stall that lasts gets
		// exact per-window deltas from its second line on.
		c.procs.push(ps)
		c.lastProcAt = now
	}
	return out
}

func (c *cpuConsumers) cgroupTop(cur cgroupSample, err error, since time.Time) topList {
	if err != nil {
		return topList{err: err}
	}
	base, truncated, ok := c.cgroups.baseline(since)
	if !ok {
		return topList{err: errNoBaseline}
	}
	all, err := cgroupDeltas(base, cur, c.ncpu)
	if err != nil {
		return topList{err: err}
	}
	return topList{consumers: topConsumers(all, c.topN), over: cur.at.Sub(base.at), truncated: truncated}
}

func (c *cpuConsumers) procTop(cur procSample, err error, since time.Time) topList {
	if err != nil {
		return topList{err: err}
	}
	base, truncated, ok := c.procs.baseline(since)
	if !ok {
		return topList{err: errNoBaseline}
	}
	return topList{
		consumers: topConsumers(procDeltas(base, cur, c.ncpu), c.topN),
		over:      cur.at.Sub(base.at),
		truncated: truncated,
	}
}

func appendTop(attrs []any, key, overKey, truncatedKey string, t topList) []any {
	if t.err != nil {
		return append(attrs, key, "unavailable: "+t.err.Error())
	}
	attrs = append(attrs, key, formatConsumers(t.consumers), overKey, t.over.Milliseconds())
	if t.truncated {
		attrs = append(attrs, truncatedKey, true)
	}
	return attrs
}

// scanBudget is one scan's deadline.
type scanBudget struct {
	clock    func() time.Time
	deadline time.Time
}

func (c *cpuConsumers) newBudget() *scanBudget {
	return &scanBudget{clock: c.clock, deadline: c.clock().Add(consumerScanBudget)}
}

func (b *scanBudget) spent() bool {
	return b.clock().After(b.deadline)
}

// sampleCgroups reads cpu.stat of the root and of every known cgroup. With
// relist, or when none is known yet, it first walks the hierarchy from the root
// down to pod level to find them.
func (c *cpuConsumers) sampleCgroups(now time.Time, relist bool) (cgroupSample, error) {
	// The budget starts before the first file: a slow read of the root's
	// cpu.stat is charged to it like any other.
	budget := c.newBudget()
	root, err := c.cgroupUsage("/")
	if err != nil {
		// cgroup v1 has no cpu.stat at the top of /sys/fs/cgroup (it has one
		// directory per controller there), and neither has a node without
		// cgroupfs mounted.
		return cgroupSample{}, fmt.Errorf("%w (%w)", errNoCgroupV2, err)
	}
	if budget.spent() {
		return cgroupSample{}, errScanBudget
	}
	s := cgroupSample{at: now, usage: make(map[string]uint64, len(c.cgroupPaths)+1), unknown: map[string]struct{}{}}
	s.usage["/"] = root
	if c.cgroupPaths == nil {
		// Nothing to fall back on: list now, unless the last attempt failed
		// less than an interval ago.
		relist = c.cgroupListedAt.IsZero() || now.Sub(c.cgroupListedAt) >= cgroupListInterval
		if !relist {
			return cgroupSample{}, c.cgroupListErr
		}
	}
	read := c.readKnownCgroups
	if relist {
		read = c.listCgroups
	}
	if err := read(&s, budget); err != nil {
		return cgroupSample{}, err
	}
	if budget.spent() {
		return cgroupSample{}, errScanBudget
	}
	if len(s.usage) == 1 {
		// A container with its own cgroup namespace sees its own cgroup as
		// the root, with nothing below it: that is this container, not the
		// node.
		return cgroupSample{}, errOwnCgroup
	}
	return s, nil
}

// listCgroups walks the hierarchy into s and remembers the cgroups it found.
// When the walk fails, the set from the last listing that succeeded stays in
// use and this one sample is lost.
func (c *cpuConsumers) listCgroups(s *cgroupSample, budget *scanBudget) error {
	c.cgroupListedAt = s.at
	if err := c.walkCgroups(s, "/", 1, budget); err != nil {
		c.cgroupListErr = err
		return err
	}
	c.cgroupPaths = make([]string, 0, len(s.usage)+len(s.unknown))
	for p := range s.usage {
		if p != "/" {
			c.cgroupPaths = append(c.cgroupPaths, p)
		}
	}
	for p := range s.unknown {
		c.cgroupPaths = append(c.cgroupPaths, p) // there: read it again next sample
	}
	return nil
}

// cgroupGone reports whether a cpu.stat error means the cgroup was removed:
// the file is not there (ENOENT), or it was opened just before the cgroup went
// and the read finds no cgroup behind it (ENODEV). Any other error, and a file
// that does not parse, is a cgroup that still exists.
func cgroupGone(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENODEV)
}

// readKnownCgroups samples the cgroups of the last listing. One that has been
// removed since is forgotten; what it used stays in its parent's entry. One
// that is there and could not be read stays known and is marked unread in this
// sample, so the next one does not find it "new" with its lifetime's usage.
func (c *cpuConsumers) readKnownCgroups(s *cgroupSample, budget *scanBudget) error {
	known := c.cgroupPaths[:0]
	for _, p := range c.cgroupPaths {
		if budget.spent() {
			return errScanBudget
		}
		usage, err := c.cgroupUsage(p)
		if cgroupGone(err) {
			continue
		}
		known = append(known, p)
		if err != nil {
			s.unknown[p] = struct{}{}
			continue
		}
		s.usage[p] = usage
	}
	c.cgroupPaths = known
	return nil
}

func (c *cpuConsumers) cgroupUsage(rel string) (uint64, error) {
	b, err := c.fs.readFile(path.Join(c.cgroupRoot, rel, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	return parseCgroupUsage(b)
}

// cgroupChildren lists the cgroups directly below dir for a walk that has
// sampled s so far.
func (c *cpuConsumers) cgroupChildren(s *cgroupSample, dir string, budget *scanBudget) ([]string, error) {
	// Before every listing, not only before every file: the read of dir's own
	// cpu.stat may be what used the budget up, and a directory listing is the
	// dearer of the two calls to make on top of it.
	if budget.spent() {
		return nil, errScanBudget
	}
	names, more, err := c.fs.subdirs(path.Join(c.cgroupRoot, dir), cgroupMaxCount)
	if err != nil {
		// Removed since its parent listed it, or not ours to list: it stays
		// a single entry.
		return nil, nil
	}
	if more || len(s.usage)+len(s.unknown)+len(names) > cgroupMaxCount {
		return nil, fmt.Errorf("%w (more than %d cgroups)", errTooMany, cgroupMaxCount)
	}
	return names, nil
}

// walkCgroups samples the children of dir, at depth, and theirs.
func (c *cpuConsumers) walkCgroups(s *cgroupSample, dir string, depth int, budget *scanBudget) error {
	names, err := c.cgroupChildren(s, dir, budget)
	if err != nil {
		return err
	}
	for _, name := range names {
		if budget.spent() {
			return errScanBudget
		}
		child := path.Join(dir, name)
		usage, readErr := c.cgroupUsage(child)
		if cgroupGone(readErr) {
			// Removed mid-walk. What it used stays in its parent's entry.
			continue
		}
		if readErr != nil {
			// There, and unread: known from now on, nothing below it listed
			// until the next walk.
			s.unknown[child] = struct{}{}
			continue
		}
		s.usage[child] = usage
		if depth >= cgroupMaxDepth || isPodCgroup(name) {
			continue
		}
		if err := c.walkCgroups(s, child, depth+1, budget); err != nil {
			return err
		}
	}
	return nil
}

// procsVisible reports why processes cannot be sampled, or nil. PID 2 is
// kthreadd in the host's PID namespace and in no other, so that is the test
// for "this /proc lists the node's processes".
func (c *cpuConsumers) procsVisible() error {
	if c.procChecked {
		return c.procErr
	}
	c.procChecked = true
	b, err := c.fs.readFile(path.Join(c.procRoot, "2", "stat"))
	if err != nil {
		c.procErr = errPodPIDNS
		return c.procErr
	}
	if comm, _, _, parseErr := parseProcCPU(b); parseErr != nil || comm != "kthreadd" {
		c.procErr = errPodPIDNS
	}
	return c.procErr
}

// sampleProcs reads /proc/<pid>/stat for every process.
func (c *cpuConsumers) sampleProcs(now time.Time) (procSample, error) {
	budget := c.newBudget()
	names, more, err := c.fs.subdirs(c.procRoot, procMaxCount)
	if err != nil {
		return procSample{}, fmt.Errorf("listing /proc: %w", err)
	}
	if more {
		return procSample{}, fmt.Errorf("%w (more than %d entries in /proc)", errTooMany, procMaxCount)
	}
	s := procSample{at: now, procs: make(map[procKey]procUse, len(names)), unknown: map[int]struct{}{}}
	for _, name := range names {
		pid, convErr := strconv.Atoi(name)
		if convErr != nil {
			continue // /proc/sys, /proc/irq, …
		}
		if budget.spent() {
			return procSample{}, errScanBudget
		}
		b, readErr := c.fs.readFile(path.Join(c.procRoot, name, "stat"))
		if errors.Is(readErr, syscall.ENOENT) || errors.Is(readErr, syscall.ESRCH) {
			continue // exited since the listing
		}
		if readErr != nil {
			s.unknown[pid] = struct{}{} // there, and not ours to read
			continue
		}
		comm, usec, start, parseErr := parseProcCPU(b)
		if parseErr != nil {
			s.unknown[pid] = struct{}{}
			continue
		}
		s.procs[procKey{pid: pid, start: start}] = procUse{comm: comm, usec: usec}
	}
	if budget.spent() {
		return procSample{}, errScanBudget
	}
	return s, nil
}
