// This file is the live gate for aether#1008: a `quic:` twin cluster added
// AFTER its h2 base is already subscribed must get its endpoints promptly.
//
// # The failure
//
// A twin is a clone of the base service cluster (proxy.QUICClusterFrom). While
// it kept the base's eds_cluster_config.service_name, both clusters subscribed
// to the SAME EDS resource name on the SAME delta-ADS mux. Envoy's delta
// WatchMap deduplicates subscription interest per (type_url, resource name):
// when the twin arrives in a later CDS update -- a new ServiceAccount's first
// pod on the node -- its watch adds nothing to resource_names_subscribe, no
// request goes out, the control plane correctly sends nothing (the resource
// did not change), and the twin sits in warming until initial_fetch_timeout
// (15 s) expires. On talos that was 1,060 client-visible 503/NC in 11 s.
//
// It is #842's mechanism (SDS then) on EDS, and it only bites the LATE twin:
// at startup base and twin arrive in one CDS response and one subscribe
// serves both, so a harness that publishes everything up front can never see it.
//
// # What runs
//
// The pinned proxy against the go-control-plane snapshot cache served as a
// DELTA ADS stream (ads_sds_test.go's control plane and bootstrap), with the
// clusters and their load assignments delivered over it. The twin is built by
// production's proxy.QUICClusterFrom and its load assignment by
// proxy.LoadAssignmentAlias; only the TRANSPORT is swapped for the base's
// cleartext h2, because the transport is not under test and a QUIC destination
// would add a certificate dimension this defect does not have. The EDS shape --
// the thing under test -- is production's, byte for byte.
//
// The OLD shape is kept as an explicit negative control
// (TestLateQUICTwinWithSharedEDSNameStaysWarming): it must reproduce the
// 15 s warming and init_fetch_timeout, or the green arm proves nothing.
package mtlspool

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	// twinDestSvc is the destination service the twins front.
	twinDestSvc = "demo/echo"
	twinDomain  = "aether.internal"

	// targetHeader names the cluster a request is routed to (cluster_header),
	// so one listener reaches the base and every twin directly. A cluster that
	// is still warming is not in the worker's cluster table: the router answers
	// 503 exactly as production's route did (response flag NC).
	targetHeader = "x-aether-target-cluster"

	// lateTwinBudget is how long a twin introduced by a later snapshot may take
	// to serve its first 200. It covers one local delta round trip (CDS add,
	// EDS subscribe, EDS response) with two orders of magnitude of slack; the
	// failure it bounds is the 15 s initial_fetch_timeout.
	lateTwinBudget = 3 * time.Second

	// envoyInitialFetchTimeout is Envoy's default initial_fetch_timeout, which
	// the node proxy's `ads: {}` EDS config source inherits.
	envoyInitialFetchTimeout = 15 * time.Second
)

const (
	twinSourceA = "spiffe://" + twinDomain + "/ns/demo/sa/source-a"
	twinSourceB = "spiffe://" + twinDomain + "/ns/demo/sa/source-b"
)

// publish replaces the whole served snapshot under a new version -- what the
// agent's generateSnapshot does on every change.
func (cp *adsControlPlane) publish(t *testing.T, version string, resources map[resourcev3.Type][]types.Resource) {
	t.Helper()

	snapshot, err := cachev3.NewSnapshot(version, resources)
	if err != nil {
		t.Fatalf("build snapshot %s: %v", version, err)
	}
	if err := cp.cache.SetSnapshot(context.Background(), envoyNodeID, snapshot); err != nil {
		t.Fatalf("set snapshot %s: %v", version, err)
	}
	cp.resources = resources
}

// startH2CDestination serves 200 over cleartext HTTP/2 (prior knowledge), the
// protocol the base cluster's explicit http2 options speak.
func startH2CDestination(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		Protocols:         new(http.Protocols),
	}
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// twinFixture is the base service cluster, its load assignment, and the
// builders for one twin per source identity.
type twinFixture struct {
	base    *clusterv3.Cluster
	baseCLA *endpointv3.ClusterLoadAssignment
	// sharedEDSName reproduces the pre-#1008 shape: the twin keeps the base's
	// EDS service name and no load assignment is published under its own.
	sharedEDSName bool
}

func newTwinFixture(t *testing.T, destAddr string, sharedEDSName bool) *twinFixture {
	t.Helper()

	fqdn := proxy.ServiceClusterName(twinDestSvc, twinDomain)
	// Production's bare base: EDS over `ads: {}` under the bare service name.
	base := proxy.NewServiceCluster(fqdn, twinDestSvc, twinDestSvc, nil)
	require.Equal(t, twinDestSvc, base.GetEdsClusterConfig().GetServiceName())
	_, overADS := base.GetEdsClusterConfig().GetEdsConfig().GetConfigSourceSpecifier().(*corev3.ConfigSource_Ads)
	require.True(t, overADS, "the base must subscribe over ADS, as production's does")
	return &twinFixture{
		base:          base,
		baseCLA:       staticEndpoint(twinDestSvc, destAddr),
		sharedEDSName: sharedEDSName,
	}
}

