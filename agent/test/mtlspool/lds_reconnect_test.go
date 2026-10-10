// What a proxy says about the listeners it holds, and what the agent's ACK
// tracker makes of it, measured on the pinned proxy (issue #1624).
//
// The CNI server waits, on a pod ADD and DEL, for the proxy to hold or to have
// dropped the pod's listeners (ack.Tracker.WaitListenerPresent /
// WaitListenerAbsent). The tracker keeps, per stream, what the proxy on it
// holds: what it stated in its opening Listener request, with what it
// acknowledged since. That account rests on things only the real proxy and
// the real go-control-plane server can show:
//
//  1. The proxy's first Listener request on a stream states, in
//     initial_resource_versions, every listener it holds at the version it
//     was sent, and nothing else. A listener missing from it is one the proxy
//     does not hold, which is what lets a pod DEL return (#1572).
//  2. go-control-plane sends nothing for a listener stated at the version of
//     the snapshot, so nothing would ever acknowledge it by name (#1511).
//  3. A listener it rejected is not stated at the rejected version.
//  4. A rejected Listener response is applied in part: the proxy keeps the
//     listeners of it that it could build. So a rejection does not say the
//     proxy lacks the listener.
//
// cds_reconnect_test.go measures the same for clusters, for the acknowledged
// pin gauge (#1483).
package mtlspool

import (
	"context"
	"log/slog"
	"maps"
	"net"
	"slices"
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

// ldsUnresolvedWait is how long a wait that must not return is given. The
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

	cp := &ldsControlPlane{tracker: ack.NewTracker(slog.New(slog.DiscardHandler))}
	cache := cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil)
	cp.adsControlPlane = &adsControlPlane{socketPath: socketPath, cache: cache}
	cp.publish(t, "v1", listeners...)

	// As the node agent wires it: present is "at the version this cache serves".
	cp.tracker.SetPublishedVersion(ack.SnapshotVersions(cache, envoyNodeID))
	tracked := cp.tracker.Callbacks()
	// The tracker runs first, so by the time a message is in the record the
	// tracker has acted on it.
	callbacks := serverv3.CallbackFuncs{
		DeltaStreamOpenFunc: tracked.OnDeltaStreamOpen,
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
	cp.stop = gs.Stop
	return cp
}

// publish serves the listeners as the snapshot named version.
func (cp *ldsControlPlane) publish(t *testing.T, version string, listeners ...types.Resource) {
	t.Helper()
	snapshot, err := cachev3.NewSnapshot(version, map[resourcev3.Type][]types.Resource{resourcev3.ListenerType: listeners})
	require.NoError(t, err)
	require.NoError(t, snapshot.ConstructVersionMap())
	cp.versions = maps.Clone(snapshot.GetVersionMap(resourcev3.ListenerType))
	require.NoError(t, cp.cache.SetSnapshot(context.Background(), envoyNodeID, snapshot))
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

// answered is the Listener response built from the named snapshot and the
// proxy's answer to it.
func (cp *ldsControlPlane) answered(t *testing.T, version string) (*discoverygrpc.DeltaDiscoveryResponse, *discoverygrpc.DeltaDiscoveryRequest) {
	t.Helper()
	var resp *discoverygrpc.DeltaDiscoveryResponse
	var answer *discoverygrpc.DeltaDiscoveryRequest
	require.Eventually(t, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		for _, sent := range cp.responses {
			if sent.GetSystemVersionInfo() != version {
				continue
			}
			for _, req := range cp.requests {
				if req.GetResponseNonce() == sent.GetNonce() {
					resp, answer = sent, req
					return true
				}
			}
		}
		return false
	}, cdsExchangeWait, 20*time.Millisecond, "no answered Listener response of snapshot %s", version)
	return resp, answer
}

func (cp *ldsControlPlane) requirePresent(t *testing.T, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cdsExchangeWait)
	defer cancel()
	require.NoError(t, cp.tracker.WaitListenerPresent(ctx, name), msgAndArgs...)
}

