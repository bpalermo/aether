package main

import (
	"os"

	"github.com/bpalermo/aether/cni/internal/log"
	"github.com/bpalermo/aether/cni/pkg/plugin"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
	"go.uber.org/zap"
)

func main() {
	logger, err := log.NewLogger()
	defer func(logger *zap.Logger) {
		if err := logger.Sync(); err != nil {
			logger.Error("failed to sync logger", zap.Error(err))
		}
	}(logger)

	if err != nil {
		logger.Fatal("failed to initialize logger", zap.Error(err))
		os.Exit(1)
	}
	logger.Info("aether CNI plugin started")

	p := plugin.NewAetherPlugin(logger)

	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:    p.CmdAdd,
		Check:  p.CmdCheck,
		Del:    p.CmdDel,
		GC:     p.CmdGC,
		Status: p.CmdStatus,
	}, version.All, "CNI aether plugin v0.0.1")
}
