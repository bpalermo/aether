// Command proxy-ready is the aether-proxy pod's exec readiness probe (#673).
//
// It exists for exactly one reason: to be TINY. The probe used to re-exec
// /opt/aether/supervisor — which IS the 67MB agent binary, self-copied under
// another name by the install-supervisor initContainer — once every 2s per pod.
// Continuous profiling (2026-09-04) showed >=31% of the supervisor container's
// CPU was process launch: runtime.main 20.27%, runtime.doInit1 19.75% (package
// init: controller-runtime scheme registration, protobuf descriptor registries),
// against 0.52% for cobra dispatch and a rounding error for the stat itself.
//
// That cost is package-level init(), which the Go runtime runs BEFORE main() is
// entered, regardless of argv. An early return at the top of main cannot avoid
// it. The only way to not pay it is to not link those packages.
//
// Therefore: no cobra, no logging framework, no controller-runtime, no
// protobuf, no OTel — no third-party imports at all beyond the stdlib-only
// aethermesh.dev/common/readymarker. Keep it that way;
// //agent/cmd/proxy-ready:deps_test fails the build if anything else is linked.
//
// An httpGet/tcpSocket probe is NOT an alternative here: the proxy DaemonSet is
// hostNetwork:true with maxSurge:1, so the predecessor and successor pods share
// the host network namespace for the whole handoff and no port-based check can
// be proven pod-local (that is why #582 was closed for the identically shaped
// mesh-dns DaemonSet, and why OPA's diagnostic API in this same pod moved to a
// per-pod UDS). Probing Envoy's admin endpoint is not equivalent either: a
// draining hot-restart parent answers LIVE at its old epoch for the entire
// --parent-shutdown-time-s window (proposal 001, lesson 6), and the supervisor
// deliberately HOLDS readiness while it is still the serving parent. The
// ready-marker is pod-local by construction — it lives in a per-pod emptyDir —
// and the supervisor's write/clear logic is the only thing that decides it.
package main

import (
	"flag"
	"fmt"
	"os"

	"aethermesh.dev/common/readymarker"
)

// defaultReadyMarker must match the supervisor's --ready-marker default and the
// path the chart mounts the per-pod ready-marker emptyDir at.
const defaultReadyMarker = "/var/run/aether-proxy/ready"

// run parses args and returns nil iff the pod is ready. It uses its own FlagSet
// rather than flag.CommandLine so the whole decision is a pure function of its
// arguments — which is what makes it directly testable.
func run(args []string) error {
	fs := flag.NewFlagSet("proxy-ready", flag.ContinueOnError)
	marker := fs.String("ready-marker", defaultReadyMarker,
		"Pod-local readiness marker path; exit 0 iff it exists")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return readymarker.Check(*marker)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
