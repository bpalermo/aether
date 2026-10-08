package hotrestart

import (
	"errors"
	"io/fs"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCPUs is a machine as Envoy's CPU detection sees it. The zero affinity is
// "no usable mask", which Envoy ignores.
type fakeCPUs struct {
	online      int
	onlineErr   error
	affinity    int
	affinityErr error
	// files maps a path to its content; a path not in it does not exist.
	// unreadable paths exist and fail to read.
	files      map[string]string
	unreadable map[string]bool
	// symlinks maps a directory to the directory it is a symlink to.
	symlinks map[string]string
}

func (f *fakeCPUs) OnlineCPUs() (int, error)   { return f.online, f.onlineErr }
func (f *fakeCPUs) AffinityCPUs() (int, error) { return f.affinity, f.affinityErr }

// follow cleans path and follows the symlinked directories, as a filesystem
// does: Envoy asks for "/sys/fs/cgroup//cpu.max" when its cgroup is the root
// of the mount, and so does the code under test.
func (f *fakeCPUs) follow(path string) string {
	path = filepath.Clean(path)
	for link, target := range f.symlinks {
		if rest, ok := strings.CutPrefix(path, link+"/"); ok {
			return target + "/" + rest
		}
	}
	return path
}

// EvalSymlinks resolves path as realpath does, and fails when nothing is
// there.
func (f *fakeCPUs) EvalSymlinks(path string) (string, error) {
	path = f.follow(path)
	if _, ok := f.files[path]; !ok && !f.unreadable[path] {
		return "", &fs.PathError{Op: "lstat", Path: path, Err: fs.ErrNotExist}
	}
	return path, nil
}

func (f *fakeCPUs) ReadFile(path string) ([]byte, error) {
	path = f.follow(path)
	if f.unreadable[path] {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
	}
	content, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return []byte(content), nil
}

const (
	// A container on a cgroup v2 node, with its own cgroup namespace.
	v2MountInfo = "1432 1431 0:30 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw,nsdelegate,memory_recursiveprot\n"
	v2Cgroup    = "0::/\n"
	v2CPUMax    = "/sys/fs/cgroup/cpu.max"

	// A container on a cgroup v1 node.
	v1MountInfo = "" +
		"1500 1499 0:31 / /sys/fs/cgroup/memory ro,nosuid,nodev,noexec,relatime - cgroup cgroup rw,memory\n" +
		"1501 1499 0:32 / /sys/fs/cgroup/cpu,cpuacct ro,nosuid,nodev,noexec,relatime - cgroup cgroup rw,cpu,cpuacct\n" +
		"1502 1499 0:33 / /sys/fs/cgroup/cpuset ro,nosuid,nodev,noexec,relatime - cgroup cgroup rw,cpuset\n"
	v1Cgroup = "" +
		"12:memory:/\n" +
		"5:cpuset:/\n" +
		"4:cpu,cpuacct:/\n"
	v1Quota  = "/sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us"
	v1Period = "/sys/fs/cgroup/cpu,cpuacct/cpu.cfs_period_us"
)

// v2Machine is a cgroup v2 container whose cpu.max holds cpuMax.
func v2Machine(online, affinity int, cpuMax string) *fakeCPUs {
	return &fakeCPUs{online: online, affinity: affinity, files: map[string]string{
		procMountInfoPath: v2MountInfo,
		procCgroupPath:    v2Cgroup,
		v2CPUMax:          cpuMax,
	}}
}

// v1Machine is a cgroup v1 container with the given quota and period files.
func v1Machine(online, affinity int, quota, period string) *fakeCPUs {
	return &fakeCPUs{online: online, affinity: affinity, files: map[string]string{
		procMountInfoPath: v1MountInfo,
		procCgroupPath:    v1Cgroup,
		v1Quota:           quota,
		v1Period:          period,
	}}
}

// with returns f with extra files laid over its own.
func (f *fakeCPUs) with(files map[string]string) *fakeCPUs {
	out := *f
	out.files = maps.Clone(f.files)
	maps.Copy(out.files, files)
	return &out
}

func workersOf(t *testing.T, src cpuSources, environ ...string) int {
	t.Helper()
	c, err := envoyDefaultConcurrency(src, environ)
	require.NoError(t, err)
	return c.workers
}

// Issue #1442: a process pinned to fewer CPUs than the machine has runs that
// many workers.
func TestEnvoyDefaultConcurrencyFollowsAffinity(t *testing.T) {
	for name, tc := range map[string]struct {
		src  *fakeCPUs
		want int
	}{
		"no restriction":                    {&fakeCPUs{online: 20, affinity: 20}, 20},
		"pinned to 2 of 20":                 {&fakeCPUs{online: 20, affinity: 2}, 2},
		"pinned to 1 of 20":                 {&fakeCPUs{online: 20, affinity: 1}, 1},
		"pinned to 3 of 4":                  {&fakeCPUs{online: 4, affinity: 3}, 3},
		"a mask above the online CPUs":      {&fakeCPUs{online: 4, affinity: 8}, 4},
		"an empty mask":                     {&fakeCPUs{online: 4, affinity: 0}, 4},
		"sched_getaffinity fails":           {&fakeCPUs{online: 4, affinity: 2, affinityErr: errors.New("EINVAL")}, 4},
		"no cgroup mounted at all, 2 of 20": {&fakeCPUs{online: 20, affinity: 2, files: map[string]string{procMountInfoPath: "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n"}}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, workersOf(t, tc.src))
		})
	}
}

