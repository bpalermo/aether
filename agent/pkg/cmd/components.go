package cmd

import (
	cniServer "github.com/bpalermo/aether/agent/pkg/cni/server"
	xdsServer "github.com/bpalermo/aether/agent/pkg/xds/server"
	"github.com/bpalermo/aether/agent/pkg/xds/snapshot"
)

type agentComponents struct {
	xdsSnapshot *snapshot.XdsSnapshot
	xdsServer   *xdsServer.XdsServer
	cniServer   *cniServer.CNIServer
}
