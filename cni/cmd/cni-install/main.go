package main

import (
	"context"
	"os"

	"aethermesh.dev/cni/internal/cmd"
	"aethermesh.dev/common/signals"
)

func main() {
	// //common/signals, not controller-runtime's SetupSignalHandler: this init
	// container copies a binary and writes a conflist, and that one import linked
	// all of client-go into it (issue #772, phase B1). Behaviour is identical —
	// first signal cancels, second exits 1.
	ctx, stop := signals.NotifyContext(context.Background())
	defer stop()
	rootCmd := cmd.GetCommand()
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
