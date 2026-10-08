package mtlspool

// The live gate for issue #1020: demand-scoped `quic:` twins, fetched on
// demand.
//
// # What changed
//
// Before #1020 the agent built one twin per local ServiceAccount for every
// QUIC-enabled destination up front (118 twins on rev242, 2 of them ever
// used). Now the selection route still carries one arm per local
// ServiceAccount -- arm = the source's SPIFFE ID, target = its twin's name --
// but the twin itself is NOT in CDS until that source dials. Its first request
// resolves through the matcher cluster-specifier to a cluster the proxy does
// not have; the HCM's on_demand filter asks for it by name over ODCDS; the
// agent validates the name, records the pair, and publishes the twin with its
// own load assignment; the paused request resumes on it, over HTTP/3.
//
// # What runs
//
// The pinned proxy over one delta-ADS stream (ads_sds_test.go), with
// production's pieces wherever they exist: proxy.NewServiceCluster for the h2
// base, proxy.QUICClusterFrom + proxy.LoadAssignmentAlias for the twin,
// proxy.ApplyQUICClusterSelection for the route, proxy.OnDemandHTTPFilter for
// the ODCDS filter, proxy.BuildSourceFilterStates for the identity stamp, and
// proxy.ParseQUICClusterName for the name check. The control plane's delta
// request hook plays the node agent's on-demand observer, classifying each
// request with the agent's own quicdemand.Requests (issue #1033: first use,
// a fresh stream's re-subscription, or a twin merely held), and -- when told
// to admit -- republishes the snapshot with the twin. The destination is a
// real HTTP/3 server that requires the client certificate and reports the SAN
// it verified.
//
// # Agent restarts (issue #1033)
//
// restartAgent stops the control plane and starts a fresh one on the same
// socket with an empty demand set; the proxy keeps running and reconnects.
// Two facts about the pinned proxy decide what the agent may do with the twins
// it re-states on the fresh stream, and both are asserted here: a twin held
// only through the wildcard is dropped when answered absent and re-fetched on
// demand by the next request that routes to it; a twin fetched on demand holds
// an ODCDS subscription Envoy never re-sends, so answering it absent strands
// it (503 at the on_demand timeout).
//
// # Sources and destinations that come back (issue #1036)
//
// quic_dormant_test.go: a twin fetched on demand whose pair loses its source
// or destination is kept dormant by the agent's quicdemand.Ledger and pushed
// again, with no request, when the pair is valid again.
//
// The negative control (TestOnDemandQUICTwinRefusedFails) refuses every
// name: the first request must fail with 503, which proves the harness can
// see the failure mode the green arm claims to avoid.

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/internal/xds/quicdemand"
	"aethermesh.dev/agent/test/envoybin"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// odcdsDestSvc is the QUIC-enabled destination.
	odcdsDestSvc = "demo/echo"

	// firstRequestODCDSBudget is the issue's bound on a new pair's first
	// request: one local ODCDS round trip, one EDS round trip for the twin's
	// own load assignment, the SDS fetches for its certificate, and a QUIC
	// handshake.
	firstRequestODCDSBudget = time.Second
)

// odcdsAgent is the control-plane side: the served resources, the named CDS
// subscriptions the proxy sent, and the admit decision.
type odcdsAgent struct {
	t     *testing.T
	cp    *adsControlPlane
	admit bool

	destAddr string
	baseCLA  *endpointv3.ClusterLoadAssignment

	mu        sync.Mutex
	version   int
	resources map[resourcev3.Type][]types.Resource
	// twinRequests is the AGENT's classifier (agent/internal/xds/quicdemand,
	// issue #1033). One per control plane process, as in the agent.
	twinRequests *quicdemand.Requests
	// ignoreResubscribed is the strand control: it answers a fresh stream's
	// re-subscribed twins absent instead of serving them.
	ignoreResubscribed bool
	// requests is every first-use `quic:` name the proxy subscribed to, in
	// order, once per subscribe (a repeat subscribe is a repeat entry).
	requests []string
	// resubscribed / heldOnly are the twins a fresh stream's first CDS
	// request re-stated: with a live on-demand subscription, or merely held.
	resubscribed []string
	heldOnly     []string
	// published is the twins added to the snapshot.
	published []string

	// ledger is the AGENT's subscription ledger (quicdemand.Ledger, issue
	// #1036): which twins the proxy holds an ODCDS subscription for, and the
	// dormant pairs whose twin was removed while it does. One per control
	// plane process, as in the agent.
	ledger *quicdemand.Ledger[string]
	// forget is the #1036 control: today's #1035 rule, which forgets a pair on
	// removal evidence instead of keeping it dormant.
	forget bool
	// awaySources / destGone are the pair validity the agent judges: a source
	// ServiceAccount with no pod on the node, a destination out of the node's
	// dependency set.
	awaySources map[string]bool
	destGone    bool
	// unsubscribed is every resource name, of any type, the proxy has
	// unsubscribed from: how the gate sees a removed twin fully torn down (its
	// EDS name and its certificate's SDS name released).
	unsubscribed map[string]bool
}

