package hotrestart

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// TestAdminReverifyIntervalDerivation pins the derivation itself: the interval
// is a third of the tighter of the two bounds, clamped into [floor, ceiling].
func TestAdminReverifyIntervalDerivation(t *testing.T) {
	tests := []struct {
		name    string
		pst     time.Duration
		aud     time.Duration
		want    time.Duration
		clamped bool
	}{
		{name: "deployed chart values", pst: 15 * time.Second, aud: 30 * time.Second, want: 5 * time.Second},
		{name: "cli defaults", pst: 60 * time.Second, aud: 30 * time.Second, want: 10 * time.Second},
		{name: "unset parent shutdown is not bounding", pst: 0, aud: 30 * time.Second, want: 10 * time.Second},
		{name: "floor", pst: 5 * time.Second, aud: 30 * time.Second, want: 2 * time.Second, clamped: true},
		{name: "ceiling", pst: 300 * time.Second, aud: 300 * time.Second, want: 15 * time.Second, clamped: true},
		{name: "floor wins over a tiny budget", pst: time.Second, aud: time.Second, want: 2 * time.Second, clamped: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := adminReverifyIntervalFor(tc.pst, tc.aud)
			assert.Equal(t, tc.want, got)
			if tc.clamped {
				return
			}
			// Unclamped, the whole point of the divisor holds: the derived
			// interval fits adminReverifyDivisor times inside the budget.
			budget := tc.aud
			if tc.pst > 0 && tc.pst < budget {
				budget = tc.pst
			}
			assert.LessOrEqual(t, got*adminReverifyDivisor, budget,
				"the derived interval must fit %d times inside the budget", adminReverifyDivisor)
		})
	}
}

// TestCheckAdminReverifyMarginFlagsTooShortParentShutdown covers the residual
// case the floor introduces. It is reported, never fatal — Run only logs it.
func TestCheckAdminReverifyMarginFlagsTooShortParentShutdown(t *testing.T) {
	tooShort := New(Config{ParentShutdownTime: time.Second}, slog.New(slog.DiscardHandler), nil)
	err := tooShort.checkAdminReverifyMargin()
	require.Error(t, err, "a 1s parent-shutdown-time cannot hold three 2s re-verifications")
	assert.Contains(t, err.Error(), "2s", "the error must name the derived interval")
	assert.Contains(t, err.Error(), "1s", "the error must name the configured parent-shutdown-time")
	assert.Contains(t, err.Error(), "6s", "the error must name the parent-shutdown-time that restores the margin")

	deployed := New(Config{ParentShutdownTime: 15 * time.Second}, slog.New(slog.DiscardHandler), nil)
	assert.NoError(t, deployed.checkAdminReverifyMargin(), "the deployed configuration must hold the margin")
}

// TestAdminReverifyMarginHoldsAgainstDeployedChartValues is the pin that #666
// was missing: the derivation is only correct against the values we actually
// ship, and those live in the chart, not in this package. Editing
// charts/aether/values.yaml into a shape where the epoch-identity probe can no
// longer land inside the draining parent's window fails here.
func TestAdminReverifyMarginHoldsAgainstDeployedChartValues(t *testing.T) {
	const chartValues = "charts/aether/values.yaml"
	path := findRepoFile(t, chartValues)

	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)

	var values struct {
		Proxy struct {
			HotRestart map[string]any `json:"hotRestart"`
		} `json:"proxy"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &values), "decoding %s", path)
	hr := values.Proxy.HotRestart
	require.NotEmpty(t, hr, "%s has no proxy.hotRestart block", chartValues)

	drain := chartDuration(t, chartValues, hr, "drainTime")
	pst := chartDuration(t, chartValues, hr, "parentShutdownTime")
	aud := chartDuration(t, chartValues, hr, "adminUnresponsiveDeadline")

	// Go through the real effective path, so a change to either the defaulting
	// or the derivation is caught here too.
	s := New(Config{ParentShutdownTime: pst, AdminUnresponsiveDeadline: aud}, slog.New(slog.DiscardHandler), nil)
	interval := s.adminReverifyInterval()

	require.Positive(t, pst, "%s: proxy.hotRestart.parentShutdownTime must be set", chartValues)
	assert.Greater(t, pst, drain,
		"%s: proxy.hotRestart.parentShutdownTime must exceed drainTime", chartValues)
	assert.LessOrEqual(t, interval*adminReverifyDivisor, pst,
		"%s: proxy.hotRestart.parentShutdownTime (%s) leaves no room for %d re-verifications of %s; "+
			"the epoch-identity probe would not be diagnosed while the draining parent lives",
		chartValues, pst, adminReverifyDivisor, interval)
	assert.Less(t, interval, pst,
		"%s: the re-verify interval (%s) must land strictly inside parentShutdownTime (%s)",
		chartValues, interval, pst)
	assert.GreaterOrEqual(t, interval, adminReverifyFloor,
		"the re-verify interval must never drop below two watchdog ticks")
	assert.NoError(t, s.checkAdminReverifyMargin(),
		"%s: proxy.hotRestart values lose the admin re-verify margin", chartValues)
}

// findRepoFile locates a repo-relative path from the test's working directory,
// which is the package dir under both `go test` and Bazel's runfiles tree.
func findRepoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Never skip: this test exists precisely to fail when the pinned file is
	// not the one we think it is.
	t.Fatalf("%s not found above the working directory; "+
		"under Bazel the go_test needs data = [\"//charts/aether:values.yaml\"]", rel)
	return ""
}

// chartDuration reads one proxy.hotRestart value. The block mixes YAML strings
// ("15s") with plain numbers (0 for "use the built-in default").
func chartDuration(t *testing.T, file string, hr map[string]any, key string) time.Duration {
	t.Helper()
	v, ok := hr[key]
	require.True(t, ok, "%s: proxy.hotRestart.%s is missing", file, key)
	d, err := durationValue(v)
	require.NoError(t, err, "%s: proxy.hotRestart.%s", file, key)
	return d
}

func durationValue(v any) (time.Duration, error) {
	switch t := v.(type) {
	case string:
		return time.ParseDuration(t)
	case float64:
		if t == 0 {
			return 0, nil
		}
		return 0, fmt.Errorf("bare number %v is only meaningful as 0 (use the built-in default)", t)
	case int:
		if t == 0 {
			return 0, nil
		}
		return 0, fmt.Errorf("bare number %v is only meaningful as 0 (use the built-in default)", t)
	default:
		return 0, fmt.Errorf("unexpected type %T", v)
	}
}
