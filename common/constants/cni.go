package constants

const (
	// CNIDefaultRegistryPath is the default directory where CNI plugin data is stored
	CNIDefaultRegistryPath = "/var/lib/aether/registry"

	// CNIDefaultNetDir is the host directory holding the CNI network configs
	// kubelet reads. Aether chains itself into the primary CNI's conflist there
	// (cni-install at pod start, the agent's re-assert loop thereafter, #645).
	CNIDefaultNetDir = "/etc/cni/net.d"
)
