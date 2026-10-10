package cache

import (
	"log/slog"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Issue #1635: one load assignment per name reaches the snapshot.
//
// The HTTP default entry, the TCP floor entry and the UDP floor entry of one
// service all name the service's BARE load assignment (<ns>/<svc>). The rule
// these tests pin: it is published by exactly one entry, the first of HTTP,
// TCP, UDP that is built from a LIVE listing; an entry retained after its
// listing went (serviceRetentionGrace) publishes an empty one only while no
// live entry publishes that name. A retained entry keeps its cluster and its
// vhost either way.
//
// Before the fix a retained entry kept an EMPTY load assignment under the
// name a live entry published a populated one, go-control-plane kept one of
// the two, and which one was redrawn on every build.

const (
	bareBuilds = 32

	bareSvc    = "demo/db"  // TCP in the fixture (2 rows)
	bareUDPSvc = "demo/dns" // UDP in the fixture (1 row)
)

const (
	http = registryv1.Service_PROTOCOL_HTTP
	tcp  = registryv1.Service_PROTOCOL_TCP
	udp  = registryv1.Service_PROTOCOL_UDP
)

// bareFixture is the registry-reuse fixture with the capture floors rendered
// (so the tcp: and udp: clusters and their own-name copies of the bare load
// assignment are published), the log recorded and the metrics readable.
type bareFixture struct {
	*reuseFixture
	rec    *recorder
	reader *sdkmetric.ManualReader
}

func newBareFixture(t *testing.T) *bareFixture {
	t.Helper()
	f := &bareFixture{reuseFixture: newReuseFixture(t), rec: &recorder{}, reader: sdkmetric.NewManualReader()}
	f.c.log = slog.New(&captureHandler{rec: f.rec})
	m, err := cachemetrics.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(f.reader)).Meter("test"))
	require.NoError(t, err)
	f.c.metrics = m
	f.c.SetCaptureEnabled(true)
	f.c.SetCaptureTCPServices([]capture.CaptureTCPService{
		{ServiceName: bareSvc, ClusterIP: "10.96.0.60", PrimaryIsTCP: true},
		{ServiceName: bareUDPSvc, ClusterIP: "10.96.0.61", PrimaryIsTCP: true},
	})
	f.c.SetUDPServiceRoutes(map[string][]proxy.L4Backend{
		bareSvc:    {{Service: bareSvc, Cluster: proxy.UDPClusterName(bareSvc, f.c.meshDomain), Weight: 1}},
		bareUDPSvc: {{Service: bareUDPSvc, Cluster: proxy.UDPClusterName(bareUDPSvc, f.c.meshDomain), Weight: 1}},
	})
	f.refresh(t)
	return f
}

// list sets one service's rows under one protocol: n pods whose addresses
// start at 10.2.<block>.1, so two listings of one service never share a pod
// (a pod is listed under the one protocol it declares). n == 0 removes the
// listing.
func (f *bareFixture) list(p registryv1.Service_Protocol, svc string, block, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n == 0 {
		delete(f.listing[p], svc)
		return
	}
	rows := make([]*registryv1.ServiceEndpoint, 0, n)
	for i := 1; i <= n; i++ {
		rows = append(rows, reuseEndpoint("10.2."+string(rune('0'+block))+"."+string(rune('0'+i))))
	}
	f.listing[p][svc] = rows
}

// lbEndpoints is how many endpoints a load assignment carries.
func lbEndpoints(cla *endpointv3.ClusterLoadAssignment) int {
	n := 0
	for _, l := range cla.GetEndpoints() {
		n += len(l.GetLbEndpoints())
	}
	return n
}

// published is how many endpoints the load assignment the proxy is sent under
// name carries, or -1 when none is published under it.
func published(s *cachev3.Snapshot, name string) int {
	cla, ok := s.GetResources(resourcev3.EndpointType)[name].(*endpointv3.ClusterLoadAssignment)
	if !ok {
		return -1
	}
	return lbEndpoints(cla)
}

