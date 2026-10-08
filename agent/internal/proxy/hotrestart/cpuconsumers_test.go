package hotrestart

import (
	"fmt"
	"math"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStatFS is an in-memory procfs + cgroupfs: files by absolute path, a
// directory being any prefix of one. errs makes a path fail the way the real
// filesystems do (a process gone mid-scan, an entry this user may not read).
type fakeStatFS struct {
	files map[string]string
	errs  map[string]error
	// reads and lists count the calls, for the cost assertions; procLists is
	// the listings of /proc itself (one per process scan).
	reads, lists, procLists int
}

func newFakeStatFS() *fakeStatFS {
	return &fakeStatFS{files: map[string]string{}, errs: map[string]error{}}
}

func (f *fakeStatFS) readFile(name string) ([]byte, error) {
	f.reads++
	if err := f.errs[name]; err != nil {
		return nil, err
	}
	content, ok := f.files[name]
	if !ok {
		return nil, syscall.ENOENT
	}
	return []byte(content), nil
}

func (f *fakeStatFS) subdirs(dir string, limit int) ([]string, bool, error) {
	f.lists++
	if dir == fakeProcRoot {
		f.procLists++
	}
	if err := f.errs[dir]; err != nil {
		return nil, false, err
	}
	seen := map[string]struct{}{}
	found := false
	for name := range f.files {
		rest, ok := strings.CutPrefix(name, strings.TrimSuffix(dir, "/")+"/")
		if !ok {
			continue
		}
		found = true
		if child, _, isDir := strings.Cut(rest, "/"); isDir {
			seen[child] = struct{}{}
		}
	}
	if !found {
		return nil, false, syscall.ENOENT
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) > limit {
		return names[:limit], true, nil
	}
	return names, false, nil
}

const (
	fakeProcRoot   = "/proc"
	fakeCgroupRoot = "/sys/fs/cgroup"
)

// cgroup sets one cgroup's cumulative CPU time ("/" is the root).
func (f *fakeStatFS) cgroup(rel string, usage time.Duration) {
	f.files[path.Join(fakeCgroupRoot, rel, "cpu.stat")] = fmt.Sprintf(
		"usage_usec %d\nuser_usec 1\nsystem_usec 1\nnr_periods 0\n", usage.Microseconds())
}

func (f *fakeStatFS) removeCgroup(rel string) {
	delete(f.files, path.Join(fakeCgroupRoot, rel, "cpu.stat"))
}

// process writes /proc/<pid>/stat as the kernel does: 52 fields, utime the
// 14th, stime the 15th, starttime the 22nd, in 10 ms ticks.
func (f *fakeStatFS) process(pid int, comm string, utime, stime time.Duration, start uint64) {
	fields := make([]string, 52)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[1], fields[2] = strconv.Itoa(pid), "("+comm+")", "S"
	fields[13] = strconv.FormatInt(utime.Milliseconds()/10, 10)
	fields[14] = strconv.FormatInt(stime.Milliseconds()/10, 10)
	fields[21] = strconv.FormatUint(start, 10)
	f.files[path.Join(fakeProcRoot, strconv.Itoa(pid), "stat")] = strings.Join(fields, " ") + "\n"
}

func (f *fakeStatFS) exit(pid int) {
	delete(f.files, path.Join(fakeProcRoot, strconv.Itoa(pid), "stat"))
}

// hostPIDNamespace makes /proc look like the node's: PID 2 is kthreadd.
func (f *fakeStatFS) hostPIDNamespace() {
	f.process(1, "init", 0, 0, 1)
	f.process(2, "kthreadd", 0, 0, 2)
}

// talosNode lays out the cgroups a Talos worker has, all at zero.
func (f *fakeStatFS) talosNode() {
	for _, rel := range []string{
		"/", "/init", "/system", "/system/apid", "/system/udevd",
		"/podruntime", "/podruntime/kubelet", "/podruntime/runtime",
		"/kubepods", "/kubepods/burstable", "/kubepods/besteffort",
		"/kubepods/burstable/" + podA, "/kubepods/burstable/" + podB,
		"/kubepods/" + podC,
	} {
		f.cgroup(rel, 0)
	}
}

const (
	podA = "pod0f3c1a2b-1111-4222-8333-444455556666"
	podB = "pod9d8c7b6a-1111-4222-8333-444455556666"
	podC = "podaaaabbbb-1111-4222-8333-444455556666" // guaranteed QoS: directly under /kubepods
)

func newTestConsumers(fs *fakeStatFS, topN int) *cpuConsumers {
	return newCPUConsumers(fs, fakeProcRoot, fakeCgroupRoot, topN)
}

// report1 is a report for one stall.
func report1(c *cpuConsumers, now time.Time, span time.Duration) []any {
	return c.report(now, span)[0]
}

func attrMap(t *testing.T, attrs []any) map[string]any {
	t.Helper()
	require.Zero(t, len(attrs)%2, "%v", attrs)
	m := make(map[string]any, len(attrs)/2)
	for i := 0; i < len(attrs); i += 2 {
		key, ok := attrs[i].(string)
		require.True(t, ok, "%v", attrs[i])
		m[key] = attrs[i+1]
	}
	return m
}

var consumersT0 = time.Unix(1791441776, 0) // 2026-10-08T06:42:56Z

// TestCgroupConsumersAreExclusiveOrderedAndBounded is the #1389 question as a
// test: the kubelet's cgroup used most of a second, a pod some, and the line
// has to say so — each cgroup counted once, largest first, at most N.
func TestCgroupConsumersAreExclusiveOrderedAndBounded(t *testing.T) {
	fs := newFakeStatFS()
	fs.talosNode()
	c := newTestConsumers(fs, 3)
	c.roll(consumersT0)

	// One second later. usage_usec is INCLUSIVE, so every ancestor carries its
	// descendants' time:
	//   kubelet 620 ms, runtime 90 ms       -> /podruntime 710 ms, nothing of its own
	//   pod A 300 ms, pod B 40 ms, and 25 ms in /kubepods/burstable outside any pod
	//   pod C (guaranteed) 10 ms            -> /kubepods 375 ms
	//   /system/apid 5 ms, /init 0
	//   the root: 1150 ms, of which 60 ms is in no child (kernel threads)
	fs.cgroup("/podruntime/kubelet", 620*time.Millisecond)
	fs.cgroup("/podruntime/runtime", 90*time.Millisecond)
	fs.cgroup("/podruntime", 710*time.Millisecond)
	fs.cgroup("/kubepods/burstable/"+podA, 300*time.Millisecond)
	fs.cgroup("/kubepods/burstable/"+podB, 40*time.Millisecond)
	fs.cgroup("/kubepods/burstable", 365*time.Millisecond)
	fs.cgroup("/kubepods/"+podC, 10*time.Millisecond)
	fs.cgroup("/kubepods", 375*time.Millisecond)
	fs.cgroup("/system/apid", 5*time.Millisecond)
	fs.cgroup("/system", 5*time.Millisecond)
	fs.cgroup("/", 1150*time.Millisecond)

	got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
	assert.Equal(t,
		"/podruntime/kubelet=620ms /kubepods/burstable/"+podA+"=300ms /podruntime/runtime=90ms",
		got[attrTopCgroups], "the three largest, largest first; /podruntime itself used nothing")
	assert.EqualValues(t, 1000, got[attrTopCgroupsOverMs])

	// With room for all of them, every millisecond of the root's 1150 is in
	// exactly one entry, and a cgroup that used nothing is not listed.
	all, err := cgroupDeltas(c.cgroups.samples[0], c.cgroups.samples[1])
	require.NoError(t, err)
	top := topConsumers(all, 100)
	var sum time.Duration
	names := make([]string, 0, len(top))
	for _, e := range top {
		sum += e.cpu
		names = append(names, e.name)
	}
	assert.Equal(t, 1150*time.Millisecond, sum, "%v", top)
	assert.Equal(t, []string{
		"/podruntime/kubelet", "/kubepods/burstable/" + podA, "/podruntime/runtime", "/",
		"/kubepods/burstable/" + podB, "/kubepods/burstable", "/kubepods/" + podC, "/system/apid",
	}, names)
}

// TestCgroupWalkStopsAtAPodAndAtTheDepthLimit: one entry per pod (never its
// containers), and nothing below the depth limit.
func TestCgroupWalkStopsAtAPodAndAtTheDepthLimit(t *testing.T) {
	fs := newFakeStatFS()
	fs.talosNode()
	fs.cgroup("/kubepods/burstable/"+podA+"/0123456789abcdef", time.Second)
	fs.cgroup("/kubepods/"+podC+"/fedcba9876543210", time.Second)
	fs.cgroup("/a/b/c/d/e", time.Second)
	for _, rel := range []string{"/a", "/a/b", "/a/b/c", "/a/b/c/d"} {
		fs.cgroup(rel, time.Second)
	}
	// The systemd driver's layout reaches a pod at depth 4.
	const systemdPod = "/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-burstable.slice/kubelet-kubepods-burstable-pod0f3c1a2b_1111.slice"
	for rel := systemdPod; rel != "/"; rel = path.Dir(rel) {
		fs.cgroup(rel, 0)
	}
	fs.cgroup(systemdPod+"/cri-containerd-0123.scope", 0)

	s, err := newTestConsumers(fs, 5).sampleCgroups(consumersT0, false)
	require.NoError(t, err)
	assert.Contains(t, s.usage, "/kubepods/burstable/"+podA)
	assert.Contains(t, s.usage, "/a/b/c/d")
	assert.Contains(t, s.usage, systemdPod)
	for p := range s.usage {
		assert.NotContains(t, p, "0123456789abcdef", "a container below a burstable pod")
		assert.NotContains(t, p, "fedcba9876543210", "a container below a guaranteed pod")
		assert.NotContains(t, p, "cri-containerd", "a container below a systemd pod slice")
		assert.NotEqual(t, "/a/b/c/d/e", p, "below the depth limit")
	}

	for name, want := range map[string]bool{
		podA: true, "kubelet-kubepods-burstable-pod0f3c1a2b_1111.slice": true,
		"podruntime": false, "kubepods": false, "kubelet-kubepods.slice": false, "pod": false, "apid": false,
	} {
		assert.Equal(t, want, isPodCgroup(name), name)
	}
}

// TestCgroupThatAppearsOrVanishesBetweenSamples: the cgroups are listed every
// cgroupListInterval, not every sample. A pod created since the last listing is
// counted in its parent's entry until the next one; a pod removed leaves what
// it used in its parent's entry too, because that is where the kernel keeps it.
func TestCgroupThatAppearsOrVanishesBetweenSamples(t *testing.T) {
	fs := newFakeStatFS()
	fs.talosNode()
	fs.cgroup("/kubepods/burstable/"+podB, 5*time.Second) // long-lived, about to be removed
	fs.cgroup("/kubepods/burstable", 5*time.Second)
	fs.cgroup("/kubepods", 5*time.Second)
	fs.cgroup("/", 5*time.Second)
	c := newTestConsumers(fs, 5)
	c.roll(consumersT0)
	listsPerWalk := fs.lists

	const podNew = "pod77777777-1111-4222-8333-444455556666"
	burstable := 5 * time.Second
	advance := func(podNewUsed, removedUsed time.Duration) {
		burstable += podNewUsed + removedUsed
		fs.cgroup("/kubepods/burstable", burstable)
		fs.cgroup("/kubepods", burstable)
		fs.cgroup("/", burstable)
	}
	fs.removeCgroup("/kubepods/burstable/" + podB) // used 70 ms more, then went
	fs.cgroup("/kubepods/burstable/"+podNew, 200*time.Millisecond)
	advance(200*time.Millisecond, 70*time.Millisecond)

	got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
	assert.Equal(t, "/kubepods/burstable=270ms", got[attrTopCgroups],
		"the removed pod's 70 ms and the not yet listed pod's 200 ms")
	assert.Equal(t, listsPerWalk, fs.lists, "a report never lists directories")
	assert.NotContains(t, c.cgroupPaths, "/kubepods/burstable/"+podB, "the removed pod is forgotten")

	// Quiet windows up to the next listing, which finds the new pod.
	for i := 2; i <= 10; i++ {
		c.roll(consumersT0.Add(time.Duration(i) * time.Second))
	}
	assert.Equal(t, 2*listsPerWalk, fs.lists, "one more listing in ten windows")
	assert.Contains(t, c.cgroupPaths, "/kubepods/burstable/"+podNew)

	fs.cgroup("/kubepods/burstable/"+podNew, 500*time.Millisecond)
	advance(300*time.Millisecond, 0)
	got = attrMap(t, report1(c, consumersT0.Add(11*time.Second), time.Second))
	assert.Equal(t, "/kubepods/burstable/"+podNew+"=300ms", got[attrTopCgroups], "its own entry from then on")
}

// TestFailedCgroupListingIsNotRetriedEveryWindow: a listing that cannot
// succeed (more cgroups than a sample may hold, or over its budget) is tried
// once per cgroupListInterval, not once per window, and a set that was listed
// successfully before stays in use.
func TestFailedCgroupListingIsNotRetriedEveryWindow(t *testing.T) {
	t.Run("never succeeded", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.cgroup("/", 0)
		fs.cgroup("/kubepods", 0)
		for i := range cgroupMaxCount {
			fs.cgroup("/kubepods/pod"+fmt.Sprintf("%08x", i), 0)
		}
		c := newTestConsumers(fs, 5)
		c.roll(consumersT0)
		lists := fs.lists
		require.Positive(t, lists)
		for i := 1; i < 10; i++ {
			c.roll(consumersT0.Add(time.Duration(i) * time.Second))
		}
		got := attrMap(t, report1(c, consumersT0.Add(9500*time.Millisecond), time.Second))
		assert.Equal(t, "unavailable: too many entries to sample (more than 1024 cgroups)", got[attrTopCgroups],
			"the field still says why")
		assert.Equal(t, lists, fs.lists, "no second listing inside the interval, from a roll or a report")

		c.roll(consumersT0.Add(10 * time.Second))
		assert.Equal(t, 2*lists, fs.lists, "one more attempt when the interval is up")
	})
	t.Run("succeeded before", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.talosNode()
		c := newTestConsumers(fs, 5)
		c.roll(consumersT0)
		known := len(c.cgroupPaths)
		require.Positive(t, known)

		// The node grows past the cap: the next listing fails.
		for i := range cgroupMaxCount {
			fs.cgroup("/kubepods/besteffort/pod"+fmt.Sprintf("%08x", i), 0)
		}
		for i := 1; i <= 10; i++ {
			c.roll(consumersT0.Add(time.Duration(i) * time.Second))
		}
		lists := fs.lists
		assert.Len(t, c.cgroupPaths, known, "the last good set is kept")

		fs.cgroup("/init", 300*time.Millisecond)
		fs.cgroup("/", 300*time.Millisecond)
		got := attrMap(t, report1(c, consumersT0.Add(12*time.Second), time.Second))
		assert.Equal(t, "/init=300ms", got[attrTopCgroups], "and still reported from")
		c.roll(consumersT0.Add(13 * time.Second))
		assert.Equal(t, lists, fs.lists, "no listing between attempts")
	})
}

