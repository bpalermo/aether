package envoy_validate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// Issue #1636. A pod name is an RFC 1123 subdomain: it may contain dots, and a
// dot is the separator of an Envoy stat name. The agent used to put the pod
// name into its per-pod stat names as it is, so a pod "web.external-7d9f" had
//
//	cluster.health_team-a_web.external-7d9f.membership_healthy
//	listener.inbound_team-a_web.external-7d9f.downstream_cx_total
//
// and the chart's stats_config, which reads everything up to the FIRST dot as
// the cluster or the listener, saw a cluster "health_team-a_web" with a stat
// "external-7d9f.membership_healthy". That matches the exclusion of the probe
// clusters' dead `external.*` subtree, the gauge is never allocated, and the
// health gateway's health_check filter, which reads it, answers 503 for a
// healthy pod. The rest of the pod name also stayed in the metric NAME.
//
// Since #1636 the agent spells a pod in a STAT name with '~' for each dot of
// the pod name (proxy.PodStatKey), and no resource is renamed. These two tests
// run the pinned proxy with the chart's stats_config as it is: the first reads
// what is allocated and how it is exported, the second drives the health
// gateway.

// adversarialPods are pod names chosen against the chart's stats_config: a
// second name label that is one of the exclusion's alternatives, labels that
// are words the tag extractors look for, and several dots.
func adversarialPods() []*cniv1.CNIPod {
	names := []struct{ ns, name string }{
		// The #1634 review's set: no dot, but each a word of the extractors.
		{"h3", "h3"},
		{"0", "1"},
		{"a-b", "c"},
		{"a", "b-c"},
		{"inbound", "out-http"},
		// A second label that starts like an excluded subtree of the probe
		// clusters (the reported cases, then the rest of the alternation).
		{"team-a", "web.external-7d9f-abcde"},
		{"team-a", "api.http2-0"},
		{"team-a", "job.retry-1"},
		{"team-a", "old.http1-0"},
		{"team-a", "x.assignment-2"},
		{"team-a", "db.default.total-match"},
		{"team-a", "c.circuit-breakers"},
		// A second label that IS a stat or a subtree name of a listener or a
		// cluster, and names of several labels.
		{"team-a", "n.ssl"},
		{"team-a", "m.membership-healthy"},
		{"team-a", "w.worker-0"},
		{"team-a", "a.b.c"},
		{"team-a", "1.2.3"},
		{"team-a", "h3.h3"},
	}
	pods := make([]*cniv1.CNIPod, 0, len(names))
	for _, n := range names {
		pods = append(pods, &cniv1.CNIPod{Namespace: n.ns, Name: n.name, ServiceAccount: "sa", NetworkNamespace: "/var/run/netns/cni-" + n.ns + "-" + n.name})
	}
	return pods
}

// podStatNames is what the agent's generators give one pod: the stat prefixes
// of its three tagged listeners and its two probe clusters.
type podStatNames struct {
	pod                    *cniv1.CNIPod
	inbound, outbound, h3  string
	health, inboundReady   statsCluster
	healthName, readyName  string
	wantNamespace, wantPod string
}

func statNamesOf(t *testing.T, pod *cniv1.CNIPod) podStatNames {
	t.Helper()
	inbound, outbound, _, health, err := proxy.GenerateListenersFromRegistryPod(pod, "aether.internal", "mesh.internal", false, false, nil, nil, "")
	require.NoError(t, err)
	quic, err := proxy.NewInboundQUICListener(pod, "aether.internal", "mesh.internal", false, false, nil, nil)
	require.NoError(t, err)
	ready := proxy.NewInboundReadyProbeCluster(proxy.InboundReadyClusterName(pod), pod.GetNetworkNamespace(),
		"spiffe://aether.internal/node", "ROOTCA", proxy.SpiffeIDFromPod(pod, "aether.internal"))
	return podStatNames{
		pod:      pod,
		inbound:  inbound.GetStatPrefix(),
		outbound: outbound.GetStatPrefix(),
		h3:       quic.GetStatPrefix(),
		health:   statsCluster{name: health.GetName(), altStatName: health.GetAltStatName()},
		inboundReady: statsCluster{
			name:        ready.GetName(),
			altStatName: ready.GetAltStatName(),
		},
		healthName:    clusterStatsKey(health),
		readyName:     clusterStatsKey(ready),
		wantNamespace: pod.GetNamespace(),
		// The documented label value: the pod name with '~' for each dot.
		wantPod: strings.ReplaceAll(pod.GetName(), ".", "~"),
	}
}

