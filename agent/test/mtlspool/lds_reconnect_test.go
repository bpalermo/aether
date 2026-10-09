// What a reconnecting proxy says about the listeners it holds, and what the
// agent's ACK tracker makes of it, measured on the pinned proxy (issue #1511).
//
// The CNI server waits, on a pod ADD and DEL, for the proxy to acknowledge the
// pod's listeners (ack.Tracker.WaitListenerPresent / WaitListenerAbsent). An
// agent that restarts finds a proxy that already holds them, so they are never
// sent on the new stream and never acknowledged by name. What the tracker
// reads instead is the opening exchange of the Listener type, and that reading
// rests on three things only the real proxy and the real go-control-plane
// server can show:
//
//  1. The proxy opens the Listener type as a wildcard subscription. The
//     tracker reads the opening exchange for no other kind: for a subscription
//     by name go-control-plane compares only the subscribed names.
//  2. Its first Listener request states, in initial_resource_versions, every
//     listener it holds at the version it was sent, and go-control-plane sends
//     nothing for one stated at the version of the snapshot.
//  3. A listener it rejected is not stated at the rejected version, so the
//     update is sent again and nothing is concluded from the statement.
//
// cds_reconnect_test.go measures the same for clusters, for the acknowledged
// pin gauge (#1483).
package mtlspool

import (
	"context"
	"log/slog"
	"maps"
	"net"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/ack"
	"aethermesh.dev/agent/internal/xds/config"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// ldsUnresolvedWait is how long a wait that must not resolve is given. The
// exchange it follows is already complete when it starts, so nothing is in
// flight that a longer wait would catch.
const ldsUnresolvedWait = 500 * time.Millisecond

// ldsControlPlane is one control-plane process as the proxy sees it: a
// go-control-plane snapshot cache served over ADS with the agent's ACK tracker
// on its callbacks, and a record of the Listener requests and responses.
type ldsControlPlane struct {
	*adsControlPlane
	tracker  *ack.Tracker
	versions map[string]string // listener name -> version of the served snapshot

	mu        sync.Mutex
	requests  []*discoverygrpc.DeltaDiscoveryRequest
	responses []*discoverygrpc.DeltaDiscoveryResponse
}

func startLDSControlPlane(t *testing.T, socketPath string, listeners ...types.Resource) *ldsControlPlane {
	t.Helper()

	snapshot, err := cachev3.NewSnapshot("v1", map[resourcev3.Type][]types.Resource{resourcev3.ListenerType: listeners})
	require.NoError(t, err)
	require.NoError(t, snapshot.ConstructVersionMap())
	cache := cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil)
	require.NoError(t, cache.SetSnapshot(context.Background(), envoyNodeID, snapshot))

	cp := &ldsControlPlane{
		tracker:  ack.NewTracker(slog.New(slog.DiscardHandler)),
		versions: maps.Clone(snapshot.GetVersionMap(resourcev3.ListenerType)),
	}
	// As the node agent wires it: present is "at the version this cache serves".
	cp.tracker.SetPublishedVersion(ack.SnapshotVersions(cache, envoyNodeID))
	tracked := cp.tracker.Callbacks()
	// The tracker runs first, so by the time a message is in the record the
	// tracker has acted on it.
	callbacks := serverv3.CallbackFuncs{
		StreamDeltaRequestFunc: func(stream int64, req *discoverygrpc.DeltaDiscoveryRequest) error {
			err := tracked.OnStreamDeltaRequest(stream, req)
			if req.GetTypeUrl() == resourcev3.ListenerType {
				cp.mu.Lock()
				cp.requests = append(cp.requests, req)
				cp.mu.Unlock()
			}
			return err
		},
		StreamDeltaResponseFunc: func(stream int64, req *discoverygrpc.DeltaDiscoveryRequest, resp *discoverygrpc.DeltaDiscoveryResponse) {
			tracked.OnStreamDeltaResponse(stream, req, resp)
			if resp.GetTypeUrl() == resourcev3.ListenerType {
				cp.mu.Lock()
				cp.responses = append(cp.responses, resp)
				cp.mu.Unlock()
			}
		},
		DeltaStreamClosedFunc: tracked.OnDeltaStreamClosed,
	}

	ln, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	gs := grpc.NewServer()
	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(gs, serverv3.NewServer(context.Background(), cache, callbacks))
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)

	cp.adsControlPlane = &adsControlPlane{socketPath: socketPath, cache: cache, stop: gs.Stop}
	return cp
}

// restart is an agent restart as the proxy sees it: the control plane stops,
// which drops the ADS stream, and a new one, with a new tracker that knows
// nothing, serves listeners on the same socket.
func (cp *ldsControlPlane) restart(t *testing.T, listeners ...types.Resource) *ldsControlPlane {
	t.Helper()
	cp.stop()
	return startLDSControlPlane(t, cp.socketPath, listeners...)
}

