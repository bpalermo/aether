package server

import (
	"context"
	"log/slog"
	"testing"

	"aethermesh.dev/agent/internal/xds/cache"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnDemandObserver_RecordsNamedCDSSubscriptions verifies a named delta
// CDS subscription (the on_demand filter requesting an undistributed cluster)
// lands in the dependency set, while wildcard subscriptions, per-pod cluster
// names, and other type URLs are ignored.
func TestOnDemandObserver_RecordsNamedCDSSubscriptions(t *testing.T) {
	c := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	o := newOnDemandObserver(c, &mockRegistry{}, slog.New(slog.DiscardHandler))

	// Named CDS subscription for a mesh authority: observed under the bare
	// service name (the suffix is the deterministic bridge between the
	// data-plane cluster name and the control-plane keys).
	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"svc-on-demand.team-a.aether.internal"},
	}))
	assert.Contains(t, c.DependencySet(), "team-a/svc-on-demand")

	// Wildcard, per-pod names, names outside the mesh domain, and nested
	// labels under it: all ignored.
	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"*", "", "app_pod-1", "health_pod-1", "svc-bare", "a.b.c.aether.internal", ".aether.internal"},
	}))
	deps := c.DependencySet()
	assert.Len(t, deps, 1, "only the mesh-authority subscription is observed")

	// EDS subscriptions are named per cluster; they must not be observed.
	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.EndpointType,
		ResourceNamesSubscribe: []string{"svc-eds-sub.aether.internal"},
	}))
	assert.NotContains(t, c.DependencySet(), "svc-eds-sub")
}

// TestCombinedCallbacks_Dispatch verifies the combiner reaches every member.
func TestCombinedCallbacks_Dispatch(t *testing.T) {
	c1 := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	c2 := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	combined := combinedCallbacks{
		newOnDemandObserver(c1, &mockRegistry{}, slog.New(slog.DiscardHandler)).Callbacks(),
		newOnDemandObserver(c2, &mockRegistry{}, slog.New(slog.DiscardHandler)).Callbacks(),
	}

	require.NoError(t, combined.OnStreamDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"svc-x.team-a.aether.internal"},
	}))
	assert.Contains(t, c1.DependencySet(), "team-a/svc-x")
	assert.Contains(t, c2.DependencySet(), "team-a/svc-x")

	// Unwired hooks are nil-safe.
	require.NoError(t, combined.OnStreamOpen(context.Background(), 1, resourcev3.ClusterType))
	combined.OnStreamClosed(1, nil)
}

// catalogRegistry wraps mockRegistry with a fixed service catalog.
type catalogRegistry struct {
	*mockRegistry
	known map[string]bool
}

func (c *catalogRegistry) HasService(name string) bool { return c.known[name] }

// TestOnDemandObserver_CatalogGate verifies nonexistent services are rejected
// before touching the dependency set, while known services are observed.
func TestOnDemandObserver_CatalogGate(t *testing.T) {
	c := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	reg := &catalogRegistry{mockRegistry: &mockRegistry{}, known: map[string]bool{"team-a/svc-real": true}}
	o := newOnDemandObserver(c, reg, slog.New(slog.DiscardHandler))

	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"svc-real.team-a.aether.internal", "svc-ghost.team-a.aether.internal"},
	}))
	deps := c.DependencySet()
	assert.Contains(t, deps, "team-a/svc-real", "catalog hit is observed")
	assert.NotContains(t, deps, "team-a/svc-ghost", "catalog miss never pollutes the dependency set")
}

// TestOnDemandObserver_TracksLiveSubscriptionsForTTLExemption verifies the
// observer records a named CDS subscription as a LIVE on-demand subscription
// (the observed-use signal that exempts the dependency from the idle TTL,
// issue #682), keyed by the requested authority — including the
// "<fqdn>:<port>" spelling the ODCDS catch-all's cluster_header produces — and
// releases it on unsubscribe and on stream close.
func TestOnDemandObserver_TracksLiveSubscriptionsForTTLExemption(t *testing.T) {
	const (
		service   = "team-a/echo"
		authority = "echo.team-a.aether.internal:18081"
	)

	c := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	o := newOnDemandObserver(c, &mockRegistry{}, slog.New(slog.DiscardHandler))
	cb := o.Callbacks()

	require.NoError(t, cb.OnStreamDeltaRequest(7, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{authority},
	}))
	assert.Contains(t, c.DependencySet(), service, "the :port authority maps to the bare service key")
	assert.Contains(t, c.OnDemandServices(), service, "a named subscription is recorded as live use")

	// Explicit unsubscribe releases it.
	require.NoError(t, cb.OnStreamDeltaRequest(7, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                  resourcev3.ClusterType,
		ResourceNamesUnsubscribe: []string{authority},
	}))
	assert.NotContains(t, c.OnDemandServices(), service, "unsubscribe releases the exemption")

	// Re-subscribe, then close the stream: the pin dies with the stream.
	require.NoError(t, cb.OnStreamDeltaRequest(7, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{authority},
	}))
	require.Contains(t, c.OnDemandServices(), service)
	cb.OnDeltaStreamClosed(7, nil)
	assert.Empty(t, c.OnDemandServices(), "stream close releases every pin it held")

	// A catalog miss is never pinned (it never reaches the dependency set either).
	reg := &catalogRegistry{mockRegistry: &mockRegistry{}, known: map[string]bool{}}
	ghostObserver := newOnDemandObserver(c, reg, slog.New(slog.DiscardHandler))
	require.NoError(t, ghostObserver.onDeltaRequest(8, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                resourcev3.ClusterType,
		ResourceNamesSubscribe: []string{"ghost.team-a.aether.internal:9000"},
	}))
	assert.Empty(t, c.OnDemandServices(), "a ghost service can never pin a dependency")
}

