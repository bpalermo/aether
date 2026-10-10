package cache

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
	"github.com/stretchr/testify/require"
)

// Issue #1105: what one snapshot build costs at a realistic node shape, and how
// much of it the ADS stream waits out behind the snapshot-cache mutex.
//
// The shape is a busy reference-cluster worker: 50 local pods (10 ServiceAccounts), 100
// in-scope mesh Services with 3 endpoints each, one SVID per pod, capture with
// redirect-all on, and 10 QUIC twins. Every xDS type has an open delta watch,
// as on a live node: go-control-plane only builds a snapshot's version map
// (deterministic marshal + sha256 of EVERY resource) when a delta watch exists,
// so without them the benchmark would not see the cost at all.
//
// Run it under a 200m-like quota and one P, e.g.
//
//	bazel build //agent/internal/xds/cache:cache_test
//	systemd-run --user --scope -p CPUQuota=20% -- env GOMAXPROCS=1 \
//	  bazel-bin/agent/internal/xds/cache/cache_test_/cache_test \
//	  -test.run '^$' -test.bench BenchmarkSnapshotBuild -test.benchtime 40x

const (
	buildBenchPods     = 50
	buildBenchSAs      = 10
	buildBenchServices = 100
	buildBenchTwins    = 10
)

func buildBenchService(i int) string { return fmt.Sprintf("demo/svc-%03d", i) }

// buildBenchEndpoints is the registry listing; flip perturbs one service's
// endpoint set so a registry refresh is a real (small) change.
func buildBenchEndpoints(flip int) map[string][]*registryv1.ServiceEndpoint {
	out := make(map[string][]*registryv1.ServiceEndpoint, buildBenchServices)
	for i := range buildBenchServices {
		eps := make([]*registryv1.ServiceEndpoint, 0, 3)
		for j := range 3 {
			node := fmt.Sprintf("node-%d", 2+j)
			if j == 0 && i%5 == 0 {
				node = "node-1" // some endpoints are node-local
			}
			eps = append(eps, makeEndpoint(fmt.Sprintf("10.%d.%d.%d", 2+j, i/250, 1+i%250), "cluster-1", node, 8080))
		}
		if i == flip%buildBenchServices {
			eps = eps[:2]
		}
		out[buildBenchService(i)] = eps
	}
	return out
}

// fakeSVID is secret material sized like a real SPIRE SVID: a ~1.5 KiB chain
// and a ~240 B key. Its content is irrelevant; its size is what is hashed.
func fakeSVID(name string) *tlsv3.Secret {
	chain := strings.Repeat("C", 1536) + name
	key := strings.Repeat("K", 240) + name
	return &tlsv3.Secret{
		Name: name,
		Type: &tlsv3.Secret_TlsCertificate{TlsCertificate: &tlsv3.TlsCertificate{
			CertificateChain: &corev3.DataSource{Specifier: &corev3.DataSource_InlineBytes{InlineBytes: []byte(chain)}},
			PrivateKey:       &corev3.DataSource{Specifier: &corev3.DataSource_InlineBytes{InlineBytes: []byte(key)}},
		}},
	}
}

func newBuildBenchCache(tb testing.TB) (*SnapshotCache, *mockRegistry, *atomic.Int64) {
	tb.Helper()
	ctx := context.Background()
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	secrets := []*tlsv3.Secret{fakeSVID(nodeIdentity), fakeSVID("ROOTCA")}
	for i := range buildBenchPods {
		sa := fmt.Sprintf("sa-%d", i%buildBenchSAs)
		require.NoError(tb, c.AddPod(ctx, &cniv1.CNIPod{
			Name: fmt.Sprintf("app-%02d", i), Namespace: "demo", ServiceAccount: sa,
			NetworkNamespace: fmt.Sprintf("/var/run/netns/cni-app-%02d", i),
			ContainerId:      fmt.Sprintf("c-%02d", i),
			Ips:              []string{fmt.Sprintf("10.1.0.%d", i+1)},
			Labels:           map[string]string{"app": fmt.Sprintf("svc-%03d", i%buildBenchServices)},
		}, quicDemandTD))
		// One secret per pod for the SDS byte volume; the first pod of each
		// ServiceAccount carries the SA's exact SPIFFE ID, so the QUIC
		// fan-out sees its client certificate and the 10 observed twins
		// below are actually published (#1115: with only "#<i>" names every
		// identity was awaiting its certificate and no twin was built).
		name := fmt.Sprintf("spiffe://%s/ns/demo/sa/%s", quicDemandTD, sa)
		if i >= buildBenchSAs {
			name = fmt.Sprintf("%s#%d", name, i)
		}
		secrets = append(secrets, fakeSVID(name))
	}
	require.NoError(tb, c.SetNodeIdentity(ctx, nodeIdentity))
	require.NoError(tb, c.SetSecrets(ctx, secrets))

	authorities := make(map[string]string, buildBenchServices)
	services := make([]string, 0, buildBenchServices)
	for i := range buildBenchServices {
		svc := buildBenchService(i)
		authorities[svc] = strings.TrimPrefix(svc, "demo/") + ".demo.svc.cluster.local"
		services = append(services, svc)
	}
	c.SetCaptureAuthorities(authorities)
	declareDeps(c, services...)

	var flip atomic.Int64
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return buildBenchEndpoints(int(flip.Load())), nil
		},
	}
	require.NoError(tb, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	c.markLocalPodsSynced()
	for i := range buildBenchTwins {
		d, reason := c.recordQUICPair(testQUICStream, proxy.QUICClusterName(buildBenchService(i), c.meshDomain, "demo/sa-0"))
		require.NotEqual(tb, QUICTwinRefused, d, reason)
	}
	require.NoError(tb, c.generateSnapshot(ctx))
	return c, reg, &flip
}

