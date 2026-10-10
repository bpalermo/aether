package cache

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Issue #1584: two pods with the same NAME in different namespaces on one node
// (two StatefulSets called "web", each with a "web-0").
//
// The per-pod xDS resource names used to derive from the pod name alone, so
// the two pods shared every one of them. The cache built two resources under
// each name and go-control-plane kept one, by map order, independently for
// listeners and clusters: one pod lost its listeners and the other's inbound
// listener could deliver to the first pod's application. The names now carry
// the namespace (proxy.PodResourceKey), and these tests pin what follows:
// both pods are published, each with its own resources, on every build.

const (
	sameNameNetnsA = "/var/run/netns/cni-aaaa"
	sameNameNetnsB = "/var/run/netns/cni-bbbb"
	sameNameNetnsC = "/var/run/netns/cni-cccc"
	sameNameIDA    = "spiffe://aether.internal/ns/ns-a/sa/web"
	sameNameIDB    = "spiffe://aether.internal/ns/ns-b/sa/web"
	// sameNameBuilds is how many consecutive snapshot builds the "it never
	// moves" test samples. Before the fix the published pod was redrawn from
	// map order on every build (about 1:7 on a two-entry map, measured), so
	// the chance of 256 builds all drawing the same way was (7/8)^256.
	sameNameBuilds = 256

	duplicateNamesCtr = "aether.agent.snapshot.duplicate_resource_names"
	duplicateNamesMsg = "more than one resource of a type carries the same name: the proxy is sent only one of them, and which one can change from build to build"
)

func sameNamePod(namespace, netns, containerID string) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             "web-0",
		Namespace:        namespace,
		ServiceAccount:   "web",
		NetworkNamespace: netns,
		ContainerId:      containerID,
	}
}

func sameNamePodA() *cniv1.CNIPod { return sameNamePod("ns-a", sameNameNetnsA, "container-a") }
func sameNamePodB() *cniv1.CNIPod { return sameNamePod("ns-b", sameNameNetnsB, "container-b") }

// perPodListenerNames and perPodClusterNames are everything the cache
// publishes for one pod in this fixture (capture on, SPIRE on, node SVID
// served), spelled out so a renamed or dropped resource fails the test.
func perPodListenerNames(ns string) []string {
	return []string{"inbound_" + ns + "_web-0", "inbound_" + ns + "_web-0_h3", "outbound_http_" + ns + "_web-0", "capture_" + ns + "_web-0"}
}

func perPodClusterNames(ns string) []string {
	return []string{"app_" + ns + "_web-0_8080", "health_" + ns + "_web-0", "inboundready_" + ns + "_web-0"}
}

// newSameNameCache is a mesh-mode cache with capture on, both pods' SVIDs
// served and the node identity in force, so every per-pod resource kind the
// agent builds is in the snapshot.
func newSameNameCache(t *testing.T) (*SnapshotCache, *recorder, *sdkmetric.ManualReader) {
	t.Helper()
	c, rec, reader := newBindingTestCache(t)
	c.SetCaptureEnabled(true)
	serveSecrets(c, sameNameIDA, sameNameIDB, inboundTrustBundleSDS)
	require.NoError(t, c.SetNodeIdentity(context.Background(), nodeIdentity))
	return c, rec, reader
}

// publishedListeners is the listener set of the snapshot the cache serves: what
// go-control-plane hands the proxy, indexed by resource name.
func publishedListeners(t *testing.T, c *SnapshotCache) map[string]*listenerv3.Listener {
	t.Helper()
	snap, err := c.GetSnapshot(c.nodeName)
	require.NoError(t, err)
	out := map[string]*listenerv3.Listener{}
	for name, r := range snap.GetResources(resourcev3.ListenerType) {
		out[name] = r.(*listenerv3.Listener)
	}
	return out
}

func publishedClusters(t *testing.T, c *SnapshotCache) map[string]*clusterv3.Cluster {
	t.Helper()
	snap, err := c.GetSnapshot(c.nodeName)
	require.NoError(t, err)
	out := map[string]*clusterv3.Cluster{}
	for name, r := range snap.GetResources(resourcev3.ClusterType) {
		out[name] = r.(*clusterv3.Cluster)
	}
	return out
}