// TestOnDemandObserver_ResumesHeldClustersOnAFreshStream is the #682
// post-restart regression test.
//
// The observed half of the node dependency set is process-local memory, so a
// restarted agent starts empty and its first snapshot drops every
// ODCDS-acquired upstream — and a reconnecting Envoy never re-asks for them: a
// name it is still "waiting for server" on appears in neither
// initial_resource_versions nor resource_names_subscribe (markStreamFresh
// clears the pending-add set), and the on_demand filter dedupes every later
// re-subscribe. On talos (rev194, 2026-09-05) that was 14.05s of 503s on w01
// and 14.67s on w03 with the agent logging nothing, ending only when Envoy's
// init-fetch timeout reset its subscription state.
//
// The clusters the proxy reports it HOLDS are the evidence: the first request
// of the stream must re-seed the dependency set from them, with no named
// subscribe at all.
func TestOnDemandObserver_ResumesHeldClustersOnAFreshStream(t *testing.T) {
	c := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	reg := &catalogRegistry{mockRegistry: &mockRegistry{}, known: map[string]bool{
		"aether-test/echo": true,
		"team-a/svc-tcp":   true,
	}}
	o := newOnDemandObserver(c, reg, slog.New(slog.DiscardHandler))

	// The reconnect handshake: everything the proxy still holds, NOTHING newly
	// subscribed — exactly what Envoy sends after the agent restarts.
	require.NoError(t, o.Callbacks().OnStreamDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl: resourcev3.ClusterType,
		InitialResourceVersions: map[string]string{
			"echo.aether-test.aether.internal":       "v7",
			"echo.aether-test.aether.internal:18081": "v7",
			"app_pod-1_8080":                         "v7",
			"health_pod-1":                           "v7",
			"tcp:svc-tcp.team-a.aether.internal":     "v7",
			"ghost.team-a.aether.internal":           "v7",
			"passthrough":                            "v7",
		},
	}))

	deps := c.DependencySet()
	assert.Contains(t, deps, "aether-test/echo",
		"a held cluster re-seeds the dependency set on the first request of the stream")
	assert.NotContains(t, deps, "team-a/svc-tcp",
		"the tcp: floor cluster is not a mesh service authority (it must not mint 'team-a/tcp:svc-tcp' either)")
	assert.NotContains(t, deps, "team-a/tcp:svc-tcp")
	assert.NotContains(t, deps, "team-a/ghost", "a held cluster whose service left the catalog is not restored")
	assert.Len(t, deps, 1, "per-pod and non-mesh names are ignored; both echo spellings collapse to one key")

	// Restored entries are observations, not live-subscription pins: an upstream
	// the proxy has stopped using must still age out of the demand set.
	assert.Empty(t, c.OnDemandServices(), "a held resource is not a live on-demand subscription")
}

// TestOnDemandObserver_ResumeIsIdempotentAndFirstRequestOnly verifies the resume
// pass costs nothing on the steady-state requests of a stream (Envoy sends
// initial_resource_versions only on the first request of each stream) and never
// resurrects a service the demand set has deliberately dropped since.
func TestOnDemandObserver_ResumeIsIdempotentAndFirstRequestOnly(t *testing.T) {
	c := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	reg := &catalogRegistry{mockRegistry: &mockRegistry{}, known: map[string]bool{"aether-test/echo": true}}
	o := newOnDemandObserver(c, reg, slog.New(slog.DiscardHandler))

	held := &discoveryv3.DeltaDiscoveryRequest{
		TypeUrl:                 resourcev3.ClusterType,
		InitialResourceVersions: map[string]string{"echo.aether-test.aether.internal:18081": "v7"},
	}
	require.NoError(t, o.onDeltaRequest(1, held))
	require.Contains(t, c.DependencySet(), "aether-test/echo")
	require.NoError(t, o.onDeltaRequest(1, held))
	assert.Len(t, c.DependencySet(), 1, "re-running the resume pass is idempotent")

	// A steady-state ACK carries no held inventory and changes nothing.
	require.NoError(t, o.onDeltaRequest(1, &discoveryv3.DeltaDiscoveryRequest{TypeUrl: resourcev3.ClusterType}))
	assert.Len(t, c.DependencySet(), 1)
}