func (cp *ldsControlPlane) requireAbsent(t *testing.T, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cdsExchangeWait)
	defer cancel()
	require.NoError(t, cp.tracker.WaitListenerAbsent(ctx, name), msgAndArgs...)
}

// requireNotAbsent asserts the removal wait runs to its deadline.
func (cp *ldsControlPlane) requireNotAbsent(t *testing.T, name string, msgAndArgs ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ldsUnresolvedWait)
	defer cancel()
	err := cp.tracker.WaitListenerAbsent(ctx, name)
	require.Error(t, err, msgAndArgs...)
	require.Contains(t, err.Error(), "timed out", msgAndArgs...)
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

// proxyListeners is the names of the listeners the proxy's admin endpoint
// reports: what it runs, whatever it told the control plane.
func proxyListeners(t *testing.T, e *envoyProc) []string {
	t.Helper()
	statuses, err := e.listeners()
	require.NoError(t, err)
	names := make([]string, 0, len(statuses))
	for _, status := range statuses {
		names = append(names, status.GetName())
	}
	return names
}

// TestReconnectingProxyStatesTheListenersItHolds is an agent restart against a
// proxy that already holds the pod's listener.
//
// Measured: the proxy states both listeners at the versions it was sent; the
// server, publishing exactly those, answers with an empty response.
//
// Required of the agent: the restarted agent's wait for the listener returns
// on that exchange (#1511), its wait for the removal of a listener the proxy
// did not state returns at once, and its wait for the removal of one the
// proxy stated returns when the proxy has acknowledged the removal (#1572).
func TestReconnectingProxyStatesTheListenersItHolds(t *testing.T) {
	listeners := func() []types.Resource {
		return []types.Resource{heldListener("held-a", 200), heldListener("held-b", 200)}
	}
	before := startLDSControlPlane(t, cdsSocketPath(t), listeners()...)
	startProxyOnCDS(t, before.socketPath)

	// A proxy with nothing states nothing, is sent both listeners, and the
	// wait is answered by the ACK of the response that carried them.
	assert.Empty(t, before.firstRequest(t).GetInitialResourceVersions(), "a new proxy holds no listener to state")
	sent, answer := before.answered(t, "v1")
	assert.ElementsMatch(t, []string{"held-a", "held-b"}, resourceNames(sent))
	require.Nil(t, answer.GetErrorDetail(), "fixture: the proxy accepts both listeners")
	before.requirePresent(t, "held-a")

	after := before.restart(t, listeners()...)

	// Fact 1: the proxy states what it holds.
	reconnect := after.firstRequest(t)
	t.Logf("first Listener request after the restart: subscribe=%v unsubscribe=%v initial_resource_versions=%v",
		reconnect.GetResourceNamesSubscribe(), reconnect.GetResourceNamesUnsubscribe(), reconnect.GetInitialResourceVersions())
	require.Equal(t, before.versions, reconnect.GetInitialResourceVersions(),
		"the reconnecting proxy must state every listener it holds, at the version it was sent")
	require.Equal(t, after.versions, reconnect.GetInitialResourceVersions(),
		"fixture: the restarted control plane serves exactly what the proxy holds")

	// Fact 2: nothing is sent for them.
	opening, answer := after.answered(t, "v1")
	assert.Empty(t, opening.GetResources(), "nothing to add: the proxy holds every listener at this version")
	assert.Empty(t, opening.GetRemovedResources())
	require.Nil(t, answer.GetErrorDetail(), "the proxy ACKs the empty first response")

	// The agent's reading. The exchange is over, so a wait that is going to
	// return returns at once: the deadline is only what a failure costs.
	after.requirePresent(t, "held-a", "a listener the proxy stated at the published version (#1511)")
	after.requirePresent(t, "held-b")
	after.requireAbsent(t, "never-published", "the proxy said what it holds, and this is not in it")
	after.requireNotAbsent(t, "held-b", "the proxy holds it (#1572)")

	ctx, cancel := context.WithTimeout(context.Background(), ldsUnresolvedWait)
	defer cancel()
	require.Error(t, after.tracker.WaitListenerPresent(ctx, "never-published"), "a listener nobody stated is not held")

	// A pod DEL: held-b leaves the snapshot. Other content is published
	// under held-a (a same-named replacement pod), which the proxy holds at
	// the old version.
	after.publish(t, "v2", heldListener("held-a", 204))
	require.NotEqual(t, before.versions["held-a"], after.versions["held-a"])
	after.requireAbsent(t, "held-b", "the proxy acknowledges the removal")
	after.requirePresent(t, "held-a", "the proxy acknowledges the replacement's listener")
	update, answer := after.answered(t, "v2")
	require.Nil(t, answer.GetErrorDetail())
	assert.Equal(t, []string{"held-a"}, resourceNames(update), "the waits were answered by the response that carried the change")
	assert.Equal(t, []string{"held-b"}, update.GetRemovedResources())
}

