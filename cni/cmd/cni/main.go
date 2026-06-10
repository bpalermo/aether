package main

import (
	"log"
	"os"
	"time"

	cnilog "github.com/bpalermo/aether/cni/internal/log"
	"github.com/bpalermo/aether/cni/internal/plugin"
	"github.com/bpalermo/aether/cni/internal/telemetry"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
	"go.uber.org/zap"
)

// instrument wraps a CNI command handler with operation metrics. Telemetry is
// initialized lazily inside the handler (the OTLP endpoint lives in the
// netconf, parsed there); recording after the handler returns is therefore
// already past initialization — or a no-op when telemetry is disabled.
func instrument(op string, fn func(*skel.CmdArgs) error) func(*skel.CmdArgs) error {
	return func(args *skel.CmdArgs) error {
		start := time.Now()
		err := fn(args)
		telemetry.RecordOperation(op, time.Since(start), err)
		return err
	}
}

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

	// The plugin process exits after a single CNI operation, so batched spans
	// and metrics must be flushed on every path — including failures — before
	// the error is reported to the runtime.
	e := skel.PluginMainFuncsWithError(skel.CNIFuncs{
		Add:    instrument("add", p.CmdAdd),
		Check:  instrument("check", p.CmdCheck),
		Del:    instrument("del", p.CmdDel),
		GC:     instrument("gc", p.CmdGC),
		Status: instrument("status", p.CmdStatus),
	}, version.All, "CNI aether plugin v0.0.1")
	telemetry.Flush(logger)
	if e != nil {
		if err := e.Print(); err != nil {
			log.Print("Error writing error JSON to stdout: ", err)
		}
		os.Exit(1)
	}
}
