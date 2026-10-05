package main

import (
	"fmt"
	"syscall"
)

// checkUnixSocket returns nil iff a stream connect to the AF_UNIX socket at path
// succeeds, and closes the connection straight away without writing a byte.
//
// A socket FILE is not enough: it outlives a listener that crashed, and a
// restarted sidecar in the same pod finds its predecessor's file still in the
// emptyDir. Only a listener that is accepting makes connect(2) succeed (a stale
// file answers ECONNREFUSED, a missing one ENOENT).
//
// It is raw syscall rather than net.Dial on purpose: the net package would add
// a large share of this binary for one connect(2), and the whole value of
// proxy-ready is to be tiny (#673). A connect to a listener whose backlog is
// full blocks; the kubelet's exec-probe timeoutSeconds bounds that and fails
// the probe, which is the right answer for a listener that is not keeping up.
func checkUnixSocket(path string) error {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("not ready: socket: %w", err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := syscall.Connect(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		return fmt.Errorf("not ready: connect %s: %w", path, err)
	}
	return nil
}
