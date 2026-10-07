package envoy_validate

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tcp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
)

// l4Bootstraps are the fixtures that carry L4 clusters and capture L4 chains,
// with the minimum each must hold for the aether#1023 gates not to pass
// vacuously.
var l4Bootstraps = []struct {
	name     string
	fn       func() ([]byte, error)
	clusters int // L4 (tcp:/udp:) clusters
	chains   int // tcp_proxy chains on a capture listener
}{
	{"capture_tcproute_bootstrap.json", CaptureTCPRouteBootstrapJSON, 3, 2},
	{"capture_tlsroute_bootstrap.json", CaptureTLSRouteBootstrapJSON, 3, 5},
	{"capture_udp_bootstrap.json", CaptureUDPBootstrapJSON, 2, 0},
	{"capture_bootstrap.json", CaptureBootstrapJSON, 0, 2},
}

// TestL4GatesAreNotVacuous: the two aether#1023 checks TestEnvoyValidate runs
// on every bootstrap examined something. A fixture that lost its L4 clusters
// or chains would otherwise pass both by having nothing to check.
func TestL4GatesAreNotVacuous(t *testing.T) {
	for _, b := range l4Bootstraps {
		t.Run(b.name, func(t *testing.T) {
			data, err := b.fn()
			require.NoError(t, err)
			bad, n, err := L4StatKeyViolations(data)
			require.NoError(t, err)
			assert.Empty(t, bad)
			assert.GreaterOrEqual(t, n, b.clusters, "L4 clusters examined")
			missing, checked, err := CaptureTCPChainsWithoutL4AccessLog(data)
			require.NoError(t, err)
			assert.Empty(t, missing)
			assert.GreaterOrEqual(t, checked, b.chains, "capture tcp_proxy chains examined")
		})
	}
}

// TestL4StatKeyGateCatchesACollapsedKey drives the stat-key gate red: each L4
// cluster in turn is collapsed back onto the bare "<ns>/<svc>" key (the
// pre-#1023 shape), or given the wrong kind prefix, and the gate must name it.
func TestL4StatKeyGateCatchesACollapsedKey(t *testing.T) {
	data, err := CaptureTCPRouteBootstrapJSON()
	require.NoError(t, err)
	base := &bootstrapv3.Bootstrap{}
	require.NoError(t, protojson.Unmarshal(data, base))

	mutated := 0
	for i, c := range base.GetStaticResources().GetClusters() {
		name := c.GetName()
		if !strings.HasPrefix(name, "tcp:") {
			continue
		}
		for label, key := range map[string]string{
			"bare service key": strings.SplitN(strings.TrimPrefix(c.GetAltStatName(), proxy.L4StatKeyTCPPrefix), "_", 2)[0],
			"wrong kind":       proxy.L4StatKeyUDPPrefix + strings.TrimPrefix(c.GetAltStatName(), proxy.L4StatKeyTCPPrefix),
			// The cluster-name spelling: Envoy would export it as the
			// underscore form, so a builder emitting it is wrong too.
			"colon spelling": strings.Replace(c.GetAltStatName(), "_", ":", 1),
		} {
			bs := cloneBootstrap(t, base)
			bs.StaticResources.Clusters[i].AltStatName = key
			bad, _, err := L4StatKeyViolations(marshalForCheck(t, bs))
			require.NoError(t, err)
			assert.NotEmpty(t, bad, "%s collapsed to %q (%s) was not reported", name, key, label)
			mutated++
		}
	}
	require.GreaterOrEqual(t, mutated, 6, "fixture carries too few tcp: clusters for the check to mean anything")
}

// TestL4AccessLogGateCatchesAnUnloggedChain drives the access-log gate red: the
// logger is stripped from each capture tcp_proxy chain in turn.
func TestL4AccessLogGateCatchesAnUnloggedChain(t *testing.T) {
	data, err := CaptureTLSRouteBootstrapJSON()
	require.NoError(t, err)
	base := &bootstrapv3.Bootstrap{}
	require.NoError(t, protojson.Unmarshal(data, base))

	stripped := 0
	for li, l := range base.GetStaticResources().GetListeners() {
		for ci, fc := range l.GetFilterChains() {
			for fi, f := range fc.GetFilters() {
				tc := &tcp_proxyv3.TcpProxy{}
				if f.GetTypedConfig() == nil || f.GetTypedConfig().UnmarshalTo(tc) != nil {
					continue
				}
				bs := cloneBootstrap(t, base)
				tc.AccessLog = nil
				a, err := anypb.New(tc)
				require.NoError(t, err)
				bs.StaticResources.Listeners[li].FilterChains[ci].Filters[fi].ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: a}
				missing, _, err := CaptureTCPChainsWithoutL4AccessLog(marshalForCheck(t, bs))
				require.NoError(t, err)
				assert.Equal(t, []string{l.GetName() + "/" + fc.GetName()}, missing)
				stripped++
			}
		}
	}
	require.GreaterOrEqual(t, stripped, 5, "fixture carries too few capture L4 chains")
}

