package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/extensionfilter"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// determinismRuns is how many independent cache builds are compared. Every
// input below is fed in from a Go map or a shuffled slice, so each run gets a
// fresh random iteration order; with three or more entries per map, a
// pre-fix build agreeing across all of these by luck is vanishingly unlikely.
const determinismRuns = 12

// TestSnapshotDeterministic_ShuffledInputOrder is the #135-redux guard (#772,
// races report S6/S7): the same logical configuration, fed in a different
// order, must produce BYTE-IDENTICAL xDS resources.
//
// Why bytes and not proto.Equal: go-control-plane versions each resource for
// delta-xDS by hashing its marshalled bytes, and Envoy skips an update only on
// an unchanged MessageUtil::hash. proto.MarshalOptions{Deterministic:true}
// canonicalises map fields but preserves repeated-field order exactly as built
// — so a repeated field assembled by ranging a Go map re-hashes on every push
// even when nothing changed. That is incident #135's mechanism: every push
// became a full resource replace (RDS route-table rebuild, CDS replace, the
// RSS cascade).
//
// The failures this pins, in order of severity, all of which reproduce on the
// pre-fix tree:
//   - cap_http and aether_outbound RouteConfiguration.virtual_hosts built by
//     ranging c.clusters / c.captureAuthorities / gammaRoutes (S6);
//   - the redirect-all catch-all's known-target routes, where order is not
//     just bytes: Envoy evaluates a vhost's routes first-match-wins;
//   - the HCM's http_filters, where order is the filter chain's EXECUTION
//     order;
//   - the UDP capture listener's primary cluster, picked by map order, which
//     is outright routing nondeterminism (S7).
func TestSnapshotDeterministic_ShuffledInputOrder(t *testing.T) {
	var want map[string]string
	for run := range determinismRuns {
		rng := rand.New(rand.NewPCG(uint64(run), 0x772a4))
		c := buildDeterminismCache(t, rng)
		got := fingerprintCache(t, c)
		if run == 0 {
			want = got
			require.NotEmpty(t, want)
			continue
		}
		for key, wantDigest := range want {
			require.Equalf(t, wantDigest, got[key],
				"run %d produced different bytes for %s: xDS resource assembly is order-dependent "+
					"(see agent/internal/xds/cache/ordering.go — every map-derived resource slice must be sorted)",
				run, key)
		}
		require.Len(t, got, len(want), "run %d emitted a different set of resource groups", run)
	}
}

