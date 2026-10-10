package envoy_validate

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What the chart's stats_config does with the per-pod names the agent gives
// its listeners and probe clusters (issue #1584), measured on the pinned proxy.
//
// The names carry the pod's namespace since #1584 (proxy.PodResourceKey:
// "<namespace>_<pod>"), and three things in the chart's node-proxy bootstrap
// read them:
//
//   - the aether.namespace and aether.pod stats tags, extracted from the
//     listener stat prefixes inbound_<ns>_<pod> and out_http_<ns>_<pod>;
//   - the exclusion of the health_ and inboundready_ probe clusters' dead
//     subtrees;
//   - and, by what that exclusion must NOT match, the membership gauges the
//     health gateway's health_check filter reads. A pattern that swallowed
//     them answers 503 for every pod on the node (the 2026-06-11 outage).
//
// The test takes the stats_config block out of the chart template as it is,
// runs the proxy with it and with listeners and clusters named by the agent's
// own generators, and reads the admin endpoint.
func TestChartStatsConfigOnNamespacedPodNames(t *testing.T) {
	bin := envoyBinary(t)

	pod := &cniv1.CNIPod{Name: "web-0", Namespace: "team-a", ServiceAccount: "web", NetworkNamespace: "/var/run/netns/cni-aaaa"}
	inbound, outbound, _, health, err := proxy.GenerateListenersFromRegistryPod(pod, "aether.internal", "mesh.internal", false, false, nil, nil, "")
	require.NoError(t, err)
	listenerPrefixes := []string{inbound.GetStatPrefix(), outbound.GetStatPrefix(), proxy.InboundQUICListenerName(pod)}
	require.Equal(t, []string{"inbound_team-a_web-0", "out_http_team-a_web-0", "inbound_team-a_web-0_h3"}, listenerPrefixes)
	healthCluster, readyCluster := health.GetName(), proxy.InboundReadyClusterName(pod)
	require.Equal(t, "health_team-a_web-0", healthCluster)
	require.Equal(t, "inboundready_team-a_web-0", readyCluster)

	// What an agent from before #1584 writes for a pod "api-7": the proxy
	// rolls separately from the agent, so for the length of an upgrade a proxy
	// with this bootstrap can be sent these.
	oldAgentPrefixes := []string{"inbound_api-7", "out_http_api-7", "inbound_api-7_h3"}

	dir := t.TempDir()
	bootstrap := filepath.Join(dir, "stats_bootstrap.yaml")
	adminPath := filepath.Join(dir, "admin-address")
	require.NoError(t, os.WriteFile(bootstrap, []byte(podStatsBootstrap(t, append(listenerPrefixes, oldAgentPrefixes...), []string{healthCluster, readyCluster})), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "-c", bootstrap, "--disable-hot-restart", "--concurrency", "1", "--log-level", "warn", "--admin-address-path", adminPath)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	require.NoError(t, cmd.Start())
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		<-exited
	})

	get := func(path string) (string, bool) {
		addr, err := os.ReadFile(adminPath)
		if err != nil || len(bytes.TrimSpace(addr)) == 0 {
			return "", false
		}
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + strings.TrimSpace(string(addr)) + path)
		if err != nil {
			return "", false
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return string(body), err == nil && resp.StatusCode == http.StatusOK
	}
	var plain, prom string
	deadline := time.Now().Add(30 * time.Second)
	for {
		var okPlain, okProm bool
		plain, okPlain = get("/stats")
		prom, okProm = get("/stats/prometheus")
		if okPlain && okProm && strings.Contains(plain, "listener."+listenerPrefixes[0]+".") {
			break
		}
		select {
		case <-exited:
			t.Fatalf("envoy exited:\n%s", output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no stats from the proxy's admin endpoint in 30s\n%s", output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	stat := map[string]bool{}
	for _, line := range strings.Split(plain, "\n") {
		if name, _, ok := strings.Cut(line, ": "); ok {
			stat[name] = true
		}
	}

	// The tags. Each listener's stats are exported under ONE family per stat,
	// with the pod's namespace and name as two labels. The QUIC listener's
	// "_h3" stays in the pod label, as it did before #1584: it is what keeps
	// a pod's two inbound listeners apart in a family that has no other label.
	for _, want := range []string{
		`envoy_listener_inbound_downstream_cx_total{aether_namespace="team-a",aether_pod="web-0"}`,
		`envoy_listener_out_http_downstream_cx_total{aether_namespace="team-a",aether_pod="web-0"}`,
		`envoy_listener_inbound_downstream_cx_total{aether_namespace="team-a",aether_pod="web-0_h3"}`,
	} {
		assert.Containsf(t, prom, want, "the chart's aether.namespace / aether.pod stats tags do not produce %s", want)
	}
	assert.NotContains(t, prom, "envoy_listener_inbound_team", "a per-pod listener stat kept the pod in its NAME: one metric family per pod")
	assert.NotContains(t, prom, "envoy_listener_out_http_team")
	assert.NotContains(t, prom, `aether_pod="team-a_web-0`, "the namespace is its own label, not part of aether.pod")

	// The old agent's prefixes during an upgrade: the pod is still a label and
	// never part of a metric NAME. (Its QUIC prefix has no namespace to tell
	// from the pod, and reads as namespace "api-7", pod "h3": wrong for that
	// window, and still one family.)
	assert.Contains(t, prom, `envoy_listener_inbound_downstream_cx_total{aether_pod="api-7"}`)
	assert.Contains(t, prom, `envoy_listener_out_http_downstream_cx_total{aether_pod="api-7"}`)
	assert.Contains(t, prom, `envoy_listener_inbound_downstream_cx_total{aether_namespace="api-7",aether_pod="h3"}`)
	for _, line := range strings.Split(prom, "\n") {
		if strings.HasPrefix(line, "# TYPE envoy_listener_") {
			family := strings.Fields(line)[2]
			assert.Falsef(t, strings.Contains(family, "api_7") || strings.Contains(family, "web_0") || strings.Contains(family, "team_a"),
				"metric family %s carries a pod in its name", family)
		}
	}

	for _, cluster := range []string{healthCluster, readyCluster} {
		// What the health gateway's health_check filter READS. Never excluded.
		for _, gauge := range []string{"membership_healthy", "membership_total", "membership_degraded"} {
			assert.Truef(t, stat["cluster."+cluster+"."+gauge],
				"cluster.%s.%s is not allocated: a stats exclusion of the chart swallows it, and the health gateway answers 503 for the pod", cluster, gauge)
		}
		// What the exclusion is for: the probe clusters' permanently-zero
		// subtrees are still dropped under the new names.
		for _, dead := range []string{"upstream_cx_total", "upstream_rq_total", "lb_healthy_panic", "retry_or_shadow_abandoned", "max_host_weight", "bind_errors"} {
			assert.Falsef(t, stat["cluster."+cluster+"."+dead], "cluster.%s.%s is allocated: the chart's exclusion no longer matches the cluster's name", cluster, dead)
		}
	}
	// Their cluster label is the whole name, as before.
	assert.Contains(t, prom, `envoy_cluster_membership_healthy{aether_cluster="health_team-a_web-0"}`)
	assert.Contains(t, prom, `envoy_cluster_membership_healthy{aether_cluster="inboundready_team-a_web-0"}`)
}

// podStatsBootstrap is a bootstrap with the chart's stats_config, one TCP
// listener per given stat prefix and one STATIC cluster per given name.
func podStatsBootstrap(t *testing.T, listenerStatPrefixes, clusters []string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("node:\n  id: pod-stats-test\n  cluster: pod-stats-test\n")
	b.WriteString("admin:\n  address:\n    socket_address:\n      address: 127.0.0.1\n      port_value: 0\n")
	b.WriteString(chartStatsConfig(t))
	b.WriteString("\nstatic_resources:\n  listeners:\n")
	for _, prefix := range listenerStatPrefixes {
		fmt.Fprintf(&b, `    - name: %[1]s
      stat_prefix: %[1]s
      address:
        socket_address:
          address: 127.0.0.1
          port_value: 0
      filter_chains:
        - filters:
            - name: envoy.filters.network.tcp_proxy
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
                stat_prefix: pod_stats_test
                cluster: %[2]s
`, prefix, clusters[0])
	}
	b.WriteString("  clusters:\n")
	for _, name := range clusters {
		fmt.Fprintf(&b, `    - name: %[1]s
      type: STATIC
      connect_timeout: 1s
      load_assignment:
        cluster_name: %[1]s
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: 127.0.0.1
                      port_value: 9
`, name)
	}
	return b.String()
}

// chartStatsConfig returns the stats_config block of the chart's node-proxy
// bootstrap, comments dropped, at the bootstrap's top level. The block ends at
// the first line that is not deeper than `stats_config:`.
func chartStatsConfig(t *testing.T) string {
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
			if trimmed == "stats_config:" {
				blockIndent = line[:len(line)-len(strings.TrimLeft(line, " "))]
				block = []string{trimmed}
			}
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, blockIndent+" ") {
			break
		}
		require.NotContainsf(t, line, "{{", "%s: a template directive inside stats_config; this reader takes the block as text", rel)
		block = append(block, strings.TrimPrefix(line, blockIndent))
	}
	require.NoError(t, sc.Err())
	if len(block) < 2 {
		t.Fatalf("%s: no stats_config block", rel)
	}
	return strings.Join(block, "\n")
}