// TestCgroupFirstListedInsideTheInterval: when the baseline is from before the
// listing that found a cgroup, the cgroup's usage is all that is known about
// it, and it may be older than the interval. Its entry is bounded by what its
// parent used in the interval.
func TestCgroupFirstListedInsideTheInterval(t *testing.T) {
	base := cgroupSample{usage: map[string]uint64{"/": 100_000_000, "/system": 60_000_000}}
	cur := cgroupSample{usage: map[string]uint64{
		"/": 100_500_000, "/system": 60_030_000,
		"/system/apid":      7_200_000_000, // an hour of CPU, 30 ms of it in this interval
		"/kubepods":         470_000,       // first listed, as is its child
		"/kubepods/" + podC: 120_000,
	}}
	all, err := cgroupDeltas(base, cur)
	require.NoError(t, err)
	assert.Equal(t, "/kubepods=350ms /kubepods/"+podC+"=120ms /system/apid=30ms", formatConsumers(topConsumers(all, 10)))

	// Two newly listed siblings that each have more lifetime usage than their
	// parent used in the interval cannot both be given all of it, and the one
	// sibling whose delta is exact keeps it: the entries never add up to more
	// than the root's delta.
	base = cgroupSample{usage: map[string]uint64{"/": 0, "/kubepods": 0, "/kubepods/podffffffff-known": 1_000_000}}
	cur = cgroupSample{usage: map[string]uint64{
		"/": 100_000, "/kubepods": 100_000,
		"/kubepods/podffffffff-known": 1_040_000, // exactly 40 ms, and it sorts last
		"/kubepods/pod11111111-new":   5_000_000,
		"/kubepods/pod22222222-new":   5_000_000,
	}}
	all, err = cgroupDeltas(base, cur)
	require.NoError(t, err)
	top := topConsumers(all, 10)
	assert.Equal(t, "/kubepods/pod11111111-new=60ms /kubepods/podffffffff-known=40ms", formatConsumers(top))
	var sum time.Duration
	for _, e := range top {
		sum += e.cpu
	}
	assert.Equal(t, 100*time.Millisecond, sum, "the root's delta, no more")

	_, err = cgroupDeltas(cgroupSample{usage: map[string]uint64{}}, cur)
	assert.ErrorIs(t, err, errRootNotInOld)
	_, err = cgroupDeltas(base, cgroupSample{usage: map[string]uint64{}})
	assert.ErrorIs(t, err, errNoCgroupV2)
}

