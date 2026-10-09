package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aethermesh.dev/common/telemetry/setup"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
)

// TestTelemetryResourceHostName pins which of this binary's two commands puts
// host.name on its telemetry resource (#1596).
//
// The node agent is hostNetwork: the hostname the OTel host detector reads is the
// node's, and it stays. `agent edge` is a Deployment on the pod network, so there
// the hostname is the POD's name, and a collector that derives a node label from
// host.name ahead of k8s.node.name labels every edge control plane series with a
// pod (#1041, where the prober had the same resource). The edge's pod is already
// named by k8s.pod.name, which the chart sets, so nothing is lost.
//
// Both commands share one cfg and one PersistentPreRunE, which is where the logger
// is built, so the test runs that hook for each command and reads the resource the
// command's providers are then built from.
func TestTelemetryResourceHostName(t *testing.T) {
	hostname, err := os.Hostname()
	require.NoError(t, err)

	for _, tc := range []struct {
		name     string
		cmd      *cobra.Command
		service  string
		wantHost bool
	}{
		{name: "the node agent is hostNetwork and keeps host.name", cmd: rootCmd, service: name, wantHost: true},
		{name: "the edge control plane is on the pod network and has none", cmd: edgeCmd, service: edgeName, wantHost: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "k8s.node.name=n1,k8s.pod.name=p1")
			t.Setenv("OTEL_SERVICE_NAME", "")

			// The hook writes the package's cfg and logger; put both back.
			savedCfg, savedLogger, savedShutdown := *cfg, l, logShutdown
			t.Cleanup(func() { *cfg, l, logShutdown = savedCfg, savedLogger, savedShutdown })
			// No MeshConfig file and no OTLP log export: the hook then needs no
			// cluster and no collector.
			cfg.MeshConfigPath = filepath.Join(t.TempDir(), "absent.yaml")
			cfg.LogsEnabled = false

			require.NoError(t, rootCmd.PersistentPreRunE(tc.cmd, nil))

			res, err := setup.NewResource(context.Background(), cfg.Telemetry(tc.service, Version))
			require.NoError(t, err)

			host, has := res.Set().Value("host.name")
			if tc.wantHost {
				assert.Equal(t, hostname, host.AsString(), "host.name is this host's name")
			} else {
				assert.False(t, has, "host.name = %q: on the pod network that is the pod name, not a host", host.AsString())
			}
			for _, key := range []attribute.Key{"k8s.node.name", "k8s.pod.name"} {
				assert.True(t, res.Set().HasValue(key), "%s comes from the chart and is kept", key)
			}
		})
	}
}
