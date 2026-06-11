package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRegistrarConfig(t *testing.T) {
	c := NewRegistrarConfig()

	require.NotNil(t, c)
}

func TestRegistrarConfig_DefaultValues(t *testing.T) {
	c := NewRegistrarConfig()

	tests := []struct {
		name     string
		got      interface{}
		expected interface{}
	}{
		{
			name:     "registry backend defaults to kubernetes",
			got:      c.RegistryBackend,
			expected: "kubernetes",
		},
		{
			name:     "gRPC address defaults to :8443",
			got:      c.GRPCAddress,
			expected: ":8443",
		},
		{
			name:     "sync interval defaults to 5s",
			got:      c.SyncInterval,
			expected: 5 * time.Second,
		},
		{
			name:     "etcd endpoints defaults to localhost:2379",
			got:      c.EtcdEndpoints,
			expected: []string{"localhost:2379"},
		},
		{
			name:     "debug is false by default",
			got:      c.Debug,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.got)
		})
	}
}

func TestRegistrarConfig_ConfigurableFields(t *testing.T) {
	c := NewRegistrarConfig()

	c.Debug = true
	c.ClusterName = "my-cluster"
	c.RegistryBackend = "etcd"
	c.GRPCAddress = ":8443"
	c.SyncInterval = 30 * time.Second
	c.EtcdEndpoints = []string{"etcd-0:2379", "etcd-1:2379"}

	assert.True(t, c.Debug)
	assert.Equal(t, "my-cluster", c.ClusterName)
	assert.Equal(t, "etcd", c.RegistryBackend)
	assert.Equal(t, ":8443", c.GRPCAddress)
	assert.Equal(t, 30*time.Second, c.SyncInterval)
	assert.Equal(t, []string{"etcd-0:2379", "etcd-1:2379"}, c.EtcdEndpoints)
}

func TestRegistrarConfig_InstancesAreIndependent(t *testing.T) {
	cfg1 := NewRegistrarConfig()
	cfg2 := NewRegistrarConfig()

	cfg1.RegistryBackend = "dynamodb"

	assert.Equal(t, "dynamodb", cfg1.RegistryBackend)
	assert.Equal(t, "kubernetes", cfg2.RegistryBackend)
}