// onDelta is the control plane's request hook: the agent's onDeltaRequest,
// admitting what the agent admits -- first-use twins, and the twins the proxy
// re-subscribes on a fresh stream -- and nothing the proxy merely holds.
func (a *odcdsAgent) onDelta(streamID int64, req *discoverygrpc.DeltaDiscoveryRequest) {
	a.mu.Lock()
	for _, name := range req.GetResourceNamesUnsubscribe() {
		a.unsubscribed[name] = true
	}
	cls := a.twinRequests.Classify(streamID, req)
	// The agent's ledger (issue #1036): a fresh stream re-states the proxy's
	// subscriptions; every name it asks for is one it now holds.
	if cls.Fresh {
		for _, name := range a.ledger.Restate(streamID, cls.Resubscribed) {
			a.t.Logf("[odcds] fresh stream %d: pruned dormant %s (no subscription left)", streamID, name)
		}
	}
	for _, name := range append(slices.Clone(cls.FirstUse), cls.Resubscribed...) {
		a.ledger.Subscribe(streamID, name)
	}
	a.resubscribed = append(a.resubscribed, cls.Resubscribed...)
	a.heldOnly = append(a.heldOnly, cls.HeldOnly...)
	a.requests = append(a.requests, cls.FirstUse...)
	ignoreResubscribed := a.ignoreResubscribed
	a.mu.Unlock()
	if len(cls.Resubscribed)+len(cls.HeldOnly) > 0 {
		a.t.Logf("[odcds] fresh stream %d re-stated: resubscribed=%v held_only=%v (held-only admits nothing; ignore_resubscribed=%v)",
			streamID, cls.Resubscribed, cls.HeldOnly, ignoreResubscribed)
	}
	admit := slices.Clone(cls.FirstUse)
	if !ignoreResubscribed {
		admit = append(admit, cls.Resubscribed...)
	}
	for _, name := range cls.FirstUse {
		a.t.Logf("[odcds] CDS subscribe %s (admit=%v)", name, a.admit)
	}
	if !a.admit {
		return
	}
	for _, name := range admit {
		// Off the stream goroutine, as the agent does: publishing from
		// inside the request callback could block on this very stream.
		go a.publishTwin(name)
	}
}

// publishTwin builds the twin and its load assignment for name and adds both
// to the served snapshot in ONE new version (the #1008 rule).
func (a *odcdsAgent) publishTwin(name string) {
	svc, source, ok := proxy.ParseQUICClusterName(name, trustDomain)
	if !ok || svc != odcdsDestSvc {
		a.t.Logf("[odcds] refusing malformed %s", name)
		return
	}
	ns, sa, _ := strings.Cut(source, "/")
	sourceID := "spiffe://" + trustDomain + "/ns/" + ns + "/sa/" + sa
	base := odcdsBase()
	twin := proxy.QUICClusterFrom(base, name, sourceID, validationContextName, []string{spiffeDest}, quicSNI, 0)
	cla := proxy.LoadAssignmentAlias(a.baseCLA, name)

	a.mu.Lock()
	defer a.mu.Unlock()
	if slices.Contains(a.published, name) {
		return
	}
	next := map[resourcev3.Type][]types.Resource{}
	for typ, res := range a.resources {
		next[typ] = slices.Clone(res)
	}
	next[resourcev3.ClusterType] = append(next[resourcev3.ClusterType], twin)
	next[resourcev3.EndpointType] = append(next[resourcev3.EndpointType], cla)
	a.version++
	snap, err := cachev3.NewSnapshot(fmt.Sprint(a.version), next)
	if err != nil {
		a.t.Errorf("build snapshot: %v", err)
		return
	}
	if err := a.cp.cache.SetSnapshot(context.Background(), envoyNodeID, snap); err != nil {
		a.t.Errorf("set snapshot: %v", err)
		return
	}
	a.resources = next
	a.published = append(a.published, name)
	a.t.Logf("[odcds] published %s (version %d)", name, a.version)
}