// TestReconnectingProxyStatesNoListenerVersionItWasNotSent is the same restart
// with an agent that now publishes another version of one listener, one the
// proxy rejects.
//
// Measured: the proxy states the version it holds, which is not the published
// one, so the listener is sent, and rejected.
//
// Required of the agent: the wait for it fails with the proxy's error. The
// listener next to it, stated at the published version and not in the
// rejected response, is held.
func TestReconnectingProxyStatesNoListenerVersionItWasNotSent(t *testing.T) {
	before := startLDSControlPlane(t, cdsSocketPath(t), heldListener("held-a", 200), heldListener("held-b", 200))
	startProxyOnCDS(t, before.socketPath)
	_, answer := before.answered(t, "v1")
	require.Nil(t, answer.GetErrorDetail(), "fixture: the proxy accepts both listeners")
	held := maps.Clone(before.versions)

	after := before.restart(t, rejectedListener("held-a"), heldListener("held-b", 200))
	require.NotEqual(t, held["held-a"], after.versions["held-a"], "fixture: the restarted control plane publishes another held-a")
	require.Equal(t, held["held-b"], after.versions["held-b"])

	reconnect := after.firstRequest(t)
	require.Equal(t, held, reconnect.GetInitialResourceVersions(), "the proxy states the versions it holds")

	sent, answer := after.answered(t, "v1")
	assert.Equal(t, []string{"held-a"}, resourceNames(sent), "only the listener stated at another version is sent")
	require.NotNil(t, answer.GetErrorDetail(), "fixture: the proxy must reject the update")
	t.Logf("the proxy's NACK: %s", answer.GetErrorDetail().GetMessage())

	ctx, cancel := context.WithTimeout(context.Background(), cdsExchangeWait)
	defer cancel()
	err := after.tracker.WaitListenerPresent(ctx, "held-a")
	require.Error(t, err, "the proxy does not hold the published held-a")
	assert.Contains(t, err.Error(), "envoy rejected config")

	after.requirePresent(t, "held-b")
	after.requireNotAbsent(t, "held-a", "the proxy holds the version it stated")
}