// TestCgroupV2Absent: a cgroup v1 node, a node with no cgroupfs, and a
// container that sees only its own cgroup each say so in the field; nothing
// fails.
func TestCgroupV2Absent(t *testing.T) {
	t.Run("no cpu.stat at the root (cgroup v1)", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.files[path.Join(fakeCgroupRoot, "cpu,cpuacct/cpuacct.usage")] = "1\n"
		c := newTestConsumers(fs, 5)
		c.roll(consumersT0)
		got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
		assert.Equal(t, "unavailable: no cgroup v2 hierarchy (no such file or directory)", got[attrTopCgroups])
		assert.NotContains(t, got, attrTopCgroupsOverMs)
	})
	t.Run("own cgroup namespace", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.cgroup("/", time.Second)
		c := newTestConsumers(fs, 5)
		c.roll(consumersT0)
		got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
		assert.Equal(t, "unavailable: only this container's cgroup is visible", got[attrTopCgroups])
	})
	t.Run("malformed cpu.stat", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.files[path.Join(fakeCgroupRoot, "cpu.stat")] = "nr_periods 0\n"
		got := attrMap(t, report1(newTestConsumers(fs, 5), consumersT0, time.Second))
		assert.Contains(t, got[attrTopCgroups], "unavailable: no cgroup v2 hierarchy")
	})
}

