package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #1049 gates, at the cache.
//
// rev248 phase C (talos, 2026-09-28 15:44Z): k6 loader pods of ServiceAccount
// aether-test/default landed on every node at 15:44:46.0-46.1 and their first
// requests routed to the `quic:` twins of svc-1/svc-2. The agent published
// every twin within 10 ms of the need for it (a dormant pair republished by
// the CNI ADD's own snapshot at 46.05; a first-use pair 10 ms after its ODCDS
// request at 49.996) -- but each twin names its source's SVID statically in
// its transport socket, and SPIRE delivered that SVID only at 52.99-53.53.
// Envoy warmed the twins on SDS until then, the on_demand filter's 2 s timeout
// fired first, and 428 requests 503'd NC cluster_not_found.
//
// So an identity whose certificate the snapshot does not carry yet gets neither
// a selection arm nor a twin: its requests ride the warm h2 cluster. The
// snapshot that brings the certificate brings the arm, the twin and the twin's
// load assignment together.

func quicSecrets(sas ...string) []*tlsv3.Secret {
	out := []*tlsv3.Secret{{Name: nodeIdentity}, {Name: "ROOTCA"}}
	for _, sa := range sas {
		out = append(out, &tlsv3.Secret{Name: spiffeOf(sa)})
	}
	return out
}

// A first-use pair whose source has no certificate yet is recorded, but its
// twin -- and every source's arm without a certificate -- is held until the
// certificate lands, then published in that same snapshot.
//
// Red on the pre-#1049 cache: source-c's arm is on the routes and its twin is
// in CDS naming a secret the snapshot does not carry (Envoy warms it on SDS
// for the whole SVID latency, and the arm routes requests into that wait).
func TestQUICTwinAndArmWaitForTheSourceCertificate(t *testing.T) {
	c := newQUICDemandCache(t, "")
	ctx := context.Background()
	require.NoError(t, c.SetSecrets(ctx, quicSecrets("source-a", "source-b")))
	twinA, twinC := echoTwin(c, "source-a"), echoTwin(c, "source-c")

	observeQUIC(t, c, "demo/echo", "demo/source-a", "demo/source-c")
	st := readQUICState(t, c)
	assert.Equal(t, []string{twinA}, st.twins, "source-c has no certificate yet: its twin would warm on SDS, so it is held")
	assert.Equal(t, []string{twinA}, st.twinCLAs)
	requireSelections(t, c, st, allArms(c, "source-a", "source-b"))
	assert.Equal(t, []string{twinA, twinC}, c.QUICPairs(), "the pair is recorded all the same: it is demand")

	// SPIRE delivers source-c's SVID: the bridge's SetSecrets snapshot carries
	// the arm, the twin and its load assignment together, with no request.
	require.NoError(t, c.SetSecrets(ctx, quicSecrets(quicDemandSAs...)))
	st = readQUICState(t, c)
	assert.ElementsMatch(t, []string{twinA, twinC}, st.twins)
	assert.ElementsMatch(t, []string{twinA, twinC}, st.twinCLAs)
	requireSelections(t, c, st, allArms(c, quicDemandSAs...))
	for _, twin := range st.twins {
		assert.Contains(t, st.snap.GetResources(resourcev3.SecretType),
			twinSource(t, c, twin), "every published twin's client certificate is in the same snapshot")
	}
}

