package mtlspool

// Review of #1703, second pass, point 6: a service address added to the
// capture listener (a new raw-TCP service) is a filter-chain-only update of
// the listener, and cap_http, whose message is unchanged, is not drained: a
// keep-alive connection on it keeps being served without "Connection: close",
// and no filter chain is draining. Control: the same update with cap_http's
// own message changed drains it.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/test/envoybin"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func r2Snapshot(t *testing.T, services []ppaSvc) *cachev3Snapshot {
	t.Helper()
	ctx := context.Background()
	c := cache.NewSnapshotCache(ppNodeName, slog.New(slog.DiscardHandler))
	c.SetCaptureEnabled(true)
	c.SetCaptureRedirectAll(true)
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{Name: "client-0", Namespace: ohNS, ServiceAccount: "client", NetworkNamespace: "/proc/self/ns/net"}, ohMesh))
	require.NoError(t, c.SetNodeIdentity(ctx, ppNodeSpiffe))
	authorities := map[string]string{}
	var vips []capture.CaptureTCPService
	for _, s := range services {
		authorities[s.key()] = fmt.Sprintf("%s.%s.svc.cluster.local", s.name, s.ns)
		vips = append(vips, capture.CaptureTCPService{ServiceName: s.key(), ClusterIP: s.vip})
	}
	c.SetCaptureAuthorities(authorities)
	c.SetCaptureTCPServices(vips)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", ppNodeName, &ppaRegistry{services: services}))
	snap, err := c.GetSnapshot(ppNodeName)
	require.NoError(t, err)
	return &cachev3Snapshot{
		routes: snap.GetResources(resourcev3.RouteType), clusters: snap.GetResources(resourcev3.ClusterType),
		listeners: snap.GetResources(resourcev3.ListenerType), extensions: snap.GetResources(resourcev3.ExtensionConfigType),
	}
}

func r2Resources(t *testing.T, p *ohPorts, snap *cachev3Snapshot, backends *ohBackends) map[resourcev3.Type][]types.Resource {
	t.Helper()
	res := map[resourcev3.Type][]types.Resource{}
	for _, r := range snap.routes {
		res[resourcev3.RouteType] = append(res[resourcev3.RouteType], r)
	}
	for _, r := range snap.extensions {
		res[resourcev3.ExtensionConfigType] = append(res[resourcev3.ExtensionConfigType], r)
	}
	names := map[string]bool{proxy.PassthroughClusterName: true}
	for n := range snap.clusters {
		names[n] = true
	}
	for _, r := range snap.routes {
		b, err := proto.Marshal(r.(proto.Message))
		require.NoError(t, err)
		for _, m := range ohQUICName.FindAll(b, -1) {
			names[string(m)] = true
		}
	}
	for n := range names {
		host, port, err := net.SplitHostPort(backends.get(t, n))
		require.NoError(t, err)
		pn, _ := strconv.Atoi(port)
		res[resourcev3.ClusterType] = append(res[resourcev3.ClusterType], &clusterv3.Cluster{
			Name: n, ConnectTimeout: durationpb.New(2 * time.Second),
			ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STATIC},
			LoadAssignment: &endpointv3.ClusterLoadAssignment{ClusterName: n, Endpoints: []*endpointv3.LocalityLbEndpoints{{LbEndpoints: []*endpointv3.LbEndpoint{{
				HostIdentifier: &endpointv3.LbEndpoint_Endpoint{Endpoint: &endpointv3.Endpoint{Address: socketAddress(host, pn)}},
			}}}}},
		})
	}
	var l *listenerv3.Listener
	for name, r := range snap.listeners {
		if strings.HasPrefix(name, "capture_") && !strings.Contains(name, "udp") {
			l = proto.Clone(r).(*listenerv3.Listener)
		}
	}
	require.NotNil(t, l)
	addrs := p.addrs()
	l.Name = "capture"
	l.Address = socketAddress(addrs[0][0].(string), int(addrs[0][1].(uint32)))
	for _, a := range addrs[1:] {
		l.AdditionalAddresses = append(l.AdditionalAddresses, &listenerv3.AdditionalAddress{Address: socketAddress(a[0].(string), int(a[1].(uint32)))})
	}
	l.Transparent = nil
	l.UseOriginalDst = nil
	var lf []*listenerv3.ListenerFilter
	for _, f := range l.GetListenerFilters() {
		if !strings.Contains(f.GetName(), "original_dst") {
			lf = append(lf, f)
		}
	}
	l.ListenerFilters = lf
	res[resourcev3.ListenerType] = []types.Resource{l}
	return res
}

func r2Stat(t *testing.T, admin, name string) string {
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + admin + "/stats?filter=^" + name + "$")
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}

func r2InPlace(t *testing.T, touchHTTPChain bool) (closeOnSecond bool, inPlace, draining string) {
	return r2InPlaceDir(t, touchHTTPChain, false)
}

