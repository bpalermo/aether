package envoyargs_test

// The supervisor's prediction of Envoy's DEFAULT worker count
// (hotrestart.EnvoyDefaultConcurrency) against the pinned Envoy itself
// (issue #1442).
//
// Without --concurrency the pinned Envoy runs min(online CPUs, CPU affinity,
// cgroup CPU limit) workers, and the supervisor compares that number with a
// live predecessor's at every cross-pod handoff. The rule was read from the
// pinned source and measured on the pinned binary; these tests start that
// binary under each restriction the test can impose on itself and require the
// prediction to equal what Envoy reports, so a pin bump that changes the rule
// fails here.
//
//   - CPU affinity: always. The test narrows its own thread's mask and Envoy
//     inherits it.
//   - cgroup CPU limit: only where the test can get a cgroup with a quota
//     without root, which is a systemd user session with the cpu controller
//     delegated (a developer machine, usually). Anywhere else that test skips
//     and says why; CI runners are expected to skip it. Only cgroup v2 can be
//     reached this way. The cgroup v1 rule is covered by unit tests alone.

import (
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"aethermesh.dev/agent/internal/proxy/hotrestart"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cpuMask is a cpu_set_t.
type cpuMask [16]uint64

func (m *cpuMask) count() int {
	n := 0
	for _, w := range m {
		n += bits.OnesCount64(w)
	}
	return n
}

// restrictThisThreadTo narrows the calling thread's CPU affinity to the first
// n CPUs it is allowed now, and reports false when it is allowed fewer. The
// caller must have locked the goroutine to its thread and must never unlock
// it: the thread then ends with the goroutine and the narrowed mask with it.
func restrictThisThreadTo(t *testing.T, n int) bool {
	t.Helper()
	var mask cpuMask
	_, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY, 0, unsafe.Sizeof(mask), uintptr(unsafe.Pointer(&mask)))
	if errno != 0 {
		// More possible CPUs than one cpu_set_t holds: Envoy's own call fails
		// the same way and it ignores affinity, so there is nothing to narrow.
		t.Skipf("sched_getaffinity into one cpu_set_t fails here: %v", errno)
	}
	if mask.count() < n {
		return false
	}
	var narrow cpuMask
	for cpu := 0; cpu < len(mask)*64 && narrow.count() < n; cpu++ {
		if mask[cpu/64]&(1<<(cpu%64)) != 0 {
			narrow[cpu/64] |= 1 << (cpu % 64)
		}
	}
	_, _, errno = syscall.RawSyscall(syscall.SYS_SCHED_SETAFFINITY, 0, unsafe.Sizeof(narrow), uintptr(unsafe.Pointer(&narrow)))
	require.Zero(t, errno, "sched_setaffinity")
	return true
}

// predictedAndServed is the supervisor's prediction for an Envoy this process
// starts now without --concurrency, and the worker count that Envoy reports.
func predictedAndServed(t *testing.T, envoy string) (predicted, served int) {
	t.Helper()
	predicted, err := hotrestart.EnvoyDefaultConcurrency(os.Environ())
	require.NoError(t, err)
	return predicted, servedConcurrency(t, envoy)
}

// TestPredictedDefaultMatchesEnvoyUnderAffinity: with no --concurrency, the
// supervisor's prediction equals the pinned Envoy's worker count, as the test
// finds the machine and with its CPU affinity narrowed to 1, 2 and 3 CPUs.
func TestPredictedDefaultMatchesEnvoyUnderAffinity(t *testing.T) {
	envoy := pinnedEnvoy(t)

	t.Run("as the machine is", func(t *testing.T) {
		predicted, served := predictedAndServed(t, envoy)
		assert.Equal(t, served, predicted,
			"the pinned Envoy runs %d workers without --concurrency here and the supervisor predicts %d: "+
				"the default rule changed (hotrestart/envoycpucount.go)", served, predicted)
	})
	for _, cpus := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("affinity %d", cpus), func(t *testing.T) {
			// Never unlocked: see restrictThisThreadTo.
			runtime.LockOSThread()
			if !restrictThisThreadTo(t, cpus) {
				t.Skipf("this process is allowed fewer than %d CPUs", cpus)
			}
			predicted, served := predictedAndServed(t, envoy)
			assert.Equal(t, served, predicted,
				"pinned to %d CPUs the pinned Envoy runs %d workers and the supervisor predicts %d", cpus, served, predicted)
			// The restriction reached Envoy. Fewer is possible: a cgroup CPU
			// limit on the test itself.
			assert.LessOrEqual(t, served, cpus, "the affinity mask did not reach Envoy; this case tests nothing")
		})
	}
}