// udpEndpoints is how many endpoints the udp: cluster of svc carries (its load
// assignment is inline: a STATIC cluster), or -1 when it is not published.
func (f *bareFixture) udpEndpoints(s *cachev3.Snapshot, svc string) int {
	cl, ok := s.GetResources(resourcev3.ClusterType)[proxy.UDPClusterName(svc, f.c.meshDomain)].(*clusterv3.Cluster)
	if !ok {
		return -1
	}
	return lbEndpoints(cl.GetLoadAssignment())
}

// requireOneOwner asserts the structural rule: no two entries of the cluster
// cache hold a load assignment of one name, EQUAL ones included.
func (f *bareFixture) requireOneOwner(t *testing.T) {
	t.Helper()
	f.c.clusterMu.RLock()
	defer f.c.clusterMu.RUnlock()
	owner := map[string]string{}
	for key, e := range f.c.clusters {
		if e.loadAssignment == nil {
			continue
		}
		name := e.loadAssignment.GetClusterName()
		if prev, dup := owner[name]; dup {
			require.Failf(t, "two entries hold one load-assignment name", "%q is held by %q and by %q", name, prev, key)
		}
		owner[name] = key
	}
}

// want is what every one of a run of builds must publish under a name.
type want map[string]int

// requireStable refreshes bareBuilds times and requires, on every build: each
// name in want publishes exactly that many endpoints, one entry holds each
// name, and after the first build nothing published under those names gets a
// new version (no EDS push). It returns the last snapshot.
func (f *bareFixture) requireStable(t *testing.T, w want) *cachev3.Snapshot {
	t.Helper()
	var last *cachev3.Snapshot
	seen := map[string][]int{}
	versions := map[string]map[string]struct{}{}
	for range bareBuilds {
		last = f.refresh(t)
		f.requireOneOwner(t)
		for name := range w {
			seen[name] = append(seen[name], published(last, name))
			if versions[name] == nil {
				versions[name] = map[string]struct{}{}
			}
			versions[name][last.GetVersionMap(resourcev3.EndpointType)[name]] = struct{}{}
		}
	}
	for name, n := range w {
		exp := make([]int, bareBuilds)
		for i := range exp {
			exp[i] = n
		}
		assert.Equal(t, exp, seen[name], "endpoints published under %q over %d builds", name, bareBuilds)
		assert.Len(t, versions[name], 1, "%q must keep one version over %d unchanged builds: every new one is an EDS push", name, bareBuilds)
	}
	return last
}

// requireCheckSilent: the duplicate-name check of #1634 has nothing to say.
func (f *bareFixture) requireCheckSilent(t *testing.T) {
	t.Helper()
	assert.Empty(t, f.rec.with(duplicateNamesMsg), "no two resources of a type share a name")
	assert.Zero(t, counterValue(t, f.reader, duplicateNamesCtr))
}

func (f *bareFixture) tcpFloor(svc string) string { return proxy.TCPClusterName(svc, f.c.meshDomain) }

func (f *bareFixture) meshAlias(svc string) string {
	return proxy.PortClusterName(svc, f.c.meshDomain, meshconst.ProxyOutboundPort)
}

// requireHTTPClusterAndVhost: the retention protection. The service's h2
// cluster and its outbound vhost are still published, so a request to it is
// neither a route-table miss (404) nor an on-demand stall.
func (f *bareFixture) requireHTTPClusterAndVhost(t *testing.T, s *cachev3.Snapshot, svc string, present bool) {
	t.Helper()
	fqdn := f.fqdn(svc)
	_, hasCluster := s.GetResources(resourcev3.ClusterType)[fqdn]
	rc, ok := s.GetResources(resourcev3.RouteType)[proxy.OutboundHTTPRouteName].(*routev3.RouteConfiguration)
	require.True(t, ok)
	hasVhost := false
	for _, vh := range rc.GetVirtualHosts() {
		hasVhost = hasVhost || vh.GetName() == fqdn
	}
	assert.Equal(t, present, hasCluster, "h2 cluster %q published", fqdn)
	assert.Equal(t, present, hasVhost, "outbound vhost %q published", fqdn)
}

