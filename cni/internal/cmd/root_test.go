package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRetiredFlagsGone pins proposal 031 (round 2): the per-pod capture
// redirect is unconditional for managed pods — the netconf gate and the flag
// that wrote it must not come back. Per-pod capture.aether.io/* annotations
// remain the opt-out.
func TestRetiredFlagsGone(t *testing.T) {
	cmd := GetCommand()
	for _, name := range []string{"transparent-capture"} {
		assert.Nil(t, cmd.Flags().Lookup(name), "flag --%s was retired and must not be re-registered", name)
	}
}

// TestOTLPFlagsDeprecatedNoOps pins #1166: the plugin no longer exports
// telemetry, so cni-install writes no otlp_endpoint. The two flags a pre-#1166
// chart passes must still parse (deprecated, ignored) for one release, or that
// chart's agent pods fail in their init container.
func TestOTLPFlagsDeprecatedNoOps(t *testing.T) {
	cmd := GetCommand()
	for _, name := range []string{"otlp-endpoint", "otlp-pin-endpoint"} {
		flag := cmd.Flags().Lookup(name)
		if assert.NotNil(t, flag, "--%s must stay parseable for one release", name) {
			assert.NotEmpty(t, flag.Deprecated, "--%s must be marked deprecated", name)
		}
	}
	assert.NoError(t, cmd.Flags().Parse([]string{
		"--otlp-endpoint=otel-collector.o11y.svc.cluster.local:4317",
		"--otlp-pin-endpoint=false",
	}))
}