// TestProcessConsumers covers the delta arithmetic and the three ways a PID
// changes under the sampler: a process that exits, a PID that is reused, and a
// process that starts inside the interval.
func TestProcessConsumers(t *testing.T) {
	fs := newFakeStatFS()
	fs.hostPIDNamespace()
	fs.process(700, "kubelet", 50*time.Second, 20*time.Second, 1000)
	fs.process(800, "containerd", 30*time.Second, 5*time.Second, 1100)
	fs.process(900, "envoy", 9*time.Second, time.Second, 1200)
	fs.process(950, "short-lived", time.Second, 0, 1300)
	fs.process(960, "old-owner", 40*time.Second, 0, 1400)
	c := newTestConsumers(fs, 10)
	c.roll(consumersT0)

	// One second later:
	fs.process(700, "kubelet", 50*time.Second+400*time.Millisecond, 20*time.Second+210*time.Millisecond, 1000) // +610 ms
	fs.process(800, "containerd", 30*time.Second+50*time.Millisecond, 5*time.Second, 1100)                     // +50 ms
	fs.process(900, "envoy", 9*time.Second, time.Second, 1200)                                                 // +0: not listed
	fs.exit(950)                                                                                               // exited: not listed
	// PID 960 was reused: another start time, so its 30 ms is ALL its own —
	// against the previous owner's 40 s it would have gone "backwards" to zero.
	fs.process(960, "new-owner", 30*time.Millisecond, 0, 9000)
	fs.process(970, "runc", 20*time.Millisecond, 60*time.Millisecond, 9001) // started in the interval: +80 ms

	got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
	assert.Equal(t, "kubelet(700)=610ms runc(970)=80ms containerd(800)=50ms new-owner(960)=30ms", got[attrTopProcs])
	assert.EqualValues(t, 1000, got[attrTopProcsOverMs])
}

// TestProcessConsumersSkipUnreadableEntries: an entry that cannot be read or
// parsed is left out; the rest of the scan is reported.
func TestProcessConsumersSkipUnreadableEntries(t *testing.T) {
	fs := newFakeStatFS()
	fs.hostPIDNamespace()
	fs.process(700, "kubelet", 0, 0, 1000)
	fs.process(701, "denied", 0, 0, 1001)
	fs.process(702, "gone-mid-scan", 0, 0, 1002)
	fs.process(703, "garbled", 0, 0, 1003)
	fs.files["/proc/sys/kernel/x"] = "not a process\n" // a non-numeric directory in /proc
	c := newTestConsumers(fs, 10)
	c.roll(consumersT0)

	fs.process(700, "kubelet", 100*time.Millisecond, 0, 1000)
	fs.process(701, "denied", time.Second, 0, 1001)
	fs.errs["/proc/701/stat"] = syscall.EACCES
	fs.process(702, "gone-mid-scan", time.Second, 0, 1002)
	fs.errs["/proc/702/stat"] = syscall.ESRCH
	fs.files["/proc/703/stat"] = "703 (garbled) S 1 2\n"

	got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
	assert.Equal(t, "kubelet(700)=100ms", got[attrTopProcs])
}

// TestProcessConsumersNeedTheHostPIDNamespace: in the pod's own PID namespace
// /proc lists this container, which the stall line already describes. The scan
// is not made at all, and the field says why.
func TestProcessConsumersNeedTheHostPIDNamespace(t *testing.T) {
	for name, setup := range map[string]func(*fakeStatFS){
		"pid 2 is not kthreadd": func(fs *fakeStatFS) { fs.process(2, "envoy", time.Second, 0, 5) },
		"no pid 2":              func(*fakeStatFS) {},
	} {
		t.Run(name, func(t *testing.T) {
			fs := newFakeStatFS()
			fs.process(1, "supervisor", time.Second, 0, 1)
			fs.process(15, "envoy", time.Second, 0, 5)
			setup(fs)
			c := newTestConsumers(fs, 5)
			c.roll(consumersT0)
			c.roll(consumersT0.Add(time.Minute))
			got := attrMap(t, report1(c, consumersT0.Add(2*time.Minute), time.Second))
			assert.Equal(t, "unavailable: /proc shows only this pod's PID namespace (no hostPID)", got[attrTopProcs])
			assert.NotContains(t, got, attrTopProcsOverMs)
			assert.Zero(t, fs.lists, "/proc is never listed")
			assert.LessOrEqual(t, fs.reads, 4, "one probe of /proc/2/stat, then only the cgroup root")
		})
	}
}

// TestProcessBaselineIsRolledSlowly: with nothing starved, /proc is scanned
// once per procBaselineInterval, not once per window; cgroups every window.
func TestProcessBaselineIsRolledSlowly(t *testing.T) {
	fs := newFakeStatFS()
	fs.hostPIDNamespace()
	fs.talosNode()
	c := newTestConsumers(fs, 5)
	for i := range 25 {
		c.roll(consumersT0.Add(time.Duration(i) * time.Second))
	}
	assert.Len(t, c.procs.samples, 3, "t=0, 10 and 20 s")
	assert.Len(t, c.cgroups.samples, 25)

	// A stall 4 s after the last process baseline: the delta is taken over
	// those 4 s and says so; the cgroup delta is exact.
	fs.process(700, "kubelet", 900*time.Millisecond, 0, 9000)
	got := attrMap(t, report1(c, consumersT0.Add(25*time.Second), time.Second))
	assert.Equal(t, "kubelet(700)=900ms", got[attrTopProcs])
	assert.EqualValues(t, 5000, got[attrTopProcsOverMs])
	assert.EqualValues(t, 1000, got[attrTopCgroupsOverMs])

	// The stall's own scan is the next window's baseline: exact from then on.
	fs.process(700, "kubelet", 1500*time.Millisecond, 0, 9000)
	got = attrMap(t, report1(c, consumersT0.Add(26*time.Second), time.Second))
	assert.Equal(t, "kubelet(700)=600ms", got[attrTopProcs])
	assert.EqualValues(t, 1000, got[attrTopProcsOverMs])
}

// TestConsumersCoverTheWholeStall: a 5 s starvation is reported in the window
// it ended in, so the baseline is the sample from before it began, not the
// previous window's.
func TestConsumersCoverTheWholeStall(t *testing.T) {
	fs := newFakeStatFS()
	fs.talosNode()
	c := newTestConsumers(fs, 1)
	for i := range 10 {
		// The kubelet burns 800 ms in each of the seconds 5..9.
		if i > 4 {
			used := time.Duration(i-4) * 800 * time.Millisecond
			fs.cgroup("/podruntime/kubelet", used)
			fs.cgroup("/podruntime", used)
			fs.cgroup("/", used)
		}
		c.roll(consumersT0.Add(time.Duration(i) * time.Second))
	}
	fs.cgroup("/podruntime/kubelet", 4800*time.Millisecond)
	fs.cgroup("/podruntime", 4800*time.Millisecond)
	fs.cgroup("/", 4800*time.Millisecond)

	got := attrMap(t, report1(c, consumersT0.Add(10*time.Second), 5500*time.Millisecond))
	assert.Equal(t, "/podruntime/kubelet=4800ms", got[attrTopCgroups])
	assert.EqualValues(t, 6000, got[attrTopCgroupsOverMs], "the newest sample at least 5.5 s old")

	// Longer than everything kept: the oldest sample, and the field says how
	// far back that is.
	got = attrMap(t, report1(c, consumersT0.Add(11*time.Second), time.Hour))
	assert.EqualValues(t, 11000, got[attrTopCgroupsOverMs])
}

