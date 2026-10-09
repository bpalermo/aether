package envoy_validate

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	collectormetricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
)

// nodeProxyServiceName is the service.name the node proxy's Envoy stats are
// exported with (#1561). A metrics backend stores it as `job`, so a query can
// say {job="aether-proxy"} where it used to exclude the edge proxy by negation.
// docs/observability/metric-labels.md names it for operators, and
// //charts/aether:aether_proxy_stats_service_name_test pins the rendered block.
const nodeProxyServiceName = "aether-proxy"

// TestNodeProxyStatsSinkServiceName starts the pinned Envoy with the
// resource_detectors block of the chart's node-proxy bootstrap and reads the
// OTLP resource it exports its stats with.
//
// The name is set in the bootstrap and not in the proxy container's
// environment because the supervisor in the same container builds its own
// resource from that environment, as aether-proxy-supervisor: OTEL_SERVICE_NAME
// there would rename the supervisor's metrics. So the bootstrap has to win, and
// it has to win inside Envoy, where the environment detector reads the same
// OTEL_RESOURCE_ATTRIBUTES. Envoy merges its detectors in the order they are
// listed and a later one overwrites an earlier one, so the static detector
// comes second. This test runs the proxy with a service.name in the
// environment and expects the bootstrap's; the attributes only the environment
// sets must still arrive (the collector promotes k8s.node.name from them).
//
// `envoy --mode validate` cannot stand in for this: it accepts a detector whose
// name is misspelled (the factory is found by the config's type), and it never
// builds an export.
func TestNodeProxyStatsSinkServiceName(t *testing.T) {
	bin := envoyBinary(t)
	resources := startOTLPMetricsReceiver(t)

	bootstrap := filepath.Join(t.TempDir(), "stats_sink_bootstrap.yaml")
	require.NoError(t, os.WriteFile(bootstrap, []byte(statsSinkBootstrap(t, resources.port)), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	// Hot restart off: with it on Envoy creates shared memory under a base id,
	// and two tests on one machine would have to agree on distinct ids.
	cmd := exec.CommandContext(ctx, bin, "-c", bootstrap, "--disable-hot-restart", "--concurrency", "1", "--log-level", "warn")
	cmd.Env = append(os.Environ(), "OTEL_RESOURCE_ATTRIBUTES=service.name=from-the-environment,k8s.node.name=node-1")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	require.NoError(t, cmd.Start())
	// exited is closed once the process is gone and its output is complete;
	// stop ends the process and returns what it wrote, for a failure message.
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	stop := func() string {
		cancel()
		<-exited
		return output.String()
	}
	t.Cleanup(func() { stop() })

	select {
	case attributes := <-resources.seen:
		require.Equal(t, nodeProxyServiceName, attributes["service.name"],
			"the node proxy's stats were exported with this service.name (the container's environment said from-the-environment): the static detector of charts/aether/templates/agent-proxy-configmap.yaml must be listed after the environment one. Resource: %v", attributes)
		require.Equal(t, "node-1", attributes["k8s.node.name"],
			"the attributes of OTEL_RESOURCE_ATTRIBUTES no longer reach the resource: the environment detector is gone from the chart's block. Resource: %v", attributes)
	case <-exited:
		t.Fatalf("envoy exited before it exported any stats: %v\n%s", waitErr, output.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("envoy exported no stats in 30s\n%s", stop())
	}
}

// otlpMetricsReceiver is an OTLP/gRPC metrics endpoint on a loopback port that
// hands the test the attributes of each resource it is sent.
type otlpMetricsReceiver struct {
	collectormetricsv1.UnimplementedMetricsServiceServer
	port int
	seen chan map[string]string
}

func (r *otlpMetricsReceiver) Export(_ context.Context, req *collectormetricsv1.ExportMetricsServiceRequest) (*collectormetricsv1.ExportMetricsServiceResponse, error) {
	for _, rm := range req.GetResourceMetrics() {
		attributes := map[string]string{}
		for _, kv := range rm.GetResource().GetAttributes() {
			attributes[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		select {
		case r.seen <- attributes:
		default: // the test reads one export; later flushes are dropped
		}
	}
	return &collectormetricsv1.ExportMetricsServiceResponse{}, nil
}

func startOTLPMetricsReceiver(t *testing.T) *otlpMetricsReceiver {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r := &otlpMetricsReceiver{port: lis.Addr().(*net.TCPAddr).Port, seen: make(chan map[string]string, 1)}
	srv := grpc.NewServer()
	collectormetricsv1.RegisterMetricsServiceServer(srv, r)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return r
}

// statsSinkBootstrap is the smallest bootstrap that exports stats: the OTLP
// sink, with the chart's own resource_detectors block, and the collector
// cluster it names, pointed at the test's receiver.
func statsSinkBootstrap(t *testing.T, collectorPort int) string {
	t.Helper()
	return fmt.Sprintf(`node:
  id: stats-sink-test
  cluster: stats-sink-test
stats_flush_interval: 0.2s
stats_sinks:
  - name: envoy.stat_sinks.open_telemetry
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.stat_sinks.open_telemetry.v3.SinkConfig
      grpc_service:
        envoy_grpc:
          cluster_name: otel_collector
%s
static_resources:
  clusters:
    - name: otel_collector
      type: STATIC
      connect_timeout: 1s
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: otel_collector
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: 127.0.0.1
                      port_value: %d
`, chartResourceDetectors(t, "      "), collectorPort)
}

// chartResourceDetectors returns the resource_detectors block of the OTLP
// stats sink in the chart's node-proxy bootstrap, comments dropped, at the
// given indentation. The block ends at the sink's next field.
func chartResourceDetectors(t *testing.T, indent string) string {
	t.Helper()
	const rel = "charts/aether/templates/agent-proxy-configmap.yaml"
	f, err := os.Open(findRepoFile(t, rel))
	require.NoError(t, err)
	defer f.Close()

	var block []string
	var blockIndent string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if block == nil {
			if trimmed == "resource_detectors:" {
				blockIndent = line[:len(line)-len(strings.TrimLeft(line, " "))]
				block = []string{indent + trimmed}
			}
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// The first line that is not deeper than `resource_detectors:` is
		// the sink's next field.
		if !strings.HasPrefix(line, blockIndent+" ") {
			break
		}
		block = append(block, indent+strings.TrimPrefix(line, blockIndent))
	}
	require.NoError(t, sc.Err())
	if len(block) < 2 {
		t.Fatalf("%s: no resource_detectors block with entries under the stats sink", rel)
	}
	return strings.Join(block, "\n")
}
