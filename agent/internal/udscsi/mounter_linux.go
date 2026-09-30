package udscsi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The mount(2) flags the driver uses. Linux-only values; mounter_other.go
// declares the same names so the driver logic compiles (and fails at runtime)
// elsewhere.
const (
	// tmpfsFlags are the per-mount flags of every per-pod tmpfs: nothing on it
	// can be a setuid binary, a device node or an executable.
	tmpfsFlags uintptr = unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC
	// noSymfollowFlag (MS_NOSYMFOLLOW, Linux >= 5.10) makes path resolution
	// return ELOOP for any symlink that resides on the mount — the flag that
	// closes proposal 039's symlink confused deputy.
	//
	// It is passed to mount(2) itself, NOT applied afterwards with
	// mount_setattr(2): this process mounts in its OWN mount namespace, and the
	// copy that Bidirectional propagation creates in the host namespace (the
	// one the node proxy's HostToContainer /run/aether view clones) carries the
	// flags the mount had when the event propagated. A later mount_setattr
	// changes only this namespace's copy — seen on kind: the host's mount was
	// nosuid,nodev,noexec but NOT nosymfollow, and it followed a symlink the
	// app planted.
	noSymfollowFlag uintptr = unix.MS_NOSYMFOLLOW
	// bindFlags bind-mounts the per-pod tmpfs onto the kubelet's target_path.
	bindFlags uintptr = unix.MS_BIND
	// detachFlag is umount2's MNT_DETACH: the per-pod tmpfs is lazily
	// detached so a straggling fd cannot fail an unpublish.
	detachFlag = unix.MNT_DETACH
)

// NewMounter returns the real, host-mutating Mounter.
func NewMounter() Mounter { return unixMounter{mountinfo: "/proc/self/mountinfo"} }

type unixMounter struct {
	mountinfo string
}

func (m unixMounter) MountedFS(path string) (string, bool, error) {
	f, err := os.Open(m.mountinfo)
	if err != nil {
		return "", false, fmt.Errorf("open %s: %w", m.mountinfo, err)
	}
	defer f.Close()
	return mountedFS(f, filepath.Clean(path))
}

func (unixMounter) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }

func (unixMounter) Mount(source, target, fstype string, flags uintptr, data string) error {
	if err := unix.Mount(source, target, fstype, flags, data); err != nil {
		return fmt.Errorf("mount %s on %s (type %q, flags %#x, data %q): %w", source, target, fstype, flags, data, err)
	}
	return nil
}

func (unixMounter) Unmount(target string, flags int) error {
	if err := unix.Unmount(target, flags); err != nil {
		// EINVAL: not a mount point (any more). ENOENT: gone. Both are the
		// state an unmount is trying to reach.
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("umount %s: %w", target, err)
	}
	return nil
}

func (unixMounter) Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