// buildBenchTypes are the types a live node proxy holds a delta watch for.
var buildBenchTypes = []string{
	resourcev3.ListenerType, resourcev3.ClusterType, resourcev3.EndpointType,
	resourcev3.RouteType, resourcev3.SecretType, resourcev3.ExtensionConfigType,
}

// armDeltaWatches opens one wildcard delta watch per type, already up to date
// with the current snapshot, so the next SetSnapshot has watches to answer --
// exactly the state the ADS stream leaves between responses.
func armDeltaWatches(tb testing.TB, c *SnapshotCache) []func() {
	tb.Helper()
	cancels := make([]func(), 0, len(buildBenchTypes))
	for _, typ := range buildBenchTypes {
		_, cancel := openUpToDateDeltaWatch(tb, c, typ)
		cancels = append(cancels, cancel)
	}
	return cancels
}

// openUpToDateDeltaWatch opens a wildcard delta watch for typ whose subscriber
// already holds every version of the current snapshot, as an ADS stream does
// after ACKing it, and returns the channel the next response lands on.
func openUpToDateDeltaWatch(tb testing.TB, c *SnapshotCache, typ string) (chan cachev3.DeltaResponse, func()) {
	tb.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(tb, err)
	require.NoError(tb, snap.ConstructVersionMap())
	sub := streamv3.NewDeltaSubscription(nil, nil, snap.GetVersionMap(typ), true)
	// A non-empty nonce: an ACK, not the stream's first wildcard request
	// (which go-control-plane always answers at once).
	req := &discoveryv3.DeltaDiscoveryRequest{Node: &corev3.Node{Id: "node-1"}, TypeUrl: typ, ResponseNonce: "1"}
	ch := make(chan cachev3.DeltaResponse, 1)
	cancel, err := c.CreateDeltaWatch(req, &sub, ch)
	require.NoError(tb, err)
	require.NotNil(tb, cancel, "the %s watch must stay open (the subscriber is up to date)", typ)
	return ch, cancel
}

// stallProbe stands in for the ADS stream goroutine: it takes the snapshot
// cache's mutex over and over (as every ADS request does, via watch cancel +
// CreateDeltaWatch) and records the longest single wait.
type stallProbe struct {
	c    *SnapshotCache
	stop chan struct{}
	done sync.WaitGroup
	mu   sync.Mutex
	max  time.Duration
}

func startStallProbe(c *SnapshotCache) *stallProbe {
	p := &stallProbe{c: c, stop: make(chan struct{})}
	p.done.Add(1)
	go func() {
		defer p.done.Done()
		for {
			select {
			case <-p.stop:
				return
			default:
			}
			t0 := time.Now()
			_, _ = c.GetSnapshot("node-1")
			d := time.Since(t0)
			p.mu.Lock()
			p.max = max(p.max, d)
			p.mu.Unlock()
			// ~1k requests/s: far busier than a real ADS stream, light enough
			// not to eat the CPU quota the build is measured under.
			time.Sleep(time.Millisecond)
		}
	}()
	return p
}

func (p *stallProbe) reset() {
	p.mu.Lock()
	p.max = 0
	p.mu.Unlock()
}

func (p *stallProbe) worst() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

func (p *stallProbe) close() {
	close(p.stop)
	p.done.Wait()
}

func pct(ds []time.Duration, q float64) float64 {
	s := slices.Clone(ds)
	slices.Sort(s)
	i := int(float64(len(s)-1) * q)
	return float64(s[i].Microseconds()) / 1000
}

