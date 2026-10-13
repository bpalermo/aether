package cache

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry/endpointmeta"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// port_protocols is required (proposal 043). The tests here hand the cache
// registry rows exactly as a registry would: nothing types them on the way
// (the other fixtures of this package go through registrytest.Typed).

const (
	refusedRowMsg = "a registry row is not served: it does not say the protocol of each of its ports (port_protocols is required). " +
		"It contributes no endpoint, no port and no class; its pod must be registered again by a node agent of this chart"
	refusedRowsCtr = "aether.agent.registry.rows_refused"
)

// rawRegistry lists exactly the rows it holds: all by ListAllEndpoints, and
// cold by ListEndpoints only (a service the node watches nothing of yet, which
// the refresh fetches by name when the catalog knows it).
type rawRegistry struct {
	mockRegistry
	all  map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint
	cold map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint
}

func (r *rawRegistry) ListAllEndpoints(_ context.Context, p registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	out := map[string][]*registryv1.ServiceEndpoint{}
	for svc, rows := range r.all[p] {
		out[svc] = rows
	}
	return out, nil
}

func (r *rawRegistry) ListEndpoints(_ context.Context, svc string, p registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	if rows, ok := r.cold[p][svc]; ok {
		return rows, nil
	}
	return r.all[p][svc], nil
}

func (r *rawRegistry) HasService(name string) bool {
	for _, by := range []map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint{r.all, r.cold} {
		for _, services := range by {
			if _, ok := services[name]; ok {
				return true
			}
		}
	}
	return false
}

// counterByReason reads a counter per value of its "reason" attribute.
func counterByReason(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				reason, _ := dp.Attributes.Value("reason")
				out[reason.AsString()] += dp.Value
			}
		}
	}
	return out
}

func untypedRow(ns, ip string, port uint32, ports []uint32, pp map[uint32]registryv1.PortProtocol) *registryv1.ServiceEndpoint {
	ep := makeEndpoint(ip, "cluster-1", "node-2", port)
	ep.KubernetesMetadata.Namespace = ns
	ep.KubernetesMetadata.PodName = "pod-" + ip
	ep.Ports = ports
	ep.PortProtocols = pp
	return ep
}

