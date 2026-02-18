package cmd

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/internal/awsconfig"
	cniServer "github.com/bpalermo/aether/agent/internal/cni/server"
	xdsServer "github.com/bpalermo/aether/agent/internal/xds/server"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/install"
	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/log"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// name is the controller name used for logging
	name = "aether-agent"
)

var (
	cfg = NewAgentConfig()

	l logr.Logger
)

var rootCmd = &cobra.Command{
	Use:          "agent",
	Short:        "Runs the aether node agent.",
	SilenceUsage: true,
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		l = log.NewLogger(cfg.Debug).WithName(cmd.Name())
		ctrl.SetLogger(l)
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
	rootCmd.Flags().StringVar(&cfg.NodeName, "node-name", constants.DefaultProxyID, "The xDS proxy ID (service-node)")
	rootCmd.Flags().StringVar(&cfg.ClusterName, "cluster-name", constants.DefaultProxyID, "The xDS proxy ID (service-node)")
	rootCmd.Flags().StringVar(&cfg.ProxyServiceNodeID, "proxy-id", constants.DefaultProxyID, "The xDS proxy ID (service-node)")
	rootCmd.Flags().StringVar(&cfg.InstallConfig.CNIBinSourceDir, "cni-bin-dir", constants.DefaultCNIBinDir, "Directory from where the CNI binaries should be copied")
	rootCmd.Flags().StringVar(&cfg.InstallConfig.CNIBinTargetDir, "cni-bin-target-dir", constants.DefaultHostCNIBinDir, "Directory into which to copy the CNI binaries")
	rootCmd.Flags().StringVar(&cfg.InstallConfig.MountedCNINetDir, "mounted-cni-net-dir", constants.DefaultHostCNINetDir, "Directory where CNI network configuration files are located")
	rootCmd.Flags().StringVar(&cfg.MountedLocalStorageDir, "mounted-registry-dir", constants.DefaultHostCNIRegistryDir, "Directory where CNI registry entries are located")

	_ = rootCmd.MarkPersistentFlagRequired("cluster-name")
	_ = rootCmd.MarkPersistentFlagRequired("node-name")
	_ = rootCmd.MarkPersistentFlagRequired("proxy-id")
	_ = rootCmd.MarkPersistentFlagRequired("proxy-region")
	_ = rootCmd.MarkPersistentFlagRequired("proxy-zone")
}

func runAgent(ctx context.Context) error {
	l.Info("starting aether agent",
		"proxy-id", cfg.ProxyServiceNodeID,
		"debug", cfg.Debug,
		"clusterName", cfg.ClusterName,
	)

	// Install CNI binaries
	if err := installCNI(ctx); err != nil {
		return err
	}

	// Create a controller manager
	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})
	if err != nil {
		return err
	}

	localStorage, err := setupStorage(ctx, cfg.MountedLocalStorageDir)
	if err != nil {
		return err
	}

	ddbRegistry, err := setupRegistry(ctx, m)
	if err != nil {
		return err
	}

	if err = setXDSServer(ctx, m, ddbRegistry, localStorage); err != nil {
		return err
	}

	if err = setupCNIServer(m, localStorage, ddbRegistry); err != nil {
		return err
	}

	l.V(1).Info("waiting for local storage to be ready")
	if err = localStorage.WaitUntilReady(ctx); err != nil {
		return err
	}

	l.V(1).Info("local storage is ready, starting manager")
	return m.Start(ctx)
}

func installCNI(ctx context.Context) error {
	l.Info("installing CNI binaries")
	installer := install.NewInstaller(l, cfg.InstallConfig)
	return installer.Run(ctx)
}

func setXDSServer(ctx context.Context, m ctrl.Manager, registry registry.Registry, localStorage storage.Storage[*cniv1.CNIPod]) error {
	// Create xDS server
	xdsSrv, err := xdsServer.NewXdsServer(ctx, cfg.ClusterName, cfg.ProxyServiceNodeID, registry, localStorage, l)
	if err != nil {
		return err
	}
	if err = m.Add(xdsSrv); err != nil {
		return fmt.Errorf("failed to add xDS server: %w", err)
	}

	return nil
}

func setupCNIServer(m ctrl.Manager, localStorage storage.Storage[*cniv1.CNIPod], registry registry.Registry) error {
	// Create a registry and CNI server
	cniSrv, err := cniServer.NewCNIServer(
		cfg.ClusterName,
		cfg.NodeName,
		cfg.ProxyServiceNodeID,
		localStorage,
		registry,
		l,
		m.GetClient(),
		cfg.CNIServerConfig,
	)
	if err != nil {
		return err
	}
	if err = m.Add(cniSrv); err != nil {
		return fmt.Errorf("failed to add CNI server: %w", err)
	}

	return nil
}

func setupStorage(ctx context.Context, path string) (storage.Storage[*cniv1.CNIPod], error) {
	s := storage.NewCachedLocalStorage[*cniv1.CNIPod](
		path,
		func() *cniv1.CNIPod { return &cniv1.CNIPod{} },
	)

	if err := s.Initialize(ctx); err != nil {
		return nil, err
	}

	return s, nil
}

func setupRegistry(ctx context.Context, m ctrl.Manager) (registry.Registry, error) {
	awsCfg, err := awsconfig.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}

	reg := registry.NewDynamoDBRegistry(l, awsCfg)
	if err = m.Add(reg); err != nil {
		return nil, err
	}

	return reg, nil
}
