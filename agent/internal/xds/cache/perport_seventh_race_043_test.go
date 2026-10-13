package cache

// Seventh adversarial review of the proposal-043 stack (head 7ecbd2a3). Tests
// adopted from the review, where each failed.

import (
	"context"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/require"
)

// R7-N5c. Ruling 9 through the same hole. A service-wide filter is attached to
// a service whose entry is RETAINED (its own pods are gone; its rule still
// routes to a live backend). SetServiceChainFilters advances the rule
// generation, releases its lock, rebuilds the pod listeners and only then
// builds the snapshot that rebuilds the entries' virtual hosts. A registry
// refresh that runs in between rebuilds the entries it lists and records
// EVERY virtual host as built at the new generation; the snapshot build then
// finds nothing to do, and the retained entry keeps the virtual host without
// the filter: requests to its authority are delivered to the rule's backend
// with the filter skipped, until the next rule event or the end of the grace.
//
// The two calls run on their own goroutines here, as in the agent (the route
// reconciler and the registry refresh loop). R7_RACE_ROUNDS tunes the run.
func TestPerPort_AFilterAttachedWhileARefreshRunsReachesARetainedEntry(t *testing.T) {
	ctx := context.Background()
	h := registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	front := a043Service{name: "front", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.1", vip: "10.96.5.1"}
	back := a043Service{name: "back", ns: "demo", legacy: 8080, ports: map[uint32]registryv1.PortProtocol{8080: h}, ip: "10.0.5.2", vip: "10.96.5.2"}
	host := "front.demo." + a043Mesh + ":8080"
	c := a043Cache(t, nil)
	a043Project(c, []a043Service{front, back})
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{"demo/front": {{
		Matches:  []proxy.GammaMatch{{Prefix: "/"}},
		Backends: []proxy.GammaBackend{{Service: "demo/back", Cluster: "back.demo." + a043Mesh, Port: 8080, Weight: 1}},
	}}})
	regBoth, regBack := a043Registry([]a043Service{front, back}), a043Registry([]a043Service{back})
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", regBoth))
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", regBack)) // the front is retained
	loopback := func() r5Answer {
		require.NoError(t, c.generateSnapshot(ctx))
		return r5Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], "demo", host, "/")
	}
	base := loopback()
	require.Truef(t, base.delivered && !base.filter, "control: the retained front routes to its backend, no filter: %s", base.sig)

	rounds := r4Env("S7_RACE_ROUNDS", 400)
	skipped := 0
	first := ""
	for i := 0; i < rounds; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{"demo/front": r3ChainCfg(t)})
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i%40) * 5 * time.Microsecond)
			_ = c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", regBack)
		}()
		wg.Wait()
		// Everything has settled: the event built its snapshot, the refresh
		// built its own. What a request gets now, it gets until something
		// else changes.
		got := loopback()
		if got.delivered && !got.filter {
			skipped++
			if first == "" {
				first = got.sig
			}
			// One more refresh changes nothing: the generation says current.
			_ = c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", regBack)
			again := loopback()
			require.Truef(t, again.delivered && !again.filter, "a later refresh repaired it: %s", again.sig)
		}
		c.SetServiceChainFilters(nil)
		back := loopback()
		require.Truef(t, back.delivered && !back.filter, "reset: %s", back.sig)
	}
	t.Logf("%d rounds of {attach the filter || refresh}: the retained front's loopback virtual host was left WITHOUT the filter in %d (first: %q)", rounds, skipped, first)
	require.Zerof(t, skipped, "in %d of %d rounds a request for %s is delivered to the rule's backend with the service-wide filter skipped, and stays so after another refresh", skipped, rounds, host)
}
