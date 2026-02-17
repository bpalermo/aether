package constants

import "github.com/bpalermo/aether/constants"

const (
	DefaultProxyID         = "proxy"
	DefaultProxyRegionName = "unknown"
	DefaultProxyZoneName   = "unknown"
	DefaultClusterName     = "unknown"

	DefaultCNIBinDir          = "/opt/cni/bin"
	DefaultHostCNIBinDir      = "/host/opt/cni/bin"
	DefaultHostCNINetDir      = "/host/etc/cni/net.d"
	DefaultHostCNIRegistryDir = "/host" + constants.CNIDefaultRegistryPath

	DefaultXdsSocketPath = "/run/aether/xds.sock"
	DefaultCNISocketPath = "/run/aether/cni.sock"
)