func TestChartStatsConfigOnDottedPodNames(t *testing.T) {
	bin := envoyBinary(t)

	// The control: one pod with a plain name. Whatever metric families it
	// has are the families a node has; no other pod may add one.
	control := statNamesOf(t, &cniv1.CNIPod{Namespace: "team-a", Name: "web-0", ServiceAccount: "sa", NetworkNamespace: "/var/run/netns/cni-control"})
	_, controlProm := podStats(t, bin, []podStatNames{control})
	controlFamilies := metricFamilies(controlProm)
	require.Contains(t, controlFamilies, "envoy_listener_inbound_downstream_cx_total")
	require.Contains(t, controlFamilies, "envoy_cluster_membership_healthy")

	var all []podStatNames
	for _, pod := range adversarialPods() {
		all = append(all, statNamesOf(t, pod))
	}
	stat, prom := podStats(t, bin, append([]podStatNames{control}, all...))

	// No pod is part of a metric NAME: the families are the control's,
	// whatever the pods on the node are called. A pod name in a family is one
	// family per pod, kept by the metrics store's name index for good.
	assert.Equal(t, controlFamilies, metricFamilies(prom),
		"a pod name adds metric families: part of it is left in a stat name after the chart's tag extraction")

	for _, n := range all {
		ns, name := n.pod.GetNamespace(), n.pod.GetName()
		t.Run("stat names "+ns+"/"+name, func(t *testing.T) {
			for _, s := range []string{n.inbound, n.outbound, n.h3, n.healthName, n.readyName} {
				assert.NotContainsf(t, s, ".", "stat name %q carries a dot of the pod name", s)
			}
			// Nothing is renamed: the resources, and the health gateway
			// paths made of them, are spelled with the pod's own name.
			assert.Equal(t, "health_"+ns+"_"+name, n.health.name)
			assert.Equal(t, "inboundready_"+ns+"_"+name, n.inboundReady.name)
		})
		t.Run("tags "+ns+"/"+name, func(t *testing.T) {
			labels := `{aether_namespace="` + n.wantNamespace + `",aether_pod="` + n.wantPod
			assert.Contains(t, prom, `envoy_listener_inbound_downstream_cx_total`+labels+`"}`)
			assert.Contains(t, prom, `envoy_listener_out_http_downstream_cx_total`+labels+`"}`)
			assert.Contains(t, prom, `envoy_listener_inbound_downstream_cx_total`+labels+`_h3"}`)
			// The probe clusters' label is the whole stat name of the cluster.
			assert.Contains(t, prom, `envoy_cluster_membership_healthy{aether_cluster="health_`+ns+`_`+n.wantPod+`"}`)
			assert.Contains(t, prom, `envoy_cluster_membership_healthy{aether_cluster="inboundready_`+ns+`_`+n.wantPod+`"}`)
		})
		t.Run("membership gauges "+ns+"/"+name, func(t *testing.T) {
			for _, cluster := range []string{n.healthName, n.readyName} {
				// What the health gateway's health_check filter READS.
				for _, gauge := range []string{"membership_healthy", "membership_total", "membership_degraded"} {
					assert.Truef(t, stat["cluster."+cluster+"."+gauge],
						"cluster.%s.%s is not allocated: a stats exclusion of the chart swallows it, and the health gateway answers 503 for this pod", cluster, gauge)
				}
				// What the exclusion is for.
				for _, dead := range []string{"upstream_cx_total", "upstream_rq_total", "lb_healthy_panic", "retry_or_shadow_abandoned", "max_host_weight", "bind_errors", "assignment_stale"} {
					assert.Falsef(t, stat["cluster."+cluster+"."+dead], "cluster.%s.%s is allocated: the chart's exclusion does not match this pod's probe cluster", cluster, dead)
				}
			}
		})
	}
}