func (a *odcdsAgent) snapshotState() (requests, published []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests), slices.Clone(a.published)
}

// odcdsBase is production's h2 base for the destination: EDS over ADS under
// the bare service name, named by its mesh authority. It is published bare
// (no transport socket): no request in this file is meant to reach it.
func odcdsBase() *clusterv3.Cluster {
	return proxy.NewServiceCluster(proxy.ServiceClusterName(odcdsDestSvc, trustDomain), odcdsDestSvc, odcdsDestSvc, nil)
}

// odcdsSourceListener is sourceListener with production's QUIC selection on
// the route (arms for EVERY source, twins not built) and production's
// on_demand filter ahead of the router, as on every node-proxy HCM.
func odcdsSourceListener(t *testing.T, name, spiffeID string, port int, arms map[string]string) *listenerv3.Listener {
	t.Helper()
	l := sourceListener(name, spiffeID, port, true)
	filters := l.GetFilterChains()[0].GetFilters()
	hcmFilter := filters[len(filters)-1]
	var hcm hcmv3.HttpConnectionManager
	require.NoError(t, hcmFilter.GetTypedConfig().UnmarshalTo(&hcm))
	vh := hcm.GetRouteConfig().GetVirtualHosts()[0]
	fqdn := proxy.ServiceClusterName(odcdsDestSvc, trustDomain)
	vh.GetRoutes()[0].GetRoute().ClusterSpecifier = &routev3.RouteAction_Cluster{Cluster: fqdn}
	require.Equal(t, 1, proxy.ApplyQUICClusterSelection(vh, fqdn, arms))
	hcm.HttpFilters = []*hcmv3.HttpFilter{
		proxy.OnDemandHTTPFilter(),
		{
			Name:       "envoy.filters.http.router",
			ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: config.TypedConfig(&routerv3.Router{})},
		},
	}
	hcmFilter.ConfigType = &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(&hcm)}
	return l
}

// odcdsRun is a started proxy, its two sources and the agent stand-in.
type odcdsRun struct {
	agent *odcdsAgent
	a, b  *sourceClient
	twinA string
	twinB string
	// adminAddr is the proxy's admin listener; initial is the snapshot a
	// fresh agent serves (base + listeners + secrets, no twin).
	adminAddr string
	initial   map[resourcev3.Type][]types.Resource
}

func startODCDS(t *testing.T, admit bool) *odcdsRun {
	t.Helper()
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("locate envoy: %v", err)
	}

	p := newPKI(t)
	h3 := startDestinationH3(t, p)
	twinA := proxy.QUICClusterName(odcdsDestSvc, trustDomain, proxy.SourceSAKeyFromSpiffeID(spiffeSourceA))
	twinB := proxy.QUICClusterName(odcdsDestSvc, trustDomain, proxy.SourceSAKeyFromSpiffeID(spiffeSourceB))
	arms := map[string]string{spiffeSourceA: twinA, spiffeSourceB: twinB}

	baseCLA := staticEndpoint(odcdsDestSvc, h3.addr)
	agent := &odcdsAgent{
		t: t, admit: admit, destAddr: h3.addr, baseCLA: baseCLA, version: 1,
		twinRequests: quicdemand.NewRequests(), ledger: quicdemand.NewLedger[string](),
		awaySources: map[string]bool{}, unsubscribed: map[string]bool{},
	}
	agent.resources = map[resourcev3.Type][]types.Resource{
		// Nothing observed: the base and its load assignment, no twin.
		resourcev3.ClusterType:  {odcdsBase()},
		resourcev3.EndpointType: {baseCLA},
		resourcev3.ListenerType: {
			odcdsSourceListener(t, "source_a", spiffeSourceA, envoyPicksPort, arms),
			odcdsSourceListener(t, "source_b", spiffeSourceB, envoyPicksPort, arms),
		},
		resourcev3.SecretType: secretResources(t, p, []string{spiffeSourceA, spiffeSourceB, spiffeNode}),
	}
	agent.cp = startADSControlPlaneWithHook(t, agent.resources, agent.onDelta)
	e := launchEnvoyOverADS(t, bin, agent.cp)

	addrA, addrB := e.listenerAddr(t, "source_a"), e.listenerAddr(t, "source_b")
	return &odcdsRun{
		agent:     agent,
		a:         newSourceClient("source-a", addrA),
		b:         newSourceClient("source-b", addrB),
		twinA:     twinA,
		twinB:     twinB,
		adminAddr: e.admin,
		initial:   maps.Clone(agent.resources),
	}
}

