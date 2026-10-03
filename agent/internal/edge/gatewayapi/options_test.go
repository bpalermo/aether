package gatewayapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEdgeControllerOptions checks the edge controller keeps both of its
// options: it runs on every replica, and its queue keeps no metrics (#1131).
func TestEdgeControllerOptions(t *testing.T) {
	o := edgeControllerOptions()
	require.NotNil(t, o.NeedLeaderElection)
	assert.False(t, *o.NeedLeaderElection)
	assert.NotNil(t, o.NewQueue, "the edge controller must use ctrlqueue.NewQueue")
}
