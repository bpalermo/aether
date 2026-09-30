package udscsi

import (
	"testing"

	"aethermesh.dev/common/udspath"
	"github.com/stretchr/testify/assert"
)

// TestCarrierContract pins the two facts the node plugin and the agent's
// resolver must agree on (proposal 039 Phase 2): the driver name a pod's volume
// declares, and the host directory the per-pod tmpfs lives under — which the
// agent's --uds-csi-root defaults to and the proxy dials beneath. The plugin
// cannot import udspath (its deps_test keeps it slim), so the test does.
func TestCarrierContract(t *testing.T) {
	assert.Equal(t, udspath.CSIDriver, DriverName)
	assert.Equal(t, udspath.DefaultCSIRoot, DefaultRoot)
}
