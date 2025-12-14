package main

import (
	"os"

	"github.com/bpalermo/aether/agent/pkg/cmd"
)

const name = "agent"

func main() {
	rootCmd := cmd.GetCommand()
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