func listenerNetns(l *listenerv3.Listener) string {
	return l.GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath()
}

func clusterNetns(c *clusterv3.Cluster) string {
	return c.GetUpstreamBindConfig().GetSourceAddress().GetNetworkNamespaceFilepath()
}

// inboundCert is the SDS name of the server certificate the inbound listener
// presents.
func inboundCert(l *listenerv3.Listener) string {
	for _, fc := range l.GetFilterChains() {
		if name := downstreamCertSecretName(fc.GetTransportSocket()); name != "" {
			return name
		}
	}
	return ""
}

// requirePodPublished asserts every per-pod resource of the pod <ns>/web-0 is
// in the published snapshot, bound into netns, presenting id and naming
// nothing of the other pod (otherNS, otherID).
func requirePodPublished(t *testing.T, c *SnapshotCache, build int, ns, netns, id, otherNS, otherID string) {
	t.Helper()
	listeners, clusters := publishedListeners(t, c), publishedClusters(t, c)
	for _, name := range perPodListenerNames(ns) {
		require.Containsf(t, listeners, name, "build %d: %s is published", build, name)
		require.Equalf(t, netns, listenerNetns(listeners[name]), "build %d: %s is bound into its own pod's netns", build, name)
		text := listeners[name].String()
		require.NotContainsf(t, text, otherID, "build %d: %s carries nothing of the other pod's identity", build, name)
		require.NotContainsf(t, text, otherNS+"_web-0", "build %d: %s names nothing of the other pod's", build, name)
	}
	inbound := listeners["inbound_"+ns+"_web-0"]
	require.Equalf(t, id, inboundCert(inbound), "build %d: the inbound listener presents its own pod's SVID", build)
	require.Containsf(t, inbound.String(), "app_"+ns+"_web-0_8080", "build %d: and routes to its own pod's app cluster", build)
	for _, name := range perPodClusterNames(ns) {
		require.Containsf(t, clusters, name, "build %d: %s is published", build, name)
	}
	for _, name := range []string{"app_" + ns + "_web-0_8080", "health_" + ns + "_web-0"} {
		require.Equalf(t, netns, clusterNetns(clusters[name]), "build %d: %s dials its own pod's loopback", build, name)
	}
	ready := clusters["inboundready_"+ns+"_web-0"]
	require.Containsf(t, ready.String(), netns, "build %d: the readiness probe dials its own pod's inbound listener", build)
	require.Containsf(t, ready.String(), id, "build %d: and pins its own pod's identity", build)
	require.NotContainsf(t, ready.String(), otherID, "build %d", build)
}

// TestSameNamedPodsOfTwoNamespacesAreBothPublished: two pods of one name in
// two namespaces on one node are BOTH in the snapshot, each with its own
// netns, SVID and app cluster, and that is so on every one of 256 consecutive
// builds: nothing is decided by map order any more.
//
// Covered for both ways a pod reaches the cache: a CNI ADD and a storage load.
func TestSameNamedPodsOfTwoNamespacesAreBothPublished(t *testing.T) {
	for _, path := range []string{"cni-add", "storage-load"} {
		t.Run(path, func(t *testing.T) {
			c, rec, reader := newSameNameCache(t)
			ctx := context.Background()
			if path == "cni-add" {
				require.NoError(t, c.AddPod(ctx, sameNamePodA(), bindingTrustDomain))
				require.NoError(t, c.AddPod(ctx, sameNamePodB(), bindingTrustDomain))
			} else {
				initListeners(c)
				store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(context.Context) ([]*cniv1.CNIPod, error) {
					return []*cniv1.CNIPod{sameNamePodA(), sameNamePodB()}, nil
				})
				require.NoError(t, c.LoadListenersFromStorage(ctx, store, bindingTrustDomain))
			}

			var firstListeners, firstClusters map[string]string
			for build := range sameNameBuilds {
				if build > 0 {
					require.NoError(t, c.generateSnapshot(ctx))
				}
				requirePodPublished(t, c, build, "ns-a", sameNameNetnsA, sameNameIDA, "ns-b", sameNameIDB)
				requirePodPublished(t, c, build, "ns-b", sameNameNetnsB, sameNameIDB, "ns-a", sameNameIDA)

				// Nothing else is published, and nothing changes between
				// builds: the per-resource versions are the bytes the proxy is
				// sent.
				snap, err := c.GetSnapshot(c.nodeName)
				require.NoError(t, err)
				require.Lenf(t, snap.GetResources(resourcev3.ListenerType), 2*len(perPodListenerNames(""))+1, "build %d: both pods' listeners and the health gateway", build)
				lv, cv := snap.GetVersionMap(resourcev3.ListenerType), snap.GetVersionMap(resourcev3.ClusterType)
				if build == 0 {
					firstListeners, firstClusters = lv, cv
					continue
				}
				require.Equalf(t, firstListeners, lv, "build %d: no listener changed", build)
				require.Equalf(t, firstClusters, cv, "build %d: no cluster changed", build)
			}

			// Each pod has its own health gateway paths.
			paths := gatewayMinHealthy(t, c)
			for _, ns := range []string{"ns-a", "ns-b"} {
				assert.Equal(t, []string{"health_" + ns + "_web-0"}, paths["/healthz/health_"+ns+"_web-0"])
				assert.Equal(t, []string{"inboundready_" + ns + "_web-0"}, paths["/healthz/inboundready_"+ns+"_web-0"])
			}
			assert.Len(t, paths, 4)

			assert.Zero(t, counterValue(t, reader, duplicateNamesCtr), "no name was carried twice")
			assert.Empty(t, rec.with(duplicateNamesMsg))
		})
	}
}

