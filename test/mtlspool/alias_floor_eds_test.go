// This file is the live gate for aether#1013: a port-alias cluster
// ("<fqdn>:<port>") or a TCP floor cluster ("tcp:<fqdn>") added AFTER the
// service's default cluster is already subscribed must get its endpoints
// promptly.
//
// # The failure
//
// Both used to subscribe to the bare-service EDS resource name the default
// cluster holds. Envoy's delta-ADS WatchMap deduplicates subscription interest
// per (type_url, resource name), so a sibling added in a LATER CDS update -- a
// Service gaining a port after the node depends on it, a TCPRoute attached
// after the service is in the dependency set -- adds nothing to
// resource_names_subscribe, no request goes out, the control plane correctly
// sends nothing (the resource did not change), and the sibling warms for the
// full initial_fetch_timeout (15 s). The #1008 mechanism (quic_twin_eds_test.go)
// on the two other cluster kinds that shared the name.
//
// # What runs
//
// quic_twin_eds_test.go's harness: the pinned proxy against the go-control-plane
// snapshot cache over DELTA_GRPC ADS. The base is production's
// proxy.NewServiceCluster under the bare service name; the sibling is built by
// production's builder (proxy.NewServiceCluster for the alias,
// proxy.NewTCPServiceCluster for the floor) with the EDS name the agent's cache
// gives it -- its own cluster name -- and its load assignment by
// proxy.LoadAssignmentAlias, exactly as the cache does
// (//agent/internal/xds/cache's TestLate*SubscribesToItsOwnEDSResource pin that
// the cache emits this shape). Transport sockets are left off: the transport
// is not under test and the EDS shape is.
//
// The alias is reached through a cluster_header route, the floor through a
// tcp_proxy listener (raw TCP carrying an HTTP/1.1 request to the destination),
// so each arm measures when the late cluster actually FORWARDS.
//
// The OLD shape (sibling on the bare name, no load assignment under its own)
// is kept as a permanent negative control per kind: it must reproduce the 15 s
// warming and init_fetch_timeout, or the green arms prove nothing.
package mtlspool

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/config"
	"aethermesh.dev/agent/internal/xds/proxy"
	meshconst "aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/test/envoybin"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
)

// siblingKind is the cluster kind that shares the default cluster's membership.
type siblingKind string

const (
	siblingPortAlias siblingKind = "port-alias"
	siblingTCPFloor  siblingKind = "tcp-floor"
)

// siblingFixture is the service's default cluster, its load assignment, and the
// late sibling in either EDS shape.
type siblingFixture struct {
	kind    siblingKind
	base    *clusterv3.Cluster
	baseCLA *endpointv3.ClusterLoadAssignment
	// sharedEDSName reproduces the pre-#1013 shape: the sibling subscribes to
	// the bare service name and nothing is published under its own.
	sharedEDSName bool
}

func newSiblingFixture(t *testing.T, kind siblingKind, destAddr string, sharedEDSName bool) *siblingFixture {
	t.Helper()

	fqdn := proxy.ServiceClusterName(twinDestSvc, twinDomain)
	base := proxy.NewServiceCluster(fqdn, twinDestSvc, twinDestSvc, nil)
	require.Equal(t, twinDestSvc, base.GetEdsClusterConfig().GetServiceName(), "the default cluster keeps the bare-service EDS name")
	return &siblingFixture{
		kind:          kind,
		base:          base,
		baseCLA:       staticEndpoint(twinDestSvc, destAddr),
		sharedEDSName: sharedEDSName,
	}
}

// name is the sibling's cluster name, as the agent's cache spells it.
func (f *siblingFixture) name() string {
	if f.kind == siblingTCPFloor {
		return proxy.TCPClusterName(twinDestSvc, twinDomain)
	}
	return proxy.PortClusterName(twinDestSvc, twinDomain, meshconst.ProxyOutboundPort)
}

// sibling returns the late cluster and, in the fixed shape, the load assignment
// published under its own EDS name. Its alt_stat_name is production's: the bare
// service key, so its stats land in the default cluster's cluster.<ns>/<svc>.*
// tree -- the tree the fleet gate reads.
func (f *siblingFixture) sibling() (*clusterv3.Cluster, types.Resource) {
	name := f.name()
	eds := name
	if f.sharedEDSName {
		eds = twinDestSvc
	}
	var cl *clusterv3.Cluster
	if f.kind == siblingTCPFloor {
		cl = proxy.NewTCPServiceCluster(name, eds, twinDestSvc)
	} else {
		cl = proxy.NewServiceCluster(name, eds, twinDestSvc, nil)
	}
	if f.sharedEDSName {
		return cl, nil
	}
	return cl, proxy.LoadAssignmentAlias(f.baseCLA, name)
}

