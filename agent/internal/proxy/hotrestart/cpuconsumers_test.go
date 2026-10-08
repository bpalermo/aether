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
	// onRead, when set, runs before every readFile (a read that takes time).
	onRead func(name string)
	// reads and lists count the calls, for the cost assertions; procLists is
	// the listings of /proc itself (one per process scan).
	reads, lists, procLists int
	// listed is every directory a listing was asked for, in order.
	listed []string
}

func newFakeStatFS() *fakeStatFS {
	return &fakeStatFS{files: map[string]string{}, errs: map[string]error{}}
}

func (f *fakeStatFS) readFile(name string) ([]byte, error) {
	f.reads++
	if f.onRead != nil {
		f.onRead(name)
	}
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
	f.listed = append(f.listed, dir)
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
	all, err := cgroupDeltas(c.cgroups.samples[0], c.cgroups.samples[1], 0)
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

// TestCgroupUnreadThenReadIsNotATopConsumer: a cpu.stat that fails with
// anything but "gone" (EIO, a read that does not parse) is a cgroup that still
// exists. It is not forgotten, and when it reads again its lifetime counter is
// not taken for a new cgroup's. A cgroup that is really gone (ENOENT) is
// forgotten as before, and one that appears later under that name is new.
func TestCgroupUnreadThenReadIsNotATopConsumer(t *testing.T) {
	for name, breakIt := range map[string]func(fs *fakeStatFS, file string){
		"EIO":       func(fs *fakeStatFS, file string) { fs.errs[file] = syscall.EIO },
		"malformed": func(fs *fakeStatFS, file string) { fs.files[file] = "nr_periods 0\n" },
	} {
		t.Run(name, func(t *testing.T) {
			fs := newFakeStatFS()
			fs.talosNode()
			set := func(kubelet, podAUsed, gone time.Duration) {
				fs.cgroup("/podruntime/kubelet", time.Hour+kubelet)
				fs.cgroup("/podruntime", time.Hour+kubelet)
				fs.cgroup("/kubepods/burstable/"+podA, 2*time.Hour+podAUsed)
				fs.cgroup("/kubepods/burstable", 3*time.Hour+podAUsed+gone)
				fs.cgroup("/kubepods", 3*time.Hour+podAUsed+gone)
				fs.cgroup("/", 4*time.Hour+kubelet+podAUsed+gone)
			}
			set(0, 0, 0)
			fs.cgroup("/kubepods/burstable/"+podB, time.Hour)
			fs.files[onlineCPUsPath] = "0-3\n"
			c := newTestConsumers(fs, 5)
			c.roll(consumersT0)

			// Second 1: the kubelet's and pod A's cpu.stat cannot be read; pod B
			// is deleted after using 30 ms more.
			kubeletStat := path.Join(fakeCgroupRoot, "/podruntime/kubelet/cpu.stat")
			podAStat := path.Join(fakeCgroupRoot, "/kubepods/burstable/"+podA, "cpu.stat")
			set(100*time.Millisecond, 50*time.Millisecond, 30*time.Millisecond)
			breakIt(fs, kubeletStat)
			breakIt(fs, podAStat)
			fs.removeCgroup("/kubepods/burstable/" + podB)
			got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
			assert.Equal(t, "/podruntime=100ms /kubepods/burstable=80ms", got[attrTopCgroups],
				"what the unread and the removed ones used is in their parents' entries")
			assert.Contains(t, c.cgroupPaths, "/podruntime/kubelet", "an unread cgroup is still known")
			assert.Contains(t, c.cgroupPaths, "/kubepods/burstable/"+podA)
			assert.NotContains(t, c.cgroupPaths, "/kubepods/burstable/"+podB, "a removed one is forgotten")

			// Second 2: both read again. Against the sample that could not read
			// them they have no delta, and they are NOT new: their hours of
			// lifetime usage must not top the list.
			delete(fs.errs, kubeletStat)
			delete(fs.errs, podAStat)
			set(300*time.Millisecond, 90*time.Millisecond, 30*time.Millisecond)
			got = attrMap(t, report1(c, consumersT0.Add(2*time.Second), time.Second))
			assert.Equal(t, "/podruntime=200ms /kubepods/burstable=40ms", got[attrTopCgroups])

			// Over two seconds the baseline is the sample that did read them:
			// exact deltas again.
			got = attrMap(t, report1(c, consumersT0.Add(2*time.Second+time.Millisecond), 2*time.Second))
			assert.Equal(t, "/podruntime/kubelet=300ms /kubepods/burstable/"+podA+"=90ms /kubepods/burstable=30ms", got[attrTopCgroups])

			// Second 3: back to normal, one-second deltas.
			set(350*time.Millisecond, 100*time.Millisecond, 30*time.Millisecond)
			got = attrMap(t, report1(c, consumersT0.Add(3*time.Second), time.Second))
			assert.Equal(t, "/podruntime/kubelet=50ms /kubepods/burstable/"+podA+"=10ms", got[attrTopCgroups])

			// A cgroup of the removed pod's name appears: at the next listing it
			// is a new cgroup, as any other.
			for i := 4; i < 10; i++ {
				c.roll(consumersT0.Add(time.Duration(i) * time.Second))
			}
			fs.cgroup("/kubepods/burstable/"+podB, 70*time.Millisecond)
			fs.cgroup("/kubepods/burstable", 3*time.Hour+100*time.Millisecond+30*time.Millisecond+70*time.Millisecond)
			fs.cgroup("/kubepods", 3*time.Hour+100*time.Millisecond+30*time.Millisecond+70*time.Millisecond)
			fs.cgroup("/", 4*time.Hour+350*time.Millisecond+100*time.Millisecond+30*time.Millisecond+70*time.Millisecond)
			c.roll(consumersT0.Add(10 * time.Second)) // the listing finds it
			got = attrMap(t, report1(c, consumersT0.Add(10*time.Second+time.Millisecond), time.Second))
			assert.Equal(t, "/kubepods/burstable/"+podB+"=70ms", got[attrTopCgroups])
		})
	}
}

// TestCgroupUnreadAtTheListingIsKnown: a cgroup whose cpu.stat fails while the
// hierarchy is being listed is remembered by that listing, so the samples that
// follow read it and the one after has a delta, never a lifetime.
func TestCgroupUnreadAtTheListingIsKnown(t *testing.T) {
	fs := newFakeStatFS()
	fs.talosNode()
	fs.cgroup("/init", time.Hour)
	fs.cgroup("/", time.Hour)
	stat := path.Join(fakeCgroupRoot, "/init/cpu.stat")
	fs.errs[stat] = syscall.EIO
	c := newTestConsumers(fs, 5)
	c.roll(consumersT0)
	require.Contains(t, c.cgroupPaths, "/init")

	delete(fs.errs, stat)
	fs.cgroup("/init", time.Hour+40*time.Millisecond)
	fs.cgroup("/", time.Hour+40*time.Millisecond)
	got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
	assert.Equal(t, "/=40ms", got[attrTopCgroups], "no entry of its own against the sample that could not read it")
	fs.cgroup("/init", time.Hour+60*time.Millisecond)
	fs.cgroup("/", time.Hour+60*time.Millisecond)
	got = attrMap(t, report1(c, consumersT0.Add(2*time.Second), time.Second))
	assert.Equal(t, "/init=20ms", got[attrTopCgroups])
}

// TestCgroupDeltaIsHeldToWhatWasPossible: no cgroup can be shown using more
// than the interval on every CPU.
func TestCgroupDeltaIsHeldToWhatWasPossible(t *testing.T) {
	base := cgroupSample{at: consumersT0, usage: map[string]uint64{"/": 0}}
	cur := cgroupSample{at: consumersT0.Add(time.Second), usage: map[string]uint64{
		"/": 9_000_000_000, "/system": 8_000_000_000,
	}}
	all, err := cgroupDeltas(base, cur, 4)
	require.NoError(t, err)
	assert.Equal(t, "/system=4000ms", formatConsumers(topConsumers(all, 5)), "1 s on 4 CPUs")
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
	all, err := cgroupDeltas(base, cur, 4)
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
	all, err = cgroupDeltas(base, cur, 4)
	require.NoError(t, err)
	top := topConsumers(all, 10)
	assert.Equal(t, "/kubepods/pod11111111-new=60ms /kubepods/podffffffff-known=40ms", formatConsumers(top))
	var sum time.Duration
	for _, e := range top {
		sum += e.cpu
	}
	assert.Equal(t, 100*time.Millisecond, sum, "the root's delta, no more")

	_, err = cgroupDeltas(cgroupSample{usage: map[string]uint64{}}, cur, 4)
	assert.ErrorIs(t, err, errRootNotInOld)
	_, err = cgroupDeltas(base, cgroupSample{usage: map[string]uint64{}}, 4)
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

// TestProcessUnreadableThenReadableIsNotATopConsumer: a long-lived process the
// baseline scan could not read (or parse) and the next scan can is not a new
// process. Charged as one, its whole lifetime's CPU would top the list. A
// process that merely EXITED during the baseline scan and whose PID is then
// reused is new, and is listed.
func TestProcessUnreadableThenReadableIsNotATopConsumer(t *testing.T) {
	fs := newFakeStatFS()
	fs.hostPIDNamespace()
	fs.process(700, "kubelet", 0, 0, 1000)
	fs.process(701, "denied-then-ok", 3*time.Hour, 0, 1001)
	fs.errs["/proc/701/stat"] = syscall.EACCES
	fs.files["/proc/703/stat"] = "703 (garbled-then-ok) S 1 2\n"
	fs.process(704, "exits-mid-scan", time.Hour, 0, 1004)
	fs.errs["/proc/704/stat"] = syscall.ESRCH
	fs.files[onlineCPUsPath] = "0-3\n"
	c := newTestConsumers(fs, 10)
	c.roll(consumersT0)

	fs.process(700, "kubelet", 100*time.Millisecond, 0, 1000)
	delete(fs.errs, "/proc/701/stat")
	fs.process(701, "denied-then-ok", 3*time.Hour+20*time.Millisecond, 0, 1001)
	fs.process(703, "garbled-then-ok", 2*time.Hour, 0, 1003)
	delete(fs.errs, "/proc/704/stat")
	fs.process(704, "pid-reused", 50*time.Millisecond, 0, 9000)

	got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
	assert.Equal(t, "kubelet(700)=100ms pid-reused(704)=50ms", got[attrTopProcs])

	// This scan read them, so the next window has their deltas.
	fs.process(701, "denied-then-ok", 3*time.Hour+320*time.Millisecond, 0, 1001)
	got = attrMap(t, report1(c, consumersT0.Add(2*time.Second), time.Second))
	assert.Equal(t, "denied-then-ok(701)=300ms", got[attrTopProcs])
}

// TestProcessDeltaIsHeldToWhatWasPossible: whatever went wrong with a
// baseline, one process cannot be shown using more than the interval on every
// CPU.
func TestProcessDeltaIsHeldToWhatWasPossible(t *testing.T) {
	base := procSample{at: consumersT0, procs: map[procKey]procUse{}}
	cur := procSample{at: consumersT0.Add(2 * time.Second), procs: map[procKey]procUse{
		{pid: 9, start: 1}:  {comm: "old-and-never-seen", usec: uint64((5 * time.Hour).Microseconds())},
		{pid: 10, start: 2}: {comm: "real", usec: 1_500_000},
	}}
	assert.Equal(t, "old-and-never-seen(9)=8000ms real(10)=1500ms",
		formatConsumers(topConsumers(procDeltas(base, cur, 4), 5)), "2 s on 4 CPUs is 8000 ms")
	// No interval or no CPU count to bound by: the figures are left alone.
	cur.at = base.at
	assert.Contains(t, formatConsumers(topConsumers(procDeltas(base, cur, 4), 5)), "old-and-never-seen(9)=18000000ms")
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
			assert.LessOrEqual(t, fs.reads, 7,
				"one probe of /proc/2/stat, then per call only the cgroup root and (the calls being a minute apart) the node's CPU list")
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
	assert.NotContains(t, got, "topCgroupsTruncated", "the whole stall is covered")

	// Longer than everything kept: the oldest sample, the field says how far
	// back that is, and the line says the start of the stall is not in it.
	got = attrMap(t, report1(c, consumersT0.Add(11*time.Second), time.Hour))
	assert.EqualValues(t, 11000, got[attrTopCgroupsOverMs])
	assert.Equal(t, true, got["topCgroupsTruncated"])
}

// TestProcessConsumersSayWhenTheStallIsOlderThanTheBaseline: the same for the
// process list, whose history is four scans.
func TestProcessConsumersSayWhenTheStallIsOlderThanTheBaseline(t *testing.T) {
	fs := newFakeStatFS()
	fs.hostPIDNamespace()
	fs.process(700, "kubelet", 0, 0, 1000)
	c := newTestConsumers(fs, 5)
	c.roll(consumersT0)

	fs.process(700, "kubelet", 400*time.Millisecond, 0, 1000)
	got := attrMap(t, report1(c, consumersT0.Add(3*time.Second), 2*time.Second))
	assert.Equal(t, "kubelet(700)=400ms", got[attrTopProcs])
	assert.EqualValues(t, 3000, got[attrTopProcsOverMs])
	assert.NotContains(t, got, "topProcsTruncated", "a baseline from before the stall covers it")

	fs.process(700, "kubelet", 500*time.Millisecond, 0, 1000)
	got = attrMap(t, report1(c, consumersT0.Add(4*time.Second), time.Minute))
	assert.EqualValues(t, 4000, got[attrTopProcsOverMs], "the oldest scan kept")
	assert.Equal(t, true, got["topProcsTruncated"])
}

// TestStarvedEveryWindowStillListsNewCgroups: a node starved in every window
// only ever reports, never rolls. The cgroup directories are still listed
// again every cgroupListInterval, so a pod created during a long incident gets
// its own entry instead of staying inside its parent's for as long as the
// incident lasts. The listing is paid once per interval, not once per line.
func TestStarvedEveryWindowStillListsNewCgroups(t *testing.T) {
	const podNew = "pod12345678-1111-4222-8333-444455556666"
	fs := newFakeStatFS()
	fs.talosNode()
	c := newTestConsumers(fs, 3)
	c.roll(consumersT0)
	lists := fs.lists
	require.Positive(t, lists)

	var listsAfter int
	for i := 1; i <= 12; i++ {
		// The new pod exists from the second second on and burns 600 ms in
		// each.
		if i >= 2 {
			used := time.Duration(i-1) * 600 * time.Millisecond
			fs.cgroup("/kubepods/burstable/"+podNew, used)
			fs.cgroup("/kubepods/burstable", used)
			fs.cgroup("/kubepods", used)
			fs.cgroup("/", used)
		}
		got := attrMap(t, report1(c, consumersT0.Add(time.Duration(i)*time.Second), time.Second))
		switch {
		case i < 10:
			assert.NotContains(t, got[attrTopCgroups], podNew, "second %d: not listed yet", i)
			assert.Equal(t, lists, fs.lists, "second %d: no listing inside the interval", i)
		case i == 10:
			assert.Equal(t, "/kubepods/burstable/"+podNew+"=600ms", got[attrTopCgroups],
				"listed in the starved window the interval ran out in, and held to its parent's second")
			listsAfter = fs.lists
			assert.Greater(t, listsAfter, lists)
		default:
			assert.Equal(t, "/kubepods/burstable/"+podNew+"=600ms", got[attrTopCgroups], "second %d", i)
			assert.Equal(t, listsAfter, fs.lists, "second %d: one listing per interval, not one per line", i)
		}
	}
}

// TestListingInAStarvedWindowIsInsideTheScanBudget: the listing a report makes
// shares the report's one scan budget, a listing that runs out of it costs that
// one line its cgroups, and the next line does not try again.
func TestListingInAStarvedWindowIsInsideTheScanBudget(t *testing.T) {
	fs := newFakeStatFS()
	fs.talosNode()
	c := newTestConsumers(fs, 3)
	c.roll(consumersT0)
	known := len(c.cgroupPaths)
	for i := range 600 {
		fs.cgroup("/kubepods/besteffort/pod"+fmt.Sprintf("%08x", i), 0)
	}

	// Every look at the clock is 1 ms later: 250 files, then the deadline.
	tick := consumersT0
	c.clock = func() time.Time { tick = tick.Add(time.Millisecond); return tick }
	readsBefore, listsBefore := fs.reads, fs.lists
	got := attrMap(t, report1(c, consumersT0.Add(10*time.Second), time.Second))
	assert.Equal(t, "unavailable: scan exceeded its time budget", got[attrTopCgroups])
	assert.Greater(t, fs.lists, listsBefore, "the listing was due and was tried")
	assert.Less(t, fs.reads-readsBefore, 300, "and stopped at the deadline")
	assert.Len(t, c.cgroupPaths, known, "the last good set is kept")

	c.clock = func() time.Time { return consumersT0 }
	listsBefore = fs.lists
	fs.cgroup("/init", 300*time.Millisecond)
	fs.cgroup("/", 300*time.Millisecond)
	got = attrMap(t, report1(c, consumersT0.Add(11*time.Second), time.Second))
	assert.Equal(t, "/init=300ms", got[attrTopCgroups], "the next line reads the known set")
	assert.Equal(t, listsBefore, fs.lists, "and lists nothing")
}

// TestConsumerBoundIsTheNodesCPUCount: the figures are node-wide, so what is
// possible in an interval is the interval on every CPU of the NODE. The CPUs
// this process may run on (its cpuset) say nothing about that: a proxy held to
// two CPUs of a large node must not cut the node's usage down to two CPUs'
// worth, and then rank what is left.
func TestConsumerBoundIsTheNodesCPUCount(t *testing.T) {
	burn := func(fs *fakeStatFS, kubelet, pod time.Duration) {
		fs.cgroup("/podruntime/kubelet", kubelet)
		fs.cgroup("/podruntime", kubelet)
		fs.cgroup("/kubepods/burstable/"+podA, pod)
		fs.cgroup("/kubepods/burstable", pod)
		fs.cgroup("/kubepods", pod)
		fs.cgroup("/", kubelet+pod)
	}
	const wantUnbounded = "/kubepods/burstable/" + podA + "=3000000ms /podruntime/kubelet=1000000ms"
	t.Run("more CPUs than this process could ever have", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.talosNode()
		fs.files[onlineCPUsPath] = "0-4095\n"
		c := newTestConsumers(fs, 5)
		c.roll(consumersT0)
		// One second on a 4096-CPU node: the pod used 3000 CPU-seconds, the
		// kubelet 1000. Held to this process's own CPU count, the root's delta
		// would be cut to a fraction of that and shared out in path order,
		// and the pod, the real top consumer, would not lead the list.
		burn(fs, 1000*time.Second, 3000*time.Second)
		got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
		assert.Equal(t, wantUnbounded, got[attrTopCgroups])
	})
	t.Run("the node's count still bounds", func(t *testing.T) {
		fs := newFakeStatFS()
		fs.talosNode()
		fs.files[onlineCPUsPath] = "0-1,4-5\n"
		c := newTestConsumers(fs, 5)
		c.roll(consumersT0)
		burn(fs, 9*time.Second, 0)
		got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
		assert.Equal(t, "/podruntime/kubelet=4000ms", got[attrTopCgroups], "1 s on the node's 4 CPUs")
	})
	t.Run("unreadable: no bound rather than the wrong one", func(t *testing.T) {
		for name, breakIt := range map[string]func(*fakeStatFS){
			"missing":   func(*fakeStatFS) {},
			"EIO":       func(fs *fakeStatFS) { fs.errs[onlineCPUsPath] = syscall.EIO },
			"malformed": func(fs *fakeStatFS) { fs.files[onlineCPUsPath] = "all of them\n" },
		} {
			t.Run(name, func(t *testing.T) {
				fs := newFakeStatFS()
				fs.talosNode()
				breakIt(fs)
				c := newTestConsumers(fs, 5)
				c.roll(consumersT0)
				burn(fs, 1000*time.Second, 3000*time.Second)
				got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
				assert.Equal(t, wantUnbounded, got[attrTopCgroups])
			})
		}
	})
}

func TestSampleHistory(t *testing.T) {
	h := sampleHistory[cgroupSample]{limit: 3}
	_, _, ok := h.baseline(consumersT0)
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
		got, truncated, ok := h.baseline(consumersT0.Add(since))
		require.True(t, ok)
		assert.Equal(t, consumersT0.Add(want), got.at, "since=%v", since)
		assert.Equal(t, want > since, truncated, "since=%v: truncated only when the baseline is after it", since)
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
			if looks == len(c.cgroupPaths)+3 { // the deadline, the root, one per cgroup, then the final check
				return consumersT0.Add(time.Hour)
			}
			return consumersT0
		}
		got = attrMap(t, report1(c, consumersT0.Add(2*time.Second), time.Second))
		assert.Equal(t, "unavailable: scan exceeded its time budget", got[attrTopCgroups])
	})
	t.Run("a slow read of the root is inside the budget", func(t *testing.T) {
		// The root's cpu.stat is the first file of a cgroup scan. If reading
		// it alone uses the budget up, the scan stops there: no second budget
		// for the hierarchy.
		fs := newFakeStatFS()
		fs.talosNode()
		c := newTestConsumers(fs, 5)
		c.roll(consumersT0)
		// Time passes only while the root's cpu.stat is being read: 300 ms.
		clock := consumersT0
		c.clock = func() time.Time { return clock }
		fs.onRead = func(name string) {
			if name == path.Join(fakeCgroupRoot, "cpu.stat") {
				clock = clock.Add(consumerScanBudget + 50*time.Millisecond)
			}
		}
		readsBefore := fs.reads
		got := attrMap(t, report1(c, consumersT0.Add(time.Second), time.Second))
		assert.Equal(t, "unavailable: scan exceeded its time budget", got[attrTopCgroups])
		assert.Equal(t, 1, fs.reads-readsBefore, "only the root was read")
	})
	t.Run("no directory is listed after the deadline", func(t *testing.T) {
		// The read of /kubepods' cpu.stat uses the budget up. The walk must
		// stop there: listing /kubepods next would put a directory listing on
		// top of the read the scan is already over by.
		fs := newFakeStatFS()
		fs.talosNode()
		c := newTestConsumers(fs, 5)
		clock := consumersT0
		c.clock = func() time.Time { return clock }
		fs.onRead = func(name string) {
			if name == path.Join(fakeCgroupRoot, "kubepods", "cpu.stat") {
				clock = clock.Add(consumerScanBudget + 50*time.Millisecond)
			}
		}
		c.roll(consumersT0)
		require.ErrorIs(t, c.cgroupListErr, errScanBudget)
		assert.Equal(t, []string{fakeCgroupRoot, path.Join(fakeCgroupRoot, "init")}, fs.listed,
			"the root and /init, read before the deadline; not /kubepods, whose read ran past it")
	})
	t.Run("a scan stopped by the deadline leaves the known cgroups as they were", func(t *testing.T) {
		// The first known cgroup is gone and the budget runs out after the
		// second: forgetting the first in place would have moved the second
		// over it and left it in the set twice.
		fs := newFakeStatFS()
		fs.cgroup("/a", 0)
		fs.cgroup("/b", 0)
		c := newTestConsumers(fs, 5)
		c.cgroupPaths = []string{"/gone", "/a", "/b"}
		clock := consumersT0
		c.clock = func() time.Time { return clock }
		fs.onRead = func(name string) {
			if name == path.Join(fakeCgroupRoot, "a", "cpu.stat") {
				clock = clock.Add(consumerScanBudget + 50*time.Millisecond)
			}
		}
		s := cgroupSample{at: consumersT0, usage: map[string]uint64{}, unknown: map[string]struct{}{}}
		require.ErrorIs(t, c.readKnownCgroups(&s, c.newBudget()), errScanBudget)
		assert.Equal(t, []string{"/gone", "/a", "/b"}, c.cgroupPaths)

		// A scan that finishes forgets the removed one, and only that one.
		fs.onRead = nil
		s = cgroupSample{at: consumersT0, usage: map[string]uint64{}, unknown: map[string]struct{}{}}
		require.NoError(t, c.readKnownCgroups(&s, c.newBudget()))
		assert.Equal(t, []string{"/a", "/b"}, c.cgroupPaths)
	})
	t.Run("a listing that fails keeps the last good set", func(t *testing.T) {
		// EIO on the root's listing is not "the root has no children": the
		// walk fails, and the set of the last listing that worked stays.
		fs := newFakeStatFS()
		fs.talosNode()
		c := newTestConsumers(fs, 5)
		c.roll(consumersT0)
		require.NoError(t, c.cgroupListErr)
		before := slices.Clone(c.cgroupPaths)
		require.NotEmpty(t, before)

		fs.errs[fakeCgroupRoot] = syscall.EIO
		s := cgroupSample{at: consumersT0, usage: map[string]uint64{}, unknown: map[string]struct{}{}}
		require.ErrorIs(t, c.listCgroups(&s, c.newBudget()), syscall.EIO)
		assert.ElementsMatch(t, before, c.cgroupPaths)

		// The same below the root: /kubepods cannot be listed just now.
		delete(fs.errs, fakeCgroupRoot)
		fs.errs[path.Join(fakeCgroupRoot, "kubepods")] = syscall.EIO
		s = cgroupSample{at: consumersT0, usage: map[string]uint64{}, unknown: map[string]struct{}{}}
		require.ErrorIs(t, c.listCgroups(&s, c.newBudget()), syscall.EIO)
		assert.ElementsMatch(t, before, c.cgroupPaths)

		// One that went away between its parent's listing and its own, or
		// that this user may not list, is a single entry and no failure.
		fs.errs[path.Join(fakeCgroupRoot, "kubepods")] = syscall.ENOENT
		fs.errs[path.Join(fakeCgroupRoot, "system")] = syscall.EACCES
		s = cgroupSample{at: consumersT0, usage: map[string]uint64{}, unknown: map[string]struct{}{}}
		require.NoError(t, c.listCgroups(&s, c.newBudget()))
		assert.Contains(t, c.cgroupPaths, "/kubepods")
		assert.Contains(t, c.cgroupPaths, "/system")
		assert.NotContains(t, c.cgroupPaths, "/kubepods/burstable")
		assert.NotContains(t, c.cgroupPaths, "/system/apid")
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
		cgroupSample{usage: map[string]uint64{"/": math.MaxUint64}}, 4)
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

	// The node's CPU count, through the same reader, agrees with the
	// supervisor's other reading of that file where the sandbox shows it. It
	// is at least the CPUs this process may run on, never fewer.
	if want, err := readOnlineCPUs(); err == nil {
		assert.Equal(t, want, c.nodeCPUs(time.Now().Add(time.Hour)))
		assert.GreaterOrEqual(t, c.ncpu, runtime.NumCPU())
	} else {
		assert.Contains(t, c.nodeCPUs(time.Now().Add(time.Hour)), "unknown (")
	}
}