// ldsExchange asks the cache for the listener set as a proxy holding `held`
// would and applies the answer to `held`. nil when the cache has nothing to
// say to that proxy.
func ldsExchange(t *testing.T, c *SnapshotCache, held map[string]string) *discoveryv3.DeltaDiscoveryResponse {
	t.Helper()
	responses := make(chan cachev3.DeltaResponse, 1)
	cancel, err := c.CreateDeltaWatch(&discoveryv3.DeltaDiscoveryRequest{
		Node:    &corev3.Node{Id: c.nodeName},
		TypeUrl: resourcev3.ListenerType,
	}, streamv3.NewDeltaSubscription(nil, nil, held, true), responses)
	require.NoError(t, err)
	if cancel != nil {
		defer cancel()
	}
	select {
	case raw := <-responses:
		resp, err := raw.GetDeltaDiscoveryResponse()
		require.NoError(t, err)
		for _, r := range resp.GetResources() {
			held[r.GetName()] = r.GetVersion()
		}
		for _, name := range resp.GetRemovedResources() {
			delete(held, name)
		}
		return resp
	case <-time.After(200 * time.Millisecond):
		return nil
	}
}

func sentListenerNames(resp *discoveryv3.DeltaDiscoveryResponse) []string {
	out := make([]string, 0, len(resp.GetResources()))
	for _, r := range resp.GetResources() {
		out = append(out, r.GetName())
	}
	return out
}

// TestListenerNamesTheProxyIsSentAreAboutOnePod drives the cache's real LDS
// responses, as the proxy and the agent's acknowledgement tracker see them.
// The tracker's waits (CNI ADD: "the pod's outbound listener is acknowledged";
// CNI DEL: "it is gone") are keyed by listener NAME, so they mean something
// only when a name belongs to one pod:
//
//   - adding the second pod SENDS its listeners (before the fix its names
//     were already held by the first pod, so an ADD wait returned at once on
//     the other pod's acknowledgement);
//   - removing the first pod REMOVES its names and leaves the second pod's
//     alone (before the fix the other pod kept the names published and a DEL
//     wait ran out its deadline).
func TestListenerNamesTheProxyIsSentAreAboutOnePod(t *testing.T) {
	c, _, _ := newSameNameCache(t)
	ctx := context.Background()
	podA, podB := sameNamePodA(), sameNamePodB()
	held := map[string]string{}

	require.NoError(t, c.AddPod(ctx, podA, bindingTrustDomain))
	first := ldsExchange(t, c, held)
	require.NotNil(t, first)
	assert.Contains(t, sentListenerNames(first), proxy.OutboundListenerName(podA))

	require.NoError(t, c.AddPod(ctx, podB, bindingTrustDomain))
	second := ldsExchange(t, c, held)
	require.NotNil(t, second, "the second pod's listeners are news to the proxy")
	for _, name := range perPodListenerNames("ns-b") {
		assert.Contains(t, sentListenerNames(second), name)
	}
	for _, name := range perPodListenerNames("ns-a") {
		assert.NotContains(t, sentListenerNames(second), name, "the first pod's listeners did not change")
	}
	assert.Empty(t, second.GetRemovedResources())

	require.NoError(t, c.RemovePod(ctx, sameNameNetnsA))
	removal := ldsExchange(t, c, held)
	require.NotNil(t, removal)
	assert.ElementsMatch(t, perPodListenerNames("ns-a"), removal.GetRemovedResources(), "the first pod's DEL removes its names, all of them and no others")
	for _, name := range perPodListenerNames("ns-b") {
		assert.Contains(t, held, name, "the second pod is still held by the proxy")
		assert.NotContains(t, sentListenerNames(removal), name, "and was not touched")
	}
	requirePodPublished(t, c, 0, "ns-b", sameNameNetnsB, sameNameIDB, "ns-a", sameNameIDA)
}