// twin returns the twin cluster for one source identity and, in the fixed
// shape, the load assignment published under its own EDS name.
func (f *twinFixture) twin(t *testing.T, sourceID string) (*clusterv3.Cluster, types.Resource) {
	t.Helper()

	fqdn := f.base.GetName()
	name := proxy.QUICClusterName(twinDestSvc, twinDomain, proxy.SourceSAKeyFromSpiffeID(sourceID))
	cl := proxy.QUICClusterFrom(f.base, name, sourceID, "spiffe://"+twinDomain,
		[]string{"spiffe://" + twinDomain + "/ns/demo/sa/echo"}, proxy.QUICServerName("8080", fqdn), 0)
	// Transport swap only: cleartext h2, like the base. The EDS config is
	// untouched from QUICClusterFrom's output.
	cl.TransportSocket = nil
	baseClone, _ := proto.Clone(f.base).(*clusterv3.Cluster)
	cl.TypedExtensionProtocolOptions = baseClone.GetTypedExtensionProtocolOptions()

	if f.sharedEDSName {
		cl.EdsClusterConfig.ServiceName = f.base.GetEdsClusterConfig().GetServiceName()
		return cl, nil
	}
	require.Equal(t, name, cl.GetEdsClusterConfig().GetServiceName(), "production's twin must carry its own EDS name")
	return cl, proxy.LoadAssignmentAlias(f.baseCLA, name)
}

// resources is the snapshot content for the base plus one twin per identity.
func (f *twinFixture) resources(t *testing.T, listener *listenerv3.Listener, sources ...string) map[resourcev3.Type][]types.Resource {
	t.Helper()

	clusters := []types.Resource{f.base}
	clas := []types.Resource{f.baseCLA}
	for _, id := range sources {
		cl, cla := f.twin(t, id)
		clusters = append(clusters, cl)
		if cla != nil {
			clas = append(clas, cla)
		}
	}
	return map[resourcev3.Type][]types.Resource{
		resourcev3.ClusterType:  clusters,
		resourcev3.EndpointType: clas,
		resourcev3.ListenerType: {listener},
	}
}

// clusterHeaderListener routes every request to the cluster named by
// targetHeader.
func clusterHeaderListener(port int) *listenerv3.Listener {
	hcm := &hcmv3.HttpConnectionManager{
		StatPrefix: "twin_eds",
		CodecType:  hcmv3.HttpConnectionManager_AUTO,
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{
			RouteConfig: &routev3.RouteConfiguration{
				Name: "twin_eds",
				VirtualHosts: []*routev3.VirtualHost{{
					Name:    "all",
					Domains: []string{"*"},
					Routes: []*routev3.Route{{
						Match: &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
						Action: &routev3.Route_Route{Route: &routev3.RouteAction{
							ClusterSpecifier: &routev3.RouteAction_ClusterHeader{ClusterHeader: targetHeader},
						}},
					}},
				}},
			},
		},
		HttpFilters: []*hcmv3.HttpFilter{{
			Name:       "envoy.filters.http.router",
			ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: config.TypedConfig(&routerv3.Router{})},
		}},
	}
	return &listenerv3.Listener{
		Name:    "twin_eds",
		Address: socketAddress("127.0.0.1", port),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(hcm)},
			}},
		}},
	}
}

// statusVia sends one request through the listener to the named cluster.
func statusVia(addr, cluster string) (int, string) {
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set(targetHeader, cluster)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// statInt reads one admin counter/gauge; absent reads as 0 (Envoy does not
// render a stat it never touched).
func (h *adsProxyHandle) statInt(t *testing.T, name string) int {
	t.Helper()
	raw, ok := h.stats(t, name)[name]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("parse %s=%q: %v", name, raw, err)
	}
	return n
}

// clusterManagerCounts is one admin read of the cluster manager's totals.
type clusterManagerCounts struct {
	// added is the counter cluster_manager.cluster_added: it moves when Envoy
	// applies a CDS response that carries a cluster it did not have.
	added int
	// active and warming are the gauges active_clusters and warming_clusters.
	active, warming int
}