// TestNodeCPUsSaysWhenTheFiguresAreNotCapped: the startup line carries the
// node's CPU count, or says in words that there is none to cap the figures by.
func TestNodeCPUsSaysWhenTheFiguresAreNotCapped(t *testing.T) {
	fs := newFakeStatFS()
	fs.files[onlineCPUsPath] = "0-7\n"
	c := newTestConsumers(fs, 5)
	assert.Equal(t, 8, c.nodeCPUs(consumersT0))

	// Read again only when the interval is up, and then believed: a count that
	// can no longer be read is not carried over.
	delete(fs.files, onlineCPUsPath)
	assert.Equal(t, 8, c.nodeCPUs(consumersT0.Add(cgroupListInterval-time.Second)))
	assert.Equal(t,
		"unknown (no such file or directory): consumer figures are not capped at the interval on every CPU",
		c.nodeCPUs(consumersT0.Add(cgroupListInterval)))
	assert.Zero(t, c.ncpu)
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

// TestStallLineCoversAWaitSeenLate: under the contention this line is for, the
// supervisor is descheduled too. A wait that ended while it was not looking is
// seen ticks later, and "the tick minus its length" is then later than the
// wait began by however long the supervisor was away. The line's consumers
// must still reach back to where the wait can have begun.
func TestStallLineCoversAWaitSeenLate(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID})
	s.interval = 100 * time.Millisecond
	fs := newFakeStatFS()
	fs.talosNode()
	s.consumers = newTestConsumers(fs, 5)

	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "do_epoll_wait")
	t0 := consumersT0
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	for ms := 0; ms <= 1000; ms += 100 {
		s.tick(at(ms))
	}
	// The thread starts waiting at 1 s. From 1 s to 2 s the kubelet holds the
	// CPUs; nothing is charged to the thread until the wait ends.
	fs.cgroup("/podruntime/kubelet", 900*time.Millisecond)
	fs.cgroup("/podruntime", 900*time.Millisecond)
	fs.cgroup("/", 900*time.Millisecond)
	for ms := 1100; ms <= 3900; ms += 100 {
		s.tick(at(ms))
	}
	require.Empty(t, logs.records(t, "envoy thread stall"))

	// The supervisor is away from 3.9 s to 6 s. The wait ends at 4 s, three
	// seconds long, and is seen at 6 s.
	proc.thread(testPID, testPID, "envoy", 'R', 0, 3*time.Second, "")
	fs.cgroup("/init", 50*time.Millisecond)
	fs.cgroup("/", 950*time.Millisecond)
	s.tick(at(6000))

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 1)
	assert.Equal(t, "/podruntime/kubelet=900ms /init=50ms", recs[0][attrTopCgroups],
		"from the sample at 1 s, where the wait began; 6 s minus 3 s would start at 3 s, after the kubelet's second")
	assert.EqualValues(t, 5000, recs[0][attrTopCgroupsOverMs])
}