// TestAetherClusterTagCapturesL4Keys runs the chart's OWN aether.cluster stats
// tag regex (Envoy uses RE2; so does Go's regexp) over the per-kind L4 keys:
// each must come out whole as the tag value, with the stat name collapsing to
// the same metric family the HTTP clusters use. It also pins that the HTTP
// selectors the runbook and soak README use cannot match an L4 key, so the
// HTTP series those gates read are unchanged by #1023.
func TestAetherClusterTagCapturesL4Keys(t *testing.T) {
	re := chartAetherClusterTagRegex(t)

	for _, key := range []string{
		proxy.TCPStatKey("aether-test/tcp-echo"),
		proxy.TCPPortStatKey("aether-test/tcp-echo", 9000),
		proxy.TCPPortStatKey("aether-test/mixed-svc", 9000),
		proxy.UDPStatKey("aether-test/udp-echo"),
		"aether-test/mixed-svc",                 // HTTP default, unchanged
		"aether-test/svc-1@aether-test/default", // QUIC twin, unchanged
	} {
		// Envoy sanitizes the stat name BEFORE tag extraction
		// (Stats::Utility::sanitizeStatsName: "://", ":/" and ":" become "_").
		// The key must come out of that unchanged, or the aether_cluster label
		// is not the key the gates are written against.
		stat := envoySanitizeStatName("cluster." + key + ".ssl.fail_verify_san")
		m := re.FindStringSubmatch(stat)
		if !assert.Len(t, m, 3, "%s: the aether.cluster regex did not match", stat) {
			continue
		}
		assert.Equal(t, key, m[2], "%s: aether_cluster must be the whole key", stat)
		assert.Equal(t, "cluster.ssl.fail_verify_san", strings.Replace(stat, m[1], "", 1),
			"%s: the tag-extracted name must be the shared metric family", stat)
	}

	// PromQL =~ is fully anchored; model it the same way.
	l4 := []string{"tcp_aether-test/tcp-echo", "tcp_aether-test/tcp-echo_9000", "udp_aether-test/udp-echo"}
	matches := func(sel, v string) bool { return regexp.MustCompile("^(?:" + sel + ")$").MatchString(v) }
	for _, sel := range []string{
		`.*@.*`, `.+@.+`, `aether-test/svc-1(@.*)?`, `aether-test/svc-[12]`,
		`aether-test/(tcp-echo|mixed-svc)`, `inboundready_.*`, `health_.*`,
	} {
		for _, k := range l4 {
			assert.False(t, matches(sel, k), "HTTP selector %q must not match the L4 key %q", sel, k)
		}
	}
	for _, k := range l4 {
		assert.True(t, matches(`tcp_.*|udp_.*`, k), "the L4 gate selector must match %q", k)
	}
	assert.False(t, matches(`tcp_.*|udp_.*`, "aether-test/mixed-svc"), "the L4 gate selector must not match an HTTP key")
}

// envoySanitizeStatName mirrors Envoy's Stats::Utility::sanitizeStatsName
// (source/common/stats/utility.cc), which every scope and tag value passes
// through.
func envoySanitizeStatName(name string) string {
	return strings.NewReplacer("://", "_", ":/", "_", ":", "_", "\x00", "_").Replace(name)
}

// TestColonSpelledL4KeyWouldNotSurviveSanitization is the reason the keys use
// "_": the cluster-name spelling "tcp:<ns>/<svc>:<port>" would reach
// Prometheus as "tcp_<ns>/<svc>_<port>", and a gate on `tcp:.*` would be
// vacuous forever.
func TestColonSpelledL4KeyWouldNotSurviveSanitization(t *testing.T) {
	re := chartAetherClusterTagRegex(t)
	m := re.FindStringSubmatch(envoySanitizeStatName("cluster.tcp:aether-test/tcp-echo:9000.upstream_cx_total"))
	require.Len(t, m, 3)
	assert.Equal(t, "tcp_aether-test/tcp-echo_9000", m[2])
	assert.Equal(t, proxy.TCPPortStatKey("aether-test/tcp-echo", 9000), m[2],
		"the key aether writes must be exactly the label Envoy exports")
}

// chartAetherClusterTagRegex reads the aether.cluster tag regex out of the
// proxy bootstrap ConfigMap the chart ships.
func chartAetherClusterTagRegex(t *testing.T) *regexp.Regexp {
	t.Helper()
	const rel = "charts/aether/templates/agent-proxy-configmap.yaml"
	f, err := os.Open(findRepoFile(t, rel))
	require.NoError(t, err)
	defer f.Close()

	sc := bufio.NewScanner(f)
	inTag := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "- tag_name: aether.cluster") {
			inTag = true
			continue
		}
		if inTag && strings.HasPrefix(line, "regex:") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "regex:"))
			pattern, err := strconv.Unquote(raw) // YAML double-quoted: \\ -> \
			require.NoError(t, err, "%s: regex %s", rel, raw)
			return regexp.MustCompile(pattern)
		}
	}
	require.NoError(t, sc.Err())
	t.Fatalf("%s: no aether.cluster stats tag", rel)
	return nil
}

// findRepoFile locates a repo-relative path from the test's working directory
// (the package dir under both `go test` and Bazel's runfiles tree). Never
// skips: under Bazel the go_test needs the file in `data`.
func findRepoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("%s not found above the working directory", rel)
		}
		dir = parent
	}
}

func cloneBootstrap(t *testing.T, bs *bootstrapv3.Bootstrap) *bootstrapv3.Bootstrap {
	t.Helper()
	out := &bootstrapv3.Bootstrap{}
	require.NoError(t, protojson.Unmarshal(marshalForCheck(t, bs), out))
	return out
}

func marshalForCheck(t *testing.T, bs *bootstrapv3.Bootstrap) []byte {
	t.Helper()
	data, err := protojson.Marshal(bs)
	require.NoError(t, err)
	return data
}
