package registrar

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
)

// TestNotifyIdentityReady_DoesNotRaceSubchannelCreation is issue #1137. Until it
// was fixed, NotifyIdentityReady called grpc-go's ClientConn.ResetConnectBackoff,
// which in grpc-go 1.83.2 copies the REFERENCE to the ClientConn's subchannel
// map under cc.mu, drops the lock, and then iterates the map — while the
// balancer, creating a subchannel, writes the same map under the lock
// (newAddrConnLocked). That is a data race (and a possible fatal
// `concurrent map iteration and map write`) whenever identity is announced
// while the connection is being established — which is exactly when the agent
// announces it: at startup, with the first watch dial in flight.
//
// The test makes that window wide and keeps it open: a manual resolver churns
// the registrar's address set so the balancer creates (and removes) subchannels
// continuously, every dial is slow and fails, and identity is announced over
// and over meanwhile. Run it under --config=race. With the call present the
// detector flags the map read in ResetConnectBackoff against the write in
// newAddrConnLocked; without it there is nothing to flag.
func TestNotifyIdentityReady_DoesNotRaceSubchannelCreation(t *testing.T) {
	res := manual.NewBuilderWithScheme("aether-race-1137")
	built := make(chan struct{})
	var builtOnce sync.Once
	res.BuildCallback = func(resolver.Target, resolver.ClientConn, resolver.BuildOptions) {
		builtOnce.Do(func() { close(built) })
	}
	res.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: "registrar-0"}}})

	// A registrar that accepts slowly and never completes: every dial holds the
	// subchannel in CONNECTING for a moment, then fails it.
	slowDial := func(ctx context.Context, _ string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
		return nil, errors.New("registrar not accepting yet")
	}

	r, _ := newLoggingRegistry(t, []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithResolvers(res),
		grpc.WithContextDialer(slowDial),
	})
	r.config.Address = "aether-race-1137:///registrar"

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, r.Initialize(ctx))
	defer func() { _ = r.Close() }()

	// The watch loop's first stream takes the ClientConn out of IDLE, which
	// builds the resolver.
	select {
	case <-built:
	case <-time.After(10 * time.Second):
		t.Fatal("the ClientConn never built its resolver")
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// A fresh address each time: the balancer creates a subchannel for
			// it and removes the previous one, both writes to cc.conns.
			res.UpdateState(resolver.State{Addresses: []resolver.Address{
				{Addr: fmt.Sprintf("registrar-%d", i)},
				{Addr: fmt.Sprintf("registrar-%d-b", i)},
			}})
		}
	})

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		r.NotifyIdentityReady()
	}
	close(stop)
	wg.Wait()
}