// The cgroup v2 CPU limit, quota/period, is rounded DOWN with a floor of 1.
func TestEnvoyDefaultConcurrencyFollowsCgroupV2Limit(t *testing.T) {
	for name, tc := range map[string]struct {
		cpuMax string
		want   int
	}{
		"max (no limit)":           {"max 100000\n", 8},
		"500m":                     {"50000 100000\n", 1},
		"1":                        {"100000 100000\n", 1},
		"1500m rounds down":        {"150000 100000\n", 1},
		"1990m rounds down":        {"199000 100000\n", 1},
		"2":                        {"200000 100000\n", 2},
		"2500m rounds down":        {"250000 100000\n", 2},
		"3":                        {"300000 100000\n", 3},
		"another period":           {"50000 20000\n", 2},
		"a limit above the cores":  {"1600000 100000\n", 8},
		"a zero quota is 1 worker": {"0 100000\n", 1},
		// Malformed content is no limit, as it is for Envoy.
		"empty":             {"", 8},
		"one field":         {"150000\n", 8},
		"three fields":      {"150000 100000 1\n", 8},
		"two spaces":        {"150000  100000\n", 8},
		"not a number":      {"abc 100000\n", 8},
		"a negative quota":  {"-1 100000\n", 8},
		"a zero period":     {"150000 0\n", 8},
		"a period of words": {"150000 max\n", 8},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, workersOf(t, v2Machine(8, 8, tc.cpuMax)))
		})
	}
}

// A cgroup v1 hierarchy with the cpu controller: cpu.cfs_quota_us over
// cpu.cfs_period_us, the same rounding.
func TestEnvoyDefaultConcurrencyFollowsCgroupV1Quota(t *testing.T) {
	for name, tc := range map[string]struct {
		quota, period string
		want          int
	}{
		"-1 (no limit)":       {"-1\n", "100000\n", 8},
		"500m":                {"50000\n", "100000\n", 1},
		"1500m rounds down":   {"150000\n", "100000\n", 1},
		"2":                   {"200000\n", "100000\n", 2},
		"3500m rounds down":   {"350000\n", "100000\n", 3},
		"a limit above cores": {"1600000\n", "100000\n", 8},
		// Invalid values are no limit.
		"a zero quota":     {"0\n", "100000\n", 8},
		"a negative quota": {"-2\n", "100000\n", 8},
		"a zero period":    {"200000\n", "0\n", 8},
		"not a number":     {"max\n", "100000\n", 8},
		"an empty period":  {"200000\n", "", 8},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, workersOf(t, v1Machine(8, 8, tc.quota, tc.period)))
		})
	}

	t.Run("a missing period file is no limit", func(t *testing.T) {
		src := v1Machine(8, 8, "200000\n", "100000\n")
		delete(src.files, v1Period)
		assert.Equal(t, 8, workersOf(t, src))
	})
}