// TestTwoResourcesUnderOneNameAreLoggedAndCounted is the build-time invariant,
// on a per-pod input that still produces it: two sandboxes of the SAME pod
// (same namespace and name, two network namespaces; a replacement whose
// predecessor's CNI DEL was missed while its netns still exists). Their
// resources differ and share every name, so the proxy is sent only one of
// each. The snapshot is still set, nothing is dropped by the check; it is
// said at ERROR once per change, counted on every build it lasts, and said to
// be over when it is.
func TestTwoResourcesUnderOneNameAreLoggedAndCounted(t *testing.T) {
	c, rec, reader := newSameNameCache(t)
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, sameNamePod("ns-a", sameNameNetnsA, "container-1-old"), bindingTrustDomain))
	require.Zero(t, counterValue(t, reader, duplicateNamesCtr))
	require.Empty(t, rec.with(duplicateNamesMsg))

	rec.reset()
	require.NoError(t, c.AddPod(ctx, sameNamePod("ns-a", sameNameNetnsC, "container-9-new"), bindingTrustDomain),
		"the check reports; it does not refuse the pod or fail the build")

	errs := rec.with(duplicateNamesMsg)
	require.Len(t, errs, 2, "one line per xDS type that has a duplicate")
	byType := map[string]capturedRecord{}
	for _, e := range errs {
		assert.Equal(t, slog.LevelError, e.level)
		assert.NotContains(t, e.attrs, "issue", "the line names no one cause: more than one input produces it")
		byType[e.attrs["type"]] = e
	}
	require.Contains(t, byType, resourcev3.ListenerType)
	require.Contains(t, byType, resourcev3.ClusterType)
	assert.Equal(t, "4", byType[resourcev3.ListenerType].attrs["count"])
	assert.Equal(t, "[capture_ns-a_web-0 inbound_ns-a_web-0 inbound_ns-a_web-0_h3 outbound_http_ns-a_web-0]", byType[resourcev3.ListenerType].attrs["names"])
	assert.Equal(t, "3", byType[resourcev3.ClusterType].attrs["count"])
	assert.Equal(t, "[app_ns-a_web-0_8080 health_ns-a_web-0 inboundready_ns-a_web-0]", byType[resourcev3.ClusterType].attrs["names"])
	perBuild := int64(7)
	assert.Equal(t, perBuild, counterValue(t, reader, duplicateNamesCtr), "one per name per build")
	assert.Contains(t, publishedListeners(t, c), "inbound_ns-a_web-0", "the snapshot is set as before: one of the two is published")

	// The next builds count again and say nothing new.
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Empty(t, rec.with(duplicateNamesMsg), "logged when the set of names changes, not on every build")
	assert.Equal(t, 3*perBuild, counterValue(t, reader, duplicateNamesCtr))

	// One sandbox goes: the condition is over, said once, and the count stops.
	require.NoError(t, c.RemovePod(ctx, sameNameNetnsA))
	cleared := rec.with("no resource name is carried by more than one resource any more")
	require.Len(t, cleared, 1)
	assert.Equal(t, slog.LevelInfo, cleared[0].level)
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Equal(t, 3*perBuild, counterValue(t, reader, duplicateNamesCtr))
	assert.Len(t, rec.with("no resource name is carried by more than one resource any more"), 1)
	requirePodPublished(t, c, 0, "ns-a", sameNameNetnsC, sameNameIDA, "ns-b", sameNameIDB)
}