// r2InPlaceDir is r2InPlace adding the service (remove false) or starting
// with it and removing it (remove true).
func r2InPlaceDir(t *testing.T, touchHTTPChain, remove bool) (closeOnSecond bool, inPlace, draining string) {
	bin, err := envoybin.Path()
	if err != nil {
		t.Skipf("no pinned proxy: %v", err)
	}
	p := ohFreePorts(t)
	base := p.services()
	more := append(append([]ppaSvc{}, base...), ppaSvc{
		ns: ohNS, name: "d", ip: "10.0.5.9", vip: "127.0.0.41", legacy: p.t,
		ports: map[uint32]registryv1.PortProtocol{p.t: registryv1.PortProtocol_PORT_PROTOCOL_TCP},
	})
	backends := &ohBackends{addr: map[string]string{}}
	res1 := r2Resources(t, p, r2Snapshot(t, base), backends)
	res2 := r2Resources(t, p, r2Snapshot(t, more), backends)
	if remove {
		res1, res2 = res2, res1
	}
	l1, l2 := res1[resourcev3.ListenerType][0].(*listenerv3.Listener), res2[resourcev3.ListenerType][0].(*listenerv3.Listener)
	require.False(t, proto.Equal(l1, l2), "the new service changes the listener")
	var http1, http2 *listenerv3.FilterChain
	for _, fc := range l1.GetFilterChains() {
		if fc.GetName() == proxy.CaptureHTTPChainName {
			http1 = fc
		}
	}
	for _, fc := range l2.GetFilterChains() {
		if fc.GetName() == proxy.CaptureHTTPChainName {
			http2 = fc
		}
	}
	require.True(t, proto.Equal(http1, http2), "cap_http's chain message is unchanged by the new address")
	// The chains the update removes or changes: the ones the proxy drains.
	kept := map[string]*listenerv3.FilterChain{}
	for _, fc := range l2.GetFilterChains() {
		kept[fc.GetName()] = fc
	}
	var gone []string
	for _, fc := range l1.GetFilterChains() {
		if k, ok := kept[fc.GetName()]; !ok || !proto.Equal(k, fc) {
			gone = append(gone, fc.GetName())
		}
	}
	t.Logf("MEASURED chains the update removes or changes (before touching cap_http): %v", gone)
	r2Gone = len(gone)
	if touchHTTPChain {
		http2.Filters[len(http2.Filters)-1].Name += "_x" // a different message: that chain must drain
	}

	initial := map[resourcev3.Type][]types.Resource{}
	for k, v := range res1 {
		if k != resourcev3.ListenerType {
			initial[k] = v
		}
	}
	cp := startADSControlPlane(t, initial)
	e := launchEnvoyOverADS(t, bin, cp)
	p.release()
	s1, err := cachev3.NewSnapshot("v2", res1)
	require.NoError(t, err)
	require.NoError(t, cp.cache.SetSnapshot(context.Background(), envoyNodeID, s1))
	target := net.JoinHostPort(ohLocal, strconv.Itoa(int(p.a)))
	ppAwaitListening(t, target)
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(ohHTTP1(ohLocal, p.o, "warmup.example"), "PASSTHROUGH") {
		require.False(t, time.Now().After(deadline), "no RDS")
		time.Sleep(200 * time.Millisecond)
	}

	conn, err := net.Dial("tcp", target)
	require.NoError(t, err)
	defer conn.Close()
	br := bufio.NewReader(conn)
	host := fmt.Sprintf("a.%s.%s:%d", ohNS, ohMesh, p.a)
	send := func() *http.Response {
		_, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", host)
		require.NoError(t, err)
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp
	}
	r1 := send()
	t.Logf("MEASURED before the update: %d connection=%q", r1.StatusCode, r1.Header.Get("Connection"))
	require.Equal(t, 200, r1.StatusCode)

	all2 := map[resourcev3.Type][]types.Resource{}
	for k, v := range res2 {
		all2[k] = v
	}
	s2, err := cachev3.NewSnapshot("v3", all2)
	require.NoError(t, err)
	require.NoError(t, cp.cache.SetSnapshot(context.Background(), envoyNodeID, s2))
	deadline = time.Now().Add(20 * time.Second)
	for {
		inPlace = r2Stat(t, e.admin, "listener_manager.listener_in_place_updated")
		if !strings.HasSuffix(inPlace, ": 0") || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(time.Second)
	draining = r2Stat(t, e.admin, "listener_manager.total_filter_chains_draining")
	modified := r2Stat(t, e.admin, "listener_manager.listener_modified")
	r2 := send()
	t.Logf("MEASURED after the update: %d connection=%q; %s; %s; %s", r2.StatusCode, r2.Header.Get("Connection"), inPlace, draining, modified)
	require.Equal(t, 200, r2.StatusCode)
	return strings.EqualFold(r2.Header.Get("Connection"), "close"), inPlace, draining
}

func TestReview1703R2_LiveAddressAddIsInPlace(t *testing.T) {
	_, inPlace, draining := r2InPlace(t, false)
	require.Equal(t, "listener_manager.listener_in_place_updated: 1", inPlace)
	require.Equal(t, "listener_manager.total_filter_chains_draining: 0", draining, "cap_http (unchanged) must not drain")
}

// The address of a raw-TCP service REMOVED from the capture listener: an
// in-place update as well, and the keep-alive connection on cap_http (whose
// chain is unchanged) is served on without "Connection: close". The removed
// service's own chain is the one that drains.
func TestReview1703R2_LiveAddressRemoveIsInPlace(t *testing.T) {
	closed, inPlace, draining := r2InPlaceDir(t, false, true)
	require.Equal(t, "listener_manager.listener_in_place_updated: 1", inPlace)
	require.False(t, closed, "cap_http's keep-alive connection was told to close")
	require.NotZero(t, r2Gone, "anti-vacuity: the removal takes chains away")
	require.Equal(t, fmt.Sprintf("listener_manager.total_filter_chains_draining: %d", r2Gone), draining,
		"exactly the removed service's chains drain (cap_http, unchanged, does not)")
}

// r2Gone is how many chains the last r2InPlaceDir update removed or changed.
var r2Gone int

func TestReview1703R2_LiveAddressAddInPlaceControl(t *testing.T) {
	_, inPlace, draining := r2InPlace(t, true)
	require.Equal(t, "listener_manager.listener_in_place_updated: 1", inPlace)
	require.Equal(t, "listener_manager.total_filter_chains_draining: 1", draining, "control: a changed cap_http drains")
}