// firstRequest is the proxy's first Listener request on this control plane.
func (cp *ldsControlPlane) firstRequest(t *testing.T) *discoverygrpc.DeltaDiscoveryRequest {
	t.Helper()
	var req *discoverygrpc.DeltaDiscoveryRequest
	require.Eventually(t, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		if len(cp.requests) == 0 {
			return false
		}
		req = cp.requests[0]
		return true
	}, cdsExchangeWait, 20*time.Millisecond, "no Listener request")
	return req
}

// firstAnswer is the first Listener response and the proxy's answer to it.
func (cp *ldsControlPlane) firstAnswer(t *testing.T) (*discoverygrpc.DeltaDiscoveryResponse, *discoverygrpc.DeltaDiscoveryRequest) {
	t.Helper()
	var resp *discoverygrpc.DeltaDiscoveryResponse
	var answer *discoverygrpc.DeltaDiscoveryRequest
	require.Eventually(t, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		if len(cp.responses) == 0 {
			return false
		}
		resp = cp.responses[0]
		for _, req := range cp.requests {
			if req.GetResponseNonce() == resp.GetNonce() {
				answer = req
				return true
			}
		}
		return false
	}, cdsExchangeWait, 20*time.Millisecond, "no answered Listener response")
	return resp, answer
}

func (cp *ldsControlPlane) requirePresent(t *testing.T, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cdsExchangeWait)
	defer cancel()
	require.NoError(t, cp.tracker.WaitListenerPresent(ctx, name), msgAndArgs...)
}

// heldListener is a listener the proxy accepts: an HTTP connection manager
// that answers every request itself. status changes its bytes, and so its
// version, without changing whether the proxy accepts it.
func heldListener(name string, status uint32) *listenerv3.Listener {
	hcm := &hcmv3.HttpConnectionManager{
		StatPrefix: name,
		RouteSpecifier: &hcmv3.HttpConnectionManager_RouteConfig{RouteConfig: &routev3.RouteConfiguration{
			Name: name,
			VirtualHosts: []*routev3.VirtualHost{{
				Name:    name,
				Domains: []string{"*"},
				Routes: []*routev3.Route{{
					Match:  &routev3.RouteMatch{PathSpecifier: &routev3.RouteMatch_Prefix{Prefix: "/"}},
					Action: &routev3.Route_DirectResponse{DirectResponse: &routev3.DirectResponseAction{Status: status}},
				}},
			}},
		}},
		HttpFilters: []*hcmv3.HttpFilter{{
			Name:       "envoy.filters.http.router",
			ConfigType: &hcmv3.HttpFilter_TypedConfig{TypedConfig: config.TypedConfig(&routerv3.Router{})},
		}},
	}
	return &listenerv3.Listener{
		Name:    name,
		Address: socketAddress("127.0.0.1", envoyPicksPort),
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name:       "envoy.filters.network.http_connection_manager",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: config.TypedConfig(hcm)},
			}},
		}},
	}
}

// rejectedListener is a listener that is a valid proto, so the control plane
// serves it, and that Envoy refuses: it has no filter chain.
func rejectedListener(name string) *listenerv3.Listener {
	return &listenerv3.Listener{Name: name, Address: socketAddress("127.0.0.1", envoyPicksPort)}
}

