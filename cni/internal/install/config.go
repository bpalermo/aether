// Package install provides configuration and utilities for the CNI plugin installer.
// The cni-install binary is an init container that copies the Aether CNI plugin binary
// and configuration files to the host filesystem before the main agent starts.
package install

import "github.com/bpalermo/aether/cni/internal/constants"

// InstallerConfig holds configuration for the CNI plugin installer.
// It specifies where to find the plugin binary and config files in the container,
// and where to install them on the host filesystem.
type InstallerConfig struct {
	// Debug enables debug-level logging
	Debug bool
	// MountedCNINetDir is the location of the CNI config directory in the container
	// (typically the mount point for the host's /etc/cni/net.d)
	MountedCNINetDir string
	// CNIConfName is the name of the CNI network configuration file (e.g., "aether.conflist")
	CNIConfName string
	// CNIBinSourceDir is the directory inside the container where the CNI plugin binary is located
	// (typically /app/bin or similar)
	CNIBinSourceDir string
	// CNIBinTargetDir is the directory on the host where the CNI plugin binary should be copied
	// (typically /opt/cni/bin)
	CNIBinTargetDir string
	// OTLPEndpoint, when set, is written into the generated netconf so the CNI
	// plugin binary pushes traces and metrics to this OTLP gRPC collector.
	// Empty leaves plugin telemetry disabled.
	OTLPEndpoint string
}

// NewInstallerConfig creates a new InstallerConfig with default values.
// The defaults use standard Kubernetes CNI directory paths and the package constants.
func NewInstallerConfig() *InstallerConfig {
	return &InstallerConfig{
		CNIBinSourceDir:  constants.DefaultCNIBinDir,
		CNIBinTargetDir:  constants.DefaultHostCNIBinDir,
		MountedCNINetDir: constants.DefaultHostCNINetDir,
	}
}