// The count is the minimum of the three terms, whichever is smallest.
func TestEnvoyDefaultConcurrencyIsTheMinimumOfAllThree(t *testing.T) {
	for name, tc := range map[string]struct {
		src  *fakeCPUs
		want int
	}{
		"cgroup v2 smallest":             {v2Machine(20, 3, "150000 100000\n"), 1},
		"affinity smallest (v2)":         {v2Machine(20, 2, "300000 100000\n"), 2},
		"online smallest (v2)":           {v2Machine(2, 2, "400000 100000\n"), 2},
		"cgroup v1 smallest":             {v1Machine(20, 4, "250000\n", "100000\n"), 2},
		"affinity smallest (v1)":         {v1Machine(20, 2, "400000\n", "100000\n"), 2},
		"no limit, affinity decides":     {v2Machine(20, 6, "max 100000\n"), 6},
		"no limit, no mask, cores alone": {v2Machine(20, 20, "max 100000\n"), 20},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, workersOf(t, tc.src))
		})
	}
}

// Which cgroup Envoy reads: its own, found through /proc/self/mountinfo and
// /proc/self/cgroup. A v1 cpu hierarchy wins over v2, and no ancestor is read.
func TestEnvoyDefaultConcurrencyReadsItsOwnCgroup(t *testing.T) {
	t.Run("a host cgroup namespace reads the container's own directory", func(t *testing.T) {
		// A privileged container keeps the host's cgroup namespace: the whole
		// hierarchy is mounted and /proc/self/cgroup names the full path.
		const own = "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abcd.scope"
		src := &fakeCPUs{online: 8, affinity: 8, files: map[string]string{
			procMountInfoPath:                   "35 25 0:30 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:10 - cgroup2 cgroup2 rw,nsdelegate\n",
			procCgroupPath:                      "0::" + own + "\n",
			"/sys/fs/cgroup" + own + "/cpu.max": "200000 100000\n",
			// The pod's limit, one level up, is not read.
			"/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cpu.max": "100000 100000\n",
		}}
		assert.Equal(t, 2, workersOf(t, src))
	})
	t.Run("a mount of a subtree reads the path relative to its root", func(t *testing.T) {
		src := &fakeCPUs{online: 8, affinity: 8, files: map[string]string{
			procMountInfoPath:           "35 25 0:30 /kubepods/pod1 /sys/fs/cgroup ro - cgroup2 cgroup rw\n",
			procCgroupPath:              "0::/kubepods/pod1/c1\n",
			"/sys/fs/cgroup/c1/cpu.max": "300000 100000\n",
			"/sys/fs/cgroup/cpu.max":    "100000 100000\n",
		}}
		assert.Equal(t, 3, workersOf(t, src))
	})
	t.Run("a cgroup outside the mount root is no limit", func(t *testing.T) {
		src := &fakeCPUs{online: 8, affinity: 8, files: map[string]string{
			procMountInfoPath:        "35 25 0:30 /kubepods/pod1 /sys/fs/cgroup ro - cgroup2 cgroup rw\n",
			procCgroupPath:           "0::/system.slice/x\n",
			"/sys/fs/cgroup/cpu.max": "100000 100000\n",
		}}
		assert.Equal(t, 8, workersOf(t, src))
	})
	t.Run("a path with .. is no limit", func(t *testing.T) {
		src := v2Machine(8, 8, "100000 100000\n").with(map[string]string{procCgroupPath: "0::/../..\n"})
		assert.Equal(t, 8, workersOf(t, src))
	})
	t.Run("a v1 cpu hierarchy wins over v2 on a hybrid node", func(t *testing.T) {
		src := v1Machine(8, 8, "200000\n", "100000\n").with(map[string]string{
			procMountInfoPath:                "1400 1399 0:29 / /sys/fs/cgroup/unified ro - cgroup2 cgroup2 rw\n" + v1MountInfo,
			procCgroupPath:                   "0::/\n" + v1Cgroup,
			"/sys/fs/cgroup/unified/cpu.max": "400000 100000\n",
		})
		assert.Equal(t, 2, workersOf(t, src))
	})
	t.Run("a v1 hierarchy without the cpu controller is not one", func(t *testing.T) {
		// "cpuset" and "cpuacct" are not "cpu".
		src := v2Machine(8, 8, "300000 100000\n").with(map[string]string{
			procMountInfoPath: "1502 1499 0:33 / /sys/fs/cgroup/cpuset ro - cgroup cgroup rw,cpuset\n" +
				"1503 1499 0:34 / /sys/fs/cgroup/cpuacct ro - cgroup cgroup rw,cpuacct\n" + v2MountInfo,
			procCgroupPath: "5:cpuset:/\n6:cpuacct:/\n" + v2Cgroup,
		})
		assert.Equal(t, 3, workersOf(t, src))
	})
	t.Run("an escaped mount point", func(t *testing.T) {
		src := &fakeCPUs{online: 8, affinity: 8, files: map[string]string{
			procMountInfoPath:        `35 25 0:30 / /mnt/c\040g ro - cgroup2 cgroup rw` + "\n",
			procCgroupPath:           v2Cgroup,
			"/mnt/c g/cpu.max":       "200000 100000\n",
			"/mnt/c\\040g/cpu.max":   "100000 100000\n",
			"/sys/fs/cgroup/cpu.max": "100000 100000\n",
		}}
		assert.Equal(t, 2, workersOf(t, src))
	})
	t.Run("optional mountinfo fields before the separator", func(t *testing.T) {
		src := v2Machine(8, 8, "200000 100000\n").with(map[string]string{
			procMountInfoPath: "35 25 0:30 / /sys/fs/cgroup rw,relatime shared:10 master:3 - cgroup2 cgroup2 rw\n",
		})
		assert.Equal(t, 2, workersOf(t, src))
	})
}