// TestHealthGatewayAnswersForADottedPod drives the path the 2026-06-11 outage
// was on, for pods whose names used to lose their membership gauges: the
// agent's health gateway listener and the agent's health-probe clusters, on
// the pinned proxy with the chart's stats_config, each pod's application
// served on a Unix socket (UDS delivery, so the probe needs no network
// namespace).
//
// The gateway's health_check filter is configured by cluster NAME and reads
// the cluster's membership gauges. The cluster's stats are keyed by
// alt_stat_name for a dotted pod, so this is also the proof that the filter
// reaches the right cluster's gauges through a name that is not the stat name:
// one pod is up and one is down, each path answers for its own pod, and the
// answer follows the pod when its application comes up.
func TestHealthGatewayAnswersForADottedPod(t *testing.T) {
	bin := envoyBinary(t)

	// AF_UNIX paths are 107 bytes at most; the Bazel test tmpdir is longer.
	dir, err := os.MkdirTemp("/tmp", "aether-1636-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	type gatewayPod struct {
		pod    *cniv1.CNIPod
		socket string
		path   string
	}
	newPod := func(i int, name string) gatewayPod {
		pod := &cniv1.CNIPod{Namespace: "team-a", Name: name, ServiceAccount: "sa", NetworkNamespace: "/var/run/netns/unused"}
		return gatewayPod{pod: pod, socket: filepath.Join(dir, fmt.Sprintf("app-%d.sock", i)), path: proxy.HealthGatewayPath(proxy.HealthProbeClusterName(pod))}
	}
	up := newPod(0, "web.external-7d9f-abcde") // the reported name; application up
	late := newPod(1, "api.http2-0")           // application down, then started
	down := newPod(2, "job.retry-1")           // application never up
	plain := newPod(3, "web-0")                // no dot: the control
	pods := []gatewayPod{up, late, down, plain}
	require.Equal(t, "/healthz/health_team-a_web.external-7d9f-abcde", up.path, "the gateway path is the cluster's NAME: the pod's own name, dots included")

	var clusters []*clusterv3.Cluster
	var probes []proxy.HealthGatewayProbe
	for _, p := range pods {
		_, health := proxy.NewAppDeliveryClusters(p.pod, p.socket)
		clusters = append(clusters, health)
		probes = append(probes, proxy.NewHealthGatewayProbe(health.GetName(), ""))
	}
	gatewaySocket := filepath.Join(dir, "health.sock")
	bootstrap := &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: "health-gateway-test", Cluster: "health-gateway-test"},
		Admin: &bootstrapv3.Admin{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{
			SocketAddress: &corev3.SocketAddress{Address: "127.0.0.1", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 0}},
		}}},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Clusters:  clusters,
			Listeners: []*listenerv3.Listener{proxy.BuildHealthGatewayListener(gatewaySocket, probes)},
		},
	}
	bootstrapJSON, err := protojson.Marshal(bootstrap)
	require.NoError(t, err)
	bootstrapPath := filepath.Join(dir, "bootstrap.json")
	require.NoError(t, os.WriteFile(bootstrapPath, bootstrapJSON, 0o600))

	serveApp(t, up.socket)
	serveApp(t, plain.socket)

	// The chart's stats_config is merged over the generated bootstrap.
	envoy := startEnvoy(t, bin, dir, "-c", bootstrapPath, "--config-yaml", chartStatsConfig(t))

	status := healthGatewayStatus(gatewaySocket)

	// A healthy application behind a dotted pod name reads healthy. Before
	// #1636 this was 503 for good: the gauges the filter reads were excluded.
	envoy.eventually(t, 20*time.Second, func() bool { return status(up.path) == http.StatusOK },
		"the health gateway never answered 200 on %s for a pod whose application is up", up.path)
	envoy.eventually(t, 20*time.Second, func() bool { return status(plain.path) == http.StatusOK },
		"the health gateway never answered 200 on %s (the control pod)", plain.path)

	// Each path answers for ITS pod: the two whose applications are down are
	// 503 while the other two are 200, and a path nobody programmed is 404.
	assert.Equal(t, http.StatusServiceUnavailable, status(late.path), late.path)
	assert.Equal(t, http.StatusServiceUnavailable, status(down.path), down.path)
	assert.Equal(t, http.StatusNotFound, status("/healthz/health_team-a_nobody"))

	// The gauges behind those answers, under the cluster's STAT name.
	plainStats, ok := envoy.admin("/stats?filter=membership_")
	require.True(t, ok)
	for _, want := range []string{
		"cluster.health_team-a_web~external-7d9f-abcde.membership_healthy: 1",
		"cluster.health_team-a_web~external-7d9f-abcde.membership_total: 1",
		"cluster.health_team-a_api~http2-0.membership_healthy: 0",
		"cluster.health_team-a_api~http2-0.membership_total: 1",
		"cluster.health_team-a_job~retry-1.membership_healthy: 0",
		"cluster.health_team-a_web-0.membership_healthy: 1",
	} {
		assert.Contains(t, plainStats, want)
	}

	// The answer follows the pod: its application comes up, its path turns
	// 200 at the next active check (5 s), and the pod that stays down stays 503.
	serveApp(t, late.socket)
	envoy.eventually(t, 30*time.Second, func() bool { return status(late.path) == http.StatusOK },
		"the health gateway never answered 200 on %s after the pod's application came up", late.path)
	assert.Equal(t, http.StatusServiceUnavailable, status(down.path), down.path)
	assert.Equal(t, http.StatusOK, status(up.path), up.path)
}

