package cache

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math/rand"
	"slices"
	"strings"
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
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
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

// listingPort is the application port the pods of one listing register. One
// per protocol, on purpose: a pod is listed under the protocol it declares, so
// two listings of one service need not agree on the port, and a reader that
// takes the endpoints from one entry and the port from another is only seen
// when they differ.
var listingPort = map[registryv1.Service_Protocol]uint32{http: 8080, tcp: 9000, udp: 5353}

// list sets one service's rows under one protocol: n pods whose addresses
// start at 10.2.<block>.1, so two listings of one service never share a pod
// (a pod is listed under the one protocol it declares), each on the
// protocol's listingPort. n == 0 removes the listing.
func (f *bareFixture) list(p registryv1.Service_Protocol, svc string, block, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n == 0 {
		delete(f.listing[p], svc)
		return
	}
	rows := make([]*registryv1.ServiceEndpoint, 0, n)
	for i := 1; i <= n; i++ {
		ep := reuseEndpoint(fmt.Sprintf("10.2.%d.%d", block, i))
		ep.Port = listingPort[p]
		rows = append(rows, ep)
	}
	f.listing[p][svc] = rows
}

// udpAddrs is the sorted address:port list the udp: cluster of svc dials, or
// nil when it is not published.
func (f *bareFixture) udpAddrs(s *cachev3.Snapshot, svc string) []string {
	cl, ok := s.GetResources(resourcev3.ClusterType)[proxy.UDPClusterName(svc, f.c.meshDomain)].(*clusterv3.Cluster)
	if !ok {
		return nil
	}
	var addrs []string
	for _, l := range cl.GetLoadAssignment().GetEndpoints() {
		for _, lb := range l.GetLbEndpoints() {
			sa := lb.GetEndpoint().GetAddress().GetSocketAddress()
			addrs = append(addrs, fmt.Sprintf("%s:%d", sa.GetAddress(), sa.GetPortValue()))
		}
	}
	slices.Sort(addrs)
	return addrs
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
	f.list(tcp, bareSvc, 2, 2)
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
	assert.Equal(t, []string{"10.2.2.1:9000", "10.2.2.2:9000"}, f.udpAddrs(s, bareSvc),
		"the udp: cluster is rendered from the live entry: its endpoints AND its port, not the retained HTTP entry's 8080")
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
		assert.Equal(t, []string{"10.2.5.1:9000", "10.2.5.2:9000"}, f.udpAddrs(s, bareUDPSvc),
			"the retained udp: cluster follows the live entry, endpoints and port")
		f.requireCheckSilent(t)
	})
	t.Run("TCP listing lost, UDP stays", func(t *testing.T) {
		f.list(udp, bareUDPSvc, 3, 1)
		f.refresh(t)
		f.list(tcp, bareUDPSvc, 0, 0)
		s := f.requireStable(t, want{bareUDPSvc: 1, f.tcpFloor(bareUDPSvc): 1})
		assert.Equal(t, []string{"10.2.3.1:5353"}, f.udpAddrs(s, bareUDPSvc),
			"the live UDP pod at the port it registered, not the retained TCP entry's 9000")
		f.requireCheckSilent(t)
	})
}

// HTTP lost while UDP stays: the same shape as the issue's, on the UDP floor.
// The udp: cluster is rendered from the live load assignment, not from the
// retained HTTP entry.
func TestHTTPListingLostWhileUDPStaysPublishesTheLiveLoadAssignment(t *testing.T) {
	f := newBareFixture(t)
	f.list(udp, bareUDPSvc, 3, 1)
	f.list(http, bareUDPSvc, 4, 3)
	s := f.refresh(t)
	require.Equal(t, 3, published(s, bareUDPSvc))

	f.list(http, bareUDPSvc, 0, 0)
	s = f.requireStable(t, want{bareUDPSvc: 1, f.meshAlias(bareUDPSvc): 1})
	assert.Equal(t, []string{"10.2.3.1:5353"}, f.udpAddrs(s, bareUDPSvc),
		"datagrams reach the live UDP pod at the port it registered through the grace, not at the retained HTTP entry's 8080")
	f.requireHTTPClusterAndVhost(t, s, bareUDPSvc, true)
	f.requireCheckSilent(t)
}

