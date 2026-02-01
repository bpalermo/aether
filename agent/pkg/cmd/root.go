package cmd

import (
	"context"
	"fmt"
	"sync"

	cniServer "github.com/bpalermo/aether/agent/pkg/cni/server"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/controller"
	"github.com/bpalermo/aether/agent/pkg/install"
	xdsServer "github.com/bpalermo/aether/agent/pkg/xds/server"
	"github.com/bpalermo/aether/agent/pkg/xds/snapshot"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/log"
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
	cfg    = NewAgentConfig()
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
	RunE: func(cmd *cobra.Command, _ []string) (err error) {
		return runAgent(cmd.Context())
	},
}

// GetCommand returns the main cobra.Command object for this application
func GetCommand() *cobra.Command {
	return rootCmd
}

func init() {
	rootCmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug mode")
	rootCmd.Flags().StringVar(&cfg.ProxyServiceNodeID, "proxy-id", constants.DefaultProxyID, "The xDS proxy ID (service-node)")
	rootCmd.Flags().StringVar(&cfg.InstallConfig.CNIBinSourceDir, "cni-bin-dir", constants.DefaultCNIBinDir, "Directory from where the CNI binaries should be copied")
	rootCmd.Flags().StringVar(&cfg.InstallConfig.CNIBinTargetDir, "cni-bin-target-dir", constants.DefaultHostCNIBinDir, "Directory into which to copy the CNI binaries")
	rootCmd.Flags().StringVar(&cfg.InstallConfig.MountedCNINetDir, "mounted-cni-net-dir", constants.DefaultHostCNINetDir, "Directory where CNI network configuration files are located")
	rootCmd.Flags().StringVar(&cfg.InstallConfig.MountedCNIRegistryDir, "mounted-registry-dir", constants.DefaultHostCNIRegistryDir, "Directory where CNI registry entries are located")

	_ = rootCmd.MarkPersistentFlagRequired("cluster")
	_ = rootCmd.MarkPersistentFlagRequired("proxy-id")
}

func runAgent(ctx context.Context) error {
	logger.Info("starting aether agent",
		"proxy-id", cfg.ProxyServiceNodeID,
		"debug", cfg.Debug)

	// Install CNI binaries
	if err := installCNI(ctx); err != nil {
		return err
	}

	// Create a controller manager
	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})
	if err != nil {
		return err
	}

	// Setup all components
	initWg := &sync.WaitGroup{}
	initWg.Add(2)

	components, err := setupComponents(m, initWg)
	if err != nil {
		return err
	}

	// Add components to the manager
	if err = addComponentsToManager(m, components); err != nil {
		return err
	}

	// Setup XDS controller
	if err = setupXDSController(ctx, m, initWg, components.xdsSnapshot.GetEventChan()); err != nil {
		return err
	}

	return m.Start(ctx)
}

func installCNI(ctx context.Context) error {
	logger.Info("installing CNI binaries")
	installer := install.NewInstaller(logger, cfg.InstallConfig)
	return installer.Run(ctx)
}

type agentComponents struct {
	xdsSnapshot *snapshot.XdsSnapshot
	xdsServer   *xdsServer.XdsServer
	cniServer   *cniServer.CNIServer
}

func setupComponents(m ctrl.Manager, initWg *sync.WaitGroup) (*agentComponents, error) {
	// Create xDS snapshot
	xdsSnapshot := snapshot.NewXdsSnapshot(cfg.ProxyServiceNodeID, logger)

	// Create xDS server
	xdsSrv := xdsServer.NewXdsServer(m, logger, initWg, xdsSnapshot)

	// Create a registry and CNI server
	cniSrv := cniServer.NewCNIServer(
		logger,
		m.GetClient(),
		cfg.CNIServerConfig,
		initWg,
		xdsSnapshot.GetEventChan(),
	)

	return &agentComponents{
		xdsSnapshot: xdsSnapshot,
		xdsServer:   xdsSrv,
		cniServer:   cniSrv,
	}, nil
}

func addComponentsToManager(m ctrl.Manager, components *agentComponents) error {
	if err := m.Add(components.xdsSnapshot); err != nil {
		return fmt.Errorf("failed to add xDS snapshot: %w", err)
	}

	if err := m.Add(components.xdsServer); err != nil {
		return fmt.Errorf("failed to add xDS server: %w", err)
	}

	if err := m.Add(components.cniServer); err != nil {
		return fmt.Errorf("failed to add CNI server: %w", err)
	}

	return nil
}

func setupXDSController(ctx context.Context, m ctrl.Manager, initWg *sync.WaitGroup, eventChan chan<- *registryv1.Event) error {
	c, err := controller.NewXdsController(ctx, m, initWg, eventChan, logger)
	if err != nil {
		return fmt.Errorf("failed to create XDS controller: %w", err)
	}

	if err := c.SetupWithManager(m); err != nil {
		return fmt.Errorf("failed to setup XDS controller: %w", err)
	}

	return nil
}
