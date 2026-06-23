package plugin

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// withPodNetns runs fn with the calling goroutine setns'd into the network namespace
// at netnsPath, on a locked OS thread (so a netlink/nftables socket fn opens lands in
// that netns), then restores the original netns. A failed restore leaves the thread
// locked so the Go runtime destroys it rather than reusing a thread stuck in the pod
// netns. Used by the transparent-capture and mesh-DNS redirect installers.
func withPodNetns(netnsPath string, fn func() error) error {
	target, err := os.Open(netnsPath)
	if err != nil {
		return fmt.Errorf("opening netns %s: %w", netnsPath, err)
	}
	defer func() { _ = target.Close() }()

	runtime.LockOSThread()
	origin, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("opening current netns: %w", err)
	}
	defer func() { _ = origin.Close() }()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("entering netns %s: %w", netnsPath, err)
	}

	fnErr := fn()

	if err := unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET); err != nil {
		// Thread poisoned (stuck in the pod netns): keep it locked.
		return fmt.Errorf("restoring host netns: %w", err)
	}
	runtime.UnlockOSThread()
	return fnErr
}