// TestDuplicateResourceNames pins the rule on its own: per type, a name is
// reported when two resources under it DIFFER, and not when they are equal
// (whichever go-control-plane keeps, the proxy is sent the same thing).
func TestDuplicateResourceNames(t *testing.T) {
	cluster := func(name string, timeoutSeconds int64) types.Resource {
		return &clusterv3.Cluster{Name: name, ConnectTimeout: durationpb.New(time.Duration(timeoutSeconds) * time.Second)}
	}
	listener := func(name string) types.Resource { return &listenerv3.Listener{Name: name} }

	dups, total := duplicateResourceNames(map[resourcev3.Type][]types.Resource{
		resourcev3.ClusterType: {
			cluster("b", 1), cluster("a", 1), cluster("b", 2), // b differs
			cluster("c", 1), cluster("c", 1), // c is carried twice, equal
			cluster("z", 1), cluster("z", 2), cluster("z", 3), // z three times
		},
		resourcev3.ListenerType: {listener("a"), listener("b")}, // a cluster's name is not a listener's
		resourcev3.EndpointType: nil,
	})
	assert.Equal(t, 2, total)
	assert.Equal(t, map[resourcev3.Type][]string{resourcev3.ClusterType: {"b", "z"}}, dups)

	dups, total = duplicateResourceNames(nil)
	assert.Zero(t, total)
	assert.Empty(t, dups)
}

// TestRetainedHTTPEntryBesideALiveTCPEntryIsNotADuplicate is the input this
// check was first seen to report outside pods (review of #1634), which is not
// an input any more (#1635). A service's HTTP listing goes and its TCP listing
// stays: the HTTP entry is retained (serviceRetentionGrace), and it used to
// carry an EMPTY load assignment beside the TCP entry's populated one, two
// different resources under one name. The check said so once and counted every
// build of the grace (one ERROR, a counter of 32 over these 32 builds), while
// the published endpoints alternated between none and two.
//
// Now the retained entry publishes no load assignment under a name a live
// entry publishes: the check is silent and what is published is the live one,
// on every build. bare_cla_owner_1635_test.go pins the rule and its siblings.
func TestRetainedHTTPEntryBesideALiveTCPEntryIsNotADuplicate(t *testing.T) {
	f := newReuseFixture(t)
	rec := &recorder{}
	f.c.log = slog.New(&captureHandler{rec: rec})
	reader := sdkmetric.NewManualReader()
	m, err := cachemetrics.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	require.NoError(t, err)
	f.c.metrics = m

	f.mu.Lock()
	f.listing[registryv1.Service_PROTOCOL_HTTP]["demo/db"] = slices.Clone(f.listing[registryv1.Service_PROTOCOL_TCP]["demo/db"])
	f.mu.Unlock()
	f.refresh(t)
	require.Empty(t, rec.with(duplicateNamesMsg), "listed under both keys: the HTTP entry holds the one load assignment, no report")
	require.Zero(t, counterValue(t, reader, duplicateNamesCtr))

	f.edit(registryv1.Service_PROTOCOL_HTTP, "demo/db", func([]*registryv1.ServiceEndpoint) []*registryv1.ServiceEndpoint { return nil })
	const builds = 32
	for range builds {
		s := f.refresh(t)
		cla, ok := s.GetResources(resourcev3.EndpointType)["demo/db"].(*endpointv3.ClusterLoadAssignment)
		require.True(t, ok)
		require.Equal(t, 2, lbEndpoints(cla), "the live TCP listing's two endpoints, on every build")
		require.Contains(t, s.GetResources(resourcev3.ClusterType), f.fqdn("demo/db"), "the h2 cluster is retained")
	}

	assert.Empty(t, rec.with(duplicateNamesMsg), "one load assignment per name: nothing to report")
	assert.Zero(t, counterValue(t, reader, duplicateNamesCtr))
}
