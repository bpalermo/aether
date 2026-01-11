package install

type InstallerConfig struct {
	// Debug
	Debug bool
	// ClusterName the cluster where the agent is running in
	ClusterName string
	// XDS nodeID
	ProxyServiceNodeID string
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