// clusterManagerCounts reads the three totals in ONE admin request, so they
// describe the same moment (the admin handler runs on Envoy's main thread,
// which is also the only thread that moves a cluster between the warming and
// the active set). ok is false when the read failed or a total is missing.
func (h *adsProxyHandle) clusterManagerCounts(t *testing.T) (c clusterManagerCounts, ok bool) {
	t.Helper()

	stats := h.stats(t, "cluster_manager.")
	for name, into := range map[string]*int{
		"cluster_manager.cluster_added":    &c.added,
		"cluster_manager.active_clusters":  &c.active,
		"cluster_manager.warming_clusters": &c.warming,
	} {
		raw, present := stats[name]
		if !present {
			return clusterManagerCounts{}, false
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("parse %s=%q: %v", name, raw, err)
		}
		*into = n
	}
	return c, true
}

// lateClusterWatch times ONE cluster that a later snapshot adds, from the
// cluster manager's totals.
//
// It judges against the totals read BEFORE the publish, never against fixed
// numbers. "Nothing is warming and N clusters are active" is already true
// before Envoy has applied the CDS response when N clusters were active
// beforehand: SetSnapshot returns before the delta server has written the
// response, so a sample taken in the first milliseconds still shows the old
// state. That recorded "left warming after ~5 ms" in about 3 % of the
// race-detector runs of the shared-name negative control, whose three
// pre-existing clusters met its fixed threshold of three (aether#1416).
type lateClusterWatch struct {
	before clusterManagerCounts
	// accepted is how long after the publish cluster_added had moved: Envoy has
	// applied the CDS response and holds the late cluster, warming or active
	// (0 = never within the window). It is a counter, so a 50 ms sampler cannot
	// miss it the way it can miss a warming gauge that is back to 0 within a
	// few milliseconds on the green arm.
	accepted time.Duration
	// leftWarming is how long after the publish the late cluster was in the
	// active set with nothing warming (0 = never within the window).
	leftWarming time.Duration
}

// watchLateCluster records the totals before the publish; nothing may be
// warming at that point, or a later "warming is 0" would not be about the late
// cluster.
func (h *adsProxyHandle) watchLateCluster(t *testing.T) *lateClusterWatch {
	t.Helper()

	before, ok := h.clusterManagerCounts(t)
	require.True(t, ok, "precondition: the cluster manager's totals are readable")
	require.Equal(t, 0, before.warming, "precondition: nothing warming before the late cluster")
	return &lateClusterWatch{before: before}
}

// sample takes one reading, elapsed after the publish. The late cluster has
// left warming once one more cluster than before is ACTIVE and none is warming;
// that cannot hold before Envoy has accepted the late cluster.
func (w *lateClusterWatch) sample(t *testing.T, h *adsProxyHandle, start time.Time) {
	t.Helper()

	now, ok := h.clusterManagerCounts(t)
	if !ok {
		return
	}
	if now.added <= w.before.added {
		return // Envoy has not applied the CDS response yet: nothing to judge.
	}
	if w.accepted == 0 {
		w.accepted = time.Since(start)
	}
	if w.leftWarming == 0 && now.warming == 0 && now.active > w.before.active {
		w.leftWarming = time.Since(start)
	}
}

// lateTwinRun is what one arm observed after the second twin was published.
type lateTwinRun struct {
	// firstOK is how long the late twin took to serve its first 200 (0 = never
	// within the observation window).
	firstOK time.Duration
	// accepted is how long until Envoy held the late twin, warming or active
	// (lateClusterWatch.accepted; 0 = never within the window).
	accepted time.Duration
	// leftWarming is how long until the late twin was active with nothing
	// warming (lateClusterWatch.leftWarming; 0 = never within the window).
	leftWarming time.Duration
	// initFetchTimeouts is cluster.<twin stats key>.init_fetch_timeout at the end.
	initFetchTimeouts int
	lastStatus        int
	lastBody          string
}