// The check itself, on a duplicate that has nothing to do with pods (two
// different load assignments of one name): one ERROR naming the type and the
// name and NO issue, since more than one input can produce it, and one count
// per name per build. No input of the cache is known to produce this one any
// more (#1635), so the check is driven directly.
func TestDuplicateLoadAssignmentNameIsReportedWithoutAnIssue(t *testing.T) {
	f := newBareFixture(t)
	ctx := context.Background()
	populated := proxy.NewClusterLoadAssignment(bareSvc)
	populated.Endpoints = []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{}}}}
	resources := map[resourcev3.Type][]types.Resource{
		resourcev3.EndpointType: {proxy.NewClusterLoadAssignment(bareSvc), populated},
	}

	f.c.snapshotMu.Lock()
	f.c.reportDuplicateResourceNames(ctx, resources)
	f.c.reportDuplicateResourceNames(ctx, resources)
	f.c.snapshotMu.Unlock()

	lines := f.rec.with(duplicateNamesMsg)
	require.Len(t, lines, 1, "written when the set of names changes, not once per build")
	assert.Equal(t, slog.LevelError, lines[0].level)
	assert.Equal(t, resourcev3.EndpointType, lines[0].attrs["type"])
	assert.Equal(t, "["+bareSvc+"]", lines[0].attrs["names"])
	assert.NotContains(t, lines[0].attrs, "issue", "the line claims no cause")
	assert.Equal(t, int64(2), counterValue(t, f.reader, duplicateNamesCtr), "one per name per build")
}

// walkModel is what the random walk believes about bareSvc.
type walkModel struct {
	h, t, u   int  // 0 absent, 1 and 2 two variants of the rows
	clock     int  // seconds the retained entries were aged by
	hLost     int  // clock when the HTTP listing was first seen absent; -1 otherwise
	hRetained bool // the h2 cluster is published (live or within its grace)
	inDeps    bool
}

// walkRows are n rows of one listing. Variant 2 adds a second advertised port
// (a per-port cluster), so the walk also changes which entries a pass writes.
func walkRows(p registryv1.Service_Protocol, block, n, variant int) []*registryv1.ServiceEndpoint {
	rows := make([]*registryv1.ServiceEndpoint, 0, n)
	for i := 1; i <= n; i++ {
		ep := reuseEndpoint(fmt.Sprintf("10.2.%d.%d", block, i))
		ep.Port = listingPort[p]
		if variant == 2 {
			second := listingPort[p] + 1
			ep.Ports = []uint32{ep.GetPort(), second}
			if p == tcp {
				ep.PortProtocols = map[uint32]registryv1.PortProtocol{second: registryv1.PortProtocol_PORT_PROTOCOL_TCP}
			}
		}
		rows = append(rows, ep)
	}
	return rows
}

func (f *bareFixture) setRows(p registryv1.Service_Protocol, svc string, rows []*registryv1.ServiceEndpoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rows == nil {
		delete(f.listing[p], svc)
		return
	}
	f.listing[p][svc] = rows
}

// age moves every retained entry's absentSince back by d.
func (f *bareFixture) age(d time.Duration) {
	f.c.clusterMu.Lock()
	defer f.c.clusterMu.Unlock()
	for k, e := range f.c.clusters {
		if !e.absentSince.IsZero() {
			e.absentSince = e.absentSince.Add(-d)
			f.c.clusters[k] = e
		}
	}
}

// describe lists bareSvc's entries, for a failing walk's trace.
func (f *bareFixture) describe() string {
	f.c.clusterMu.RLock()
	defer f.c.clusterMu.RUnlock()
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(f.c.clusters)) {
		e := f.c.clusters[k]
		if e.service != bareSvc {
			continue
		}
		la := "nil"
		if e.loadAssignment != nil {
			la = fmt.Sprintf("%s(%d)", e.loadAssignment.GetClusterName(), lbEndpoints(e.loadAssignment))
		}
		fmt.Fprintf(&b, "  entry %q la=%s retained=%v\n", k, la, !e.absentSince.IsZero())
	}
	return b.String()
}

// holderViolation: no two entries hold a load assignment of one name.
func (f *bareFixture) holderViolation() string {
	f.c.clusterMu.RLock()
	defer f.c.clusterMu.RUnlock()
	owner := map[string]string{}
	for key, e := range f.c.clusters {
		if e.loadAssignment == nil {
			continue
		}
		name := e.loadAssignment.GetClusterName()
		if prev, dup := owner[name]; dup {
			return fmt.Sprintf("two holders of %q: %q and %q", name, prev, key)
		}
		owner[name] = key
	}
	return ""
}

