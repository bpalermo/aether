package hotrestart

import (
	"fmt"
	"os"
	"strings"
)

// Envoy's default worker count (issue #1442).
//
// An Envoy started without --concurrency does not run one worker per core. At
// the pinned Envoy the default is OptionsImplPlatform::getCpuCount()
// (source/server/options_impl_platform_linux.cc):
//
//	max(1, min(hardware threads, CPU affinity, cgroup CPU limit))
//
//   - hardware threads: std::thread::hardware_concurrency(), the online CPUs.
//   - CPU affinity: CPU_COUNT of sched_getaffinity() into one cpu_set_t (1024
//     CPUs). A failed call, a count of zero or one above the hardware threads
//     is ignored.
//   - cgroup CPU limit (source/server/cgroup_cpu_util.cc): the quota of the
//     process's OWN cgroup, quota/period rounded DOWN with a floor of 1. It
//     comes from cpu.max on cgroup v2, or cpu.cfs_quota_us / cpu.cfs_period_us
//     on a cgroup v1 hierarchy with the cpu controller, which wins when both
//     are mounted. No ancestor cgroup is read. Anything missing, unlimited
//     ("max", -1) or malformed is no limit. ENVOY_CGROUP_CPU_DETECTION=false
//     in Envoy's environment turns this term off.
//
// --cpuset-threads no longer changes anything, and --concurrency 0 is one
// worker, not this default (concurrencyArg).
//
// Measured on the pinned binary on a 20-CPU machine, cgroup v2 (worker count
// from /server_info and from the worker threads, which agreed):
//
//	no flag                              20
//	affinity 1 / 2 / 3 CPUs              1 / 2 / 3
//	CPU quota 0.5 / 1 / 1.5 / 1.99       1
//	CPU quota 2 / 2.5 / 3                2 / 2 / 3
//	quota 1.5 and affinity 3             1
//	quota 3 and affinity 2               2
//	quota 1.5, detection "false"/"FALSE" 20
//	quota 1.5, detection "0"             1
//	--concurrency 4, affinity 2          4
//	--concurrency 4, quota 1.5           4
//	--concurrency 0                      1
//	no quota, 1.5 on the parent cgroup   20
//
// //agent/test/envoyargs holds the prediction below against the real binary,
// so a pin bump that changes the rule fails a test.
//
// The supervisor forks Envoy, so both run in one container: the same cgroup,
// the same mount namespace, and the affinity mask the child inherits. Two
// things can still make the prediction differ from what the next Envoy runs:
// a CPU limit or mask changed between this read and Envoy's own (an in-place
// pod resize, a `taskset -p` on the running supervisor), and a mask set on one
// supervisor thread only. Both are a window or an operator action, not a
// configuration.

const (
	procMountInfoPath = "/proc/self/mountinfo"
	procCgroupPath    = "/proc/self/cgroup"
	// envoyCgroupDetectionEnv turns Envoy's cgroup term off when its value,
	// lower-cased, is "false". Any other value, and no value, leaves it on.
	envoyCgroupDetectionEnv = "ENVOY_CGROUP_CPU_DETECTION"
	// cpuSetBytes is sizeof(cpu_set_t): Envoy asks the kernel for exactly this
	// much mask, and the call fails on a machine with more possible CPUs.
	cpuSetBytes = 128
)

// cpuSources is what Envoy's default worker count is computed from. The
// supervisor reads the real machine (systemCPUSources); tests give a fake.
type cpuSources interface {
	// OnlineCPUs is the number of online CPUs, Envoy's hardware threads.
	OnlineCPUs() (int, error)
	// AffinityCPUs is the number of CPUs in the caller's affinity mask, read
	// the way Envoy reads it (one cpu_set_t).
	AffinityCPUs() (int, error)
	// ReadFile reads /proc/self/mountinfo, /proc/self/cgroup and the cgroup
	// CPU files.
	ReadFile(path string) ([]byte, error)
}

// systemCPUSources reads this process's own view, which is the view the Envoy
// it forks gets.
type systemCPUSources struct{}

func (systemCPUSources) OnlineCPUs() (int, error)             { return readOnlineCPUs() }
func (systemCPUSources) AffinityCPUs() (int, error)           { return affinityCPUs() }
func (systemCPUSources) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// envoyCPUCount is a predicted default worker count and the three terms it is
// the minimum of, for the log line of a handoff that depends on it.
type envoyCPUCount struct {
	workers  int
	hardware int
	// affinity and cgroupLimit are 0 when the term does not apply (unreadable,
	// out of range, no limit, detection off): Envoy then uses hardware.
	affinity    int
	cgroupLimit int
}

func (c envoyCPUCount) String() string {
	term := func(n int) string {
		if n == 0 {
			return "none"
		}
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("envoy default: min(online CPUs %d, CPU affinity %s, cgroup CPU limit %s)",
		c.hardware, term(c.affinity), term(c.cgroupLimit))
}

// EnvoyDefaultConcurrency is the worker count the pinned Envoy runs when it is
// started without --concurrency by this process, with environ as its
// environment. See the comment at the top of this file for the rule.
func EnvoyDefaultConcurrency(environ []string) (int, error) {
	c, err := envoyDefaultConcurrency(systemCPUSources{}, environ)
	return c.workers, err
}

// envoyDefaultConcurrency computes Envoy's default worker count from src.
//
// The only error is an unknown number of online CPUs. Envoy has fallbacks of
// its own for that inside the C++ runtime which cannot be reproduced here, so
// the count is reported unknown, and an unknown count hot-restarts: it can
// never be read as a changed one. Every other unreadable source falls back
// exactly as Envoy does when the same read fails for it (affinity: the
// hardware threads; cgroup: no limit).
func envoyDefaultConcurrency(src cpuSources, environ []string) (envoyCPUCount, error) {
	online, err := src.OnlineCPUs()
	if err != nil {
		return envoyCPUCount{}, fmt.Errorf("online CPUs: %w", err)
	}
	c := envoyCPUCount{hardware: max(1, online)}
	c.workers = c.hardware

	// Envoy takes the mask only when it holds between 1 and the hardware
	// threads; anything else, and a failed call, is the hardware threads.
	if n, err := src.AffinityCPUs(); err == nil && n > 0 && n <= c.hardware {
		c.affinity = n
		c.workers = min(c.workers, n)
	}
	if cgroupDetectionEnabled(environ) {
		if limit, ok := cgroupCPULimit(src); ok {
			c.cgroupLimit = limit
			c.workers = min(c.workers, limit)
		}
	}
	return c, nil
}

// cgroupDetectionEnabled reports whether an Envoy with this environment reads
// the cgroup CPU limit. Like getenv, the first entry for the name counts.
func cgroupDetectionEnabled(environ []string) bool {
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, envoyCgroupDetectionEnv+"="); ok {
			return strings.ToLower(v) != "false"
		}
	}
	return true
}
