//go:build !linux

package udscsi

import (
	"errors"
	"os"
)

// The node plugin is Linux-only (it mounts tmpfs in the host mount namespace).
// These declarations exist so the package compiles on a developer's macOS
// host; every operation fails.
const (
	tmpfsFlags      uintptr = 0
	noSymfollowFlag uintptr = 0
	bindFlags       uintptr = 0
	detachFlag              = 0
)

var errUnsupported = errors.New("the uds-csi node plugin only runs on Linux")

// NewMounter returns a Mounter whose every operation fails: there is no mount
// namespace to mutate off Linux.
func NewMounter() Mounter { return unsupportedMounter{} }

type unsupportedMounter struct{}

func (unsupportedMounter) MountedFS(string) (string, bool, error) { return "", false, errUnsupported }

func (unsupportedMounter) MkdirAll(string, os.FileMode) error { return errUnsupported }

func (unsupportedMounter) Mount(string, string, string, uintptr, string) error { return errUnsupported }

func (unsupportedMounter) Unmount(string, int) error { return errUnsupported }

func (unsupportedMounter) Remove(string) error { return errUnsupported }
