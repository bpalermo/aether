package cache

import (
	"context"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	streamv3 "github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBindingLinesAreOfTheSnapshotTheyName is #1621. An identity-binding line
// carries a snapshot version, and whoever correlates it with an event reads it
// as "this snapshot delivered this binding". The lines are written after
// SetSnapshot, and they used to be made from the cache's maps as they were
// then: a pod, a cluster or a secret that a mutator added after the build had
// read its resources was named under the version of a snapshot that does not
// carry it, and the build that did publish it found nothing new to say.
//
// The build here is stopped inside SetSnapshot, after it has read everything
// (a watch on a channel the test reads when it chooses). While it waits, a
// second pod is added, a cluster is added, and the first pod's secret arrives.
// None of the three is in the first snapshot, so no line of its version may
// name them, and the lines of the next version must.
func TestBindingLinesAreOfTheSnapshotTheyName(t *testing.T) {
	c, rec, _, _ := ackedPinFixture(t)
	ctx := context.Background()
	// The build waits on the watch for as long as the test takes to make its
	// changes, not for the default bound.
	c.watchAnswerTimeout = stuckProxyWait
	addPinnedCluster(c, otherClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	responses := make(chan cachev3.DeltaResponse)
	cancel, err := c.CreateDeltaWatch(&discoveryv3.DeltaDiscoveryRequest{
		Node: &corev3.Node{Id: c.nodeName}, TypeUrl: resourcev3.ClusterType, ResponseNonce: "n1",
	}, streamv3.NewDeltaSubscription(nil, nil, clusterVersions(t, c), true), responses)
	require.NoError(t, err)
	require.NotNil(t, cancel, "fixture: the watch must be open")
	defer cancel()
	builds := func() uint64 {
		c.acked.mu.Lock()
		defer c.acked.mu.Unlock()
		return c.acked.build
	}

	// The first build: pod A, and a cluster the watch is owed. It has read the
	// listener, cluster and secret maps once it has recorded its clusters for
	// the acknowledged pin state, which is the step before SetSnapshot.
	const (
		podA, podB  = "aether-test/echo-001", "aether-test/echo-002"
		lateCluster = "late.aether-test.aether.internal"
	)
	rec.reset()
	before := builds()
	addOutboundCluster(c, bindingClusterName)
	first := make(chan error, 1)
	go func() { first <- c.AddPod(ctx, watchTestPod(1), bindingTrustDomain) }()
	require.Eventually(t, func() bool { return builds() == before+1 }, stuckProxyWait, time.Millisecond,
		"fixture: the first build is at SetSnapshot")

	// Three changes the first snapshot does not carry.
	serveSecrets(c, inboundEchoIdentity)
	addOutboundCluster(c, lateCluster)
	second := make(chan error, 1)
	go func() { second <- c.AddPod(ctx, watchTestPod(2), bindingTrustDomain) }()
	require.Eventually(t, func() bool {
		c.localMu.RLock()
		defer c.localMu.RUnlock()
		_, ok := c.localWorkloads[watchTestPod(2).GetNetworkNamespace()]
		return ok
	}, stuckProxyWait, time.Millisecond, "fixture: the second pod is in the cache's maps, waiting for its build")

	// The watch takes its response: the first build goes on to its reports.
	var raw cachev3.DeltaResponse
	select {
	case raw = <-responses:
	case <-time.After(stuckProxyWait):
		t.Fatal("fixture: the first build never answered the watch")
	}
	resp, err := raw.GetDeltaDiscoveryResponse()
	require.NoError(t, err)
	sent := deltaNames(resp)
	require.Contains(t, sent, bindingClusterName, "fixture: the first snapshot has the cluster added before its build")
	require.Contains(t, sent, "app_echo-001_8080", "fixture: and the first pod")
	require.NotContains(t, sent, lateCluster, "fixture: it does not have the cluster added during its SetSnapshot")
	require.NotContains(t, sent, "app_echo-002_8080", "fixture: nor the second pod")
	firstVersion := resp.GetSystemVersionInfo()
	for _, built := range []chan error{first, second} {
		select {
		case err := <-built:
			require.NoError(t, err)
		case <-time.After(stuckProxyWait):
			t.Fatal("a build did not return after the watch took its response")
		}
	}
	secondVersion := snapshotVersion(t, c)
	require.NotEqual(t, firstVersion, secondVersion)

	type pair struct{ pod, cluster string }
	outbound := map[string]map[pair]bool{}
	for _, l := range rec.with(bindingLineMsg) {
		v := l.attrs["snapshot_version"]
		if outbound[v] == nil {
			outbound[v] = map[pair]bool{}
		}
		outbound[v][pair{l.attrs["source_pod"], l.attrs["cluster"]}] = true
	}
	assert.Equal(t, map[pair]bool{
		{podA, otherClusterName}:   true,
		{podA, bindingClusterName}: true,
	}, outbound[firstVersion], "the first snapshot binds pod A on the two clusters it carries, and nothing else")
	assert.Equal(t, map[pair]bool{
		{podA, lateCluster}:        true,
		{podB, otherClusterName}:   true,
		{podB, bindingClusterName}: true,
		{podB, lateCluster}:        true,
	}, outbound[secondVersion], "the snapshot that publishes the second pod and the late cluster is the one that names them")

	type chainState struct{ pod, served string }
	inbound := map[string]map[chainState]bool{}
	for _, l := range rec.with(inboundLineMsg) {
		v := l.attrs["snapshot_version"]
		if inbound[v] == nil {
			inbound[v] = map[chainState]bool{}
		}
		inbound[v][chainState{l.attrs["pod"], l.attrs["secret_served"]}] = true
	}
	assert.Equal(t, map[chainState]bool{{podA, "false"}: true}, inbound[firstVersion],
		"the first snapshot carries pod A's chains and not the secret they name")
	assert.Equal(t, map[chainState]bool{{podA, "true"}: true, {podB, "true"}: true}, inbound[secondVersion],
		"the next one carries the secret, and pod B's chains")
}

// TestBindingViewNamesOnlyWhatTheSnapshotCarries is the join between the
// binding view and a snapshot's listeners (bindingView). The view is one read
// of the listener map and the snapshot's listener set is another, so a pod can
// be in the view while its listeners are not in the snapshot. Such a pod is
// not named: neither as a source nor by its inbound chains. The identity
// indexed under a netns no pod owns is still reported, as the orphan it is.
func TestBindingViewNamesOnlyWhatTheSnapshotCarries(t *testing.T) {
	c, _, _, _ := ackedPinFixture(t)
	ctx := context.Background()
	serveSecrets(c, inboundEchoIdentity)
	require.NoError(t, c.AddPod(ctx, watchTestPod(1), bindingTrustDomain))
	require.NoError(t, c.AddPod(ctx, watchTestPod(2), bindingTrustDomain))
	const orphan = "/var/run/netns/no-pod"
	c.localMu.Lock()
	c.localWorkloads[orphan] = inboundSvc5Identity
	c.localMu.Unlock()
	netns1, netns2 := watchTestPod(1).GetNetworkNamespace(), watchTestPod(2).GetNetworkNamespace()

	view := c.takeBindingView()
	all := c.Listeners()
	secrets := []types.Resource{&tlsv3.Secret{Name: inboundEchoIdentity}}

	sources := view.sourceBindings(view.publishedListeners(all))
	require.Len(t, sources, 3)
	assert.Equal(t, "aether-test/echo-001", sources[netns1].pod)
	assert.Equal(t, "aether-test/echo-002", sources[netns2].pod)
	assert.Equal(t, sourceBinding{presented: inboundSvc5Identity}, sources[orphan], "an identity no pod owns has no pod")
	chains := view.inboundBindings(view.publishedListeners(all), secrets)
	pods := map[string]bool{}
	for _, b := range chains {
		pods[b.pod] = true
		assert.True(t, b.served, "the secret is one of the snapshot's")
	}
	assert.Equal(t, map[string]bool{"aether-test/echo-001": true, "aether-test/echo-002": true}, pods)
	unserved := view.inboundBindings(view.publishedListeners(all), nil)
	require.NotEmpty(t, unserved)
	for _, b := range unserved {
		assert.False(t, b.served, "served is of the snapshot's secrets, not of the cache's")
	}

	// The second pod is rebuilt after the view was taken: the snapshot carries
	// listeners that are not the ones the view saw for it.
	require.NoError(t, c.AddPod(ctx, watchTestPod(2), bindingTrustDomain))
	later := view.publishedListeners(c.Listeners())
	sources = view.sourceBindings(later)
	assert.Contains(t, sources, netns1)
	assert.NotContains(t, sources, netns2, "a pod whose listeners are not the snapshot's is not named as a source")
	assert.Contains(t, sources, orphan)
	pods = map[string]bool{}
	for _, b := range view.inboundBindings(later, secrets) {
		pods[b.pod] = true
	}
	assert.Equal(t, map[string]bool{"aether-test/echo-001": true}, pods, "nor by its inbound chains")
}

// TestBindingLinesLeaveOutAPodWhoseListenersAreNotPublished: a pod whose netns
// is gone is skipped by the build (its listeners would be refused by the
// proxy), so the snapshot delivers no binding for it and no line names it.
// When it is published again, it is named then.
func TestBindingLinesLeaveOutAPodWhoseListenersAreNotPublished(t *testing.T) {
	orig := netnsExists
	t.Cleanup(func() { netnsExists = orig })
	c, rec, _, _ := ackedPinFixture(t)
	ctx := context.Background()
	addPinnedCluster(c, otherClusterName)
	dead := watchTestPod(2).GetNetworkNamespace()
	netnsExists = func(path string) bool { return path != dead }
	require.NoError(t, c.AddPod(ctx, watchTestPod(1), bindingTrustDomain))
	require.NoError(t, c.AddPod(ctx, watchTestPod(2), bindingTrustDomain))
	for _, l := range append(rec.with(bindingLineMsg), rec.with(inboundLineMsg)...) {
		assert.NotEqual(t, "aether-test/echo-002", l.attrs["source_pod"]+l.attrs["pod"], "line %q", l.msg)
	}
	require.NotEmpty(t, rec.with(bindingLineMsg), "fixture: the live pod is named")

	netnsExists = func(string) bool { return true }
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	require.Len(t, rec.with(bindingLineMsg), 1)
	assert.Equal(t, "aether-test/echo-002", rec.with(bindingLineMsg)[0].attrs["source_pod"])
	require.NotEmpty(t, rec.with(inboundLineMsg))
	for _, l := range rec.with(inboundLineMsg) {
		assert.Equal(t, "aether-test/echo-002", l.attrs["pod"])
	}
}

// TestBindingLinesNameTheMTLSEntriesTheBuildRead: an outbound binding is of a
// cluster that binds a client certificate, which is an entry with an
// mTLS-injected cluster. An entry without one (here a plaintext floor) is in
// the same cluster map and is not named. The names are collected by the
// build's read of that map into a buffer the build reuses, which has to hold
// one build's names and not every build's.
func TestBindingLinesNameTheMTLSEntriesTheBuildRead(t *testing.T) {
	c, rec, _, _ := ackedPinFixture(t)
	ctx := context.Background()
	const plaintext = "udp:aether-test/plain"
	addPinnedCluster(c, otherClusterName)
	c.clusterMu.Lock()
	c.clusters[plaintext] = clusterEntry{cluster: &clusterv3.Cluster{Name: plaintext}, service: "aether-test/plain", plaintext: true, l4Floor: true}
	c.clusterMu.Unlock()
	c.recomputeMTLSClusters()
	c.clusterMu.RLock()
	require.Nil(t, c.clusters[plaintext].mtlsCluster, "fixture: the plaintext entry has no mTLS cluster")
	require.NotNil(t, c.clusters[otherClusterName].mtlsCluster, "fixture: the other one has")
	c.clusterMu.RUnlock()

	require.NoError(t, c.AddPod(ctx, watchTestPod(1), bindingTrustDomain))
	lines := rec.with(bindingLineMsg)
	require.Len(t, lines, 1)
	assert.Equal(t, otherClusterName, lines[0].attrs["cluster"])

	for range 5 {
		require.NoError(t, c.generateSnapshot(ctx))
	}
	assert.Equal(t, []string{otherClusterName}, c.mtlsEntries, "the buffer holds the names of the last build")
}

// TestBindingLinesNameClusterResources: an outbound binding line names a
// cluster, and a reader looks that name up in the snapshot, or on the proxy.
// So it is the name of the mTLS cluster RESOURCE the snapshot carries. An
// entry's map key is not always that: a service's default entry is keyed by
// the service and publishes a cluster named by the FQDN. And a floor entry's
// cluster is built at snapshot time, or not at all, so nothing the cluster map
// holds for it is a resource of the snapshot.
func TestBindingLinesNameClusterResources(t *testing.T) {
	c, rec, _, _ := ackedPinFixture(t)
	ctx := context.Background()
	const key, fqdn = "aether-test/echo", "echo.aether-test.aether.internal"
	c.clusterMu.Lock()
	c.clusters[key] = clusterEntry{cluster: &clusterv3.Cluster{Name: fqdn}, service: key, sanNamespaces: []string{"aether-test"}}
	c.clusterMu.Unlock()
	c.recomputeMTLSClusters()
	// A floor entry that, against what the render does today, holds an mTLS
	// cluster: the build publishes no cluster from a floor entry in this pass.
	c.clusterMu.Lock()
	c.clusters["tcp:"+fqdn] = clusterEntry{
		cluster: &clusterv3.Cluster{Name: "tcp:" + fqdn}, mtlsCluster: &clusterv3.Cluster{Name: "tcp:" + fqdn},
		service: key, sanNamespaces: []string{"aether-test"}, l4Floor: true,
	}
	c.clusterMu.Unlock()

	clusters, _, _, pins := c.clustersEndpointsVhostsAndPins()
	assert.Equal(t, []string{fqdn}, resourceNames(clusters), "fixture: the pass publishes the default cluster, by its FQDN")
	assert.Equal(t, []string{fqdn}, pins.mtls, "and those are the names the binding log is given")

	require.NoError(t, c.AddPod(ctx, watchTestPod(1), bindingTrustDomain))
	lines := rec.with(bindingLineMsg)
	require.Len(t, lines, 1)
	assert.Equal(t, fqdn, lines[0].attrs["cluster"])
	assert.Contains(t, clusterVersions(t, c), lines[0].attrs["cluster"], "the line names a cluster of the snapshot")
}
