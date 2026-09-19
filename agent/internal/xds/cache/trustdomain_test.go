package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// The 2026-09-19 main-worker-03 outage (issue #815, PR #819) in one sentence:
// a listener-regeneration path read the cache's trust domain BEFORE blocking on
// listenerMu, so it rewrote every inbound chain from a value that was still
// empty, producing `spiffe:///ns/<ns>/sa/<sa>` SDS names the agent never serves.
// Envoy then held inbound listeners with no certificate, the new mTLS
// readiness probe could never pass, and four endpoints were demoted for good.
//
// These tests pin both halves of the fix: the losing interleaving itself, and
// the invariant that no emitted resource can ever carry the malformed form.

// malformedSpiffePrefix is the exact string an empty trust domain renders.
const malformedSpiffePrefix = "spiffe:///"

const raceTrustDomain = "aether.internal"

// allowAnyNetns makes the stale-netns guard accept the synthetic paths these
// tests use, so per-pod listeners actually reach the snapshot.
func allowAnyNetns(t *testing.T) {
	t.Helper()
	orig := netnsExists
	t.Cleanup(func() { netnsExists = orig })
	netnsExists = func(string) bool { return true }
}

func raceTestPod(name, netns string) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             name,
		Namespace:        "aether-test",
		ServiceAccount:   name,
		NetworkNamespace: netns,
	}
}

// assertNoMalformedIdentity fails if ANY resource in the node's snapshot — of
// any type, including config nested inside an Any — carries `spiffe:///`.
// Marshalling to the wire format is deliberate: a proto string lands in the
// bytes verbatim, so this reaches inside typed_config that a prototext dump
// would leave opaque (see the #135 determinism lesson).
func assertNoMalformedIdentity(t *testing.T, c *SnapshotCache, node string) {
	t.Helper()
	snap, err := c.GetSnapshot(node)
	require.NoError(t, err)

	for _, typeURL := range []resourcev3.Type{
		resourcev3.ListenerType, resourcev3.ClusterType, resourcev3.EndpointType,
		resourcev3.RouteType, resourcev3.SecretType, resourcev3.ExtensionConfigType,
	} {
		for name, res := range snap.GetResources(typeURL) {
			msg, ok := res.(proto.Message)
			require.True(t, ok)
			raw, marshalErr := proto.Marshal(msg)
			require.NoError(t, marshalErr)
			assert.NotContainsf(t, string(raw), malformedSpiffePrefix,
				"%s %q carries a SPIFFE ID built from an EMPTY trust domain", typeURL, name)
		}
	}
}

// inboundSecretNames returns the SDS server-certificate name of every inbound
// filter chain in the snapshot.
func inboundSecretNames(t *testing.T, c *SnapshotCache, node string) []string {
	t.Helper()
	snap, err := c.GetSnapshot(node)
	require.NoError(t, err)

	var names []string
	for name, res := range snap.GetResources(resourcev3.ListenerType) {
		l, ok := res.(*listenerv3.Listener)
		if !ok || len(name) < len("inbound_") || name[:len("inbound_")] != "inbound_" {
			continue
		}
		for _, fc := range l.GetFilterChains() {
			if secret := downstreamCertSecretName(fc.GetTransportSocket()); secret != "" {
				names = append(names, secret)
			}
		}
	}
	return names
}

