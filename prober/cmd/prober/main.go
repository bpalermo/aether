package main

import (
	"context"
	"os"

	"aethermesh.dev/common/signals"
	"aethermesh.dev/prober/internal/cmd"
)

func main() {
	// //common/signals, not controller-runtime's SetupSignalHandler: the prober is
	// a black-box HTTP client with no Kubernetes API access at all, and that one
	// import linked client-go into it (issue #772, phase B1). Behaviour is
	// identical — first signal cancels, second exits 1.
	ctx, stop := signals.NotifyContext(context.Background())
	defer stop()
	if err := cmd.GetCommand().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