// resources is the snapshot content: the default cluster, optionally the
// sibling, and both probe listeners (the tcp_proxy one may name a cluster that
// is not there yet; tcp_proxy then just closes the connection).
func (f *siblingFixture) resources(listeners []types.Resource, withSibling bool) map[resourcev3.Type][]types.Resource {
	clusters := []types.Resource{f.base}
	clas := []types.Resource{f.baseCLA}
	if withSibling {
		cl, cla := f.sibling()
		clusters = append(clusters, cl)
		if cla != nil {
			clas = append(clas, cla)
		}
	}
	return map[resourcev3.Type][]types.Resource{
		resourcev3.ClusterType:  clusters,
		resourcev3.EndpointType: clas,
		resourcev3.ListenerType: listeners,
	}
}

// tcpProxyListener forwards every connection, raw, to cluster.
func tcpProxyListener(port int, cluster string) *listenerv3.Listener {
	return &listenerv3.Listener{
		Name:    "floor_eds",
		Address: socketAddress("127.0.0.1", port),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name: "envoy.filters.network.tcp_proxy",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(&tcpproxyv3.TcpProxy{
					StatPrefix:       "floor_eds",
					ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{Cluster: cluster},
				})},
			}},
		}},
	}
}

// statusViaTCP opens a raw TCP connection through the tcp_proxy listener and
// speaks HTTP/1.1 to the destination behind it. A cluster that is still warming
// is not in the worker's table, so tcp_proxy closes the connection: 0 + reason.
func statusViaTCP(addr string) (int, string) {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return 0, err.Error()
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: floor\r\nConnection: close\r\n\r\n"); err != nil {
		return 0, err.Error()
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	return resp.StatusCode, ""
}

// lateSiblingRun is what one arm observed after the sibling was published.
type lateSiblingRun struct {
	firstOK           time.Duration
	leftWarming       time.Duration
	initFetchTimeouts int
	lastStatus        int
	lastBody          string
}

// runLateSibling starts Envoy with the default cluster only, waits until it
// serves, then publishes a snapshot adding the sibling and observes it for up
// to window.
func runLateSibling(t *testing.T, kind siblingKind, sharedEDSName bool, window time.Duration) lateSiblingRun {
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
	f := newSiblingFixture(t, kind, dest, sharedEDSName)
	httpPort, tcpPort, adminPort := freePort(t), freePort(t), freePort(t)
	listeners := []types.Resource{clusterHeaderListener(httpPort), tcpProxyListener(tcpPort, f.name())}

	cp := startADSControlPlane(t, f.resources(listeners, false))
	launchEnvoyOverADS(t, bin, cp, adminPort)
	h := &adsProxyHandle{adminAddr: fmt.Sprintf("127.0.0.1:%d", adminPort), cp: cp}
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)
	tcpAddr := fmt.Sprintf("127.0.0.1:%d", tcpPort)
	waitListening(t, httpAddr)
	waitListening(t, tcpAddr)

	// Precondition: the default cluster is active and serving, so its EDS
	// subscription to the bare name is live before the sibling exists.
	deadline := time.Now().Add(envoyInitialFetchTimeout + 10*time.Second)
	for {
		code, body := statusVia(httpAddr, f.base.GetName())
		if code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("precondition: %s never served (last %d %q)", f.base.GetName(), code, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 0, h.statInt(t, "cluster_manager.warming_clusters"), "precondition: nothing warming before the sibling")
	// The sibling's stats share the default cluster's tree (alt_stat_name is the
	// bare service key), so this counter is exactly what the fleet gate reads.
	initFetchStat := "cluster." + twinDestSvc + ".init_fetch_timeout"
	require.Equal(t, 0, h.statInt(t, initFetchStat), "precondition: the default cluster's own EDS was answered")

	probe := func() (int, string) {
		if kind == siblingTCPFloor {
			return statusViaTCP(tcpAddr)
		}
		return statusVia(httpAddr, f.name())
	}
	if code, _ := probe(); code == http.StatusOK {
		t.Fatalf("precondition: %s served before it was published", f.name())
	}

	cp.publish(t, "2", f.resources(listeners, true))
	start := time.Now()
	t.Logf("published snapshot 2 adding %s %s (shared EDS name=%v)", kind, f.name(), sharedEDSName)

	var run lateSiblingRun
	for time.Since(start) < window {
		if run.firstOK == 0 {
			run.lastStatus, run.lastBody = probe()
			if run.lastStatus == http.StatusOK {
				run.firstOK = time.Since(start)
			}
		}
		if run.leftWarming == 0 && h.statInt(t, "cluster_manager.warming_clusters") == 0 &&
			h.statInt(t, "cluster_manager.active_clusters") >= 3 { // ADS + default + sibling
			run.leftWarming = time.Since(start)
		}
		if run.firstOK != 0 && run.leftWarming != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	run.initFetchTimeouts = h.statInt(t, initFetchStat)
	t.Logf("late %s %s: first 200 after %s, left warming after %s, %s=%d, last status %d %q",
		kind, f.name(), run.firstOK, run.leftWarming, initFetchStat, run.initFetchTimeouts, run.lastStatus, strings.TrimSpace(run.lastBody))
	return run
}

func requireLateSiblingPrompt(t *testing.T, kind siblingKind) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	run := runLateSibling(t, kind, false, lateTwinBudget)
	require.NotZerof(t, run.firstOK,
		"the late %s did not forward within %s (last %d %q): its EDS subscription was not answered -- "+
			"the aether#1013 signature (delta WatchMap dedup on a shared EDS name)", kind, lateTwinBudget, run.lastStatus, run.lastBody)
	require.NotZero(t, run.leftWarming, "cluster_manager.warming_clusters never returned to 0")
	require.Equal(t, 0, run.initFetchTimeouts, "the late %s's EDS subscription timed out", kind)
}

