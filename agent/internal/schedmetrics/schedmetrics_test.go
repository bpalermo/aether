package schedmetrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aethermesh.dev/common/procsched"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func writeThread(t *testing.T, root, tid, schedstat, status, sched string) {
	t.Helper()
	dir := filepath.Join(root, "7", "task", tid)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "schedstat"), []byte(schedstat), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644))
	if sched != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sched"), []byte(sched), 0o644))
	}
}

// collect returns every data point by "<name>[/<switch>]".
func collect(t *testing.T, r *sdkmetric.ManualReader) map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, r.Collect(context.Background(), &rm))
	out := map[string]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					key := m.Name
					if v, ok := dp.Attributes.Value(attrSwitch); ok {
						key += "/" + v.AsString()
					}
					out[key] = float64(dp.Value)
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = dp.Value
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = float64(dp.Value)
				}
			}
		}
	}
	return out
}

func TestRegisterExportsSchedulerCounters(t *testing.T) {
	root := t.TempDir()
	writeThread(t, root, "7", "2000000000 500000000 40\n",
		"voluntary_ctxt_switches:\t30\nnonvoluntary_ctxt_switches:\t4\n",
		"se.nr_migrations                             :                   9\n")
	writeThread(t, root, "8", "1000000000 250000000 20\n",
		"voluntary_ctxt_switches:\t15\nnonvoluntary_ctxt_switches:\t1\n", "")

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	reg, err := Register(provider.Meter(MeterName), procsched.Reader{Root: root, PID: "7"})
	require.NoError(t, err)
	defer func() { require.NoError(t, reg.Unregister()) }()

	got := collect(t, reader)
	assert.Equal(t, map[string]float64{
		"aether.agent.sched.context_switches/voluntary":   45,
		"aether.agent.sched.context_switches/involuntary": 5,
		"aether.agent.sched.timeslices":                   60,
		"aether.agent.sched.migrations":                   9,
		"aether.agent.sched.run_delay":                    0.75,
		"aether.agent.sched.cpu_time":                     3,
		"aether.agent.sched.threads":                      2,
	}, got)

	// Thread 8 exits: its counts stay in the totals; the thread gauge drops.
	require.NoError(t, os.RemoveAll(filepath.Join(root, "7", "task", "8")))
	got = collect(t, reader)
	assert.InDelta(t, 60, got["aether.agent.sched.timeslices"], 0)
	assert.InDelta(t, 1, got["aether.agent.sched.threads"], 0)
}

// TestRegisterWithoutProcfsReportsNothing checks an unreadable procfs leaves the
// series absent rather than failing the collection or reporting zeros.
func TestRegisterWithoutProcfsReportsNothing(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	_, err := Register(provider.Meter(MeterName), procsched.Reader{Root: t.TempDir(), PID: "7"})
	require.NoError(t, err)
	assert.Empty(t, collect(t, reader))
}