// TestRegenerationDuringTrustDomainWindowKeepsIdentities reproduces the LOSING
// INTERLEAVING measured on main-worker-03.
//
// The state it recreates is the one LoadListenersFromStorage used to publish:
// a full per-pod listener map together with an EMPTY cache trust domain
// (listeners went into the map under listenerMu at line 584-600, the trust
// domain was recorded under localMu ~700 ms later). The gamma reconciler's
// first SetServiceChainFilters landed inside that window at 17:31:22.024Z,
// sampled "" for the trust domain, blocked on listenerMu for the rest of the
// load, and then rewrote all 15 chains:
//
//	inbound chain bound to a foreign identity
//	  pod=aether-test/svc-1-… bound_spiffe_id=spiffe:///ns/aether-test/sa/svc-1
//	  pod_spiffe_id=spiffe://aether.internal/ns/aether-test/sa/svc-1
//
// On the pre-fix code this test sees `spiffe:///…` chains. With the fix the
// regeneration refuses (proxy.ErrNoTrustDomain), the pod keeps the correct
// listener it already had, and the snapshot is unchanged.
func TestRegenerationDuringTrustDomainWindowKeepsIdentities(t *testing.T) {
	allowAnyNetns(t)
	c := newTestCache("node-1")
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, raceTestPod("svc-1", "/var/run/netns/cni-a"), raceTrustDomain))
	require.NoError(t, c.AddPod(ctx, raceTestPod("svc-2", "/var/run/netns/cni-b"), raceTrustDomain))

	want := inboundSecretNames(t, c, "node-1")
	require.NotEmpty(t, want)
	for _, name := range want {
		require.Contains(t, name, raceTrustDomain)
	}

	// Re-open the window: listeners published, trust domain not (yet) known.
	// This is the only way to reach that state now — which is the point.
	c.trustDomain.Store("")

	// The gamma reconciler's first reconcile lands here.
	c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{
		"aether-test/svc-1": {Name: proxy.ExtAuthzFilterName},
	})

	assertNoMalformedIdentity(t, c, "node-1")
	assert.ElementsMatch(t, want, inboundSecretNames(t, c, "node-1"),
		"a regeneration that cannot name the identity must leave the pod's listener alone, not republish it as spiffe:///")
}

// TestCaptureAndL4RegenerationDuringWindowKeepIdentities is the same
// interleaving through the other two regeneration entry points, which read the
// trust domain for the capture chains' source-identity filter state.
func TestCaptureAndL4RegenerationDuringWindowKeepIdentities(t *testing.T) {
	for name, regenerate := range map[string]func(c *SnapshotCache){
		"capture TCP services": func(c *SnapshotCache) {
			c.SetCaptureTCPServices(nil)
		},
		"L4 routes": func(c *SnapshotCache) {
			c.regenerateAllCaptureListeners()
		},
		"service inbound filters": func(c *SnapshotCache) {
			c.SetServiceInboundFilters(map[string]proxy.ExtensionFilter{"aether-test/svc-1": {Name: proxy.ExtAuthzFilterName}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			allowAnyNetns(t)
			c := newTestCache("node-1")
			c.SetCaptureEnabled(true)
			ctx := context.Background()
			require.NoError(t, c.AddPod(ctx, raceTestPod("svc-1", "/var/run/netns/cni-a"), raceTrustDomain))

			want := inboundSecretNames(t, c, "node-1")
			require.NotEmpty(t, want)

			c.trustDomain.Store("")
			regenerate(c)

			assertNoMalformedIdentity(t, c, "node-1")
			assert.ElementsMatch(t, want, inboundSecretNames(t, c, "node-1"))
		})
	}
}

// TestLoadListenersRecordsTrustDomainBeforePublishing pins the structural half
// of the fix: the window above must not exist in the first place. The cache's
// trust domain has to be in force before a single listener enters the map, so a
// concurrent regeneration can never observe listeners without it.
func TestLoadListenersRecordsTrustDomainBeforePublishing(t *testing.T) {
	allowAnyNetns(t)
	c := newTestCache("node-1")
	ctx := context.Background()

	pod := raceTestPod("svc-1", "/var/run/netns/cni-a")
	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		// Observed at the moment the load reads storage — i.e. before any
		// listener has been generated, let alone published.
		assert.Equal(t, raceTrustDomain, c.currentTrustDomain(),
			"the trust domain must be recorded before the listener map is touched")
		return []*cniv1.CNIPod{pod}, nil
	})
	require.NoError(t, store.AddResource(ctx, types.ContainerID("c-1"), pod))

	require.NoError(t, c.LoadListenersFromStorage(ctx, store, raceTrustDomain))
	assertNoMalformedIdentity(t, c, "node-1")
}

