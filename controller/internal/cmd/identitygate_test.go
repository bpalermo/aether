package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// TestIdentityGate_OffByDefaultInTheBinary: the binary's default is off (it
// needs an image to inject); the chart turns it on.
func TestIdentityGate_OffByDefaultInTheBinary(t *testing.T) {
	g, err := identityGate(NewControllerConfig())
	require.NoError(t, err)
	assert.Nil(t, g)
}

// TestIdentityGate_FromDefaults: enabled with just an image, the gate carries
// the default socket, pull policy and tiny resources, and no timeout.
func TestIdentityGate_FromDefaults(t *testing.T) {
	c := NewControllerConfig()
	c.IdentityGate = true
	c.IdentityGateImage = "registry.example/agent@sha256:abc"
	g, err := identityGate(c)
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, DefaultSpireWorkloadSocketPath, g.WorkloadSocket)
	assert.Equal(t, corev1.PullIfNotPresent, g.PullPolicy)
	assert.Zero(t, g.Timeout)
	assert.Equal(t, "5m", g.Resources.Requests.Cpu().String())
	assert.Equal(t, "16Mi", g.Resources.Requests.Memory().String())
	assert.Equal(t, "64Mi", g.Resources.Limits.Memory().String())
	_, hasCPULimit := g.Resources.Limits[corev1.ResourceCPU]
	assert.False(t, hasCPULimit, "an empty value leaves the entry unset")
}

// TestIdentityGate_RejectsBadConfig fails startup, not admission.
func TestIdentityGate_RejectsBadConfig(t *testing.T) {
	for name, mut := range map[string]func(*ControllerConfig){
		"no image":     func(c *ControllerConfig) { c.IdentityGateImage = "" },
		"bad pull":     func(c *ControllerConfig) { c.IdentityGateImagePullPolicy = "Sometimes" },
		"bad quantity": func(c *ControllerConfig) { c.IdentityGateMemoryLimit = "lots" },
		"bad timeout":  func(c *ControllerConfig) { c.IdentityGateTimeout = -time.Second },
	} {
		c := NewControllerConfig()
		c.IdentityGate = true
		c.IdentityGateImage = "img"
		mut(c)
		_, err := identityGate(c)
		assert.Error(t, err, name)
	}
}

// TestIdentityGate_FlagsRegistered pins the flag names the chart renders.
func TestIdentityGate_FlagsRegistered(t *testing.T) {
	for _, f := range []string{
		"identity-gate", "identity-gate-image", "identity-gate-image-pull-policy",
		"identity-gate-workload-socket", "identity-gate-timeout",
		"identity-gate-cpu-request", "identity-gate-memory-request",
		"identity-gate-cpu-limit", "identity-gate-memory-limit",
	} {
		assert.NotNil(t, GetCommand().Flags().Lookup(f), "--%s", f)
	}
}
