package constants

import cniConstants "github.com/bpalermo/aether/cni/pkg/constants"

const (
	DefaultProxyID = "proxy"

	DefaultCNIBinDir          = "/opt/cni/bin"
	DefaultHostCNIBinDir      = "/host/opt/cni/bin"
	DefaultHostCNINetDir      = "/host/etc/cni/net.d"
	DefaultHostCNIRegistryDir = "/host" + cniConstants.DefaultRegistryPath

	DefaultXdsSocketPath = "/run/aether/xds.sock"
)