// restartAgent is an agent restart as the proxy sees it: the control plane is
// stopped, so the proxy's ADS stream drops, and a new one is started on the
// same socket with a FRESH process's state -- the base resources only, an
// empty demand set (nothing persisted) and a new classifier. The proxy keeps
// running with every cluster it held.
func (r *odcdsRun) restartAgent(t *testing.T) {
	t.Helper()
	a := r.agent
	a.mu.Lock()
	old := a.cp
	a.mu.Unlock()
	old.stop()

	a.mu.Lock()
	a.resources = maps.Clone(r.initial)
	a.published = nil
	a.requests = nil
	a.resubscribed = nil
	a.heldOnly = nil
	a.twinRequests = quicdemand.NewRequests()
	a.ledger = quicdemand.NewLedger[string]()
	a.version = 1
	a.mu.Unlock()
	cp := startADSControlPlaneOn(t, old.socketPath, r.initial, a.onDelta)
	a.mu.Lock()
	a.cp = cp
	a.mu.Unlock()
	t.Logf("[odcds] control plane restarted on %s with an empty demand set", old.socketPath)
}

// quicClusters is the proxy's `quic:` clusters, read from its admin
// /clusters listing -- what the proxy actually holds, independent of what the
// control plane believes it served.
func (r *odcdsRun) quicClusters(t *testing.T) []string {
	t.Helper()
	resp, err := http.Get("http://" + r.adminAddr + "/clusters")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	seen := map[string]struct{}{}
	for line := range strings.Lines(string(body)) {
		name, _, ok := strings.Cut(line, "::")
		if ok && proxy.IsQUICClusterName(name) {
			seen[name] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// timedCallOnce sends one request (no retry) and times it.
func timedCallOnce(t *testing.T, c *sourceClient) (attempt, time.Duration) {
	t.Helper()
	start := time.Now()
	at := c.callOnce(t)
	return at, time.Since(start)
}

// TestOnDemandQUICTwinPerPair: nothing observed; A's first request fetches
// ONLY A's twin over ODCDS and succeeds over HTTP/3 within
// firstRequestODCDSBudget with A's identity; B's first request likewise builds
// only B's; A's second request rides the existing twin with no new CDS
// subscribe.
func TestOnDemandQUICTwinPerPair(t *testing.T) {
	r := startODCDS(t, true)

	requests, published := r.agent.snapshotState()
	require.Empty(t, requests, "precondition: nothing asked for a twin before any request")
	require.Empty(t, published)

	// A's first request: ODCDS -> twin @A -> 200 over HTTP/3.
	at, took := timedCallOnce(t, r.a)
	t.Logf("source-a first request: %s in %s", at, took)
	require.NoError(t, at.err)
	require.Equal(t, http.StatusOK, at.status, "a new pair's first request must not fail: %s", at)
	assert.Equal(t, "HTTP/3.0", at.proto, "the first request must ride the fetched QUIC twin, not fall back")
	assert.Equal(t, spiffeSourceA, at.san, "the destination must verify source-a's own SVID")
	assert.Less(t, took, firstRequestODCDSBudget, "a new pair's first request pays one ODCDS round trip, not a timeout")
	requests, published = r.agent.snapshotState()
	assert.Equal(t, []string{r.twinA}, requests, "exactly one on-demand request, for A's twin")
	assert.Equal(t, []string{r.twinA}, published, "only A's twin was built")

	// B's first request builds only B's twin.
	at, took = timedCallOnce(t, r.b)
	t.Logf("source-b first request: %s in %s", at, took)
	require.NoError(t, at.err)
	require.Equal(t, http.StatusOK, at.status, "%s", at)
	assert.Equal(t, "HTTP/3.0", at.proto)
	assert.Equal(t, spiffeSourceB, at.san)
	assert.Less(t, took, firstRequestODCDSBudget)
	requests, published = r.agent.snapshotState()
	assert.Equal(t, []string{r.twinA, r.twinB}, requests)
	assert.Equal(t, []string{r.twinA, r.twinB}, published)

	// A's second request: the twin exists, no new CDS request.
	at, took = timedCallOnce(t, r.a)
	t.Logf("source-a second request: %s in %s", at, took)
	require.Equal(t, http.StatusOK, at.status, "%s", at)
	assert.Equal(t, "HTTP/3.0", at.proto)
	assert.Equal(t, spiffeSourceA, at.san)
	requests, _ = r.agent.snapshotState()
	assert.Equal(t, []string{r.twinA, r.twinB}, requests, "a pair whose twin exists must not ask again")
}

// waitRestated waits for the proxy to reconnect to a restarted control plane
// and re-state its twins, and returns what it re-stated.
func (r *odcdsRun) waitRestated(t *testing.T) (resubscribed, heldOnly []string) {
	t.Helper()
	require.Eventually(t, func() bool {
		r.agent.mu.Lock()
		defer r.agent.mu.Unlock()
		return len(r.agent.resubscribed)+len(r.agent.heldOnly) > 0
	}, 30*time.Second, 50*time.Millisecond, "the proxy never reconnected to the restarted control plane")
	r.agent.mu.Lock()
	defer r.agent.mu.Unlock()
	return slices.Clone(r.agent.resubscribed), slices.Clone(r.agent.heldOnly)
}

// TestOnDemandQUICHeldTwinsNotReadmittedAfterAgentRestart is the live #1033
// gate, in the talos shape.
//
// The proxy holds twins A and B that the previous agent generation built UP
// FRONT (the pre-#1020 SAs x destinations rule) and delivered through the
// wildcard: no request routed to either, and the proxy holds no on-demand
// subscription for them. The control plane is restarted with an EMPTY demand
// set, as a replaced agent with nothing persisted. On the fresh stream the
// proxy re-states both in initial_resource_versions. Neither is demand, so
// neither may be re-created: both are answered absent and the proxy drops
// them. Then A's next request routes to A's missing twin, the on_demand filter
// opens a subscription and fetches it -- real first use -- and it succeeds over
// HTTP/3 with no 503. B's twin stays absent.
//
// Red on #1032's rule (a held twin re-admits its pair): both twins are
// re-created at stream start.
func TestOnDemandQUICHeldTwinsNotReadmittedAfterAgentRestart(t *testing.T) {
	r := startODCDS(t, true)
	// The previous agent generation: both twins built up front.
	r.agent.publishTwin(r.twinA)
	r.agent.publishTwin(r.twinB)
	require.Eventually(t, func() bool {
		return slices.Equal([]string{r.twinA, r.twinB}, r.quicClusters(t))
	}, 10*time.Second, 50*time.Millisecond, "precondition: the proxy holds both up-front twins")
	requests, _ := r.agent.snapshotState()
	require.Empty(t, requests, "precondition: no request ever routed to a twin")

	r.restartAgent(t)
	resubscribed, heldOnly := r.waitRestated(t)
	t.Logf("fresh stream re-stated: resubscribed=%v held_only=%v", resubscribed, heldOnly)
	assert.Empty(t, resubscribed, "the proxy holds no on-demand subscription for an up-front twin")
	assert.ElementsMatch(t, []string{r.twinA, r.twinB}, heldOnly, "the proxy re-states both held twins")

	var held []string
	require.Eventually(t, func() bool {
		held = r.quicClusters(t)
		return len(held) == 0
	}, 10*time.Second, 50*time.Millisecond, "twins re-created at stream start (issue #1033)")
	// Give a wrong re-admission time to land before asserting it did not.
	time.Sleep(500 * time.Millisecond)
	held = r.quicClusters(t)
	requests, published := r.agent.snapshotState()
	t.Logf("after the restart, before any request: proxy holds %v, published %v", held, published)
	assert.Empty(t, held, "no twin re-created before a request routes to one")
	assert.Empty(t, requests)
	assert.Empty(t, published)

	// A request routes to A's twin: fetched on demand, 200 over HTTP/3.
	at, took := timedCallOnce(t, r.a)
	t.Logf("source-a first request after the restart: %s in %s", at, took)
	require.NoError(t, at.err)
	require.Equal(t, http.StatusOK, at.status, "real first use after a restart must not fail: %s", at)
	assert.Equal(t, "HTTP/3.0", at.proto, "it must ride the fetched twin")
	assert.Equal(t, spiffeSourceA, at.san)
	assert.Less(t, took, firstRequestODCDSBudget, "one ODCDS round trip, not a timeout")

	requests, published = r.agent.snapshotState()
	assert.Equal(t, []string{r.twinA}, requests, "exactly one first-use request, for A's twin")
	assert.Equal(t, []string{r.twinA}, published)
	assert.Equal(t, []string{r.twinA}, r.quicClusters(t), "only A's twin exists; B's stays absent")
}

// TestOnDemandQUICSubscribedTwinsServedAfterAgentRestart: twins the proxy
// fetched ON DEMAND hold a live ODCDS subscription, which Envoy keeps for the
// life of the process and never re-sends (its ODCDS manager answers every
// later request for the name "already subscribed, skipping"). After an agent
// restart with an empty demand set the proxy re-subscribes both by name on
// the fresh stream; the agent must serve them, and A's next request succeeds
// over HTTP/3 with no 503.
//
// The strand control (ignoreResubscribed) answers them absent instead, which
// is what the issue's first draft of the fix asked for: A's next request then
// 503s at the on_demand timeout, because Envoy does not re-request a name it
// is subscribed to. That is why re-subscriptions are served.
func TestOnDemandQUICSubscribedTwinsServedAfterAgentRestart(t *testing.T) {
	for _, tc := range []struct {
		name               string
		ignoreResubscribed bool
	}{
		{name: "served"},
		{name: "strand_control", ignoreResubscribed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := startODCDS(t, true)
			for _, c := range []*sourceClient{r.a, r.b} {
				at, took := timedCallOnce(t, c)
				t.Logf("%s before the restart: %s in %s", c.name, at, took)
				require.Equal(t, http.StatusOK, at.status, "%s", at)
				require.Equal(t, "HTTP/3.0", at.proto)
			}
			requests, _ := r.agent.snapshotState()
			require.Equal(t, []string{r.twinA, r.twinB}, requests, "precondition: both twins were fetched on demand")

			r.agent.mu.Lock()
			r.agent.ignoreResubscribed = tc.ignoreResubscribed
			r.agent.mu.Unlock()
			r.restartAgent(t)
			resubscribed, heldOnly := r.waitRestated(t)
			t.Logf("fresh stream re-stated: resubscribed=%v held_only=%v", resubscribed, heldOnly)
			assert.ElementsMatch(t, []string{r.twinA, r.twinB}, resubscribed, "the proxy re-subscribes both on-demand twins")
			assert.Empty(t, heldOnly)

			if tc.ignoreResubscribed {
				require.Eventually(t, func() bool { return len(r.quicClusters(t)) == 0 }, 10*time.Second, 50*time.Millisecond)
				at, took := timedCallOnce(t, r.a)
				t.Logf("source-a after the restart, re-subscriptions answered absent: %s in %s", at, took)
				assert.Equal(t, http.StatusServiceUnavailable, at.status, "an answered-absent subscribed twin is stranded: %s", at)
				requests, _ = r.agent.snapshotState()
				assert.Empty(t, requests, "Envoy never re-requests a name it is subscribed to")
				return
			}

			require.Eventually(t, func() bool {
				return slices.Equal([]string{r.twinA, r.twinB}, r.quicClusters(t))
			}, 10*time.Second, 50*time.Millisecond, "the re-subscribed twins must be served")
			at, took := timedCallOnce(t, r.a)
			t.Logf("source-a after the restart: %s in %s", at, took)
			require.NoError(t, at.err)
			require.Equal(t, http.StatusOK, at.status, "%s", at)
			assert.Equal(t, "HTTP/3.0", at.proto)
			assert.Equal(t, spiffeSourceA, at.san)
			assert.Less(t, took, firstRequestODCDSBudget)
		})
	}
}

// TestOnDemandQUICTwinRefusedFails is the negative control: a control plane
// that refuses (never publishes) the twin leaves the paused request to the
// on_demand timeout, and the router then has no cluster: 503. This is the
// documented failure mode of the on-demand path, and proves the harness sees
// a failed fetch -- the green arm's 200 is not the h2 base or a retry
// quietly succeeding.
func TestOnDemandQUICTwinRefusedFails(t *testing.T) {
	r := startODCDS(t, false)

	at, took := timedCallOnce(t, r.a)
	t.Logf("source-a first request with the twin refused: %s in %s", at, took)
	require.NoError(t, at.err)
	assert.Equal(t, http.StatusServiceUnavailable, at.status, "a refused twin must 503: %s", at)
	assert.Empty(t, at.proto, "nothing may reach the destination")
	requests, published := r.agent.snapshotState()
	assert.Contains(t, requests, r.twinA, "the proxy must still have asked for the twin")
	assert.Empty(t, published)
}