// TestUntypedRow_ContributesNothingOnAnyPath: a row that does not say the
// protocol of each of its ports gives the node no endpoint, no port and no
// class, whatever listing it is read from and by whichever read: not an HTTP
// cluster (nor the QUIC twin, the port alias or the virtual host built from
// one), not a raw-TCP floor, not a UDP floor, not a port in the tables. The
// typed row beside it is served as if it were alone. Each refused row is
// counted and logged once for as long as it is listed, not once per refresh.
func TestUntypedRow_ContributesNothingOnAnyPath(t *testing.T) {
	const (
		http = registryv1.Service_PROTOCOL_HTTP
		tcp  = registryv1.Service_PROTOCOL_TCP
		udp  = registryv1.Service_PROTOCOL_UDP
	)
	ctx := context.Background()
	names := []string{"uhttp", "utcp", "uudp", "half", "noclass", "mixed", "ok", "cold"}
	c := a043Cache(t, nil)
	rec := &recorder{}
	c.log = slog.New(&captureHandler{rec: rec})
	reader := sdkmetric.NewManualReader()
	m, err := cachemetrics.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	require.NoError(t, err)
	c.metrics = m
	var vips []capture.CaptureTCPService
	udpRoutes := map[string][]proxy.L4Backend{}
	for i, n := range names {
		svc := a043NS + "/" + n
		vips = append(vips, capture.CaptureTCPService{ServiceName: svc, ClusterIP: "10.96.9." + string(rune('1'+i))})
		udpRoutes[svc] = []proxy.L4Backend{{Service: svc, Cluster: proxy.UDPClusterName(svc, c.meshDomain), Weight: 1}}
	}
	c.SetCaptureTCPServices(vips)
	c.SetUDPServiceRoutes(udpRoutes)

	okRow := untypedRow(a043NS, "10.0.9.7", 8080, []uint32{8080, 9000}, map[uint32]registryv1.PortProtocol{8080: rtpH, 9000: rtpTCP})
	mixedTyped := untypedRow(a043NS, "10.0.9.5", 8080, []uint32{8080}, map[uint32]registryv1.PortProtocol{8080: rtpH})
	uhttp := untypedRow(a043NS, "10.0.9.1", 8080, []uint32{8080, 9090}, nil)
	reg := &rawRegistry{
		all: map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint{
			http: {
				"demo/uhttp": {uhttp},
				// One port named, one not: the row is refused whole.
				"demo/half": {untypedRow(a043NS, "10.0.9.4", 8080, []uint32{8080, 9090}, map[uint32]registryv1.PortProtocol{8080: rtpH})},
				// A port named with no class.
				"demo/noclass": {untypedRow(a043NS, "10.0.9.8", 8080, nil, map[uint32]registryv1.PortProtocol{8080: registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED})},
				"demo/mixed":   {mixedTyped, untypedRow(a043NS, "10.0.9.6", 8080, nil, nil)},
				"demo/ok":      {okRow},
			},
			tcp: {
				"demo/utcp": {untypedRow(a043NS, "10.0.9.2", 5432, nil, nil)},
				"demo/ok":   {okRow},
			},
			udp: {
				"demo/uudp": {untypedRow(a043NS, "10.0.9.3", 5353, nil, map[uint32]registryv1.PortProtocol{})},
			},
		},
		cold: map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint{
			http: {"demo/cold": {untypedRow(a043NS, "10.0.9.9", 8080, nil, nil)}},
			tcp:  {"demo/cold": {untypedRow(a043NS, "10.0.9.9", 8080, nil, nil)}},
			udp:  {"demo/cold": {untypedRow(a043NS, "10.0.9.9", 8080, nil, nil)}},
		},
	}
	load := func() { require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg)) }
	load()

	refused := []string{"uhttp", "utcp", "uudp", "half", "noclass", "cold"}
	check := func() {
		t.Helper()
		c.clusterMu.RLock()
		for name, e := range c.clusters {
			for _, n := range refused {
				assert.NotEqualf(t, a043NS+"/"+n, e.service, "cluster entry %q is built from a row without port_protocols", name)
			}
		}
		for _, n := range refused {
			assert.Falsef(t, c.servicePorts[a043NS+"/"+n].known(), "%s: ports %q were derived from a row without port_protocols", n, c.servicePorts[a043NS+"/"+n].describe())
		}
		assert.Equal(t, "8080 (http)", c.servicePorts["demo/mixed"].describe())
		assert.Equal(t, "8080 (http), 9000 (tcp)", c.servicePorts["demo/ok"].describe())
		c.clusterMu.RUnlock()

		snap, err := c.GetSnapshot("node-1")
		require.NoError(t, err)
		for _, typ := range []string{resourcev3.ClusterType, resourcev3.EndpointType} {
			for name := range snap.GetResources(typ) {
				for _, n := range refused {
					assert.Falsef(t, strings.Contains(name, n+".demo.") || name == a043NS+"/"+n,
						"%s %q is published for a service known only by rows without port_protocols", typ, name)
				}
			}
		}
		for _, res := range snap.GetResources(resourcev3.EndpointType) {
			for _, l := range res.(*endpointv3.ClusterLoadAssignment).GetEndpoints() {
				for _, lb := range l.GetLbEndpoints() {
					ip := lb.GetEndpoint().GetAddress().GetSocketAddress().GetAddress()
					assert.Containsf(t, []string{"10.0.9.5", "10.0.9.7"}, ip, "an endpoint of a row without port_protocols is published under %q", res.(*endpointv3.ClusterLoadAssignment).GetClusterName())
				}
			}
		}
		mixed, ok := snap.GetResources(resourcev3.EndpointType)["demo/mixed"].(*endpointv3.ClusterLoadAssignment)
		require.True(t, ok, "the typed row of demo/mixed is served")
		assert.Equal(t, 1, lbEndpoints(mixed))
		assert.Contains(t, snap.GetResources(resourcev3.ClusterType), "ok.demo."+a043Mesh)
		assert.Contains(t, snap.GetResources(resourcev3.ClusterType), proxy.TCPClusterName("demo/ok", a043Mesh))
		for _, n := range refused {
			got := a043Resolve(t, a043Tables(t, c)[proxy.OutboundHTTPRouteName], n+".demo."+a043Mesh+":8080")
			assert.NotEqualf(t, n+".demo."+a043Mesh, got.cluster, "%s: routed to a cluster of its own", n)
		}
	}
	check()
	// 7 rows: uhttp, utcp, uudp, mixed's second, cold under three listings
	// (one row identity per listing), and two whose map is incomplete.
	want := map[string]int64{endpointmeta.RowNoPortProtocols: 7, endpointmeta.RowPortNotTyped: 2}
	assert.Equal(t, want, counterByReason(t, reader, refusedRowsCtr))
	logged := func() map[string]int {
		out := map[string]int{}
		for _, r := range rec.with(refusedRowMsg) {
			assert.Equal(t, slog.LevelWarn, r.level)
			out[r.attrs["row"]+" "+r.attrs["reason"]]++
		}
		return out
	}
	first := logged()
	assert.Len(t, first, 9)
	assert.Equal(t, 1, first["demo/uhttp|PROTOCOL_HTTP|cluster-1|10.0.9.1 no_port_protocols"], "%v", first)
	assert.Equal(t, 1, first["demo/half|PROTOCOL_HTTP|cluster-1|10.0.9.4 port_not_typed"], "%v", first)

	// Not once per refresh.
	for range 4 {
		load()
	}
	check()
	assert.Equal(t, want, counterByReason(t, reader, refusedRowsCtr), "a row that is still listed is counted once")
	assert.Equal(t, first, logged(), "a row that is still listed is logged once")

	// A row that leaves and comes back untyped is said again; one that comes
	// back typed is served.
	reg.all[http]["demo/uhttp"] = nil
	load()
	reg.all[http]["demo/uhttp"] = []*registryv1.ServiceEndpoint{uhttp}
	load()
	want[endpointmeta.RowNoPortProtocols]++
	assert.Equal(t, want, counterByReason(t, reader, refusedRowsCtr))
	assert.Equal(t, 2, logged()["demo/uhttp|PROTOCOL_HTTP|cluster-1|10.0.9.1 no_port_protocols"])

	reg.all[http]["demo/uhttp"] = []*registryv1.ServiceEndpoint{untypedRow(a043NS, "10.0.9.1", 8080, []uint32{8080, 9090}, map[uint32]registryv1.PortProtocol{8080: rtpH, 9090: rtpH})}
	load()
	c.clusterMu.RLock()
	assert.Equal(t, "8080 (http), 9090 (http)", c.servicePorts["demo/uhttp"].describe())
	c.clusterMu.RUnlock()
	assert.Equal(t, want, counterByReason(t, reader, refusedRowsCtr))
}