func TestSampleHistory(t *testing.T) {
	h := sampleHistory[cgroupSample]{limit: 3}
	_, ok := h.baseline(consumersT0)
	assert.False(t, ok, "no sample yet")
	for i := range 5 {
		h.push(cgroupSample{at: consumersT0.Add(time.Duration(i) * time.Second)})
	}
	require.Len(t, h.samples, 3, "bounded")
	assert.Equal(t, consumersT0.Add(2*time.Second), h.samples[0].at, "the oldest are dropped")
	for since, want := range map[time.Duration]time.Duration{
		4 * time.Second:         4 * time.Second, // exactly at a sample
		3500 * time.Millisecond: 3 * time.Second, // the newest one before it
		time.Second:             2 * time.Second, // before all of them: the oldest
	} {
		got, ok := h.baseline(consumersT0.Add(since))
		require.True(t, ok)
		assert.Equal(t, consumersT0.Add(want), got.at, "since=%v", since)
	}
}

// TestConsumerNamesAreBoundedAndOneToken: a name cannot break the line's
// "name=<n>ms name=<n>ms" grammar or its length.
func TestConsumerNamesAreBoundedAndOneToken(t *testing.T) {
	fs := newFakeStatFS()
	fs.hostPIDNamespace()
	c := newTestConsumers(fs, 5)
	c.roll(consumersT0)
	fs.process(10, "tmux: server", 30*time.Millisecond, 0, 50)
	fs.process(11, `a) (b="c"`, 20*time.Millisecond, 0, 51)
	fs.process(12, "kworker/u16:3-events_unbound_and_more", 10*time.Millisecond, 0, 52)
	got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
	assert.Equal(t, `tmux:_server(10)=30ms a)_(b__c_(11)=20ms kworker/u16:3-events_...(12)=10ms`, got[attrTopProcs])

	long := "/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-burstable.slice/kubelet-kubepods-burstable-" + podA + ".slice"
	name := tailName(cleanName(long), cgroupNameMax)
	assert.Len(t, name, cgroupNameMax)
	assert.True(t, strings.HasPrefix(name, "..."), name)
	assert.True(t, strings.HasSuffix(name, podA+".slice"), "the pod's UID survives: %s", name)
	assert.Equal(t, "/kubepods/burstable/"+podA, tailName("/kubepods/burstable/"+podA, cgroupNameMax),
		"the Talos pod path fits whole")

	assert.Equal(t, "none", formatConsumers(nil))
	assert.Equal(t, maxStallTopConsumers, newTestConsumers(fs, 1000).topN, "the flag is bounded")
	assert.Equal(t, maxStallTopConsumers, nodeCPUConsumers(1000).topN)
	assert.Equal(t, DefaultStallTopConsumers, nodeCPUConsumers(DefaultStallTopConsumers).topN)
	assert.Nil(t, nodeCPUConsumers(0), "0 turns the sampling off")
	assert.Nil(t, nodeCPUConsumers(-1))
}

// TestConsumerScanIsBounded: a scan that runs past its time budget, or finds
// more entries than one sample may hold, is abandoned and reported as
// unavailable — never a partial sample, whose missing entries would look new
// (with their lifetime's CPU) next time.
func TestConsumerScanIsBounded(t *testing.T) {
	t.Run("time budget", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.hostPIDNamespace()
		fs.talosNode()
		for pid := 100; pid < 1100; pid++ {
			fs.process(pid, "p", 0, 0, uint64(pid))
		}
		c := newTestConsumers(fs, 5)
		c.roll(consumersT0)
		// Every look at the clock is 1 ms later, and the clock is looked at
		// before every file: the 250 ms are gone after about 250 processes.
		tick := consumersT0
		c.clock = func() time.Time { tick = tick.Add(time.Millisecond); return tick }
		readsBefore := fs.reads
		got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
		assert.Equal(t, "unavailable: scan exceeded its time budget", got[attrTopProcs])
		assert.Less(t, fs.reads-readsBefore, 300, "the scan stopped at the deadline, not 1000 files later")
		assert.NotContains(t, got[attrTopCgroups], "unavailable", "the small cgroup walk finished inside its own budget")
	})
	t.Run("time budget of a short scan", func(t *testing.T) {
		// Fewer files than any batch: the deadline still holds, and a scan
		// whose last read ran past it is not reported.
		fs := newFakeStatFS()
		fs.talosNode()
		c := newTestConsumers(fs, 5)
		c.roll(consumersT0)
		tick := consumersT0
		c.clock = func() time.Time { tick = tick.Add(100 * time.Millisecond); return tick }
		got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
		assert.Equal(t, "unavailable: scan exceeded its time budget", got[attrTopCgroups])

		// The last look at the clock is the one past the deadline.
		looks := 0
		c.clock = func() time.Time {
			looks++
			if looks == len(c.cgroupPaths)+2 { // the deadline, one per cgroup, then the final check
				return consumersT0.Add(time.Hour)
			}
			return consumersT0
		}
		got = attrMap(t, report1(c, consumersT0.Add(2*time.Second), time.Second))
		assert.Equal(t, "unavailable: scan exceeded its time budget", got[attrTopCgroups])
	})
	t.Run("too many processes", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.hostPIDNamespace()
		for pid := 100; pid < 100+procMaxCount; pid++ {
			fs.files["/proc/"+strconv.Itoa(pid)+"/stat"] = ""
		}
		got := attrMap(t, report1(newTestConsumers(fs, 5), consumersT0, time.Second))
		assert.Equal(t, "unavailable: too many entries to sample (more than 8192 entries in /proc)", got[attrTopProcs])
	})
	t.Run("too many cgroups", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.cgroup("/", 0)
		for i := range cgroupMaxCount {
			fs.cgroup("/kubepods/pod"+fmt.Sprintf("%08x", i), 0)
		}
		fs.cgroup("/kubepods", 0)
		got := attrMap(t, report1(newTestConsumers(fs, 5), consumersT0, time.Second))
		assert.Equal(t, "unavailable: too many entries to sample (more than 1024 cgroups)", got[attrTopCgroups])
	})
	t.Run("no baseline yet", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.hostPIDNamespace()
		fs.talosNode()
		got := attrMap(t, report1(newTestConsumers(fs, 5), consumersT0, time.Second))
		assert.Equal(t, "unavailable: no earlier sample yet", got[attrTopCgroups])
		assert.Equal(t, "unavailable: no earlier sample yet", got[attrTopProcs])
	})
}