// runLateTwin starts Envoy with the base + one twin, waits until both serve,
// then publishes a snapshot adding a second twin for a NEW identity and
// observes that twin for up to window.
func runLateTwin(t *testing.T, sharedEDSName bool, window time.Duration) lateTwinRun {
	t.Helper()

	bin, err := envoybin.Path()
	if err != nil {
		var unsupported *envoybin.ErrUnsupportedArch
		if errors.As(err, &unsupported) {
			t.Skipf("%v", err)
		}
		t.Fatalf("locate envoy: %v", err)
	}

	dest := startH2CDestination(t)
	f := newTwinFixture(t, dest, sharedEDSName)
	listener := clusterHeaderListener(envoyPicksPort)

	cp := startADSControlPlane(t, f.resources(t, listener, twinSourceA))
	e := launchEnvoyOverADS(t, bin, cp)
	h := &adsProxyHandle{adminAddr: e.admin, cp: cp}
	addr := e.listenerAddr(t, listener.GetName())

	twinA := proxy.QUICClusterName(twinDestSvc, twinDomain, proxy.SourceSAKeyFromSpiffeID(twinSourceA))
	twinB := proxy.QUICClusterName(twinDestSvc, twinDomain, proxy.SourceSAKeyFromSpiffeID(twinSourceB))
	twinBStats := proxy.QUICAltStatName(twinDestSvc, proxy.SourceSAKeyFromSpiffeID(twinSourceB))

	// Precondition: the base and the first twin are both active and serving.
	// They arrived in ONE CDS response, so one subscribe served both even in the
	// old shape -- which is exactly why a startup-only harness cannot see #1008.
	deadline := time.Now().Add(envoyInitialFetchTimeout + 10*time.Second)
	for _, name := range []string{f.base.GetName(), twinA} {
		for {
			code, body := statusVia(addr, name)
			if code == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("precondition: %s never served (last %d %q)", name, code, body)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	watch := h.watchLateCluster(t)

	// A new ServiceAccount's first pod: the agent republishes with one more twin.
	cp.publish(t, "2", f.resources(t, listener, twinSourceA, twinSourceB))
	start := time.Now()
	t.Logf("published snapshot 2 adding %s (shared EDS name=%v)", twinB, sharedEDSName)

	var run lateTwinRun
	for time.Since(start) < window {
		if run.firstOK == 0 {
			run.lastStatus, run.lastBody = statusVia(addr, twinB)
			if run.lastStatus == http.StatusOK {
				run.firstOK = time.Since(start)
			}
		}
		if watch.leftWarming == 0 {
			watch.sample(t, h, start)
		}
		if run.firstOK != 0 && watch.leftWarming != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	run.accepted, run.leftWarming = watch.accepted, watch.leftWarming
	run.initFetchTimeouts = h.statInt(t, "cluster."+twinBStats+".init_fetch_timeout")
	t.Logf("late twin %s: accepted by Envoy after %s, first 200 after %s, left warming after %s, init_fetch_timeout=%d, last status %d %q",
		twinB, run.accepted, run.firstOK, run.leftWarming, run.initFetchTimeouts, run.lastStatus, run.lastBody)
	require.NotZerof(t, run.accepted,
		"Envoy never accepted the late twin within %s (cluster_manager.cluster_added did not move): "+
			"the CDS update did not arrive, so nothing about its warming was observed", window)
	return run
}

// TestLateQUICTwinGetsEndpointsPromptly is the green arm: with the twin on its
// own EDS name (production since #1008) a twin added after its base serves
// within lateTwinBudget and never hits init_fetch_timeout.
func TestLateQUICTwinGetsEndpointsPromptly(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	run := runLateTwin(t, false, lateTwinBudget)
	require.NotZerof(t, run.firstOK,
		"the late twin did not serve within %s (last %d %q): its EDS subscription was not answered -- "+
			"the aether#1008 signature (delta WatchMap dedup on a shared EDS name)", lateTwinBudget, run.lastStatus, run.lastBody)
	require.NotZero(t, run.leftWarming, "cluster_manager.warming_clusters never returned to 0")
	require.Equal(t, 0, run.initFetchTimeouts, "the late twin's EDS subscription timed out")
}

// TestLateQUICTwinWithSharedEDSNameStaysWarming is the negative control: the
// pre-#1008 shape (twin keeps the base's EDS name, no load assignment under
// its own) MUST reproduce the outage in this harness -- no 200 within
// lateTwinBudget, warming for ~initial_fetch_timeout, init_fetch_timeout = 1.
// If this ever passes the green arm above is vacuous: the harness would no
// longer be able to see the defect it gates.
func TestLateQUICTwinWithSharedEDSNameStaysWarming(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	run := runLateTwin(t, true, envoyInitialFetchTimeout+5*time.Second)
	if run.firstOK != 0 {
		require.Greaterf(t, run.firstOK, lateTwinBudget,
			"negative control: the shared-EDS-name twin served in %s; the harness no longer reproduces aether#1008", run.firstOK)
	}
	require.NotZero(t, run.leftWarming, "negative control: the twin never left warming even after initial_fetch_timeout")
	require.Greaterf(t, run.leftWarming, envoyInitialFetchTimeout-2*time.Second,
		"negative control: the shared-EDS-name twin left warming after only %s; expected ~%s (initial_fetch_timeout)",
		run.leftWarming, envoyInitialFetchTimeout)
	require.Equal(t, 1, run.initFetchTimeouts,
		"negative control: the shared-EDS-name twin must hit init_fetch_timeout (the fleet signature)")
}