// derivedViolation: every EDS cluster of the service has its load assignment
// published, and every own-name copy has the bare one's membership.
func (f *bareFixture) derivedViolation(s *cachev3.Snapshot) string {
	eds := s.GetResources(resourcev3.EndpointType)
	for name, r := range s.GetResources(resourcev3.ClusterType) {
		cl, ok := r.(*clusterv3.Cluster)
		if !ok || cl.GetType() != clusterv3.Cluster_EDS || !strings.Contains(name, "db.demo") {
			continue
		}
		if _, ok := eds[edsServiceName(cl)]; !ok {
			return fmt.Sprintf("cluster %q subscribes to EDS %q, which is not published", name, edsServiceName(cl))
		}
	}
	bare, ok := eds[bareSvc].(*endpointv3.ClusterLoadAssignment)
	if !ok {
		return ""
	}
	for name, r := range eds {
		isCopy := name == f.tcpFloor(bareSvc) || name == f.meshAlias(bareSvc) ||
			strings.HasPrefix(name, "quic:") && strings.Contains(name, "db.demo")
		cla, ok := r.(*endpointv3.ClusterLoadAssignment)
		if isCopy && ok && lbEndpoints(cla) != lbEndpoints(bare) {
			return fmt.Sprintf("copy %q has %d endpoints, the bare one %d", name, lbEndpoints(cla), lbEndpoints(bare))
		}
	}
	return ""
}

// membershipViolation: the bare name carries the first live listing's rows, a
// live TCP listing's floor is not empty, and the udp: cluster dials the
// endpoints AND the port of the one entry that holds them.
func (f *bareFixture) membershipViolation(s *cachev3.Snapshot, m walkModel) string {
	got := published(s, bareSvc)
	if !m.inDeps {
		if got != -1 {
			return fmt.Sprintf("out of the dependency set, the bare name publishes %d", got)
		}
		return ""
	}
	var want int
	var port uint32
	switch {
	case m.h != 0:
		want, port = 3, listingPort[http]
	case m.t != 0:
		want, port = 2, listingPort[tcp]
	case m.u != 0:
		want, port = 1, listingPort[udp]
	default:
		return "" // nothing live: an empty one or none, by the retention's clock
	}
	if got != want {
		return fmt.Sprintf("the bare name publishes %d endpoints, want %d", got, want)
	}
	if c := published(s, f.tcpFloor(bareSvc)); m.t != 0 && c <= 0 {
		return fmt.Sprintf("live TCP listing: floor publishes %d", c)
	}
	addrs := f.udpAddrs(s, bareSvc)
	if len(addrs) != want {
		return fmt.Sprintf("udp: cluster dials %v, want %d endpoints", addrs, want)
	}
	for _, a := range addrs {
		if !strings.HasSuffix(a, fmt.Sprintf(":%d", port)) {
			return fmt.Sprintf("udp: cluster dials %v, want port %d (the port of the listing its endpoints are from)", addrs, port)
		}
	}
	return ""
}

// walkViolation returns the first invariant the snapshot breaks, or "".
func (f *bareFixture) walkViolation(t *testing.T, s *cachev3.Snapshot, m walkModel) string {
	t.Helper()
	for _, v := range []string{f.holderViolation(), f.derivedViolation(s), f.membershipViolation(s, m)} {
		if v != "" {
			return v
		}
	}
	// The retention protection, and the check.
	if _, hasH2 := s.GetResources(resourcev3.ClusterType)[f.fqdn(bareSvc)]; hasH2 != m.hRetained {
		return fmt.Sprintf("h2 cluster published=%v, the model says %v", hasH2, m.hRetained)
	}
	if n := counterValue(t, f.reader, duplicateNamesCtr); n != 0 {
		return fmt.Sprintf("duplicate-name counter = %d", n)
	}
	return ""
}

// walkVersions is every resource version of a snapshot, one per line.
func walkVersions(s *cachev3.Snapshot) string {
	var b strings.Builder
	for _, typ := range []string{resourcev3.EndpointType, resourcev3.ClusterType, resourcev3.RouteType, resourcev3.ListenerType} {
		vm := s.GetVersionMap(typ)
		for _, k := range slices.Sorted(maps.Keys(vm)) {
			fmt.Fprintf(&b, "%s|%s=%s\n", typ, k, vm[k])
		}
	}
	return b.String()
}