// TestReconnectingProxyStatesTheListenersItHolds is an agent restart against a
// proxy that already holds the pod's listener.
//
// Measured: the proxy opens the Listener type as a wildcard subscription and
// states both listeners at the versions it was sent; the server, publishing
// exactly those, answers with an empty response; the proxy ACKs it.
//
// Required of the agent: the restarted agent's wait for the listener returns
// on that exchange. Before #1511 it ran to its deadline, on every CNI ADD
// retried and every pod re-added after an agent restart.
func TestReconnectingProxyStatesTheListenersItHolds(t *testing.T) {
	listeners := func() []types.Resource {
		return []types.Resource{heldListener("held-a", 200), heldListener("held-b", 200)}
	}
	before := startLDSControlPlane(t, cdsSocketPath(t), listeners()...)
	startProxyOnCDS(t, before.socketPath)

	// A proxy with nothing states nothing, is sent both listeners, and the
	// wait is resolved by the ACK of the response that carried them.
	assert.Empty(t, before.firstRequest(t).GetInitialResourceVersions(), "a new proxy holds no listener to state")
	sent, answer := before.firstAnswer(t)
	assert.ElementsMatch(t, []string{"held-a", "held-b"}, resourceNames(sent))
	require.Nil(t, answer.GetErrorDetail(), "fixture: the proxy accepts both listeners")
	before.requirePresent(t, "held-a")

	after := before.restart(t, listeners()...)

	// Facts 1 and 2: a wildcard subscription that states what the proxy holds.
	reconnect := after.firstRequest(t)
	t.Logf("first Listener request after the restart: subscribe=%v unsubscribe=%v initial_resource_versions=%v",
		reconnect.GetResourceNamesSubscribe(), reconnect.GetResourceNamesUnsubscribe(), reconnect.GetInitialResourceVersions())
	subscribed := reconnect.GetResourceNamesSubscribe()
	require.True(t, len(subscribed) == 0 || (len(subscribed) == 1 && subscribed[0] == "*"),
		"the proxy must open the Listener type as a wildcard subscription, got %v", subscribed)
	require.Empty(t, reconnect.GetResourceNamesUnsubscribe())
	require.Equal(t, before.versions, reconnect.GetInitialResourceVersions(),
		"the reconnecting proxy must state every listener it holds, at the version it was sent")
	require.Equal(t, after.versions, reconnect.GetInitialResourceVersions(),
		"fixture: the restarted control plane serves exactly what the proxy holds")

	answered, answer := after.firstAnswer(t)
	assert.Empty(t, answered.GetResources(), "nothing to add: the proxy holds every listener at this version")
	assert.Empty(t, answered.GetRemovedResources())
	require.Nil(t, answer.GetErrorDetail(), "the proxy ACKs the empty first response")

	// The agent's reading. The exchange is over, so a wait that is going to
	// resolve resolves at once: the deadline is only what a failure costs.
	after.requirePresent(t, "held-a", "a listener the proxy stated at the published version must resolve its wait (#1511)")
	after.requirePresent(t, "held-b")

	ctx, cancel := context.WithTimeout(context.Background(), ldsUnresolvedWait)
	defer cancel()
	require.Error(t, after.tracker.WaitListenerPresent(ctx, "never-published"), "a listener nobody stated is not resolved")

	// Other content is published under a name the proxy holds (a same-named
	// replacement pod). What the proxy stated is the old version, so the wait
	// is answered only by its ACK of the response that carries the new one:
	// the version on the wire is the version the tracker reads as published.
	replaced, err := cachev3.NewSnapshot("v2", map[resourcev3.Type][]types.Resource{
		resourcev3.ListenerType: {heldListener("held-a", 204), heldListener("held-b", 200)},
	})
	require.NoError(t, err)
	require.NoError(t, replaced.ConstructVersionMap())
	require.NotEqual(t, after.versions["held-a"], replaced.GetVersionMap(resourcev3.ListenerType)["held-a"])
	require.NoError(t, after.cache.SetSnapshot(context.Background(), envoyNodeID, replaced))
	after.requirePresent(t, "held-a", "the proxy acknowledges the replacement's listener")
	var update *discoverygrpc.DeltaDiscoveryResponse
	after.mu.Lock()
	for _, resp := range after.responses {
		if resp.GetSystemVersionInfo() == "v2" {
			update = resp
		}
	}
	after.mu.Unlock()
	require.NotNil(t, update, "the wait was answered before the replacement was sent")
	assert.Equal(t, []string{"held-a"}, resourceNames(update))
}

// TestReconnectingProxyStatesNoListenerVersionItWasNotSent is the same restart
// with an agent that now publishes another version of one listener, one the
// proxy rejects.
//
// Measured: the proxy states the version it holds, which is not the published
// one, so the listener is sent, and rejected. Nothing in the exchange lets the
// wait for it succeed: it fails with the proxy's error. The listener next to
// it, stated at the published version, is left unknown, because the tracker
// concludes nothing from an opening response the proxy rejected.
func TestReconnectingProxyStatesNoListenerVersionItWasNotSent(t *testing.T) {
	before := startLDSControlPlane(t, cdsSocketPath(t), heldListener("held-a", 200), heldListener("held-b", 200))
	startProxyOnCDS(t, before.socketPath)
	_, answer := before.firstAnswer(t)
	require.Nil(t, answer.GetErrorDetail(), "fixture: the proxy accepts both listeners")
	held := maps.Clone(before.versions)

	after := before.restart(t, rejectedListener("held-a"), heldListener("held-b", 200))
	require.NotEqual(t, held["held-a"], after.versions["held-a"], "fixture: the restarted control plane publishes another held-a")
	require.Equal(t, held["held-b"], after.versions["held-b"])

	reconnect := after.firstRequest(t)
	require.Equal(t, held, reconnect.GetInitialResourceVersions(), "the proxy states the versions it holds")

	sent, answer := after.firstAnswer(t)
	assert.Equal(t, []string{"held-a"}, resourceNames(sent), "only the listener stated at another version is sent")
	require.NotNil(t, answer.GetErrorDetail(), "fixture: the proxy must reject the update")
	t.Logf("the proxy's NACK: %s", answer.GetErrorDetail().GetMessage())

	ctx, cancel := context.WithTimeout(context.Background(), cdsExchangeWait)
	defer cancel()
	err := after.tracker.WaitListenerPresent(ctx, "held-a")
	require.Error(t, err, "the proxy does not hold the published held-a")
	assert.Contains(t, err.Error(), "envoy rejected config")

	ctx2, cancel2 := context.WithTimeout(context.Background(), ldsUnresolvedWait)
	defer cancel2()
	err = after.tracker.WaitListenerPresent(ctx2, "held-b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out", "a rejected opening response resolves nothing")
}