// healthGatewayStatus returns a function that GETs a path of the health
// gateway on its Unix socket, one connection per request, and returns the
// status (0 when the request failed).
func healthGatewayStatus(gatewaySocket string) func(path string) int {
	gateway := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", gatewaySocket)
			},
		},
	}
	return func(path string) int {
		resp, err := gateway.Get("http://health-gateway" + path)
		if err != nil {
			return 0
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
}

// TestHealthGatewayAcrossTheStatNameChange is the upgrade, for the pods it
// changes anything for: a proxy holding the probe clusters an agent older than
// #1636 built (stats keyed by the cluster's name, dots and all) is sent the
// ones this agent builds (the same names, alt_stat_name set). The clusters
// come from a file the proxy watches, which is rewritten once.
//
// Two dotted pods, both with their applications up:
//
//   - "web.shard-0": no exclusion matched its gauges, so the old agent's
//     cluster read healthy. Its path must answer 200 before, after, and at
//     every request in between: the proxy warms the replacing cluster, first
//     health check included, before it takes the old one's place.
//   - "web.external-0": the old agent's cluster had its gauges excluded and
//     its path answered 503 although the application was up (the bug, kept
//     here as it was). It turns 200 with the new cluster.
func TestHealthGatewayAcrossTheStatNameChange(t *testing.T) {
	bin := envoyBinary(t)

	dir, err := os.MkdirTemp("/tmp", "aether-1636-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	shard := &cniv1.CNIPod{Namespace: "team-a", Name: "web.shard-0", ServiceAccount: "sa", NetworkNamespace: "/var/run/netns/unused"}
	external := &cniv1.CNIPod{Namespace: "team-a", Name: "web.external-0", ServiceAccount: "sa", NetworkNamespace: "/var/run/netns/unused"}
	shardPath := proxy.HealthGatewayPath(proxy.HealthProbeClusterName(shard))
	externalPath := proxy.HealthGatewayPath(proxy.HealthProbeClusterName(external))

	var current, old []*clusterv3.Cluster
	var probes []proxy.HealthGatewayProbe
	for i, pod := range []*cniv1.CNIPod{shard, external} {
		socket := filepath.Join(dir, fmt.Sprintf("app-%d.sock", i))
		serveApp(t, socket)
		_, health := proxy.NewAppDeliveryClusters(pod, socket)
		require.NotEmpty(t, health.GetAltStatName())
		current = append(current, health)
		// What an agent older than #1636 built: no alt_stat_name.
		before := proto.Clone(health).(*clusterv3.Cluster)
		before.AltStatName = ""
		old = append(old, before)
		probes = append(probes, proxy.NewHealthGatewayProbe(health.GetName(), ""))
	}

	cdsPath := filepath.Join(dir, "cds.json")
	writeCDS := func(version string, clusters []*clusterv3.Cluster) {
		t.Helper()
		resp := &discoveryv3.DiscoveryResponse{VersionInfo: version, TypeUrl: "type.googleapis.com/envoy.config.cluster.v3.Cluster"}
		for _, c := range clusters {
			packed, err := anypb.New(c)
			require.NoError(t, err)
			resp.Resources = append(resp.Resources, packed)
		}
		data, err := protojson.Marshal(resp)
		require.NoError(t, err)
		// The proxy watches for a move into place.
		tmp := cdsPath + ".tmp"
		require.NoError(t, os.WriteFile(tmp, data, 0o600))
		require.NoError(t, os.Rename(tmp, cdsPath))
	}
	writeCDS("old-agent", old)

	gatewaySocket := filepath.Join(dir, "health.sock")
	bootstrap := &bootstrapv3.Bootstrap{
		Node: &corev3.Node{Id: "health-gateway-test", Cluster: "health-gateway-test"},
		Admin: &bootstrapv3.Admin{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{
			SocketAddress: &corev3.SocketAddress{Address: "127.0.0.1", PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 0}},
		}}},
		StaticResources: &bootstrapv3.Bootstrap_StaticResources{
			Listeners: []*listenerv3.Listener{proxy.BuildHealthGatewayListener(gatewaySocket, probes)},
		},
		DynamicResources: &bootstrapv3.Bootstrap_DynamicResources{
			CdsConfig: &corev3.ConfigSource{
				ResourceApiVersion: corev3.ApiVersion_V3,
				ConfigSourceSpecifier: &corev3.ConfigSource_PathConfigSource{
					PathConfigSource: &corev3.PathConfigSource{Path: cdsPath},
				},
			},
		},
	}
	bootstrapJSON, err := protojson.Marshal(bootstrap)
	require.NoError(t, err)
	bootstrapPath := filepath.Join(dir, "bootstrap.json")
	require.NoError(t, os.WriteFile(bootstrapPath, bootstrapJSON, 0o600))

	envoy := startEnvoy(t, bin, dir, "-c", bootstrapPath, "--config-yaml", chartStatsConfig(t))
	status := healthGatewayStatus(gatewaySocket)

	// Under the old agent's clusters.
	envoy.eventually(t, 20*time.Second, func() bool { return status(shardPath) == http.StatusOK },
		"the health gateway never answered 200 on %s under the old agent's cluster", shardPath)
	require.Equal(t, http.StatusServiceUnavailable, status(externalPath),
		"%s: the old agent's cluster for this pod has its membership gauges excluded; if this is 200 the fixture no longer reproduces #1636", externalPath)
	before, ok := envoy.admin("/stats?filter=membership_healthy")
	require.True(t, ok)
	assert.Contains(t, before, "cluster.health_team-a_web.shard-0.membership_healthy: 1")
	assert.NotContains(t, before, "external-0")

	// The swap, with the unaffected pod's path requested throughout: the
	// requests start before the file is rewritten and go on after the proxy
	// holds both replacing clusters, however fast the proxy is.
	stop := make(chan struct{})
	type sample struct {
		at     time.Duration
		status int
	}
	polled := make(chan []sample, 1)
	var count atomic.Int64
	start := time.Now()
	go func() {
		var samples []sample
		for {
			select {
			case <-stop:
				polled <- samples
				return
			default:
			}
			samples = append(samples, sample{time.Since(start), status(shardPath)})
			count.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()
	const margin = 10
	more := func() func() bool {
		from := count.Load()
		return func() bool { return count.Load() >= from+margin }
	}
	envoy.eventually(t, 20*time.Second, more(), "the gateway was not requested before the swap")
	written := time.Since(start)
	writeCDS("this-agent", current)
	envoy.eventually(t, 20*time.Second, func() bool { return status(externalPath) == http.StatusOK },
		"the health gateway never answered 200 on %s after the proxy was sent this agent's cluster", externalPath)
	envoy.eventually(t, 20*time.Second, func() bool {
		after, ok := envoy.admin("/stats?filter=membership_healthy")
		return ok && strings.Contains(after, "cluster.health_team-a_web~shard-0.membership_healthy: 1") &&
			!strings.Contains(after, "cluster.health_team-a_web.shard-0.")
	}, "the proxy never replaced %s's cluster", shardPath)
	replaced := time.Since(start)
	envoy.eventually(t, 20*time.Second, more(), "the gateway was not requested after the swap")
	close(stop)
	samples := <-polled

	var not200 []string
	for _, s := range samples {
		if s.status != http.StatusOK {
			not200 = append(not200, fmt.Sprintf("%d at %s", s.status, s.at.Round(100*time.Microsecond)))
		}
	}
	assert.Emptyf(t, not200,
		"%s did not answer 200 for a healthy pod while its probe cluster was replaced (%d requests; the clusters were written at %s and seen replaced by %s)",
		shardPath, len(samples), written.Round(100*time.Microsecond), replaced.Round(100*time.Microsecond))
	t.Logf("%s: %d requests across the replacement of its probe cluster, %d not 200", shardPath, len(samples), len(not200))
}

// serveApp serves 200 on every path over a Unix socket: an application the
// agent delivers to by UDS, and health-checks there.
func serveApp(t *testing.T, socket string) {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// envoyProcess is a running proxy and its admin endpoint.
type envoyProcess struct {
	adminPath string
	exited    chan struct{}
	output    *bytes.Buffer
}

// startEnvoy runs the proxy with the given arguments (one worker, no hot
// restart, the admin address written into dir) until the test ends.
func startEnvoy(t *testing.T, bin, dir string, args ...string) *envoyProcess {
	t.Helper()
	p := &envoyProcess{adminPath: filepath.Join(dir, "admin-address"), exited: make(chan struct{}), output: &bytes.Buffer{}}
	ctx, cancel := context.WithCancel(context.Background())
	args = append(args, "--disable-hot-restart", "--concurrency", "1", "--log-level", "warn", "--admin-address-path", p.adminPath)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = p.output, p.output
	require.NoError(t, cmd.Start())
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		cancel()
		<-p.exited
	})
	return p
}

// admin GETs a path of the admin endpoint; false until the proxy serves it.
func (p *envoyProcess) admin(path string) (string, bool) {
	addr, err := os.ReadFile(p.adminPath)
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

// eventually polls cond until it holds, and fails the test with the proxy's
// output when the proxy exits or the time runs out first.
func (p *envoyProcess) eventually(t *testing.T, within time.Duration, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		select {
		case <-p.exited:
			t.Fatalf("envoy exited:\n%s", p.output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf(format+"\n(waited %s)\n%s", append(args, within, p.output.String())...)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// podStats runs the proxy with the chart's stats_config and the given pods'
// listeners and probe clusters, and returns the allocated stat names and the
// Prometheus rendering of the admin endpoint.
func podStats(t *testing.T, bin string, pods []podStatNames) (stat map[string]bool, prom string) {
	t.Helper()
	var prefixes []string
	var clusters []statsCluster
	for _, n := range pods {
		prefixes = append(prefixes, n.inbound, n.outbound, n.h3)
		clusters = append(clusters, n.health, n.inboundReady)
	}
	dir := t.TempDir()
	bootstrap := filepath.Join(dir, "stats_bootstrap.yaml")
	require.NoError(t, os.WriteFile(bootstrap, []byte(podStatsBootstrapWithClusters(t, prefixes, clusters)), 0o600))
	envoy := startEnvoy(t, bin, dir, "-c", bootstrap)

	var plain string
	envoy.eventually(t, 30*time.Second, func() bool {
		var okPlain, okProm bool
		plain, okPlain = envoy.admin("/stats")
		prom, okProm = envoy.admin("/stats/prometheus")
		return okPlain && okProm && strings.Contains(plain, "listener."+prefixes[0]+".")
	}, "no stats from the proxy's admin endpoint")

	stat = map[string]bool{}
	for _, line := range strings.Split(plain, "\n") {
		if name, _, ok := strings.Cut(line, ": "); ok {
			stat[name] = true
		}
	}
	return stat, prom
}

// metricFamilies is the sorted set of metric names in a Prometheus rendering.
func metricFamilies(prom string) []string {
	var families []string
	for _, line := range strings.Split(prom, "\n") {
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			families = append(families, strings.Fields(rest)[0])
		}
	}
	slices.Sort(families)
	return slices.Compact(families)
}
