//go:build linux

package hotrestart

import (
	"math/bits"
	"syscall"
	"unsafe"
)

// affinityCPUs counts the CPUs in the calling thread's affinity mask with the
// call Envoy makes: sched_getaffinity into one cpu_set_t. The kernel refuses
// the call (EINVAL) when the machine has more possible CPUs than the mask
// holds, and Envoy then ignores affinity, so the same size is asked for here.
//
// The mask is the calling thread's. Envoy reads its main thread's, which it
// inherits from the supervisor thread that forks it; every thread of the
// supervisor carries the mask the container started with unless one is changed
// from outside.
func affinityCPUs() (int, error) {
	var mask [cpuSetBytes / 8]uint64
	n, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY, 0, cpuSetBytes, uintptr(unsafe.Pointer(&mask[0])))
	if errno != 0 {
		return 0, errno
	}
	count := 0
	// The kernel returns how many bytes of the mask it wrote.
	for i := 0; i < int(n)/8 && i < len(mask); i++ {
		count += bits.OnesCount64(mask[i])
	}
	return count, nil
}
