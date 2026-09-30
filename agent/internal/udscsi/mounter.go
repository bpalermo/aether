package udscsi

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Mounter is every host-mount-namespace side effect the node plugin has. The
// driver logic talks only to this, so the unit tests exercise the whole publish
// and unpublish flow against a fake without root; the real implementation
// (mounter_linux.go) is a thin shim over golang.org/x/sys/unix — the distroless
// image has no mount(8) binary to shell out to.
type Mounter interface {
	// MountedFS reports whether path is itself a mount point and, if so, the
	// filesystem type of the topmost mount there.
	MountedFS(path string) (fstype string, mounted bool, err error)
	// MkdirAll is os.MkdirAll.
	MkdirAll(path string, perm os.FileMode) error
	// Mount is mount(2). The returned error wraps the errno.
	Mount(source, target, fstype string, flags uintptr, data string) error
	// Unmount is umount2(2).
	Unmount(target string, flags int) error
	// Remove is os.Remove; a missing path is NOT an error.
	Remove(path string) error
}

// mountedFS scans a /proc/<pid>/mountinfo stream for path as a mount point and
// returns the filesystem type of the LAST (topmost) entry there. Format, per
// proc(5):
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//	(1)(2)(3)   (4)   (5)      (6)      (7)   (8) (9)   (10)         (11)
//
// Field 5 is the mount point, octal-escaped (a space is \040); field 9, after
// the "-" separator, is the filesystem type.
func mountedFS(r io.Reader, path string) (string, bool, error) {
	var (
		fstype  string
		mounted bool
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		if unescapeMountinfo(fields[4]) != path {
			continue
		}
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			continue
		}
		fstype, mounted = fields[sep+1], true
	}
	if err := sc.Err(); err != nil {
		return "", false, fmt.Errorf("read mountinfo: %w", err)
	}
	return fstype, mounted, nil
}

// unescapeMountinfo reverses the kernel's octal escaping of space, tab, newline
// and backslash in mountinfo paths.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
