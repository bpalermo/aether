package server

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry"
	"aethermesh.dev/registry/endpointmeta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// port_protocols is required (proposal 043). These tests hand the registrar
// rows exactly as a writer or a store would: no fixture types them.

const (
	untypedStoredMsg  = "a stored registry row is not served: it does not say the protocol of each of its ports (port_protocols is required). Its pod must be registered again by a node agent of this chart"
	untypedRefusedMsg = "refused a registration: the row does not say the protocol of each of its ports (port_protocols is required)"
	untypedGoneMsg    = "no stored registry row is without port_protocols any more"
)

// msgLog records the messages logged, with the "ip" attribute of each.
type msgLog struct {
	mu   sync.Mutex
	seen []string // "<msg>|<ip>"
}

func (l *msgLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *msgLog) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *msgLog) WithGroup(string) slog.Handler            { return l }
func (l *msgLog) Handle(_ context.Context, r slog.Record) error {
	ip := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "ip" {
			ip = a.Value.String()
		}
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, r.Message+"|"+ip)
	return nil
}

func (l *msgLog) count(msg, ip string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.seen {
		if s == msg+"|"+ip {
			n++
		}
	}
	return n
}

// byReason reads an int64 counter or gauge, per value of its "reason"
// attribute.
func byReason(t *testing.T, reader *sdkmetric.ManualReader, name string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	add := func(dps []metricdata.DataPoint[int64]) {
		for _, dp := range dps {
			reason, _ := dp.Attributes.Value("reason")
			out[reason.AsString()] += dp.Value
		}
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				add(d.DataPoints)
			case metricdata.Gauge[int64]:
				add(d.DataPoints)
			default:
				t.Fatalf("metric %s is %T", name, m.Data)
			}
		}
	}
	return out
}

// storeRegistry is a store that lists exactly the rows it holds and records
// what is written to it.
type storeRegistry struct {
	registry.Registry // nil-embedded; only the methods below are used

	mu     sync.Mutex
	rows   map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint
	writes []string
}

func (s *storeRegistry) ListAllEndpoints(_ context.Context, p registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]*registryv1.ServiceEndpoint{}
	for svc, rows := range s.rows[p] {
		out[svc] = append([]*registryv1.ServiceEndpoint(nil), rows...)
	}
	return out, nil
}

func (s *storeRegistry) RegisterEndpoint(_ context.Context, svc string, _ registryv1.Service_Protocol, ep *registryv1.ServiceEndpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, svc+"/"+ep.GetIp())
	return nil
}

func (s *storeRegistry) set(p registryv1.Service_Protocol, svc string, rows ...*registryv1.ServiceEndpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[registryv1.Service_Protocol]map[string][]*registryv1.ServiceEndpoint{}
	}
	if s.rows[p] == nil {
		s.rows[p] = map[string][]*registryv1.ServiceEndpoint{}
	}
	s.rows[p][svc] = rows
}

// revStore is storeRegistry read the way the etcd backend is: every protocol
// from one read, with the store's revision.
type revStore struct{ *storeRegistry }

var _ registry.RevisionedLister = revStore{}

func (r revStore) StoreRevision() int64 { return 7 }

func (r revStore) ListAllEndpointsRevisioned(ctx context.Context, protocols []registryv1.Service_Protocol) (map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint, int64, error) {
	out := map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{}
	for _, p := range protocols {
		all, _ := r.ListAllEndpoints(ctx, p)
		for svc, rows := range all {
			if out[svc] == nil {
				out[svc] = map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{}
			}
			out[svc][p] = rows
		}
	}
	return out, 7, nil
}

func ips(rows []*registryv1.ServiceEndpoint) []string {
	out := make([]string, 0, len(rows))
	for _, ep := range rows {
		out = append(out, ep.GetIp())
	}
	return out
}

var (
	h043   = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	tcp043 = registryv1.PortProtocol_PORT_PROTOCOL_TCP
)

func row043(ip string, port uint32, ports []uint32, pp map[uint32]registryv1.PortProtocol) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{Ip: ip, ClusterName: "c1", Port: port, Ports: ports, PortProtocols: pp}
}