// buildDeterminismCache assembles one fully populated mesh-mode cache. Every
// multi-valued input is either a Go map (randomised iteration order for free)
// or a slice shuffled with rng, so two calls differ only in insertion order.
func buildDeterminismCache(t *testing.T, rng *rand.Rand) *SnapshotCache {
	t.Helper()
	ctx := context.Background()

	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	// redirect-all turns on the known-target safety net (captureKnownTargets),
	// whose entries become ordered routes on the catch-all virtual host.
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	// Three local pods, added in a shuffled order: c.listeners is keyed by
	// netns, so the per-pod listeners and app clusters come out of a map.
	pods := []*cniv1.CNIPod{
		makeCNIPod("pod-a", "aether-test", "/proc/101/ns/net"),
		makeCNIPod("pod-b", "aether-test", "/proc/102/ns/net"),
		makeCNIPod("pod-c", "aether-test", "/proc/103/ns/net"),
	}
	pods[0].ServiceAccount, pods[0].Ips = "client", []string{"10.0.1.1"}
	pods[1].ServiceAccount, pods[1].Ips = "echo", []string{"10.0.1.2"}
	pods[2].ServiceAccount, pods[2].Ips = "loadgen", []string{"10.0.1.3"}
	rng.Shuffle(len(pods), func(i, j int) { pods[i], pods[j] = pods[j], pods[i] })
	for _, pod := range pods {
		require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	}

	// SDS secrets, shuffled: c.secrets is a map keyed by secret name.
	secrets := []*tlsv3.Secret{
		{Name: "spiffe://aether.internal/ns/aether-test/sa/client"},
		{Name: "spiffe://aether.internal/ns/aether-test/sa/echo"},
		{Name: "spiffe://aether.internal/ns/aether-test/sa/loadgen"},
		{Name: "spiffe://aether.internal"},
	}
	rng.Shuffle(len(secrets), func(i, j int) { secrets[i], secrets[j] = secrets[j], secrets[i] })
	require.NoError(t, c.SetSecrets(ctx, secrets))

	// SA-backed mesh authorities → cap_http vhosts + known-target routes.
	c.SetCaptureAuthorities(map[string]string{
		"aether-test/echo":    "echo.aether-test.svc.cluster.local",
		"aether-test/client":  "client.aether-test.svc.cluster.local",
		"aether-test/loadgen": "loadgen.aether-test.svc.cluster.local",
		"aether-test/udp-a":   "udp-a.aether-test.svc.cluster.local",
		"aether-test/udp-b":   "udp-b.aether-test.svc.cluster.local",
	})

	// GAMMA rules, including a route-ONLY target (fanout, no SA-backed mesh
	// Service of its own) so appendRouteOnlyCaptureVhosts runs too.
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"aether-test/echo":    {gammaRouteTo("echo.aether-test.aether.internal")},
		"aether-test/loadgen": {gammaRouteTo("loadgen.aether-test.aether.internal")},
		"aether-test/fanout":  {gammaRouteTo("echo.aether-test.aether.internal")},
	})

	// Two distinct allow-listed chain filters plus an INBOUND one: all three
	// land in the HCM's http_filters union, whose order IS execution order.
	headerMutation := mustAny(t, "envoy.filters.http.header_mutation")
	headerToMetadata := mustAny(t, "envoy.filters.http.header_to_metadata")
	rbac := mustAny(t, extensionfilter.RBACFilterName)
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{
		"aether-test/echo":    {Name: "envoy.filters.http.header_mutation", Config: headerMutation},
		"aether-test/loadgen": {Name: "envoy.filters.http.header_to_metadata", Config: headerToMetadata},
	})
	c.SetServiceInboundFilters(map[string]proxy.ExtensionFilter{
		"aether-test/client": {Name: extensionfilter.RBACFilterName, Config: rbac},
	})

	// Two UDPRoute-backed services: the pre-fix GenerateUDPCaptureListener
	// picked the udp_proxy primary cluster by map order, so with two services
	// the pod's UDP listener forwarded to a different backend per rebuild.
	c.SetUDPServiceRoutes(map[string][]proxy.L4Backend{
		"aether-test/udp-a": {{Service: "aether-test/udp-a", Cluster: proxy.UDPClusterName("aether-test/udp-a", "aether.internal"), Weight: 1}},
		"aether-test/udp-b": {{Service: "aether-test/udp-b", Cluster: proxy.UDPClusterName("aether-test/udp-b", "aether.internal"), Weight: 1}},
	})

	svcs := []string{
		"aether-test/echo", "aether-test/client", "aether-test/loadgen",
		"aether-test/fanout", "aether-test/udp-a", "aether-test/udp-b",
	}
	declareDeps(c, svcs...)

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			out := make(map[string][]*registryv1.ServiceEndpoint, len(svcs))
			for i, svc := range svcs {
				// Two endpoints each, and a second advertised port on one, so the
				// per-port alias clusters and their vhosts are exercised as well.
				a := makeEndpoint(fmt.Sprintf("10.0.2.%d", 2*i+1), "cluster-1", "node-2", 8080)
				b := makeEndpoint(fmt.Sprintf("10.0.2.%d", 2*i+2), "cluster-1", "node-3", 8080)
				b.Ports = []uint32{8080, 9090}
				out[svc] = []*registryv1.ServiceEndpoint{a, b}
			}
			return out, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	return c
}

