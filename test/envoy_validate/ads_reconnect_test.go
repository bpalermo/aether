package envoy_validate

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

// maxADSReconnectInterval is the longest the node proxy may wait between two
// attempts to re-open its ADS stream to the node agent (issue #1103).
//
// While the stream is down the proxy routes on its last config, so a
// destination the registry marked DRAINING in the meantime is still selected.
// Envoy's xDS default is a fully jittered exponential backoff with a 30 s cap,
// which on talos-main left a proxy reconnecting 8.4 s AFTER its restarted agent
// was serving again, and sending requests to a pod drained 3.4 s earlier. The
// cap is what bounds that blind tail once the agent is back; 1 s keeps it under
// the two-phase drain's 2 s pool-close floor.
const maxADSReconnectInterval = time.Second

// TestNodeProxyADSReconnectBackoffIsBounded reads the ADS retry policy the
// chart's node-proxy bootstrap ships, asserts it is present and capped, and
// proves Envoy accepts the exact values with `envoy --mode validate`.
//
// Without a retry_back_off the bootstrap falls back to Envoy's 500 ms / 30 s
// default, and the test fails on the missing policy.
func TestNodeProxyADSReconnectBackoffIsBounded(t *testing.T) {
	base, maxInterval := chartADSRetryBackOff(t)
	require.Positive(t, base, "ads_config retry_back_off.base_interval must be > 0")
	require.LessOrEqual(t, base, maxInterval, "base_interval must not exceed max_interval")
	require.LessOrEqual(t, maxInterval, maxADSReconnectInterval,
		"ads_config retry_back_off.max_interval %s exceeds %s: after a node-agent restart the proxy keeps routing on stale endpoints (DRAINING hosts included) for up to this long once the agent is back (#1103)",
		maxInterval, maxADSReconnectInterval)

	bs, err := buildNodeBootstrap()
	require.NoError(t, err)
	ads := bs.GetDynamicResources().GetAdsConfig()
	require.NotNil(t, ads)
	require.NotEmpty(t, ads.GetGrpcServices())
	ads.GetGrpcServices()[0].GetEnvoyGrpc().RetryPolicy = &corev3.RetryPolicy{
		RetryBackOff: &corev3.BackoffStrategy{
			BaseInterval: durationpb.New(base),
			MaxInterval:  durationpb.New(maxInterval),
		},
	}

	path := filepath.Join(t.TempDir(), "node_ads_retry_bootstrap.json")
	require.NoError(t, os.WriteFile(path, marshalForCheck(t, bs), 0o600))
	out, err := exec.Command(envoyBinary(t), "--mode", "validate", "-c", path).CombinedOutput()
	t.Logf("envoy --mode validate:\n%s", out)
	require.NoError(t, err, "envoy rejected the chart's ADS retry policy")
}

// chartADSRetryBackOff returns the base and max interval of the retry_back_off
// under dynamic_resources.ads_config in the chart's node-proxy bootstrap. It
// fails the test when the block is absent.
func chartADSRetryBackOff(t *testing.T) (time.Duration, time.Duration) {
	t.Helper()
	const rel = "charts/aether/templates/agent-proxy-configmap.yaml"
	f, err := os.Open(findRepoFile(t, rel))
	require.NoError(t, err)
	defer f.Close()

	var inADS, inBackOff bool
	var base, maxInterval time.Duration
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		switch {
		case line == "ads_config:":
			inADS = true
		case inADS && (line == "cds_config:" || line == "lds_config:" || line == "static_resources:"):
			inADS, inBackOff = false, false
		case inADS && line == "retry_back_off:":
			inBackOff = true
		case inBackOff && strings.HasPrefix(line, "base_interval:"):
			base = parseChartDuration(t, strings.TrimPrefix(line, "base_interval:"))
		case inBackOff && strings.HasPrefix(line, "max_interval:"):
			maxInterval = parseChartDuration(t, strings.TrimPrefix(line, "max_interval:"))
		}
	}
	require.NoError(t, sc.Err())
	if base == 0 || maxInterval == 0 {
		t.Fatalf("%s: dynamic_resources.ads_config carries no retry_policy.retry_back_off {base_interval, max_interval}: "+
			"the proxy uses Envoy's 500 ms / 30 s xDS default and stays blind up to 30 s after a node-agent restart (#1103)", rel)
	}
	return base, maxInterval
}

func parseChartDuration(t *testing.T, v string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(strings.Trim(strings.TrimSpace(v), `"`))
	require.NoError(t, err, "duration %q", v)
	return d
}