// TestNoMalformedIdentityUnderArbitraryMutatorOrder is the property-style
// guard the re-land owes: whatever order the cache's public mutators are
// called in, and however late the trust domain arrives, nothing the cache
// emits may ever reference `spiffe:///`.
//
// The mutators run concurrently on purpose — this is the shape of the real
// failure (a reconciler racing a bulk load), and under `--config=race` it also
// covers the lock discipline the fix depends on.
func TestNoMalformedIdentityUnderArbitraryMutatorOrder(t *testing.T) {
	for round := range 8 {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			allowAnyNetns(t)
			c := newTestCache("node-1")
			c.SetCaptureEnabled(true)
			ctx := context.Background()

			pods := []*cniv1.CNIPod{
				raceTestPod("svc-1", "/var/run/netns/cni-a"),
				raceTestPod("svc-2", "/var/run/netns/cni-b"),
				raceTestPod("svc-3", "/var/run/netns/cni-c"),
			}
			store := storage.NewMockStorageWithGetAll(func(_ context.Context) ([]*cniv1.CNIPod, error) {
				return pods, nil
			})
			for i, p := range pods {
				require.NoError(t, store.AddResource(ctx, types.ContainerID(fmt.Sprintf("c-%d", i)), p))
			}

			mutators := []func(){
				func() { _ = c.LoadListenersFromStorage(ctx, store, raceTrustDomain) },
				func() { _ = c.AddPod(ctx, pods[0], raceTrustDomain) },
				func() { _ = c.SetTrustDomain(ctx, raceTrustDomain) },
				func() { _ = c.SetNodeIdentity(ctx, nodeIdentity) },
				func() {
					c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{
						fmt.Sprintf("aether-test/svc-%d", round%3+1): {Name: proxy.ExtAuthzFilterName},
					})
				},
				func() { c.SetCaptureTCPServices(nil) },
				func() { c.regenerateAllCaptureListeners() },
				func() { c.regenerateAllHTTPListeners() },
			}
			// Rotate the starting point per round so no single order is the
			// only one ever exercised.
			var wg sync.WaitGroup
			for i := range mutators {
				wg.Add(1)
				go func(m func()) {
					defer wg.Done()
					m()
				}(mutators[(i+round)%len(mutators)])
			}
			wg.Wait()

			require.NoError(t, c.generateSnapshot(ctx))
			assertNoMalformedIdentity(t, c, "node-1")
		})
	}
}

// TestTrustDomainChangeRebuildsEverythingAtomically: when SPIRE resolves a
// trust domain that differs from the seed, every identity-derived resource has
// to be rebuilt, and the snapshot that results must not mix the two.
func TestTrustDomainChangeRebuildsEverythingAtomically(t *testing.T) {
	allowAnyNetns(t)
	c := newTestCache("node-1")
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, raceTestPod("svc-1", "/var/run/netns/cni-a"), "mesh.seed"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	for _, name := range inboundSecretNames(t, c, "node-1") {
		require.Contains(t, name, "mesh.seed")
	}

	require.NoError(t, c.SetTrustDomain(ctx, raceTrustDomain))

	names := inboundSecretNames(t, c, "node-1")
	require.NotEmpty(t, names)
	for _, name := range names {
		assert.Contains(t, name, raceTrustDomain)
		assert.NotContains(t, name, "mesh.seed", "no resource may survive carrying the previous trust domain")
	}
	assertNoMalformedIdentity(t, c, "node-1")
}

// TestSetTrustDomainRefusesEmpty: an empty value must never displace a known
// trust domain. "" only ever means an uninitialised caller — accepting it would
// un-name every SDS secret in the snapshot.
func TestSetTrustDomainRefusesEmpty(t *testing.T) {
	c := newTestCache("node-1")
	require.True(t, c.setTrustDomain(raceTrustDomain))
	assert.False(t, c.setTrustDomain(""), "an empty trust domain is not a change")
	assert.Equal(t, raceTrustDomain, c.currentTrustDomain())
	assert.Equal(t, "spiffe://"+raceTrustDomain, c.validationContextName())

	empty := newTestCache("node-2")
	assert.Empty(t, empty.validationContextName(), "no trust domain must yield no name, never the bare \"spiffe://\"")
}

// TestNoResourceBytesCarryMalformedPrefix is a belt-and-braces check that the
// scan above would actually catch the regression it guards, so a future change
// cannot quietly turn it into a tautology.
func TestNoResourceBytesCarryMalformedPrefix(t *testing.T) {
	l := &listenerv3.Listener{Name: "canary"}
	raw, err := proto.Marshal(l)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), malformedSpiffePrefix)

	l.Name = proxy.SpiffeIDFromPod(raceTestPod("svc-1", "/var/run/netns/cni-a"), "")
	assert.Empty(t, l.GetName(), "SpiffeIDFromPod must return \"\" for an unknown trust domain, never spiffe:///")

	l.Name = "spiffe:///ns/aether-test/sa/svc-1"
	raw, err = proto.Marshal(l)
	require.NoError(t, err)
	assert.Contains(t, string(raw), malformedSpiffePrefix, "the wire-bytes scan must detect the malformed form")
}
