package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runReadinessCheck exercises the mesh-dns RunE in --readiness-check mode against
// the given marker path, returning the error (nil == exit 0). It drives RunE
// directly (not rootCmd.Execute) so it exercises only the probe branch, without
// the agent root command's required node/cluster flags.
func runReadinessCheck(t *testing.T, marker string) error {
	t.Helper()
	meshDnsReadinessCheck = true
	meshDnsReadyMarker = marker
	t.Cleanup(func() {
		meshDnsReadinessCheck = false
		meshDnsReadyMarker = ""
	})
	return meshDnsCmd.RunE(meshDnsCmd, nil)
}

// TestMeshDNSReadinessCheckMarkerPresent: the exec probe exits 0 when the marker exists.
func TestMeshDNSReadinessCheckMarkerPresent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "mesh-dns.ready")
	require.NoError(t, os.WriteFile(marker, nil, 0o644))

	assert.NoError(t, runReadinessCheck(t, marker), "marker present -> exit 0")
}

// TestMeshDNSReadinessCheckMarkerAbsent: the exec probe exits non-zero (returns an
// error) when the marker is absent, so k8s holds the pod NotReady.
func TestMeshDNSReadinessCheckMarkerAbsent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "does-not-exist.ready")

	assert.Error(t, runReadinessCheck(t, marker), "marker absent -> non-zero")
}