// TestStallLineCoversAWaitThatBeganBeforeTheWindow: the kernel charges a
// runqueue wait when it ends, and the sampler sees it on the next tick. A
// 500 ms wait seen 100 ms into a window began 400 ms before that window, in a
// second somebody else had the CPUs. The line's consumers must cover that
// second too: the ones of the window alone are not the ones that caused it.
func TestStallLineCoversAWaitThatBeganBeforeTheWindow(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID})
	fs := newFakeStatFS()
	fs.talosNode()
	s.consumers = newTestConsumers(fs, 5)

	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "do_epoll_wait")
	t0 := consumersT0
	s.tick(t0)
	s.tick(t0.Add(time.Second)) // a quiet second

	// Second 2: the kubelet holds the CPUs. The thread starts waiting at
	// 1.6 s; nothing is charged yet, so the window closes quiet.
	fs.cgroup("/podruntime/kubelet", 900*time.Millisecond)
	fs.cgroup("/podruntime", 900*time.Millisecond)
	fs.cgroup("/", 900*time.Millisecond)
	s.tick(t0.Add(2 * time.Second))
	require.Empty(t, logs.records(t, "envoy thread stall"))

	// Second 3: the wait ends at 2.1 s and is seen on that tick. For the rest
	// of the second only /init does a little.
	proc.thread(testPID, testPID, "envoy", 'R', 0, 500*time.Millisecond, "")
	s.tick(t0.Add(2100 * time.Millisecond))
	fs.cgroup("/init", 50*time.Millisecond)
	fs.cgroup("/", 950*time.Millisecond)
	s.tick(t0.Add(3 * time.Second))

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 1)
	assert.Contains(t, toStrings(t, recs[0]["threads"])[0], "envoy[starved] cpu=0ms runq=500ms")
	assert.Equal(t, "/podruntime/kubelet=900ms /init=50ms", recs[0][attrTopCgroups],
		"from the sample before the wait began (1 s), not from the window's start (2 s)")
	assert.EqualValues(t, 2000, recs[0][attrTopCgroupsOverMs])
	assert.NotContains(t, recs[0], "topCgroupsTruncated")

	// A wait wholly inside its window is still measured over the window.
	fs.cgroup("/init", 350*time.Millisecond)
	fs.cgroup("/", 1250*time.Millisecond)
	proc.thread(testPID, testPID, "envoy", 'R', 0, 800*time.Millisecond, "")
	s.tick(t0.Add(3500 * time.Millisecond))
	s.tick(t0.Add(4 * time.Second))
	recs = logs.records(t, "envoy thread stall")
	require.Len(t, recs, 2)
	assert.Equal(t, "/init=300ms", recs[1][attrTopCgroups])
	assert.EqualValues(t, 1000, recs[1][attrTopCgroupsOverMs])
}