// mountedAt is a cgroup v2 container whose hierarchy is mounted at mountPoint,
// with a two-CPU quota, on an 8-CPU machine.
func mountedAt(mountPoint string) *fakeCPUs {
	return &fakeCPUs{online: 8, affinity: 8, files: map[string]string{
		procMountInfoPath:       "35 25 0:30 / " + mountPoint + " ro - cgroup2 cgroup rw\n",
		procCgroupPath:          v2Cgroup,
		mountPoint + "/cpu.max": "200000 100000\n",
	}}
}

// Envoy reads the cgroup files through a file reader that refuses some paths
// (InstanceImplPosix::illegalPath). A refused file is no limit; a file it
// reads counts. The quota is 2 CPUs of 8 throughout.
func TestEnvoyDefaultConcurrencyReadsOnlyWhatEnvoyReads(t *testing.T) {
	const limited, unlimited = 2, 8
	for name, tc := range map[string]struct {
		src  *fakeCPUs
		want int
	}{
		// Legal before anything is resolved.
		"/sys/fs/cgroup, the usual place": {mountedAt("/sys/fs/cgroup"), limited},
		"below /sys/fs/cgroup":            {mountedAt("/sys/fs/cgroup/unified"), limited},
		// Anywhere outside /dev, /sys and /proc.
		"/mnt/cgroup":           {mountedAt("/mnt/cgroup"), limited},
		"/sysroot is not /sys":  {mountedAt("/sysroot/cgroup"), limited},
		"/device is not /dev":   {mountedAt("/device/cgroup"), limited},
		"/process is not /proc": {mountedAt("/process/cgroup"), limited},
		// The Linux exception: /dev/shm.
		"below /dev/shm":             {mountedAt("/dev/shm/cgroup"), limited},
		"directly /dev/shm":          {mountedAt("/dev/shm"), limited},
		"/dev/shmem is not /dev/shm": {mountedAt("/dev/shmem/cgroup"), unlimited},
		// The reserved directories.
		"elsewhere under /sys":                  {mountedAt("/sys/kernel/cg"), unlimited},
		"/sys/fs/cgroup2 is not /sys/fs/cgroup": {mountedAt("/sys/fs/cgroup2"), unlimited},
		"under /proc":                           {mountedAt("/proc/cg"), unlimited},
		"under /dev":                            {mountedAt("/dev/cg"), unlimited},
		"directly /dev":                         {mountedAt("/dev"), unlimited},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, workersOf(t, tc.src))
		})
	}

	// The reserved directories are judged on the RESOLVED path.
	t.Run("a symlink from outside into /sys is refused", func(t *testing.T) {
		src := mountedAt("/sys/kernel/cg")
		src.files[procMountInfoPath] = "35 25 0:30 / /mnt/cg ro - cgroup2 cgroup rw\n"
		src.symlinks = map[string]string{"/mnt/cg": "/sys/kernel/cg"}
		assert.Equal(t, unlimited, workersOf(t, src))
	})
	t.Run("a symlink from outside into /dev/shm is read", func(t *testing.T) {
		src := mountedAt("/dev/shm/cg")
		src.files[procMountInfoPath] = "35 25 0:30 / /mnt/cg ro - cgroup2 cgroup rw\n"
		src.symlinks = map[string]string{"/mnt/cg": "/dev/shm/cg"}
		assert.Equal(t, limited, workersOf(t, src))
	})
	t.Run("a symlink from /dev into a legal place is read", func(t *testing.T) {
		src := mountedAt("/mnt/cg")
		src.files[procMountInfoPath] = "35 25 0:30 / /dev/cg ro - cgroup2 cgroup rw\n"
		src.symlinks = map[string]string{"/dev/cg": "/mnt/cg"}
		assert.Equal(t, limited, workersOf(t, src))
	})
	// The first two checks are on the path as written.
	t.Run("a path written under /sys/fs/cgroup/ is read wherever it leads", func(t *testing.T) {
		src := mountedAt("/sys/kernel/cg")
		src.files[procMountInfoPath] = "35 25 0:30 / /sys/fs/cgroup/link ro - cgroup2 cgroup rw\n"
		src.symlinks = map[string]string{"/sys/fs/cgroup/link": "/sys/kernel/cg"}
		assert.Equal(t, limited, workersOf(t, src))
	})
}

