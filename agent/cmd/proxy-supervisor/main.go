// Command proxy-supervisor is the Envoy hot-restart supervisor run as the
// aether-proxy container entrypoint (proposal 001, carved out of the agent
// binary by issue #772 phase B2).
//
// It used to be `agent proxy-supervisor`, a subcommand of //agent/cmd/agent, so
// the initContainer staged the WHOLE 65MiB agent binary — controller-runtime,
// client-go, go-control-plane, SPIRE, Gateway API, miekg/dns — onto the proxy
// pod's shared volume and ran it as PID 1 of the proxy container. None of that
// is reachable from the supervisor's actual work: //agent/internal/proxy/hotrestart
// plus //common/readymarker need ~22 modules, no Kubernetes, no xDS and no SPIRE.
// The single line of coupling was manager.SetupLogging, whose only heavy act
// (ctrl.SetLogger) is meaningless in a process that has no controller-runtime
// manager; it is log.Named(log.NewLogger(debug), name) here.
//
// This is the same carve-out //agent/cmd/proxy-ready (#673) and
// //agent/cmd/mesh-dns (#583) already did, and the reason matters twice over in
// this container: Go package init() runs before main() is entered, so every byte
// linked here is paid on every supervisor start and on every hot-restart epoch,
// and #673 measured that cost at >=31% of this container's CPU when the same
// binary was additionally re-exec'd as the readiness probe.
//
// Keep it that way: //agent/cmd/proxy-supervisor:deps_test fails the build if
// this binary ever links controller-runtime, client-go, go-control-plane or
// SPIRE, and scripts/check-proxy-supervisor-deps.sh asserts the same on the
// build graph.
package main

import (
	"context"
	"os"

	"aethermesh.dev/agent/internal/supervisorcmd"
	"aethermesh.dev/common/signals"
)

// Version is set at build time via -ldflags (Bazel x_defs). It becomes the OTel
// service.version on the supervisor's pushed hot-restart metrics.
var Version = "dev"

func main() {
	// //common/signals rather than ctrl.SetupSignalHandler() (#777): identical
	// behaviour — first SIGTERM cancels the context so the supervisor runs its own
	// shutdown, a second one exits 1 — with zero external modules, where
	// controller-runtime's costs all of client-go.
	//
	// The first signal must reach the supervisor rather than the process: it owns
	// a live Envoy child and a cross-pod hot-restart handoff, and its Run() is what
	// terminates the child in the right order.
	ctx, stop := signals.NotifyContext(context.Background())
	defer stop()

	if err := supervisorcmd.New(Version).ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
