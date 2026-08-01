// Package udspath resolves the endpoint.aether.io/uds-socket annotation to the
// host path of a workload's Unix socket (proposal 034).
//
// Pathname Unix sockets are mount-namespace-scoped, so the node proxy (host
// mount namespace) reaches a workload socket through kubelet's pod-volumes
// directory: a socket in a pod emptyDir is host-visible at
//
//	<kubelet-pods-dir>/<pod-UID>/volumes/kubernetes.io~empty-dir/<volume>/<file>
//
// The annotation value ("<volume>/<file>") is attacker-influenced input (any
// pod author can set it), so resolution is fail-closed: both components must
// be single, clean path segments — a malicious annotation must never address
// another pod's volume or an arbitrary host path.
package udspath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// emptyDirSegment is the kubelet volume-plugin directory for emptyDir volumes.
// It is part of the validated contract: only emptyDir volumes carry sockets
// (CSI/projected volumes are out of scope, see proposal 034).
const emptyDirSegment = "kubernetes.io~empty-dir"

// Resolve maps an endpoint.aether.io/uds-socket annotation value plus the
// pod's Kubernetes UID onto the socket's host path under kubeletPodsDir.
// kubeletPodsDir must be an absolute path (the --kubelet-pods-dir flag).
func Resolve(kubeletPodsDir, podUID, annotation string) (string, error) {
	if !filepath.IsAbs(kubeletPodsDir) {
		return "", fmt.Errorf("kubelet pods dir %q is not absolute", kubeletPodsDir)
	}
	if err := validateSegment("pod UID", podUID); err != nil {
		return "", err
	}
	volume, file, ok := strings.Cut(annotation, "/")
	if !ok {
		return "", fmt.Errorf("uds-socket annotation %q is not <volume>/<file>", annotation)
	}
	if err := validateSegment("volume name", volume); err != nil {
		return "", err
	}
	if err := validateSegment("socket file", file); err != nil {
		return "", err
	}
	return filepath.Join(kubeletPodsDir, podUID, "volumes", emptyDirSegment, volume, file), nil
}

// validateSegment rejects anything that is not a single, clean, relative path
// segment: empty strings, path separators (which would smuggle extra
// components past the <volume>/<file> split), "." and ".." (traversal), and
// NUL (C-string truncation at the syscall boundary).
func validateSegment(what, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%s is empty", what)
	case s == "." || s == "..":
		return fmt.Errorf("%s %q is a relative path element", what, s)
	case strings.ContainsAny(s, "/\x00"):
		return fmt.Errorf("%s %q contains a path separator or NUL", what, s)
	case filepath.Clean(s) != s:
		return fmt.Errorf("%s %q is not a clean path segment", what, s)
	}
	return nil
}