func TestParseProcCPU(t *testing.T) {
	// Verbatim shape of a kernel line, with the awkward comm the kernel allows.
	line := "4001 (a) b (c)) S 1 4001 4001 0 -1 4194560 100 0 0 0 123 45 0 0 20 0 5 0 987654 1000000 200 18446744073709551615 0 0\n"
	comm, usec, start, err := parseProcCPU([]byte(line))
	require.NoError(t, err)
	assert.Equal(t, "a) b (c)", comm)
	assert.EqualValues(t, (123+45)*10_000, usec, "utime+stime, 10 ms ticks")
	assert.EqualValues(t, 987654, start)

	for _, bad := range []string{
		"", "4001 envoy S 1", "4001 (envoy", "4001 (envoy) S 1 2 3",
		"4001 (e) S 1 1 1 0 -1 0 0 0 0 0 x 45 0 0 20 0 5 0 9 0",
		"4001 (e) S 1 1 1 0 -1 0 0 0 0 0 1 y 0 0 20 0 5 0 9 0",
		"4001 (e) S 1 1 1 0 -1 0 0 0 0 0 1 2 0 0 20 0 5 0 z 0",
	} {
		_, _, _, err := parseProcCPU([]byte(bad))
		assert.ErrorIs(t, err, errMalformed, "%q", bad)
	}
}

// TestKernelCountersSaturate: an absurd counter becomes the largest figure, not
// a negative or a small wrapped one.
func TestKernelCountersSaturate(t *testing.T) {
	assert.Equal(t, 1500*time.Millisecond, usecDuration(1_500_000))
	assert.Equal(t, time.Duration(math.MaxInt64), usecDuration(math.MaxUint64))
	assert.Equal(t, time.Duration(math.MaxInt64), usecDuration(math.MaxInt64/1000+1))
	assert.Positive(t, usecDuration(math.MaxInt64/1000))

	assert.EqualValues(t, 30_000, ticksToUsec(1, 2))
	assert.EqualValues(t, uint64(math.MaxUint64), ticksToUsec(math.MaxUint64, 0))
	assert.EqualValues(t, uint64(math.MaxUint64), ticksToUsec(math.MaxUint64/usecPerTick, 1))
	assert.EqualValues(t, uint64(math.MaxUint64), ticksToUsec(math.MaxUint64-1, 5), "the sum itself wraps")

	all, err := cgroupDeltas(
		cgroupSample{usage: map[string]uint64{"/": 0}},
		cgroupSample{usage: map[string]uint64{"/": math.MaxUint64}})
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Positive(t, all[0].cpu)
}

func TestParseCgroupUsage(t *testing.T) {
	// Verbatim cpu.stat of a cgroup with the cpu controller on.
	got, err := parseCgroupUsage([]byte("usage_usec 388314838000\nuser_usec 312168799000\nsystem_usec 76146039000\n" +
		"core_sched.force_idle_usec 0\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n"))
	require.NoError(t, err)
	assert.EqualValues(t, 388314838000, got)
	for _, bad := range []string{"", "user_usec 1\n", "usage_usec x\n"} {
		_, err := parseCgroupUsage([]byte(bad))
		assert.Error(t, err, "%q", bad)
	}
}

// TestStallLineNamesTheConsumersOnlyWhenStarved drives the stall sampler end to
// end: a [starved] line carries the consumers over the stall, a [blocked] or
// [busy] one does not, and a sampler that cannot read anything still logs the
// stall.
func TestStallLineNamesTheConsumersOnlyWhenStarved(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID, testEpoch + 1: testPID + 100})
	fs := newFakeStatFS()
	fs.talosNode()
	s.consumers = newTestConsumers(fs, 2)

	// Epoch 119's worker will be starved, epoch 120's blocked.
	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "do_epoll_wait")
	proc.thread(testPID, 4001, "wrk:worker_0", 'S', 0, 0, "do_epoll_wait")
	proc.thread(testPID+100, testPID+100, "envoy", 'S', 0, 0, "do_epoll_wait")
	proc.thread(testPID+100, 4101, "wrk:worker_0", 'S', 0, 0, "do_epoll_wait")
	t0 := consumersT0
	s.tick(t0)

	// A quiet second: baselines only.
	s.tick(t0.Add(time.Second))
	require.Empty(t, logs.records(t, "envoy thread stall"))

	// The next two seconds the kubelet holds the CPUs; the worker's 1.7 s wait
	// is charged when it ends, in the second of those windows.
	fs.cgroup("/podruntime/kubelet", 900*time.Millisecond)
	fs.cgroup("/podruntime", 900*time.Millisecond)
	fs.cgroup("/", 900*time.Millisecond)
	proc.thread(testPID+100, 4101, "wrk:worker_0", 'S', 0, 0, "__futex_wait")
	s.tick(t0.Add(2 * time.Second))

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 1)
	assert.EqualValues(t, testEpoch+1, recs[0]["epoch"])
	assert.Contains(t, toStrings(t, recs[0]["threads"])[0], "[blocked]")
	assert.NotContains(t, recs[0], attrTopCgroups, "a blocked thread is not waiting for a CPU")
	assert.NotContains(t, recs[0], attrTopProcs)

	fs.cgroup("/podruntime/kubelet", 1800*time.Millisecond)
	fs.cgroup("/kubepods/burstable/"+podA, 150*time.Millisecond)
	fs.cgroup("/kubepods/burstable", 150*time.Millisecond)
	fs.cgroup("/kubepods", 150*time.Millisecond)
	fs.cgroup("/podruntime", 1800*time.Millisecond)
	fs.cgroup("/", 1950*time.Millisecond)
	proc.thread(testPID, 4001, "wrk:worker_0", 'R', 50*time.Millisecond, 1700*time.Millisecond, "")
	s.tick(t0.Add(3 * time.Second))

	// Both epochs are flagged in this window; only the starved one's line
	// carries the consumers.
	recs = logs.records(t, "envoy thread stall")
	require.Len(t, recs, 3)
	starved, blocked := recs[1], recs[2]
	assert.EqualValues(t, testEpoch+1, blocked["epoch"])
	assert.NotContains(t, blocked, attrTopCgroups, "the blocked epoch's line in a window another epoch starved in")
	assert.NotContains(t, blocked, attrTopProcs)
	assert.EqualValues(t, testEpoch, starved["epoch"])
	assert.Contains(t, toStrings(t, starved["threads"])[0], "wrk:worker_0[starved] cpu=50ms runq=1700ms")
	assert.Equal(t, "/podruntime/kubelet=1800ms /kubepods/burstable/"+podA+"=150ms", starved[attrTopCgroups],
		"over the 1.7 s the thread waited, not only the last second")
	assert.EqualValues(t, 2000, starved[attrTopCgroupsOverMs])
	assert.Equal(t, "unavailable: /proc shows only this pod's PID namespace (no hostPID)", starved[attrTopProcs])
	assert.EqualValues(t, 118, starved["handoffPeer"], "the rest of the line is unchanged")

	// Everything unreadable: the stall line is still there, the fields say why.
	fs.errs[path.Join(fakeCgroupRoot, "cpu.stat")] = syscall.EIO
	proc.thread(testPID, 4001, "wrk:worker_0", 'R', 60*time.Millisecond, 2500*time.Millisecond, "")
	proc.thread(testPID+100, 4101, "wrk:worker_0", 'S', 0, 0, "do_epoll_wait")
	s.tick(t0.Add(4 * time.Second))
	recs = logs.records(t, "envoy thread stall")
	require.Len(t, recs, 4)
	assert.Contains(t, toStrings(t, recs[3]["threads"])[0], "[starved]")
	assert.Equal(t, "unavailable: no cgroup v2 hierarchy (input/output error)", recs[3][attrTopCgroups])

	// Turned off: no fields at all.
	s.consumers = nil
	proc.thread(testPID, 4001, "wrk:worker_0", 'R', 70*time.Millisecond, 3300*time.Millisecond, "")
	s.tick(t0.Add(5 * time.Second))
	recs = logs.records(t, "envoy thread stall")
	require.Len(t, recs, 5)
	assert.NotContains(t, recs[4], attrTopCgroups)
	assert.NotContains(t, recs[4], attrTopProcs)
}