// walkStep applies one random change to the listings and the model and
// returns its name.
func (f *bareFixture) walkStep(r *rand.Rand, m *walkModel) string {
	set := func(p registryv1.Service_Protocol, block, n, variant int) {
		if variant == 0 {
			f.setRows(p, bareSvc, nil)
			return
		}
		f.setRows(p, bareSvc, walkRows(p, block, n, variant))
	}
	switch k := r.Intn(10); {
	case k < 3:
		m.h = r.Intn(3)
		set(http, 4, 3, m.h)
		return fmt.Sprintf("H=%d", m.h)
	case k < 6:
		m.t = r.Intn(3)
		set(tcp, 2, 2, m.t)
		return fmt.Sprintf("T=%d", m.t)
	case k < 8:
		m.u = r.Intn(2)
		set(udp, 3, 1, m.u)
		return fmt.Sprintf("U=%d", m.u)
	case k < 9:
		f.age(50 * time.Second)
		m.clock += 50
		return "age50"
	default: // two listings change in one refresh
		m.h, m.t = r.Intn(2), r.Intn(2)
		set(http, 4, 3, m.h)
		set(tcp, 2, 2, m.t)
		return fmt.Sprintf("H=%d,T=%d", m.h, m.t)
	}
}

// retainH2 moves the model's view of the h2 cluster one refresh on.
func (m *walkModel) retainH2() {
	switch {
	case !m.inDeps:
		m.hRetained, m.hLost = false, -1
	case m.h != 0:
		m.hRetained, m.hLost = true, -1
	case m.hRetained && m.hLost < 0:
		m.hLost = m.clock
	case m.hRetained && m.clock-m.hLost > 90:
		m.hRetained, m.hLost = false, -1
	}
}

// walk runs one seeded walk and returns the first violation and the trace.
func walk(t *testing.T, seed int64, steps int, withDeps bool) (string, []string) {
	t.Helper()
	f := newBareFixture(t)
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // a reproducible test sequence
	twin := proxy.QUICClusterName(bareSvc, f.c.meshDomain, "demo/"+reuseSA)
	m := walkModel{t: 1, hLost: -1, inDeps: true}
	f.setRows(tcp, bareSvc, walkRows(tcp, 2, 2, 1))
	f.refresh(t)
	var trace []string
	for step := range steps {
		op := f.walkStep(r, &m)
		if withDeps && r.Intn(6) == 0 {
			m.inDeps = !m.inDeps
			// A captured TCP service is a dependency too, so it leaves and
			// returns with the declaration.
			captured := []capture.CaptureTCPService{{ServiceName: bareUDPSvc, ClusterIP: "10.96.0.61", PrimaryIsTCP: true}}
			if m.inDeps {
				declareDeps(f.c, "demo/echo", "demo/other", bareSvc, bareUDPSvc)
				captured = append(captured, capture.CaptureTCPService{ServiceName: bareSvc, ClusterIP: "10.96.0.60", PrimaryIsTCP: true})
			} else {
				declareDeps(f.c, "demo/echo", "demo/other", bareUDPSvc)
			}
			f.c.SetCaptureTCPServices(captured)
			op += fmt.Sprintf("+deps=%v", m.inDeps)
		}
		if r.Intn(4) == 0 {
			f.c.recordQUICPair(testQUICStream, twin) // the proxy asks for the twin
			op += "+twin"
		}
		s := f.refresh(t)
		m.retainH2()
		trace = append(trace, fmt.Sprintf("%02d %-10s clock=%d h=%d t=%d u=%d\n%s", step, op, m.clock, m.h, m.t, m.u, f.describe()))
		if v := f.walkViolation(t, s, m); v != "" {
			return v, trace
		}
		base := walkVersions(s)
		for i := range 4 {
			s = f.refresh(t)
			if v := f.walkViolation(t, s, m); v != "" {
				return fmt.Sprintf("on unchanged rebuild %d: %s", i, v), trace
			}
			if walkVersions(s) != base {
				return fmt.Sprintf("a version changed on unchanged rebuild %d", i), trace
			}
		}
	}
	return "", trace
}

// A seeded random walk over one service's three listings (rows appear, change
// shape and go, one or two listings per refresh), time jumps across the
// retention grace, the dependency set and the QUIC twin. After every refresh
// and on four unchanged rebuilds of it: one holder per name, every EDS
// cluster's load assignment published, the bare name carrying the first live
// listing's rows, no live floor empty, the udp: cluster on the holder's
// endpoints and port, every copy equal to the bare one, the h2 cluster
// retained for exactly its grace, no version moving, the check silent.
// (From the adversarial review of the fix: before it, nearly every seed
// failed.)
func TestOneHolderPerNameHoldsOverARandomWalk(t *testing.T) {
	const seeds, steps = 12, 40
	for seed := int64(1); seed <= seeds; seed++ {
		v, trace := walk(t, seed, steps, seed%2 == 0)
		if v == "" {
			continue
		}
		if len(trace) > 5 {
			trace = trace[len(trace)-5:]
		}
		t.Errorf("seed %d: %s\n%s", seed, v, strings.Join(trace, ""))
	}
}
