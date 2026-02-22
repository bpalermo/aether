package constants

import "github.com/bpalermo/aether/constants"

const (
	DefaultProxyID = "proxy"

	DefaultHostCNIRegistryDir = "/host" + constants.CNIDefaultRegistryPath

	DefaultXdsSocketPath = "/run/aether/xds.sock"
	DefaultCNISocketPath = "/run/aether/cni.sock"
)