// TestEachStarvedLineCoversItsOwnStall: in a handoff two epochs can be starved
// in the same window for very different lengths. The sources are sampled once,
// and each line's consumers are taken over its own stall.
func TestEachStarvedLineCoversItsOwnStall(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID, testEpoch + 1: testPID + 100})
	fs := newFakeStatFS()
	fs.talosNode()
	s.consumers = newTestConsumers(fs, 1)

	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "do_epoll_wait")
	proc.thread(testPID+100, testPID+100, "envoy", 'S', 0, 0, "do_epoll_wait")
	s.tick(consumersT0)
	for i := 1; i <= 5; i++ {
		// The kubelet burns 700 ms in every second.
		used := time.Duration(i) * 700 * time.Millisecond
		fs.cgroup("/podruntime/kubelet", used)
		fs.cgroup("/podruntime", used)
		fs.cgroup("/", used)
		if i == 5 {
			// The predecessor's wait of 4.2 s and the successor's of 300 ms
			// both end in the fifth second.
			proc.thread(testPID, testPID, "envoy", 'R', 0, 4200*time.Millisecond, "")
			proc.thread(testPID+100, testPID+100, "envoy", 'R', 0, 300*time.Millisecond, "")
		}
		s.tick(consumersT0.Add(time.Duration(i) * time.Second))
	}

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 2)
	assert.EqualValues(t, testEpoch, recs[0]["epoch"])
	assert.Equal(t, "/podruntime/kubelet=3500ms", recs[0][attrTopCgroups])
	assert.EqualValues(t, 5000, recs[0][attrTopCgroupsOverMs], "the newest sample at least 4.2 s old")
	assert.EqualValues(t, testEpoch+1, recs[1]["epoch"])
	assert.Equal(t, "/podruntime/kubelet=700ms", recs[1][attrTopCgroups])
	assert.EqualValues(t, 1000, recs[1][attrTopCgroupsOverMs], "its own stall fits in the window")
	assert.Len(t, s.consumers.cgroups.samples, 6, "one sample for both lines")
}

// TestBlockedLineBeforeAStarvedOne: the consumers go to the starved epoch's
// line wherever it comes in the window's lines, never to a blocked epoch's line
// that is logged before it.
func TestBlockedLineBeforeAStarvedOne(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID, testEpoch + 1: testPID + 100})
	fs := newFakeStatFS()
	fs.talosNode()
	s.consumers = newTestConsumers(fs, 1)

	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "do_epoll_wait")
	proc.thread(testPID+100, testPID+100, "envoy", 'S', 0, 0, "do_epoll_wait")
	s.tick(consumersT0)
	fs.cgroup("/init", 400*time.Millisecond)
	fs.cgroup("/", 400*time.Millisecond)
	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "__futex_wait")
	proc.thread(testPID+100, testPID+100, "envoy", 'R', 0, 600*time.Millisecond, "")
	s.tick(consumersT0.Add(time.Second))

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 2)
	assert.EqualValues(t, testEpoch, recs[0]["epoch"])
	assert.Contains(t, toStrings(t, recs[0]["threads"])[0], "[blocked]")
	assert.NotContains(t, recs[0], attrTopCgroups)
	assert.EqualValues(t, testEpoch+1, recs[1]["epoch"])
	assert.Equal(t, "/init=400ms", recs[1][attrTopCgroups])
}

// TestBlockedOrBusyWindowDoesNotScanProcesses: the /proc scan is paid only for
// a starved window. A window that flags a thread as blocked or busy rolls the
// cheap baseline like a quiet one.
func TestBlockedOrBusyWindowDoesNotScanProcesses(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID})
	fs := newFakeStatFS()
	fs.hostPIDNamespace()
	fs.talosNode()
	s.consumers = newTestConsumers(fs, 2)

	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "do_epoll_wait")
	proc.thread(testPID, 4001, "wrk:worker_0", 'S', 0, 0, "do_epoll_wait")
	s.tick(consumersT0)
	require.Equal(t, 1, fs.procLists, "one /proc scan: the baseline")

	proc.thread(testPID, testPID, "envoy", 'R', 990*time.Millisecond, 0, "")
	proc.thread(testPID, 4001, "wrk:worker_0", 'S', 0, 0, "__futex_wait")
	s.tick(consumersT0.Add(time.Second))

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 1)
	threads := toStrings(t, recs[0]["threads"])
	require.Len(t, threads, 2)
	assert.Contains(t, threads[0], "[busy]")
	assert.Contains(t, threads[1], "[blocked]")
	assert.Equal(t, 1, fs.procLists, "no second /proc scan")
	assert.Len(t, s.consumers.cgroups.samples, 2, "the cgroup baseline still rolls")
}