// The issue's input. A service listed under HTTP (3 pods) and TCP (2 other
// pods) loses its HTTP listing. For the retention grace its h2 cluster and
// vhost stay; the one load assignment published under the bare name is the
// live TCP listing's, on every build, and so is every copy derived from it:
// the TCP floor's (tcp_proxy has no on-demand path to recover from an empty
// one), the h2 port alias's and the QUIC twin's.
func TestHTTPListingLostWhileTCPStaysPublishesTheLiveLoadAssignment(t *testing.T) {
	f := newBareFixture(t)
	f.list(http, bareSvc, 4, 3)
	s := f.refresh(t)
	twin := proxy.QUICClusterName(bareSvc, f.c.meshDomain, "demo/"+reuseSA)
	d, reason := f.c.recordQUICPair(testQUICStream, twin)
	require.NotEqual(t, QUICTwinRefused, d, reason)
	s = f.refresh(t)
	require.Equal(t, 3, published(s, bareSvc), "both listed: the HTTP entry owns the bare name")
	require.Equal(t, 3, published(s, twin), "fixture: the twin is published with its own copy")
	f.requireOneOwner(t)
	f.requireCheckSilent(t)

	f.list(http, bareSvc, 0, 0)
	s = f.requireStable(t, want{
		bareSvc:              2,
		f.tcpFloor(bareSvc):  2,
		f.meshAlias(bareSvc): 2,
		twin:                 2,
	})
	f.requireHTTPClusterAndVhost(t, s, bareSvc, true)
	assert.Contains(t, s.GetResources(resourcev3.ClusterType), twin, "the retained cluster's twin stays too")
	assert.Equal(t, 2, f.udpEndpoints(s, bareSvc), "the udp: cluster is rendered from the live load assignment")
	f.requireCheckSilent(t)

	// Past the grace the retained entry is pruned and nothing else changes.
	f.c.serviceRetentionGrace = time.Nanosecond
	f.refresh(t)
	s = f.refresh(t)
	f.requireHTTPClusterAndVhost(t, s, bareSvc, false)
	assert.Equal(t, 2, published(s, bareSvc))
	assert.Equal(t, 2, published(s, f.tcpFloor(bareSvc)))
	f.requireOneOwner(t)
	f.requireCheckSilent(t)
}

// The mirror: the TCP listing goes and the HTTP one stays. The HTTP entry
// owned the bare name and keeps it; the retained floor entry held none.
func TestTCPListingLostWhileHTTPStaysKeepsTheHTTPLoadAssignment(t *testing.T) {
	f := newBareFixture(t)
	f.list(http, bareSvc, 4, 3)
	f.refresh(t)

	f.list(tcp, bareSvc, 0, 0)
	s := f.requireStable(t, want{bareSvc: 3, f.tcpFloor(bareSvc): 3, f.meshAlias(bareSvc): 3})
	assert.Contains(t, s.GetResources(resourcev3.ClusterType), f.tcpFloor(bareSvc), "the floor cluster is retained")
	f.requireHTTPClusterAndVhost(t, s, bareSvc, true)
	f.requireCheckSilent(t)
}

// A service that changes protocol between two refreshes: its TCP listing goes
// in the refresh its HTTP listing appears in. The retained floor entry OWNED
// the bare name; it gives it up to the live HTTP entry.
func TestTCPListingReplacedByAnHTTPListingInOneRefresh(t *testing.T) {
	f := newBareFixture(t)
	require.Equal(t, 2, published(f.snapshot(t), bareSvc), "fixture: TCP-only, the floor entry owns the bare name")

	f.list(tcp, bareSvc, 0, 0)
	f.list(http, bareSvc, 4, 3)
	s := f.requireStable(t, want{bareSvc: 3, f.tcpFloor(bareSvc): 3})
	assert.Contains(t, s.GetResources(resourcev3.ClusterType), f.tcpFloor(bareSvc), "the floor cluster is retained")
	f.requireCheckSilent(t)
}