func requireLateSiblingWithSharedNameStaysWarming(t *testing.T, kind siblingKind) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}
	run := runLateSibling(t, kind, true, envoyInitialFetchTimeout+5*time.Second)
	if run.firstOK != 0 {
		require.Greaterf(t, run.firstOK, lateTwinBudget,
			"negative control: the shared-EDS-name %s forwarded in %s; the harness no longer reproduces aether#1013", kind, run.firstOK)
	}
	require.NotZerof(t, run.leftWarming, "negative control: the %s never left warming even after initial_fetch_timeout", kind)
	require.Greaterf(t, run.leftWarming, envoyInitialFetchTimeout-2*time.Second,
		"negative control: the shared-EDS-name %s left warming after only %s; expected ~%s (initial_fetch_timeout)",
		kind, run.leftWarming, envoyInitialFetchTimeout)
	require.Equalf(t, 1, run.initFetchTimeouts,
		"negative control: the shared-EDS-name %s must hit init_fetch_timeout (the fleet signature)", kind)
}

// TestLatePortAliasGetsEndpointsPromptly is the green arm for port aliases:
// with the alias on its own EDS name (production since #1013) an alias added
// after the default cluster forwards within lateTwinBudget and never hits
// init_fetch_timeout.
func TestLatePortAliasGetsEndpointsPromptly(t *testing.T) {
	requireLateSiblingPrompt(t, siblingPortAlias)
}

// TestLatePortAliasWithSharedEDSNameStaysWarming is the port-alias negative
// control: the pre-#1013 shape MUST reproduce ~15 s of warming and
// init_fetch_timeout = 1, or the green arm is vacuous.
func TestLatePortAliasWithSharedEDSNameStaysWarming(t *testing.T) {
	requireLateSiblingWithSharedNameStaysWarming(t, siblingPortAlias)
}

// TestLateTCPFloorGetsEndpointsPromptly is the green arm for the TCP floor: a
// floor added after the default cluster forwards a raw TCP connection within
// lateTwinBudget and never hits init_fetch_timeout.
func TestLateTCPFloorGetsEndpointsPromptly(t *testing.T) {
	requireLateSiblingPrompt(t, siblingTCPFloor)
}

// TestLateTCPFloorWithSharedEDSNameStaysWarming is the TCP-floor negative
// control: the pre-#1013 shape MUST reproduce the outage, during which
// tcp_proxy closes every captured connection to the service.
func TestLateTCPFloorWithSharedEDSNameStaysWarming(t *testing.T) {
	requireLateSiblingWithSharedNameStaysWarming(t, siblingTCPFloor)
}