// TestOSStatFSReadsTheLiveFilesystems checks the real reader against this
// test process's own /proc entry (and the cgroup hierarchy, where the sandbox
// shows one): the files the supervisor reads in production.
func TestOSStatFSReadsTheLiveFilesystems(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	fs := &osStatFS{}
	b, err := fs.readFile("/proc/self/stat")
	require.NoError(t, err)
	comm, _, start, err := parseProcCPU(b)
	require.NoError(t, err)
	assert.NotEmpty(t, comm)
	assert.NotZero(t, start)

	names, more, err := fs.subdirs("/proc", procMaxCount)
	require.NoError(t, err)
	assert.False(t, more)
	assert.Contains(t, names, strconv.Itoa(os.Getpid()))
	assert.NotContains(t, names, "stat", "files are not listed")

	one, more, err := fs.subdirs("/proc", 1)
	require.NoError(t, err)
	assert.Len(t, one, 1)
	assert.True(t, more, "cut at the limit")

	_, err = fs.readFile("/proc/self/no-such-file")
	assert.ErrorIs(t, err, syscall.ENOENT)
	_, _, err = fs.subdirs("/proc/self/no-such-dir", 1)
	assert.ErrorIs(t, err, syscall.ENOENT)

	c := newCPUConsumers(fs, "/proc", "/sys/fs/cgroup", DefaultStallTopConsumers)
	ps, err := c.sampleProcs(time.Now())
	require.NoError(t, err)
	found := false
	for k := range ps.procs {
		found = found || k.pid == os.Getpid()
	}
	assert.True(t, found, "this process is in its own /proc")

	// Whatever this host's cgroup setup is, a report is two fields that parse.
	c.roll(time.Now().Add(-time.Second))
	got := attrMap(t, report1(c, time.Now(), time.Second))
	assert.Contains(t, got, attrTopCgroups)
	assert.Contains(t, got, attrTopProcs)
	t.Logf("live report: %v", got)
}

// TestStallLineFirstWindowHasABaseline: the baseline is taken when the first
// window opens, so a stall in the supervisor's first second is attributed too.
func TestStallLineFirstWindowHasABaseline(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID})
	fs := newFakeStatFS()
	fs.talosNode()
	s.consumers = newTestConsumers(fs, 2)

	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "do_epoll_wait")
	s.tick(consumersT0)
	fs.cgroup("/init", 400*time.Millisecond)
	fs.cgroup("/", 400*time.Millisecond)
	proc.thread(testPID, testPID, "envoy", 'R', 0, 700*time.Millisecond, "")
	s.tick(consumersT0.Add(time.Second))

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 1)
	assert.Equal(t, "/init=400ms", recs[0][attrTopCgroups])
	assert.EqualValues(t, 1000, recs[0][attrTopCgroupsOverMs])
}

// countingFS counts what one sample reads from the real filesystems.
type countingFS struct {
	osStatFS
	files, dirs int
}

func (c *countingFS) readFile(name string) ([]byte, error) {
	c.files++
	return c.osStatFS.readFile(name)
}

func (c *countingFS) subdirs(dir string, limit int) ([]string, bool, error) {
	c.dirs++
	return c.osStatFS.subdirs(dir, limit)
}

// BenchmarkConsumerSample measures one sample of each source on this host:
//
//	go test ./agent/internal/proxy/hotrestart/ -run '^$' -bench ConsumerSample
//
// It reports the files and directories read per sample next to the time, which
// is how the cost in the runbook was measured.
func BenchmarkConsumerSample(b *testing.B) {
	if runtime.GOOS != "linux" {
		b.Skip("procfs is Linux-only")
	}
	b.Run("cgroups", func(b *testing.B) {
		fs := &countingFS{}
		c := newCPUConsumers(fs, "/proc", "/sys/fs/cgroup", DefaultStallTopConsumers)
		if _, err := c.sampleCgroups(time.Now(), true); err != nil { // the listing, paid every cgroupListInterval
			b.Skip(err)
		}
		b.Logf("listing: %d directories, %d files", fs.dirs, fs.files)
		fs.dirs, fs.files = 0, 0
		entries := 0
		for b.Loop() {
			s, err := c.sampleCgroups(time.Now(), false)
			if err != nil {
				b.Skip(err)
			}
			entries = len(s.usage)
		}
		b.ReportMetric(float64(fs.files)/float64(b.N), "files/op")
		b.ReportMetric(float64(fs.dirs)/float64(b.N), "dirs/op")
		b.ReportMetric(float64(entries), "cgroups")
	})
	b.Run("processes", func(b *testing.B) {
		fs := &countingFS{}
		c := newCPUConsumers(fs, "/proc", "/sys/fs/cgroup", DefaultStallTopConsumers)
		c.clock = func() time.Time { return time.Time{} } // no time budget: measure the whole scan
		entries := 0
		for b.Loop() {
			s, err := c.sampleProcs(time.Now())
			if err != nil {
				b.Skip(err)
			}
			entries = len(s.procs)
		}
		b.ReportMetric(float64(fs.files)/float64(b.N), "files/op")
		b.ReportMetric(float64(fs.dirs)/float64(b.N), "dirs/op")
		b.ReportMetric(float64(entries), "processes")
	})
}

// A delta under one millisecond would render as `name=0ms`: it must not take
// a top-N slot from a consumer that has something to say.
func TestTopConsumersDropsWhatWouldPrintAsZero(t *testing.T) {
	all := []consumer{
		{name: "/kubepods", cpu: 3 * time.Millisecond},
		{name: "/system/a", cpu: 999 * time.Microsecond},
		{name: "/system/b", cpu: 400 * time.Microsecond},
		{name: "/init", cpu: time.Millisecond},
		{name: "/gone", cpu: 0},
	}
	top := topConsumers(all, 3)
	assert.Equal(t, []consumer{
		{name: "/kubepods", cpu: 3 * time.Millisecond},
		{name: "/init", cpu: time.Millisecond},
	}, top)
}
