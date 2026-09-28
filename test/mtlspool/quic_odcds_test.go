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
// request hook plays the node agent's on-demand observer: it sees the named
// CDS subscription, and -- when told to admit it -- republishes the snapshot
// with the twin. The destination is a real HTTP/3 server that requires the
// client certificate and reports the SAN it verified.
//
// The negative control (TestOnDemandQUICTwinRefusedFails) refuses every
// name: the first request must fail with 503, which proves the harness can
// see the failure mode the green arm claims to avoid.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/test/envoybin"
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
	// requests is every `quic:` name the proxy subscribed to, in order, once
	// per subscribe (a repeat subscribe is a repeat entry).
	requests []string
	// published is the twins added to the snapshot.
	published []string
}

// onDelta is the control plane's request hook: the agent's onDeltaRequest.
func (a *odcdsAgent) onDelta(_ int64, req *discoverygrpc.DeltaDiscoveryRequest) {
	if req.GetTypeUrl() != resourcev3.ClusterType {
		return
	}
	for _, name := range req.GetResourceNamesSubscribe() {
		if !proxy.IsQUICClusterName(name) {
			continue
		}
		a.mu.Lock()
		a.requests = append(a.requests, name)
		a.mu.Unlock()
		a.t.Logf("[odcds] CDS subscribe %s (admit=%v)", name, a.admit)
		if a.admit {
			// Off the stream goroutine, as the agent does: publishing from
			// inside the request callback could block on this very stream.
			go a.publishTwin(name)
		}
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
	twin := proxy.QUICClusterFrom(base, name, sourceID, validationContextName, []string{spiffeDest}, quicSNI)
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

	portA, portB, adminPort := freePort(t), freePort(t), freePort(t)
	baseCLA := staticEndpoint(odcdsDestSvc, h3.addr)
	agent := &odcdsAgent{t: t, admit: admit, destAddr: h3.addr, baseCLA: baseCLA, version: 1}
	agent.resources = map[resourcev3.Type][]types.Resource{
		// Nothing observed: the base and its load assignment, no twin.
		resourcev3.ClusterType:  {odcdsBase()},
		resourcev3.EndpointType: {baseCLA},
		resourcev3.ListenerType: {
			odcdsSourceListener(t, "source_a", spiffeSourceA, portA, arms),
			odcdsSourceListener(t, "source_b", spiffeSourceB, portB, arms),
		},
		resourcev3.SecretType: secretResources(t, p, []string{spiffeSourceA, spiffeSourceB, spiffeNode}),
	}
	agent.cp = startADSControlPlaneWithHook(t, agent.resources, agent.onDelta)
	launchEnvoyOverADS(t, bin, agent.cp, adminPort)

	addrA, addrB := fmt.Sprintf("127.0.0.1:%d", portA), fmt.Sprintf("127.0.0.1:%d", portB)
	waitListening(t, addrA)
	waitListening(t, addrB)
	return &odcdsRun{
		agent: agent,
		a:     newSourceClient("source-a", addrA),
		b:     newSourceClient("source-b", addrB),
		twinA: twinA,
		twinB: twinB,
	}
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
