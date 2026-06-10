package server

import (
	"context"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/storage"
	"github.com/bpalermo/aether/agent/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sweepRegistry serves a fixed endpoint listing and records UnregisterEndpoint calls.
type sweepRegistry struct {
	testRegistry
	listing      map[string][]*registryv1.ServiceEndpoint
	unregistered map[string][]string // service -> ips
}

func (r *sweepRegistry) ListAllEndpoints(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	return r.listing, nil
}

func (r *sweepRegistry) UnregisterEndpoint(_ context.Context, service, ip string) error {
	if r.unregistered == nil {
		r.unregistered = map[string][]string{}
	}
	r.unregistered[service] = append(r.unregistered[service], ip)
	return nil
}

func sweepEndpoint(ip, node, pod string) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{
		Ip: ip,
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: "default",
			PodName:   pod,
			NodeName:  node,
		},
	}
}

// TestSweepGhostEndpoints: only this node's endpoints without a live local pod
// are deregistered — live pods, other nodes' endpoints, and terminating pods'
// (already deregistered) entries are handled correctly.
func TestSweepGhostEndpoints(t *testing.T) {
	ctx := context.Background()

	live := validCNIPod("pod-live", "default", "container-live") // Ips: 10.0.0.1
	terminating := validCNIPod("pod-term", "default", "container-term")
	terminating.Ips = []string{"10.0.0.2"}
	terminating.Terminating = true

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-live"), live))
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-term"), terminating))

	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{
		"svc-a": {
			sweepEndpoint("10.0.0.1", "test-node", "pod-live"),   // live -> keep
			sweepEndpoint("10.0.0.9", "test-node", "pod-ghost"),  // ghost -> deregister
			sweepEndpoint("10.0.0.3", "other-node", "pod-other"), // other node -> ignore
		},
		"svc-b": {
			sweepEndpoint("10.0.0.2", "test-node", "pod-term"), // terminating ghost -> deregister
		},
	}}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), "127.0.0.1:1")

	s.sweepGhostEndpoints(ctx)

	assert.Equal(t, map[string][]string{
		"svc-a": {"10.0.0.9"},
		"svc-b": {"10.0.0.2"},
	}, reg.unregistered)
}