// TestRejectedListenerResponseIsAppliedInPart is why a rejection is not read
// as "the proxy lacks the listener".
//
// Measured: one Listener response adds a listener the proxy can build and one
// it cannot. The proxy NACKs the response, and runs the one it could build.
//
// Required of the agent: the wait for that listener's removal (a pod DEL: the
// listener's sockets are in the pod's network namespace) does not return on
// the rejection. It returns when the proxy has acknowledged the removal,
// which go-control-plane sends for a resource it wrote, acknowledged or not.
func TestRejectedListenerResponseIsAppliedInPart(t *testing.T) {
	cp := startLDSControlPlane(t, cdsSocketPath(t), heldListener("held-a", 200))
	proxy := startProxyOnCDS(t, cp.socketPath)
	_, answer := cp.answered(t, "v1")
	require.Nil(t, answer.GetErrorDetail(), "fixture: the proxy accepts the first listener")
	cp.requirePresent(t, "held-a")
	cp.requireAbsent(t, "built", "fixture: not sent yet")

	cp.publish(t, "v2", heldListener("held-a", 200), heldListener("built", 200), rejectedListener("refused"))
	sent, answer := cp.answered(t, "v2")
	require.ElementsMatch(t, []string{"built", "refused"}, resourceNames(sent), "fixture: one response carries both")
	require.NotNil(t, answer.GetErrorDetail(), "fixture: the proxy must reject the response")
	t.Logf("the proxy's NACK: %s", answer.GetErrorDetail().GetMessage())

	running := proxyListeners(t, proxy)
	t.Logf("listeners the proxy runs after the NACK: %v", running)
	require.Contains(t, running, "built", "measured: the proxy keeps the listener of a rejected response that it could build")
	require.NotContains(t, running, "refused")

	ctx, cancel := context.WithTimeout(context.Background(), cdsExchangeWait)
	defer cancel()
	err := cp.tracker.WaitListenerPresent(ctx, "built")
	require.Error(t, err, "its response was rejected: the agent does not call it acknowledged")
	assert.Contains(t, err.Error(), "envoy rejected config")
	cp.requireNotAbsent(t, "built", "the proxy runs it")

	// The pod is deleted.
	cp.publish(t, "v3", heldListener("held-a", 200))
	removal, answer := cp.answered(t, "v3")
	require.Nil(t, answer.GetErrorDetail())
	assert.ElementsMatch(t, []string{"built", "refused"}, removal.GetRemovedResources())
	cp.requireAbsent(t, "built")
	assert.False(t, slices.Contains(proxyListeners(t, proxy), "built"), "the proxy dropped it")
}

// TestListenerOfARejectedResponseIsNotStatedAtReconnect is the limit of
// reading a proxy's statement as everything it holds.
//
// Measured: the listener the proxy kept from a rejected response (see
// TestRejectedListenerResponseIsAppliedInPart) is not in its statement when
// it reconnects, and it still runs it. A control plane that no longer
// publishes the listener therefore never removes it.
//
// What the agent makes of it: the restarted agent reads the proxy as not
// holding the listener, and a wait for its removal returns at once. Nothing
// the agent is told on the stream says otherwise. This test fails, and the
// limit can go, when the pinned proxy states what it runs.
func TestListenerOfARejectedResponseIsNotStatedAtReconnect(t *testing.T) {
	before := startLDSControlPlane(t, cdsSocketPath(t), heldListener("held-a", 200))
	proxy := startProxyOnCDS(t, before.socketPath)
	before.answered(t, "v1")
	before.publish(t, "v2", heldListener("held-a", 200), heldListener("built", 200), rejectedListener("refused"))
	_, answer := before.answered(t, "v2")
	require.NotNil(t, answer.GetErrorDetail(), "fixture: the proxy must reject the response")
	require.Contains(t, proxyListeners(t, proxy), "built", "fixture: the proxy runs the listener it could build")

	// The agent restarts; the pod of that listener is gone meanwhile.
	after := before.restart(t, heldListener("held-a", 200))
	stated := after.firstRequest(t).GetInitialResourceVersions()
	t.Logf("stated after a response rejected in part: %v", stated)
	require.NotContains(t, stated, "built", "measured: the proxy does not state a listener it kept from a rejected response")
	opening, _ := after.answered(t, "v1")
	assert.Empty(t, opening.GetRemovedResources(), "so nothing removes it")
	assert.Contains(t, proxyListeners(t, proxy), "built", "and the proxy goes on running it")

	after.requireAbsent(t, "built", "the limit: the agent has no word of it")
}