// The rule itself, branch by branch, against the pinned source.
func TestEnvoyIllegalPath(t *testing.T) {
	src := &fakeCPUs{
		files: map[string]string{
			"/etc/envoy.yaml": "", "/dev/null": "", "/dev/shm/x": "", "/dev/shm": "", "/dev": "", "/sys": "", "/proc": "",
			"/sys/kernel/x": "", "/proc/stat": "", "/proc/self/status": "", "/sysroot/x": "", "/dev/shmx": "",
			"/real/x": "",
		},
		symlinks: map[string]string{"/link-to-proc": "/proc", "/dev/link-out": "/real"},
	}
	for path, illegal := range map[string]bool{
		// 1. /dev/fd/ as written, resolved or not.
		"/dev/fd/3":   false,
		"/dev/fd/999": false,
		// 2. The cgroup detection's own files, as written, existing or not.
		"/proc/self/mountinfo":     false,
		"/proc/self/cgroup":        false,
		"/sys/fs/cgroup/cpu.max":   false,
		"/sys/fs/cgroup//cpu.max":  false,
		"/sys/fs/cgroup/a/b/c.max": false,
		// 3. A path that does not resolve.
		"/no/such/file":  true,
		"/sys/fs/cgroup": true,
		// 4. The reserved directories and the /dev/shm exception.
		"/dev":               true,
		"/sys":               true,
		"/proc":              true,
		"/dev/null":          true,
		"/sys/kernel/x":      true,
		"/proc/stat":         true,
		"/proc/self/status":  true,
		"/dev/shm":           false,
		"/dev/shm/x":         false,
		"/dev/shmx":          true,
		"/link-to-proc/stat": true,
		// 5. Everything else.
		"/etc/envoy.yaml": false,
		"/sysroot/x":      false,
		"/dev/link-out/x": false,
	} {
		assert.Equal(t, illegal, envoyIllegalPath(src, path), path)
	}
}