// The incident's exact shape: a dormant pair restored from the ledger after an
// agent restart, and its source's first pod arriving on the node. The pod's
// own snapshot (the CNI ADD) revives the pair but must not publish a twin whose
// certificate is still in flight; the certificate's snapshot publishes it.
func TestQUICDormantPairReturnAfterLedgerReloadWaitsForCertificate(t *testing.T) {
	path := filepath.Join(t.TempDir(), ObservedUpstreamsFile)
	ctx := context.Background()
	c := newQUICDemandCache(t, path)
	observeQUIC(t, c, "demo/echo", "demo/source-a")
	require.NoError(t, c.RemovePod(ctx, "/var/run/netns/cni-source-a"))
	require.Equal(t, []string{echoTwin(c, "source-a")}, c.DormantQUICPairs())
	c.FlushObservedUpstreams()

	// Agent restart with source-a away; SPIRE serves the identities present.
	restarted := newQUICDemandCacheWith(t, path, "source-b", "source-c")
	twinA := echoTwin(restarted, "source-a")
	require.NoError(t, restarted.SetSecrets(ctx, quicSecrets("source-b", "source-c")))
	require.Equal(t, []string{twinA}, restarted.DormantQUICPairs(), "restored from the ledger")
	restarted.RestateQUICSubscriptions(ctx, testQUICStream, []string{twinA})
	require.Equal(t, []string{twinA}, restarted.DormantQUICPairs(), "the proxy still subscribes to it")

	// k6's new pod lands (CNI ADD). The pair is valid again, but the SVID is not
	// here yet.
	addSourcePod(t, restarted, "source-a", "/var/run/netns/cni-source-a-new")
	st := readQUICState(t, restarted)
	assert.Equal(t, []string{twinA}, restarted.QUICPairs(), "revived by the CNI ADD's snapshot")
	assert.Empty(t, st.twins, "but its twin waits for source-a's certificate")
	requireSelections(t, restarted, st, allArms(restarted, "source-b", "source-c"))

	// The SVID arrives: republished with no request, arm and all.
	require.NoError(t, restarted.SetSecrets(ctx, quicSecrets(quicDemandSAs...)))
	st = readQUICState(t, restarted)
	assert.Equal(t, []string{twinA}, st.twins)
	assert.Equal(t, []string{twinA}, st.twinCLAs)
	requireSelections(t, restarted, st, allArms(restarted, quicDemandSAs...))
}

// Before SPIRE serves anything every mTLS cluster is equally blocked, so the
// gate holds nothing back: the fan-out keeps its pre-#1049 shape.
func TestQUICCertificateGateInactiveWithoutAnySecret(t *testing.T) {
	c := newQUICDemandCache(t, "")
	observeQUIC(t, c, "demo/echo", "demo/source-b")
	st := readQUICState(t, c)
	assert.Equal(t, []string{echoTwin(c, "source-b")}, st.twins)
	requireSelections(t, c, st, allArms(c, quicDemandSAs...))
}

// The agent's side of first use is not serialized behind a registry reload
// (issue #1049 asked whether it was): the reload's slow part -- the registry
// listing -- runs under no cache lock, so an ODCDS request's twin is published
// while one is in flight. Measured, not assumed: the elapsed time is logged
// and must stay well under the on_demand timeout.
func TestQUICFirstUsePublishedWhileARegistryReloadIsInFlight(t *testing.T) {
	c := newQUICDemandCache(t, "")
	ctx := context.Background()
	require.NoError(t, c.SetSecrets(ctx, quicSecrets(quicDemandSAs...)))

	entered, release := make(chan struct{}), make(chan struct{})
	reloaded := make(chan error, 1)
	blocking := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol != registryv1.Service_PROTOCOL_HTTP {
				return map[string][]*registryv1.ServiceEndpoint{}, nil
			}
			close(entered)
			<-release
			return map[string][]*registryv1.ServiceEndpoint{
				"demo/echo":  {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 8080)},
				"demo/other": {makeEndpoint("10.0.3.2", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	go func() { reloaded <- c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", blocking) }()
	<-entered
	t.Cleanup(func() {
		close(release)
		require.NoError(t, <-reloaded)
	})

	twin := echoTwin(c, "source-b")
	start := time.Now()
	decision, reason := c.ObserveQUICTwin(ctx, testQUICStream, twin)
	require.Equal(t, QUICTwinAdded, decision, reason)
	require.Eventually(t, func() bool {
		snap, err := c.GetSnapshot("node-1")
		if err != nil {
			return false
		}
		_, ok := snap.GetResources(resourcev3.ClusterType)[twin]
		return ok
	}, time.Second, time.Millisecond, "the twin must be published within 1 s of its ODCDS request, reload or not")
	elapsed := time.Since(start)
	t.Logf("ODCDS request -> twin in the snapshot: %s (a registry reload blocked throughout)", elapsed)
	select {
	case <-reloaded:
		t.Fatal("the reload finished early: the test did not overlap it")
	default:
	}
}

// twinSource returns the client-certificate secret a published twin names.
func twinSource(t *testing.T, c *SnapshotCache, twin string) string {
	t.Helper()
	for _, sa := range quicDemandSAs {
		if echoTwin(c, sa) == twin {
			return spiffeOf(sa)
		}
	}
	t.Fatalf("unknown twin %s", twin)
	return ""
}
