package cmd

import (
	"sync"

	"github.com/bpalermo/aether/agent/internal/log"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/controller"
	"github.com/bpalermo/aether/agent/pkg/install"
	"github.com/bpalermo/aether/agent/pkg/watcher"
	"github.com/bpalermo/aether/agent/pkg/xds/server"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	// name is the controller name used for logging
	name = "aether-agent"
)

var (
	cfg = &install.InstallerConfig{}

	logger logr.Logger
)

var rootCmd = &cobra.Command{
	Use:          "agent",
	Short:        "Runs the aether node agent.",
	SilenceUsage: true,
	PersistentPreRun: func(_ *cobra.Command, _ []string) {
		opts := log.DefaultOptions()
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
	rootCmd.Flags().StringVar(&cfg.ProxyServiceNodeID, "proxy-id", constants.DefaultProxyID, "The xDS proxy ID (service-node)")
	rootCmd.Flags().StringVar(&cfg.CNIBinSourceDir, "cni-bin-dir", constants.DefaultCNIBinDir, "Directory from where the CNI binaries should be copied")
	rootCmd.Flags().StringVar(&cfg.CNIBinTargetDir, "cni-bin-target-dir", constants.DefaultHostCNIBinDir, "Directory into which to copy the CNI binaries")
	rootCmd.Flags().StringVar(&cfg.MountedCNINetDir, "mounted-cni-net-dir", constants.DefaultHostCNINetDir, "Directory where CNI network configuration files are located")
	rootCmd.Flags().StringVar(&cfg.MountedCNIRegistryDir, "mounted-registry-dir", constants.DefaultHostCNIRegistryDir, "Directory where CNI registry entries are located")

	_ = rootCmd.MarkPersistentFlagRequired("node-id")
}

func runAgent() error {
	var ctx = ctrl.SetupSignalHandler()

	logger.Info("starting aether agent", "proxy-id", cfg.ProxyServiceNodeID)

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
	registry := server.NewXdsRegistry(cfg.ProxyServiceNodeID, logger)
	if err = m.Add(registry); err != nil {
		return err
	}

	// Create xDS server
	srv := server.NewXdsServer(
		m,
		logger,
		initWg,
		registry,
	)
	if err = m.Add(srv); err != nil {
		return err
	}

	// Create the registry watcher
	registryWatcher, err := watcher.NewCNIWatcher(
		cfg.MountedCNIRegistryDir,
		initWg,
		srv.GetRegistryEventChan(),
		logger,
	)
	if err != nil {
		return err
	}

	if err = m.Add(registryWatcher); err != nil {
		return err
	}

	// Create the XDS controller
	c, err := controller.NewXdsController(
		ctx,
		m,
		initWg,
		srv.GetRegistryEventChan(),
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
