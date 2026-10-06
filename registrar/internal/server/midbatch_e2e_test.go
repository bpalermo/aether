package server

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/registry/registrarclient"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// multiUnregisterRegistry accepts a multi-IP unregister, so one
// UnregisterEndpoint call is one batch.
type multiUnregisterRegistry struct{ flakyRegistry }

func (*multiUnregisterRegistry) UnregisterEndpoints(context.Context, string, []string) error {
	return nil
}

// cutStream fails the send of the first ENDPOINT_REMOVED for cutIP once armed:
// the stream ends between two events of one batch.
type cutStream struct {
	grpc.ServerStream
	armed *atomic.Bool
	cutIP string
}

func (c *cutStream) SendMsg(m any) error {
	if e, ok := m.(*registrarv1.WatchEndpointsResponse); ok &&
		e.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED &&
		e.GetEndpoint().GetIp() == c.cutIP && c.armed.CompareAndSwap(true, false) {
		return status.Error(codes.Unavailable, "test: cut mid-batch")
	}
	return c.ServerStream.SendMsg(m)
}

// TestWatchEndpoints_MidBatchCutThenABAResends (#1269) drives the REAL
// registrar server and the REAL agent client (registrarclient) end to end:
//
//  1. a = {.1, .2}; the agent watches (full) and is current.
//  2. UnregisterEndpoint(a, [.1, .2]) is ONE batch: REMOVED .1, REMOVED .2,
//     SERVICE_REMOVED a (versioned). The stream is cut after REMOVED .1.
//  3. While the agent backs off, .1 and .2 are registered again: the contents
//     return to the hash the agent's token names (ABA -> "renamed").
//  4. The agent must end with {.1, .2}. Red before #1269: the reconnect
//     presented the pre-batch token, was answered "renamed", and the agent
//     kept [.2] for good.
func TestWatchEndpoints_MidBatchCutThenABAResends(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	snap := NewSnapshot()
	snap.DiffAndReplaceAt(listing(map[string][]string{"ns/a": {"10.0.0.1", "10.0.0.2"}}), Origin{Revision: 5})
	s := NewRegistrarServer(&multiUnregisterRegistry{}, snap, NewBroadcaster(log, nil), "127.0.0.1:0", log, nil)
	synced := make(chan struct{})
	close(synced)
	s.GateOnSync(synced)

	var armed atomic.Bool
	var opens atomic.Int32
	hold := make(chan struct{})
	grpcSrv := grpc.NewServer(grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		if opens.Add(1) >= 2 {
			<-hold // the reconnect waits until the contents have returned
		}
		return h(srv, &cutStream{ServerStream: ss, armed: &armed, cutIP: "10.0.0.2"})
	}))
	registrarv1.RegisterRegistrarServiceServer(grpcSrv, s)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	agent := registrarclient.New(log, registrarclient.Config{
		Address: "passthrough:///midbatch", ClusterName: "c", NodeName: "n",
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, agent.Initialize(ctx))
	defer func() { _ = agent.Close() }()
	readyCtx, rc := context.WithTimeout(ctx, 10*time.Second)
	defer rc()
	require.NoError(t, agent.WaitReady(readyCtx))

	ips := func() []string {
		m, err := agent.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
		require.NoError(t, err)
		var out []string
		for _, e := range m["ns/a"] {
			out = append(out, e.GetIp())
		}
		slices.Sort(out)
		return out
	}
	require.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, ips())

	armed.Store(true)
	_, err := s.UnregisterEndpoint(ctx, &registrarv1.UnregisterEndpointRequest{ServiceName: "ns/a", Ips: []string{"10.0.0.1", "10.0.0.2"}})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return slices.Equal(ips(), []string{"10.0.0.2"}) }, 5*time.Second, 5*time.Millisecond,
		"precondition: the cut left the batch's prefix applied")

	for _, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		_, err := s.RegisterEndpoint(ctx, addReq("ns/a", ip, registryv1.Service_PROTOCOL_HTTP))
		require.NoError(t, err)
	}
	close(hold)
	require.Eventually(t, func() bool { return opens.Load() >= 2 }, 10*time.Second, 5*time.Millisecond, "no reconnect")
	require.Eventually(t, func() bool { return slices.Equal(ips(), []string{"10.0.0.1", "10.0.0.2"}) }, 5*time.Second, 10*time.Millisecond,
		"the agent must converge on the registrar's contents; got %v", ips())
}