// TestRegisterEndpoint_RefusesARowWithoutPortProtocols: the write API refuses
// such a row, whatever the backend behind it: INVALID_ARGUMENT naming the
// reason, nothing in the snapshot, nothing queued for the store, nothing
// broadcast, one count by reason.
func TestRegisterEndpoint_RefusesARowWithoutPortProtocols(t *testing.T) {
	for _, c := range []struct {
		name   string
		row    *registryv1.ServiceEndpoint
		reason string
	}{
		{"no map", row043("10.0.0.9", 8080, []uint32{8080}, nil), endpointmeta.RowNoPortProtocols},
		{"an empty map", row043("10.0.0.9", 8080, nil, map[uint32]registryv1.PortProtocol{}), endpointmeta.RowNoPortProtocols},
		{"no endpoint", nil, endpointmeta.RowNoPortProtocols},
		{"a listed port is not named", row043("10.0.0.9", 8080, []uint32{8080, 9090}, map[uint32]registryv1.PortProtocol{8080: h043}), endpointmeta.RowPortNotTyped},
		{"a port named with no class", row043("10.0.0.9", 8080, nil, map[uint32]registryv1.PortProtocol{8080: registryv1.PortProtocol_PORT_PROTOCOL_UNSPECIFIED}), endpointmeta.RowPortNotTyped},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, protocol := range []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP, registryv1.Service_PROTOCOL_TCP, registryv1.Service_PROTOCOL_UDP} {
				store := &storeRegistry{}
				metrics, reader := newTestMetrics(t)
				logs := &msgLog{}
				snap := NewSnapshot()
				bc := NewBroadcaster(slog.New(slog.DiscardHandler), nil)
				s := NewRegistrarServer(store, snap, bc, "127.0.0.1:0", slog.New(logs), metrics)
				q := NewWriteBehindQueue(store, slog.New(slog.DiscardHandler), nil)
				s.UseWriteBehind(q)
				events := bc.Subscribe("w", nil)

				_, err := s.RegisterEndpoint(context.Background(), &registrarv1.RegisterEndpointRequest{
					ServiceName: "ns/svc", Protocol: protocol, Endpoint: c.row,
				})
				require.Error(t, err, "listing %s: the row was accepted", protocol)
				assert.Equal(t, codes.InvalidArgument, status.Code(err))
				assert.Contains(t, err.Error(), c.reason)
				assert.Contains(t, err.Error(), "port_protocols is required")

				for _, p := range []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP, registryv1.Service_PROTOCOL_TCP, registryv1.Service_PROTOCOL_UDP} {
					assert.Empty(t, snap.GetAll(p), "the refused row is in the snapshot")
				}
				assert.False(t, q.Shielding("ns/svc", c.row.GetIp()), "the refused row is queued for the store")
				assert.Empty(t, store.writes)
				select {
				case e := <-events:
					t.Fatalf("the refused row was broadcast: %v", e)
				case <-time.After(20 * time.Millisecond):
				}
				assert.Equal(t, map[string]int64{c.reason: 1}, byReason(t, reader, "aether.registrar.rows.refused"))
				assert.Equal(t, 1, logs.count(untypedRefusedMsg, c.row.GetIp()))
			}
		})
	}
}

