package main

import (
	"context"
	"log"
	"os"

	cnilog "github.com/bpalermo/aether/cni/internal/log"
	"github.com/bpalermo/aether/cni/internal/plugin"
	"github.com/bpalermo/aether/cni/internal/telemetry"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
	"go.uber.org/zap"
)

func main() {
	logger, err := cnilog.NewLogger()
	defer func(logger *zap.Logger) {
		if err := logger.Sync(); err != nil {
			logger.Error("failed to sync logger", zap.Error(err))
		}
	}(logger)

	if err != nil {
		logger.Fatal("failed to initialize logger", zap.Error(err))
		os.Exit(1)
	}
	logger.Debug("aether CNI plugin started")

	p := plugin.NewAetherPlugin(logger)

	// Detached netns-unpin mode (spawned by CmdDel, not by the runtime): wait
	// out the drain tail, then release the pin. See plugin.RunDetachedUnpin.
	if len(os.Args) > 1 && os.Args[1] == plugin.NetnsUnpinSubcommand {
		p.RunDetachedUnpin(os.Args[2:])
		return
	}

	// Opt-in tracing (OTEL_EXPORTER_OTLP_ENDPOINT). The plugin process exits
	// after a single CNI operation, so batched spans must be flushed on every
	// path — including failures — before the error is reported to the runtime.
	flushTraces := telemetry.Setup(context.Background(), logger)

	e := skel.PluginMainFuncsWithError(skel.CNIFuncs{
		Add:    p.CmdAdd,
		Check:  p.CmdCheck,
		Del:    p.CmdDel,
		GC:     p.CmdGC,
		Status: p.CmdStatus,
	}, version.All, "CNI aether plugin v0.0.1")
	flushTraces()
	if e != nil {
		if err := e.Print(); err != nil {
			log.Print("Error writing error JSON to stdout: ", err)
		}
		os.Exit(1)
	}
}
