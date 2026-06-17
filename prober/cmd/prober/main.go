package main

import (
	"os"

	"github.com/bpalermo/aether/prober/internal/cmd"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	ctx := ctrl.SetupSignalHandler()
	if err := cmd.GetCommand().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
