package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestOTLPFlagsRejected pins the end of the #1166 compatibility window: the
// plugin exports no telemetry, the chart has passed cni-install no OTLP flag
// since 2.4.0, and the two flags that were kept as deprecated no-ops for one
// release are gone. Cobra must refuse them by name, before the installer runs:
// the source and target directories do not exist, so an error that is not
// "unknown flag" means the run got past flag parsing.
func TestOTLPFlagsRejected(t *testing.T) {
	for _, arg := range []string{
		"--otlp-endpoint=otel-collector.example.com:4317",
		"--otlp-pin-endpoint=false",
	} {
		t.Run(arg, func(t *testing.T) {
			cmd := GetCommand()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{arg, "--cni-bin-dir", "/nonexistent/src", "--cni-bin-target-dir", "/nonexistent/dst"})
			t.Cleanup(func() {
				// The command is a package global: leave it as other tests expect it.
				cmd.SetOut(nil)
				cmd.SetErr(nil)
				cmd.SetArgs(nil)
				for _, name := range []string{"cni-bin-dir", "cni-bin-target-dir"} {
					if f := cmd.Flags().Lookup(name); f != nil {
						_ = f.Value.Set(f.DefValue)
						f.Changed = false
					}
				}
			})

			err := cmd.Execute()
			require.Error(t, err)
			name, _, _ := strings.Cut(arg, "=")
			assert.Equal(t, "unknown flag: "+name, err.Error())
			assert.Nil(t, cmd.Flags().Lookup(strings.TrimPrefix(name, "--")),
				"flag %s was retired and must not be re-registered", name)
		})
	}
}
