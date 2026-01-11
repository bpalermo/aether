package cmd

import (
	"sync"

	"github.com/bpalermo/aether/agent/internal/log"
	cniServer "github.com/bpalermo/aether/agent/pkg/cni/server"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/controller"
	"github.com/bpalermo/aether/agent/pkg/install"
	"github.com/bpalermo/aether/agent/pkg/xds/registry"
	xdsServer "github.com/bpalermo/aether/agent/pkg/xds/server"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"go.uber.org/zap/zapcore"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	// name is the controller name used for logging
	name = "aether-agent"
)

var (
	cfg    = &install.InstallerConfig{}
	debug  bool
	logger logr.Logger
)

var rootCmd = &cobra.Command{
	Use:          "agent",
	Short:        "Runs the aether node agent.",
	SilenceUsage: true,
	PersistentPreRun: func(_ *cobra.Command, _ []string) {
		opts := log.DefaultOptions()
		if debug {
			opts.Development = true
			opts.Level = zapcore.DebugLevel
		}
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
		logger = ctrl.Log.WithName(name)
	},
	RunE: func(_ *cobra.Command, _ []string) (err error) {
		return runAgent()
	},
}

// GetCommand returns the main cobra.Command object for this application
func GetCommand() *cobra.Command {
	return rootCmd
}

func init() {
	rootCmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug mode")
	rootCmd.Flags().StringVar(&cfg.ClusterName, "cluster", "", "Cluster name. It will be used to push registration info to the control plane.")
	rootCmd.Flags().StringVar(&cfg.ProxyServiceNodeID, "proxy-id", constants.DefaultProxyID, "The xDS proxy ID (service-node)")
	rootCmd.Flags().StringVar(&cfg.CNIBinSourceDir, "cni-bin-dir", constants.DefaultCNIBinDir, "Directory from where the CNI binaries should be copied")
	rootCmd.Flags().StringVar(&cfg.CNIBinTargetDir, "cni-bin-target-dir", constants.DefaultHostCNIBinDir, "Directory into which to copy the CNI binaries")
	rootCmd.Flags().StringVar(&cfg.MountedCNINetDir, "mounted-cni-net-dir", constants.DefaultHostCNINetDir, "Directory where CNI network configuration files are located")
	rootCmd.Flags().StringVar(&cfg.MountedCNIRegistryDir, "mounted-registry-dir", constants.DefaultHostCNIRegistryDir, "Directory where CNI registry entries are located")

	_ = rootCmd.MarkPersistentFlagRequired("cluster")
	_ = rootCmd.MarkPersistentFlagRequired("proxy-id")
}

func runAgent() error {
	var ctx = ctrl.SetupSignalHandler()

	logger.Info("starting aether agent",
		"proxy-id", cfg.ProxyServiceNodeID,
		"cluster", cfg.ClusterName,
		"debug", cfg.Debug)

	logger.Info("installing CNI binaries")
	installer := install.NewInstaller(logger, cfg)
	err := installer.Run(ctx)
	if err != nil {
		return err
	}

	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})
	if err != nil {
		return err
	}

	// use wait group for initialization synchronization
	initWg := &sync.WaitGroup{}
	initWg.Add(2)

	// Create xDS registry
	r := registry.NewXdsRegistry(cfg.ProxyServiceNodeID, logger)
	if err = m.Add(r); err != nil {
		return err
	}

	// Create xDS server
	xdsSrv := xdsServer.NewXdsServer(
		m,
		logger,
		initWg,
		r,
	)
	if err = m.Add(xdsSrv); err != nil {
		return err
	}

	cniSrv := cniServer.NewCNIServer(
		logger,
		cfg.MountedCNIRegistryDir,
		"",
		initWg,
		r.GetEventChan(),
	)

	if err = m.Add(cniSrv); err != nil {
		return err
	}

	// Create the XDS controller
	c, err := controller.NewXdsController(
		ctx,
		m,
		initWg,
		r.GetEventChan(),
		logger,
	)
	if err != nil {
		return err
	}

	err = c.SetupWithManager(m)
	if err != nil {
		return err
	}

	return m.Start(ctx)
}