// TestRegisterEndpoint_AcceptsATypedRow is the control of the test above.
func TestRegisterEndpoint_AcceptsATypedRow(t *testing.T) {
	metrics, reader := newTestMetrics(t)
	snap := NewSnapshot()
	s := NewRegistrarServer(&storeRegistry{}, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), "127.0.0.1:0", slog.New(slog.DiscardHandler), metrics)
	_, err := s.RegisterEndpoint(context.Background(), &registrarv1.RegisterEndpointRequest{
		ServiceName: "ns/svc", Protocol: registryv1.Service_PROTOCOL_HTTP,
		Endpoint: row043("10.0.0.9", 8080, []uint32{8080, 9000}, map[uint32]registryv1.PortProtocol{8080: h043, 9000: tcp043}),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.9"}, ips(snap.GetAll(registryv1.Service_PROTOCOL_HTTP)["ns/svc"]))
	assert.Empty(t, byReason(t, reader, "aether.registrar.rows.refused"))
}

// TestSync_FiltersStoredRowsWithoutPortProtocols: a row already in the store
// (written before the upgrade, or by a peer cluster's older registrar into a
// shared etcd) is FILTERED at the registrar: it is in no listing the agents
// watch or the Service generator reads. It is said once per row, not once per
// sync, and counted in a gauge that returns to zero when the rows are gone.
func TestSync_FiltersStoredRowsWithoutPortProtocols(t *testing.T) {
	t.Run("a listing per protocol (kubernetes)", func(t *testing.T) { syncFiltersStoredRows(t, false) })
	t.Run("one revisioned read (etcd)", func(t *testing.T) { syncFiltersStoredRows(t, true) })
}

func syncFiltersStoredRows(t *testing.T, revisioned bool) {
	t.Helper()
	const (
		http = registryv1.Service_PROTOCOL_HTTP
		tcp  = registryv1.Service_PROTOCOL_TCP
		udp  = registryv1.Service_PROTOCOL_UDP
	)
	store := &storeRegistry{}
	typed := row043("10.0.0.1", 8080, []uint32{8080}, map[uint32]registryv1.PortProtocol{8080: h043})
	store.set(http, "ns/mixed", typed, row043("10.0.0.2", 8080, nil, nil))
	store.set(http, "ns/old", row043("10.0.0.3", 8080, []uint32{8080, 9090}, nil))
	store.set(tcp, "ns/olddb", row043("10.0.0.4", 5432, nil, nil))
	store.set(udp, "ns/olddns", row043("10.0.0.5", 5353, nil, nil))
	store.set(http, "ns/half", row043("10.0.0.6", 8080, []uint32{8080, 9090}, map[uint32]registryv1.PortProtocol{8080: h043}))

	metrics, reader := newTestMetrics(t)
	logs := &msgLog{}
	snap := NewSnapshot()
	var reg registry.Registry = store
	if revisioned {
		reg = revStore{store}
	}
	s := NewSyncer(reg, snap, NewBroadcaster(slog.New(slog.DiscardHandler), nil), time.Hour, slog.New(logs), metrics)

	check := func() {
		t.Helper()
		got := snap.GetAll(http)
		assert.Equal(t, []string{"10.0.0.1"}, ips(got["ns/mixed"]), "only the typed row of a service is listed")
		assert.Empty(t, got["ns/old"], "an untyped row is listed under HTTP")
		assert.Empty(t, got["ns/half"], "a row with an untyped port is listed")
		assert.Empty(t, snap.GetAll(tcp)["ns/olddb"], "an untyped row is listed under TCP")
		assert.Empty(t, snap.GetAll(udp)["ns/olddns"], "an untyped row is listed under UDP")
		assert.Equal(t, map[string]int64{endpointmeta.RowNoPortProtocols: 4, endpointmeta.RowPortNotTyped: 1},
			byReason(t, reader, "aether.registrar.rows.untyped"))
	}
	s.sync(context.Background())
	check()
	for range 5 {
		s.sync(context.Background())
	}
	check()
	for _, ip := range []string{"10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6"} {
		assert.Equalf(t, 1, logs.count(untypedStoredMsg, ip), "row %s: said once per row, not once per sync", ip)
	}
	assert.Zero(t, logs.count(untypedStoredMsg, "10.0.0.1"), "the typed row is not reported")
	assert.Zero(t, logs.count(untypedGoneMsg, ""))

	// The pods are registered again by an agent that types them: the rows
	// are served, the gauge reads zero for both reasons, and it is said.
	store.set(http, "ns/mixed", typed, row043("10.0.0.2", 8080, nil, map[uint32]registryv1.PortProtocol{8080: h043}))
	store.set(http, "ns/old", row043("10.0.0.3", 8080, []uint32{8080, 9090}, map[uint32]registryv1.PortProtocol{8080: h043, 9090: h043}))
	store.set(tcp, "ns/olddb")
	store.set(udp, "ns/olddns")
	store.set(http, "ns/half")
	s.sync(context.Background())
	assert.ElementsMatch(t, []string{"10.0.0.1", "10.0.0.2"}, ips(snap.GetAll(http)["ns/mixed"]))
	assert.Equal(t, []string{"10.0.0.3"}, ips(snap.GetAll(http)["ns/old"]))
	assert.Equal(t, map[string]int64{endpointmeta.RowNoPortProtocols: 0, endpointmeta.RowPortNotTyped: 0},
		byReason(t, reader, "aether.registrar.rows.untyped"), "a reason that went to zero reads zero, not its last value")
	assert.Equal(t, 1, logs.count(untypedGoneMsg, ""))

	// A row that comes back untyped is said again.
	store.set(tcp, "ns/olddb", row043("10.0.0.4", 5432, nil, nil))
	s.sync(context.Background())
	assert.Equal(t, 2, logs.count(untypedStoredMsg, "10.0.0.4"))
}