// cloneResources copies the current snapshot's resources into a fresh,
// unhashed snapshot -- what a build hands SetSnapshot.
func cloneSnapshot(tb testing.TB, c *SnapshotCache, version string) *cachev3.Snapshot {
	tb.Helper()
	snap, err := c.GetSnapshot("node-1")
	require.NoError(tb, err)
	res := make(map[resourcev3.Type][]types.Resource, len(buildBenchTypes))
	for _, typ := range buildBenchTypes {
		for _, r := range snap.GetResources(typ) {
			res[typ] = append(res[typ], r)
		}
	}
	out, err := cachev3.NewSnapshot(version, res)
	require.NoError(tb, err)
	return out
}

// BenchmarkSnapshotBuild reports, per build: total wall clock (p50/p99) and
// the longest the ADS stand-in waited for the cache mutex (p50/p99). Two
// triggers: "rebuild" (nothing changed -- the same-shape rebuilds of the
// #1086 timeline), "registry-refresh" (the registry listing is reloaded and
// two services' endpoints changed since the last refresh: the one perturbed
// now and the one perturbed last time, restored) and "registry-refresh-
// nochange" (the listing is reloaded and nothing in it changed).
//
// versions-memo/build and versions-hashed/build are the
// aether.agent.snapshot.resource_versions{source="memo"|"hashed"} split: a
// resource is a memo hit iff it is the same object under the same name as in
// the previous snapshot (versionMemo.version, outside an audit).
//
// BenchmarkSnapshotVersioning (versionmemo_test.go) splits out the
// versioning cost itself.
func BenchmarkSnapshotBuild(b *testing.B) {
	ctx := context.Background()
	run := func(b *testing.B, step func(c *SnapshotCache, reg *mockRegistry, flip *atomic.Int64, i int)) {
		c, reg, flip := newBuildBenchCache(b)
		productionMemo(c)
		defer productionReuse()()
		probe := startStallProbe(c)
		defer probe.close()
		var total, stall []time.Duration
		var memoHits, hashed int
		b.ResetTimer()
		for i := range b.N {
			b.StopTimer()
			cancels := armDeltaWatches(b, c)
			probe.reset()
			before, err := c.GetSnapshot("node-1")
			require.NoError(b, err)
			b.StartTimer()
			t0 := time.Now()
			step(c, reg, flip, i)
			total = append(total, time.Since(t0))
			b.StopTimer()
			after, err := c.GetSnapshot("node-1")
			require.NoError(b, err)
			h, n := memoSplit(before, after)
			memoHits += h
			hashed += n
			stall = append(stall, probe.worst())
			for _, cancel := range cancels {
				cancel()
			}
			b.StartTimer()
		}
		b.StopTimer()
		b.ReportMetric(pct(total, 0.5), "build-p50-ms")
		b.ReportMetric(pct(total, 0.99), "build-p99-ms")
		b.ReportMetric(pct(stall, 0.5), "ads-stall-p50-ms")
		b.ReportMetric(pct(stall, 0.99), "ads-stall-p99-ms")
		b.ReportMetric(float64(memoHits)/float64(b.N), "versions-memo/build")
		b.ReportMetric(float64(hashed)/float64(b.N), "versions-hashed/build")
	}
	b.Run("rebuild", func(b *testing.B) {
		run(b, func(c *SnapshotCache, _ *mockRegistry, _ *atomic.Int64, _ int) {
			require.NoError(b, c.generateSnapshot(ctx))
		})
	})
	b.Run("registry-refresh", func(b *testing.B) {
		run(b, func(c *SnapshotCache, reg *mockRegistry, flip *atomic.Int64, i int) {
			flip.Store(int64(i + 1))
			require.NoError(b, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		})
	})
	b.Run("registry-refresh-nochange", func(b *testing.B) {
		run(b, func(c *SnapshotCache, reg *mockRegistry, _ *atomic.Int64, _ int) {
			require.NoError(b, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
		})
	})
}

// memoSplit counts, across every type, the resources of after that are the
// same object under the same name as in before (version-memo hits) and the
// rest (hashed).
func memoSplit(before, after cachev3.ResourceSnapshot) (hits, hashed int) {
	for i := range types.UnknownType {
		typ, err := cachev3.GetResponseTypeURL(types.ResponseType(i))
		if err != nil {
			continue
		}
		prev := before.GetResources(typ)
		for name, r := range after.GetResources(typ) {
			if prev[name] == r {
				hits++
			} else {
				hashed++
			}
		}
	}
	return hits, hashed
}