// ENVOY_CGROUP_CPU_DETECTION=false in the child's environment turns the cgroup
// term off and nothing else.
func TestEnvoyDefaultConcurrencyCgroupDetectionSwitch(t *testing.T) {
	src := v2Machine(20, 4, "150000 100000\n")
	assert.Equal(t, 1, workersOf(t, src), "on by default")
	assert.Equal(t, 1, workersOf(t, src, "PATH=/bin", "ENVOY_CGROUP_CPU_DETECTION=true"))
	assert.Equal(t, 1, workersOf(t, src, "ENVOY_CGROUP_CPU_DETECTION=0"), `only "false" turns it off`)
	assert.Equal(t, 1, workersOf(t, src, "ENVOY_CGROUP_CPU_DETECTION="))
	assert.Equal(t, 4, workersOf(t, src, "ENVOY_CGROUP_CPU_DETECTION=false"), "affinity still applies")
	assert.Equal(t, 4, workersOf(t, src, "PATH=/bin", "ENVOY_CGROUP_CPU_DETECTION=FALSE"))
	assert.Equal(t, 4, workersOf(t, src, "ENVOY_CGROUP_CPU_DETECTION=false", "ENVOY_CGROUP_CPU_DETECTION=true"),
		"the first entry is the one getenv returns")
	assert.Equal(t, 1, workersOf(t, src, "XENVOY_CGROUP_CPU_DETECTION=false", "ENVOY_CGROUP_CPU_DETECTION_X=false"))
}

// A source that cannot be read falls back the way Envoy falls back when the
// same read fails for it. Only an unknown number of online CPUs is an error,
// and the caller hot-restarts on an error.
func TestEnvoyDefaultConcurrencyUnreadableSources(t *testing.T) {
	base := v2Machine(8, 4, "200000 100000\n")

	t.Run("online CPUs unknown is an error, never a guess", func(t *testing.T) {
		src := base.with(nil)
		src.onlineErr = errors.New("no sysfs")
		_, err := envoyDefaultConcurrency(src, nil)
		require.Error(t, err)
	})
	t.Run("zero online CPUs counts as one", func(t *testing.T) {
		assert.Equal(t, 1, workersOf(t, &fakeCPUs{online: 0}))
	})
	t.Run("affinity unreadable: the cgroup limit still applies", func(t *testing.T) {
		src := base.with(nil)
		src.affinityErr = errors.New("EINVAL")
		assert.Equal(t, 2, workersOf(t, src))
	})
	for _, path := range []string{procMountInfoPath, procCgroupPath, v2CPUMax} {
		t.Run("unreadable "+path+": affinity still applies", func(t *testing.T) {
			src := base.with(nil)
			src.unreadable = map[string]bool{path: true}
			assert.Equal(t, 4, workersOf(t, src))
		})
		t.Run("missing "+path+": affinity still applies", func(t *testing.T) {
			src := base.with(nil)
			delete(src.files, path)
			assert.Equal(t, 4, workersOf(t, src))
		})
	}
	t.Run("everything but the online CPUs unreadable", func(t *testing.T) {
		assert.Equal(t, 8, workersOf(t, &fakeCPUs{online: 8, affinityErr: errors.New("EPERM")}))
	})
	t.Run("malformed mountinfo and cgroup lines are skipped", func(t *testing.T) {
		src := base.with(map[string]string{
			procMountInfoPath: "garbage\n\n35 25\n35 25 0:30 / /x\n35 25 0:30 / /x opts cgroup2\n" + v2MountInfo,
			procCgroupPath:    "nocolon\n1:onlyone\n" + v2Cgroup,
		})
		assert.Equal(t, 2, workersOf(t, src))
	})
}

// The terms are reported with the count, for the handoff log line.
func TestEnvoyCPUCountString(t *testing.T) {
	c, err := envoyDefaultConcurrency(v2Machine(20, 2, "150000 100000\n"), nil)
	require.NoError(t, err)
	assert.Equal(t, "envoy default: min(online CPUs 20, CPU affinity 2, cgroup CPU limit 1)", c.String())

	c, err = envoyDefaultConcurrency(&fakeCPUs{online: 4, affinityErr: errors.New("EINVAL")}, nil)
	require.NoError(t, err)
	assert.Equal(t, "envoy default: min(online CPUs 4, CPU affinity none, cgroup CPU limit none)", c.String())
}