// TestUntypedRow_TheListingLendsNoClass pins the three readers that used to
// take a class from the listing a row was read from, each on its own (the
// filter in front of them is tested above): a row without port_protocols, or
// a port its map does not name, is no port of the endpoint under any listing.
func TestUntypedRow_TheListingLendsNoClass(t *testing.T) {
	listings := []registryv1.Service_Protocol{
		registryv1.Service_PROTOCOL_UNSPECIFIED, registryv1.Service_PROTOCOL_HTTP,
		registryv1.Service_PROTOCOL_TCP, registryv1.Service_PROTOCOL_UDP,
	}
	bare := &registryv1.ServiceEndpoint{Ip: "10.0.0.1", Port: 8080, Ports: []uint32{8080, 9090}}
	half := &registryv1.ServiceEndpoint{Ip: "10.0.0.2", Port: 8080, Ports: []uint32{8080, 9090}, PortProtocols: map[uint32]registryv1.PortProtocol{8080: rtpH}}
	noClass := &registryv1.ServiceEndpoint{Ip: "10.0.0.3", Port: 8080, PortProtocols: map[uint32]registryv1.PortProtocol{8080: registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED}}
	for _, listed := range listings {
		assert.Emptyf(t, endpointPortClasses(bare, listed), "listed under %s", listed)
		assert.Equalf(t, map[uint32]registryv1.PortProtocol{8080: rtpH}, endpointPortClasses(half, listed), "listed under %s: 9090 is not named", listed)
		assert.Emptyf(t, endpointPortClasses(noClass, listed), "listed under %s", listed)
	}
	for _, port := range []uint32{0, 8080, 9090} {
		assert.Falsef(t, servesPortAsHTTP(bare, port), "port %d of a row without port_protocols is HTTP because it was read from the HTTP listing", port)
		assert.Falsef(t, listsPort(bare, port), "port %d of a row without port_protocols is listed", port)
		assert.False(t, servesPortAsHTTP(noClass, port))
		assert.False(t, listsPort(noClass, port))
	}
	assert.True(t, servesPortAsHTTP(half, 8080))
	assert.False(t, servesPortAsHTTP(half, 9090), "a port the map does not name is HTTP")
	assert.False(t, listsPort(half, 9090))
	tcpRow := &registryv1.ServiceEndpoint{Ip: "10.0.0.4", Port: 9000, PortProtocols: map[uint32]registryv1.PortProtocol{9000: rtpTCP}}
	assert.False(t, servesPortAsHTTP(tcpRow, 9000))
	assert.True(t, listsPort(tcpRow, 9000))
}