// TestConcurrencyFlagIgnoresAffinity: an explicit --concurrency is taken as it
// is, whatever the mask, so the supervisor is right to compare the flag alone.
func TestConcurrencyFlagIgnoresAffinity(t *testing.T) {
	envoy := pinnedEnvoy(t)
	runtime.LockOSThread()
	if !restrictThisThreadTo(t, 1) {
		t.Skip("this process is allowed no CPU")
	}
	assert.Equal(t, 3, servedConcurrency(t, envoy, "--concurrency", "3"))
}

const (
	// cgroupChildEnv marks the re-executed test binary that runs inside a
	// cgroup with a CPU quota; its value is the quota in whole CPUs, rounded
	// down, which the Envoy started there must not exceed.
	cgroupChildEnv = "AETHER_ENVOYARGS_CGROUP_QUOTA_CPUS"
	cgroupTestName = "TestPredictedDefaultMatchesEnvoyUnderACgroupLimit"
)

// TestPredictedDefaultMatchesEnvoyUnderACgroupLimit runs the comparison inside
// a transient systemd user scope with a CPU quota, for quotas on both sides of
// a whole number: Envoy rounds the limit DOWN (1.5 CPUs is one worker).
func TestPredictedDefaultMatchesEnvoyUnderACgroupLimit(t *testing.T) {
	envoy := pinnedEnvoy(t)
	if want := os.Getenv(cgroupChildEnv); want != "" {
		cgroupLimitChild(t, envoy, want)
		return
	}

	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Skipf("no systemd-run: a cgroup with a CPU limit cannot be made here without root (%v)", err)
	}
	env := os.Environ()
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		// Bazel does not pass it; the user manager's bus lives under it.
		env = append(env, "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(os.Getuid()))
	}
	scope := func(quota string, argv ...string) *exec.Cmd {
		args := append([]string{"--user", "--scope", "--quiet", "--collect", "-p", "CPUQuota=" + quota}, argv...)
		cmd := exec.CommandContext(t.Context(), systemdRun, args...)
		cmd.Env = env
		return cmd
	}
	if out, err := scope("150%", "true").CombinedOutput(); err != nil {
		t.Skipf("no systemd user session to make a cgroup with a CPU limit in (%v): %s", err, strings.TrimSpace(string(out)))
	}

	for _, tc := range []struct {
		quota string
		cpus  int
	}{
		{"150%", 1},
		{"200%", 2},
		{"250%", 2},
		{"300%", 3},
	} {
		t.Run("CPUQuota "+tc.quota, func(t *testing.T) {
			cmd := scope(tc.quota, os.Args[0], "-test.run", "^"+cgroupTestName+"$", "-test.v")
			cmd.Env = append(cmd.Env, cgroupChildEnv+"="+strconv.Itoa(tc.cpus))
			out, err := cmd.CombinedOutput()
			if err == nil && strings.Contains(string(out), "--- SKIP: "+cgroupTestName) {
				t.Skipf("the scope did not get the limit:\n%s", out)
			}
			require.NoError(t, err, "inside a scope with CPUQuota=%s:\n%s", tc.quota, out)
			t.Logf("%s", out)
		})
	}
}

// cgroupLimitChild is the test body inside the scope.
func cgroupLimitChild(t *testing.T, envoy, wantCPUs string) {
	t.Helper()
	want, err := strconv.Atoi(wantCPUs)
	require.NoError(t, err)

	cpuMax, why := ownCgroupV2CPUMax()
	if why != "" {
		t.Skipf("no CPU limit on this scope: %s", why)
	}
	predicted, served := predictedAndServed(t, envoy)
	t.Logf("cpu.max %q: envoy runs %d workers, predicted %d", cpuMax, served, predicted)
	assert.Equal(t, served, predicted,
		"with cpu.max %q the pinned Envoy runs %d workers and the supervisor predicts %d", cpuMax, served, predicted)
	assert.LessOrEqual(t, served, want, "the CPU limit did not reach Envoy; this case tests nothing")

	// The switch that turns the cgroup term off is part of the rule.
	t.Setenv("ENVOY_CGROUP_CPU_DETECTION", "false")
	predicted, served = predictedAndServed(t, envoy)
	assert.Equal(t, served, predicted, "with ENVOY_CGROUP_CPU_DETECTION=false")
}

// ownCgroupV2CPUMax reads this process's cgroup v2 cpu.max, or says why there
// is no limit to test with (a user manager without the cpu controller
// delegated gives a scope with no cpu.max at all).
func ownCgroupV2CPUMax() (cpuMax, why string) {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err.Error()
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		path, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		content, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "cpu.max"))
		if err != nil {
			return "", "the cpu controller is not enabled for this cgroup: " + err.Error()
		}
		cpuMax = strings.TrimSpace(string(content))
		if strings.HasPrefix(cpuMax, "max") {
			return "", "cpu.max is " + cpuMax
		}
		return cpuMax, ""
	}
	return "", "not a cgroup v2 machine"
}