// The real machine: the prediction is at least one worker and no more than the
// online CPUs, and the affinity read works on Linux.
func TestSystemCPUSources(t *testing.T) {
	online, err := readOnlineCPUs()
	if err != nil {
		t.Skipf("no %s here: %v", onlineCPUsPath, err)
	}
	n, err := EnvoyDefaultConcurrency(nil)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1)
	assert.LessOrEqual(t, n, online)

	affinity, err := systemCPUSources{}.AffinityCPUs()
	if err != nil {
		// A machine with more possible CPUs than one cpu_set_t holds: the
		// kernel refuses the call, Envoy ignores affinity and so does the
		// prediction (TestEnvoyDefaultConcurrencyFollowsAffinity).
		t.Skipf("sched_getaffinity into one cpu_set_t fails here: %v", err)
	}
	assert.GreaterOrEqual(t, affinity, 1)
	assert.LessOrEqual(t, affinity, online)
}

// Issue #1442, at the handoff. With no --concurrency on either side, a
// predecessor that runs fewer workers than the node has cores is not a changed
// worker count when the successor's container is restricted the same way: the
// roll must stay a hot restart.
func TestConcurrencyDefaultUnderARestrictionHotRestarts(t *testing.T) {
	for name, tc := range map[string]struct {
		predecessor int
		src         *fakeCPUs
	}{
		"affinity 2 on a 20-CPU node":                   {2, &fakeCPUs{online: 20, affinity: 2}},
		"a 1500m CPU limit on a 4-CPU node (cgroup v2)": {1, v2Machine(4, 4, "150000 100000\n")},
		"a 2-CPU limit on a 4-CPU node (cgroup v2)":     {2, v2Machine(4, 4, "200000 100000\n")},
		"a 2-CPU quota on a 4-CPU node (cgroup v1)":     {2, v1Machine(4, 4, "200000\n", "100000\n")},
		"limit 3, affinity 2, 20 CPUs":                  {2, v2Machine(20, 2, "300000 100000\n")},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPredecessor(t, tc.predecessor)
			s := newSuccessorSupervisor(t, f, 0)
			s.cpus = tc.src
			initStartEpochWithin(t, s, 10*time.Second)
			assertHotRestart(t, s, f)
		})
	}
}

// The other direction: a restriction that really gives the successor another
// count than the predecessor's is still a worker-count change.
func TestConcurrencyDefaultUnderARestrictionStillSeesAChange(t *testing.T) {
	// The predecessor ran --concurrency 4; the successor has no flag and a
	// 2-CPU limit, so it will run 2 although the node has 4 cores.
	f := newPredecessor(t, 4)
	s := newSuccessorSupervisor(t, f, 0)
	s.cpus = v2Machine(4, 4, "200000 100000\n")
	initStartEpochWithin(t, s, 10*time.Second)
	assertFreshAfterDrain(t, s, f)
}

// Envoy's unescapePath gives strtol the three octal digits inside a longer
// string, so a fourth octal digit right behind them makes it reject the escape
// and keep the backslash. The port must decode exactly the same paths.
func TestUnescapeMountInfoPathFollowsEnvoy(t *testing.T) {
	for in, want := range map[string]string{
		`/mnt/c\040g`:      "/mnt/c g",    // an escape, then a non-digit
		`/mnt/c\040`:       "/mnt/c ",     // an escape at the end
		`/mnt/c\0407`:      `/mnt/c\0407`, // a fourth octal digit: kept as written
		`/mnt/c\0408`:      "/mnt/c 8",    // 8 is not octal: decoded
		`/mnt/c\04`:        `/mnt/c\04`,   // too short
		`/mnt/c\08a`:       `/mnt/c\08a`,  // not three octal digits
		`/mnt/c\777`:       `/mnt/c\777`,  // above 255
		`/mnt/a\040b\011c`: "/mnt/a b\tc", // two escapes
		"/sys/fs/cgroup":   "/sys/fs/cgroup",
	} {
		assert.Equal(t, want, unescapeMountInfoPath(in), in)
	}
}
