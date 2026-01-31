package main

import (
	"os"

	"github.com/bpalermo/aether/register/pkg/cmd"
)

func main() {
	rootCmd := cmd.GetCommand()
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