// TestStallLineSaysWhenTheStallIsOlderThanTheHistory: a wait that began before
// the oldest cgroup sample kept is measured from that sample, and the line
// says so instead of leaving it to be worked out from two numbers.
func TestStallLineSaysWhenTheStallIsOlderThanTheHistory(t *testing.T) {
	proc := newFakeProc(t)
	logs := &capturedLog{}
	s := newTestStallSampler(proc, logs, nil, map[int]int{testEpoch: testPID})
	fs := newFakeStatFS()
	fs.talosNode()
	s.consumers = newTestConsumers(fs, 5)

	proc.thread(testPID, testPID, "envoy", 'S', 0, 0, "do_epoll_wait")
	for i := range cgroupHistoryLen + 8 {
		s.tick(consumersT0.Add(time.Duration(i) * time.Second))
	}
	require.Len(t, s.consumers.cgroups.samples, cgroupHistoryLen)
	end := consumersT0.Add(time.Duration(cgroupHistoryLen+8) * time.Second)
	fs.cgroup("/init", 700*time.Millisecond)
	fs.cgroup("/", 700*time.Millisecond)
	proc.thread(testPID, testPID, "envoy", 'R', 0, time.Duration(cgroupHistoryLen+4)*time.Second, "")
	s.tick(end)

	recs := logs.records(t, "envoy thread stall")
	require.Len(t, recs, 1)
	assert.EqualValues(t, cgroupHistoryLen*1000, recs[0][attrTopCgroupsOverMs], "from the oldest sample kept")
	assert.Equal(t, true, recs[0]["topCgroupsTruncated"])
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
