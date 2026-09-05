// Command mesh-dns-ready is the aether-mesh-dns pod's exec readiness probe (#683).
//
// It exists for exactly one reason: to be TINY. The probe used to re-exec
// /mesh-dns — the 16.9MB resolver daemon itself — once every 15s per pod. That
// is the same anti-pattern #673/#677 removed from the proxy, one size down:
// continuous profiling on 2026-09-05 attributed ~10 core-seconds per 25 minutes
// fleet-wide (~3-4% of this daemon's CPU) to pure process launch —
// runc 3.26%, libcontainer.(*linuxSetnsInit).Init 2.71%, libcontainer.Init
// 2.37%, a SECOND runtime.main 3.60%, runtime.doInit1 0.55% — against a rounding
// error for the stat the probe actually performs.
//
// That cost is container exec plus package-level init(), which the Go runtime
// runs BEFORE main() is entered regardless of argv. An early return at the top
// of mesh-dns's main cannot avoid it; the only way to not pay it is to not link
// those packages. mesh-dns links cobra, fsnotify, miekg/dns and the OTel SDK
// (exporters, batch log processor, runtime instrumentation), which build
// registries and providers in init().
//
// Therefore: no cobra, no logging framework, no OTel, no DNS library — no
// third-party imports at all beyond the stdlib-only
// aethermesh.dev/common/readymarker. Keep it that way;
// //agent/cmd/mesh-dns-ready:deps_test fails the build if anything else is
// linked.
//
// An httpGet/tcpSocket probe is NOT an alternative here, and that is settled
// precedent rather than a judgement call: the mesh-dns DaemonSet is
// hostNetwork:true with maxSurge:1, so the predecessor and successor pods share
// the host network namespace for the whole handoff. A health port either
// collides (wedging the surge) or is SO_REUSEPORT-ambiguous — the kubelet's
// probe lands on the PEER pod's socket and marks this pod ready before it has
// bound anything. #582 proposed exactly that for THIS DaemonSet and was closed
// abandoned for exactly that reason (see agent/cmd/mesh-dns/main.go's header).
// The ready marker is pod-local by construction: it lives in a per-pod emptyDir
// that the resolver writes only after its UDP+TCP listeners are bound, and
// clears when its self-check declares the resolver wedged.
package main

import (
	"flag"
	"fmt"
	"os"

	"aethermesh.dev/common/readymarker"
)

// defaultReadyMarker must match the mesh-dns daemon's --ready-marker default and
// the path the chart mounts the per-pod ready-marker emptyDir at.
const defaultReadyMarker = "/run/aether/mesh-dns.ready"

// run parses args and returns nil iff the pod is ready. It uses its own FlagSet
// rather than flag.CommandLine so the whole decision is a pure function of its
// arguments — which is what makes it directly testable.
func run(args []string) error {
	fs := flag.NewFlagSet("mesh-dns-ready", flag.ContinueOnError)
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