// gammaRouteTo builds a minimal prefix route to one backend cluster.
func gammaRouteTo(cluster string) proxy.GammaRoute {
	return proxy.GammaRoute{
		Matches:  []proxy.GammaMatch{{Prefix: "/"}},
		Backends: []proxy.GammaBackend{{Cluster: cluster, Weight: 1}},
	}
}

// mustAny returns the allow-listed filter's empty default config as an Any.
func mustAny(t *testing.T, filter string) *anypb.Any {
	t.Helper()
	msg, ok := extensionfilter.DefaultConfig(filter)
	require.Truef(t, ok, "%s must be allow-listed", filter)
	any, err := anypb.New(msg)
	require.NoError(t, err)
	return any
}

// fingerprintCache reduces everything the cache would hand Envoy to a map of
// digests. Two groups are covered, because they fail differently:
//
//   - "snapshot/<typeURL>": every resource in the node's snapshot, marshalled
//     with go-control-plane's own MarshalResource (deterministic marshal) and
//     visited in NAME order. Insensitive to the order of the per-type slice
//     (go-control-plane indexes resources by name), sensitive to the order of
//     every repeated field INSIDE a resource — which is the protocol-visible
//     class: RDS virtual_hosts, a vhost's routes, an HCM's http_filters.
//   - "ordered/<builder>": the slices the snapshot builders return, in the
//     order they return them. These are sorted so the cache emits a canonical
//     resource set, which keeps this test able to compare bytes at all.
func fingerprintCache(t *testing.T, c *SnapshotCache) map[string]string {
	t.Helper()

	out := make(map[string]string)

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	for _, typeURL := range []resourcev3.Type{
		resourcev3.ListenerType,
		resourcev3.ClusterType,
		resourcev3.EndpointType,
		resourcev3.RouteType,
		resourcev3.SecretType,
		resourcev3.ExtensionConfigType,
	} {
		items := snap.GetResources(typeURL)
		require.NotEmptyf(t, items, "the fixture must emit %s resources or this test proves nothing", typeURL)
		h := sha256.New()
		for _, name := range slices.Sorted(maps.Keys(items)) {
			b, err := cachev3.MarshalResource(items[name])
			require.NoError(t, err)
			_, _ = h.Write([]byte(name))
			_, _ = h.Write(b)
		}
		out["snapshot/"+typeURL] = hex.EncodeToString(h.Sum(nil))
	}

	clusters, endpoints, vhosts := c.clustersEndpointsAndVhosts()
	vhostResources := make([]types.Resource, 0, len(vhosts))
	for _, vh := range vhosts {
		vhostResources = append(vhostResources, vh)
	}
	captureVhostResources := make([]types.Resource, 0)
	for _, vh := range c.captureVhosts() {
		captureVhostResources = append(captureVhostResources, vh)
	}
	c.secretMu.RLock()
	secrets := make([]types.Resource, 0, len(c.secrets))
	for _, s := range c.secrets {
		secrets = append(secrets, s)
	}
	c.secretMu.RUnlock()
	sortResourcesByName(secrets)

	for name, ordered := range map[string][]types.Resource{
		"listeners":         c.Listeners(),
		"appClusters":       c.appClusters(),
		"captureUDPCluster": c.captureUDPClusters(),
		"clusters":          clusters,
		"endpoints":         endpoints,
		"outboundVhosts":    vhostResources,
		"captureVhosts":     captureVhostResources,
		"secrets":           secrets,
		"virtualHosts":      c.VirtualHosts(),
	} {
		require.NotEmptyf(t, ordered, "the fixture must emit %s or this test proves nothing", name)
		h := sha256.New()
		for _, r := range ordered {
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
			require.NoError(t, err)
			_, _ = h.Write(b)
		}
		out["ordered/"+name] = hex.EncodeToString(h.Sum(nil))
	}

	return out
}