// Both listings go. One EMPTY load assignment is published under the bare
// name (the retention's honest 503), both clusters stay.
func TestBothListingsLostPublishOneEmptyLoadAssignment(t *testing.T) {
	f := newBareFixture(t)
	f.list(http, bareSvc, 4, 3)
	f.refresh(t)

	f.list(http, bareSvc, 0, 0)
	f.list(tcp, bareSvc, 0, 0)
	s := f.requireStable(t, want{bareSvc: 0, f.tcpFloor(bareSvc): 0, f.meshAlias(bareSvc): 0})
	f.requireHTTPClusterAndVhost(t, s, bareSvc, true)
	assert.Contains(t, s.GetResources(resourcev3.ClusterType), f.tcpFloor(bareSvc))
	f.requireCheckSilent(t)

	// The TCP listing comes back while the HTTP entry is still retained:
	// the retained entry was holding the (empty) bare name and gives it up.
	f.list(tcp, bareSvc, 2, 2)
	s = f.requireStable(t, want{bareSvc: 2, f.tcpFloor(bareSvc): 2, f.meshAlias(bareSvc): 2})
	f.requireHTTPClusterAndVhost(t, s, bareSvc, true)
	f.requireCheckSilent(t)

	// And the HTTP listing after it: the HTTP entry owns the name again.
	f.list(http, bareSvc, 4, 3)
	f.requireStable(t, want{bareSvc: 3, f.tcpFloor(bareSvc): 3})
	f.requireCheckSilent(t)
}

// UDP shares the mechanism: the UDP floor entry names the bare load
// assignment too. A service listed under TCP (2 pods) and UDP (1 other pod)
// and not under HTTP publishes ONE, the TCP entry's; when either listing
// goes, the live one's.
func TestTCPAndUDPListingsPublishOneLoadAssignment(t *testing.T) {
	f := newBareFixture(t)
	require.Equal(t, 1, published(f.snapshot(t), bareUDPSvc), "fixture: UDP-only")

	f.list(tcp, bareUDPSvc, 5, 2)
	f.requireStable(t, want{bareUDPSvc: 2, f.tcpFloor(bareUDPSvc): 2})
	f.requireCheckSilent(t)

	t.Run("UDP listing lost, TCP stays", func(t *testing.T) {
		f.list(udp, bareUDPSvc, 0, 0)
		s := f.requireStable(t, want{bareUDPSvc: 2, f.tcpFloor(bareUDPSvc): 2})
		assert.Equal(t, 2, f.udpEndpoints(s, bareUDPSvc), "the retained udp: cluster follows the live load assignment")
		f.requireCheckSilent(t)
	})
	t.Run("TCP listing lost, UDP stays", func(t *testing.T) {
		f.list(udp, bareUDPSvc, 3, 1)
		f.refresh(t)
		f.list(tcp, bareUDPSvc, 0, 0)
		s := f.requireStable(t, want{bareUDPSvc: 1, f.tcpFloor(bareUDPSvc): 1})
		assert.Equal(t, 1, f.udpEndpoints(s, bareUDPSvc))
		f.requireCheckSilent(t)
	})
}

// HTTP lost while UDP stays: the same shape as the issue's, on the UDP floor.
// The udp: cluster is rendered from the live load assignment, not from the
// retained HTTP entry.
func TestHTTPListingLostWhileUDPStaysPublishesTheLiveLoadAssignment(t *testing.T) {
	f := newBareFixture(t)
	f.list(http, bareUDPSvc, 4, 3)
	s := f.refresh(t)
	require.Equal(t, 3, published(s, bareUDPSvc))

	f.list(http, bareUDPSvc, 0, 0)
	s = f.requireStable(t, want{bareUDPSvc: 1, f.meshAlias(bareUDPSvc): 1})
	assert.Equal(t, 1, f.udpEndpoints(s, bareUDPSvc), "datagrams reach the live UDP endpoint through the grace")
	f.requireHTTPClusterAndVhost(t, s, bareUDPSvc, true)
	f.requireCheckSilent(t)
}
