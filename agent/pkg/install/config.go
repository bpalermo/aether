package install

import "github.com/bpalermo/aether/agent/pkg/constants"

type InstallerConfig struct {
	// Location of the CNI config files in the container's filesystem (mount location of the CNINetDir)
	MountedCNINetDir string
	// Name of the CNI config file
	CNIConfName string
	// Directory from where the CNI binaries should be copied
	CNIBinSourceDir string
	// Directory into which to copy the CNI binaries
	CNIBinTargetDir string
	// Directory into which to registry entries are created
	MountedCNIRegistryDir string
}

func NewInstallerConfig() *InstallerConfig {
	return &InstallerConfig{
		CNIBinSourceDir:       constants.DefaultCNIBinDir,
		CNIBinTargetDir:       constants.DefaultHostCNIBinDir,
		MountedCNINetDir:      constants.DefaultHostCNINetDir,
		MountedCNIRegistryDir: constants.DefaultHostCNIRegistryDir,
	}
}
