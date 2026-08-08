package main

import (
	"os"

	"aethermesh.dev/cni/internal/cmd"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	ctx := ctrl.SetupSignalHandler()
	rootCmd := cmd.GetCommand()
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
