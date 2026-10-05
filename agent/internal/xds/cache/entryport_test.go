package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEntryPort pins the port parse every capture path shares (#1220): a value
// outside 1..65535 is rejected, never narrowed. The wrap rows are the CodeQL
// go/incorrect-integer-conversion shape: Atoi then uint32() turns 2^32+80 into
// 80, a VALID port that is not the one registered.
func TestEntryPort(t *testing.T) {
	tests := []struct {
		in     string
		want   uint32
		wantOK bool
	}{
		{"80", 80, true},
		{"1", 1, true},
		{"65535", 65535, true},    // max valid
		{"65536", 0, false},       // max+1
		{"0", 0, false},           // not a port
		{"-1", 0, false},          // negative
		{"-4294967216", 0, false}, // negative that wraps to 80 through uint32()
		{"4294967376", 0, false},  // 2^32+80: wraps to 80 through uint32()
		{"4295032831", 0, false},  // 2^32+65535
		{"", 0, false},
		{"+80", 0, false},
		{" 80", 0, false},
		{"eighty", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := entryPort(tt.in)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPrimaryPortOf_RejectsOutOfRange(t *testing.T) {
	assert.Equal(t, uint32(8080), primaryPortOf(clusterEntry{sni: "8080"}))
	assert.Zero(t, primaryPortOf(clusterEntry{sni: "4294975376"}), "2^32+8080 must not alias port 8080")
	assert.Zero(t, primaryPortOf(clusterEntry{sni: "65536"}))
	assert.Zero(t, primaryPortOf(clusterEntry{}))
}

// TestUDPClusterForLocked_OutOfRangePortSkipped corrupts a UDP entry's sni to a
// value that wraps to a valid port through uint32() and asserts the UDP cluster
// is skipped rather than published against the wrapped port.
func TestUDPClusterForLocked_OutOfRangePortSkipped(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name:             "udpwrap-0",
		Namespace:        "aether-test",
		ServiceAccount:   "udpwrap",
		NetworkNamespace: "/var/run/netns/cni-udpwrap",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/udpwrap")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1",
		udpOnlyRegistry("aether-test/udpwrap", "10.0.0.42", 5353)))

	key := proxy.UDPClusterName("aether-test/udpwrap", c.meshDomain)
	c.clusterMu.Lock()
	entry, ok := c.clusters[key]
	require.True(t, ok)
	entry.sni = "4294972649" // 2^32+5353
	c.clusters[key] = entry
	name := c.udpClusterForLocked("aether-test/udpwrap")
	c.clusterMu.Unlock()

	assert.Empty(t, name, "an out-of-range port must not name a UDP cluster")
}
