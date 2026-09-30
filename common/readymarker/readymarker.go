// Package readymarker is the single definition of the pod-local readiness-marker
// predicate used by the aether-proxy pod: ready iff the marker path stats.
//
// It is deliberately stdlib-only (#673). The kubelet execs the reader binary
// every 2s per pod, and the reader used to be the 67MB agent binary — so the
// package this predicate lives in must never drag in controller-runtime, the
// Kubernetes client, protobuf descriptor registries, or any other init()-heavy
// dependency. Keep the import list at `fmt` + `os`.
package readymarker

import (
	"fmt"
	"os"
)

// Check returns nil iff path exists.
//
// Semantics are exactly os.Stat's, unchanged from the supervisor's former
// --readiness-check exec-probe mode this replaced (#673; the flag has since been
// removed): symlinks are followed (a dangling
// symlink is NOT ready) and a directory at path IS ready. The supervisor writes
// a regular file (see hotrestart.Supervisor.setReady), so those shapes never
// occur in practice; they are pinned here only so the reader stays bug-for-bug
// identical to what it replaced.
func Check(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("not ready: %w", err)
	}
	return nil
}
