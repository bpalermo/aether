package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// resolvePIDFromNetns returns the PID of a process whose network namespace
// matches the netns file at netnsPath, comparing the file's device and inode
// numbers against every /proc/<pid>/ns/net. A bind-mounted named netns and a
// process's ns/net link share the same nsfs device+inode when they refer to the
// same namespace, so this is the same identity check `ip netns identify` uses.
//
// The agent must run in the host PID namespace for the /proc scan to see the
// container (sandbox) processes; otherwise no match is found.
func resolvePIDFromNetns(netnsPath string) (int32, error) {
	target, err := os.Stat(netnsPath)
	if err != nil {
		return 0, fmt.Errorf("stat netns %q: %w", netnsPath, err)
	}
	targetSys, ok := target.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("unexpected stat type for %q", netnsPath)
	}

	matches, err := filepath.Glob("/proc/[0-9]*/ns/net")
	if err != nil {
		return 0, fmt.Errorf("glob /proc: %w", err)
	}

	for _, candidate := range matches {
		info, statErr := os.Stat(candidate)
		if statErr != nil {
			continue
		}
		sys, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		if sys.Dev == targetSys.Dev && sys.Ino == targetSys.Ino {
			pid, perr := pidFromProcNetnsPath(candidate)
			if perr != nil {
				continue
			}
			return pid, nil
		}
	}

	return 0, fmt.Errorf("no process found owning netns %q", netnsPath)
}

// pidFromProcNetnsPath extracts <pid> from a "/proc/<pid>/ns/net" path.
func pidFromProcNetnsPath(p string) (int32, error) {
	const prefix = "/proc/"
	const suffix = "/ns/net"
	if len(p) <= len(prefix)+len(suffix) {
		return 0, fmt.Errorf("unexpected proc netns path %q", p)
	}
	pidStr := p[len(prefix) : len(p)-len(suffix)]
	pid, err := strconv.ParseInt(pidStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse pid from %q: %w", p, err)
	}
	return int32(pid), nil
}

// resolvePIDWithRetry polls resolve until it returns a PID or ctx is done. The
// container runtime starts the sandbox process shortly after CNI ADD returns, so
// the netns is briefly unowned; retrying bridges that gap.
func resolvePIDWithRetry(ctx context.Context, resolve func() (int32, error), interval time.Duration) (int32, error) {
	for {
		pid, err := resolve()
		if err == nil {
			return pid, nil
		}

		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("resolving pid: %w (last attempt: %v)", ctx.Err(), err)
		case <-time.After(interval):
		}
	}
}
