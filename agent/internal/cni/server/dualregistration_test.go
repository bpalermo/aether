package server

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// perProtocolRegistry records which registry KEYS an endpoint was written
// under, and can fail a chosen one.
//
// That failure mode is the whole point: a registry does not reject a
// half-written dual-protocol pod. One key succeeds, the other does not, and
// nothing but the caller is in a position to notice.
type perProtocolRegistry struct {
	testRegistry
	mu      sync.Mutex
	writes  []registryv1.Service_Protocol
	failFor registryv1.Service_Protocol // UNSPECIFIED matches nothing
}

func (r *perProtocolRegistry) RegisterEndpoint(_ context.Context, _ string, p registryv1.Service_Protocol, _ *registryv1.ServiceEndpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, p)
	if r.failFor != registryv1.Service_PROTOCOL_UNSPECIFIED && p == r.failFor {
		return assert.AnError
	}
	return nil
}

func (r *perProtocolRegistry) Writes() []registryv1.Service_Protocol {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]registryv1.Service_Protocol(nil), r.writes...)
}

// TestRegisterHealthTransition_DualKeyPromotion is proposal 037 Risk 5.
//
// Promotion re-registers the endpoint HEALTHY under every key the pod holds.
// If one key succeeds, the other fails, and the transition is recorded anyway,
// the next tick sees prev == want and never retries — the second listing's
// endpoint stays UNHEALTHY **forever**. Nothing errors: that cluster is simply
// empty of healthy hosts, which reads exactly like a service with no pods.
//
// So the assertion is not "both writes were attempted". It is that a partial
// failure is treated as a failure, leaving the transition unrecorded and the
// retry alive.
func TestRegisterHealthTransition_DualKeyPromotion(t *testing.T) {
	protocols := []registryv1.Service_Protocol{
		registryv1.Service_PROTOCOL_HTTP,
		registryv1.Service_PROTOCOL_TCP,
	}
	const key = "container-dual"

	setup := func(t *testing.T, reg *perProtocolRegistry) (*CNIServer, *cniv1.CNIPod) {
		t.Helper()
		pod := validCNIPod("pod-dual", "default", key)
		pod.Annotations[aetherannotations.AnnotationEndpointPorts] = "8080,9000=tcp"

		store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
			return []*cniv1.CNIPod{pod}, nil
		})
		require.NoError(t, store.AddResource(context.Background(), types.ContainerID(key), pod))

		srvr := newTestCNIServer(nil, store, reg,
			cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")
		return srvr, pod
	}

	promote := func(srvr *CNIServer, pod *cniv1.CNIPod, state *livenessState) {
		srvr.registerHealthTransition(context.Background(), state, pod, key,
			registryv1.ServiceEndpoint_HEALTH_UNHEALTHY,
			registryv1.ServiceEndpoint_HEALTH_HEALTHY,
			false, "default/dual", protocols, dualProtocolEndpoint())
	}

	t.Run("success writes BOTH keys and records the transition", func(t *testing.T) {
		reg := &perProtocolRegistry{}
		srvr, pod := setup(t, reg)
		state := newLivenessState()

		promote(srvr, pod, state)

		assert.ElementsMatch(t, protocols, reg.Writes(),
			"a dual-protocol pod must be promoted under both keys, or one listing never sees it healthy")
		assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, state.last[key])
	})

	t.Run("a partial failure must NOT record the transition", func(t *testing.T) {
		reg := &perProtocolRegistry{failFor: registryv1.Service_PROTOCOL_TCP}
		srvr, pod := setup(t, reg)
		state := newLivenessState()

		promote(srvr, pod, state)

		// Both were attempted — the loop does not stop at the first error,
		// because the HTTP key should still be promoted.
		assert.ElementsMatch(t, protocols, reg.Writes())
		// ...but the HTTP success must not be banked. If it were, prev == want
		// on the next tick and the TCP key is stuck UNHEALTHY with no retry
		// and no error anywhere.
		assert.NotContains(t, state.last, key,
			"a partial promotion must leave the transition unrecorded so the next tick retries")
	})

	t.Run("a single-protocol pod is unchanged", func(t *testing.T) {
		reg := &perProtocolRegistry{}
		srvr, pod := setup(t, reg)
		state := newLivenessState()

		srvr.registerHealthTransition(context.Background(), state, pod, key,
			registryv1.ServiceEndpoint_HEALTH_UNHEALTHY,
			registryv1.ServiceEndpoint_HEALTH_HEALTHY,
			false, "default/dual",
			[]registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP},
			dualProtocolEndpoint())

		assert.Equal(t, []registryv1.Service_Protocol{registryv1.Service_PROTOCOL_HTTP}, reg.Writes(),
			"every pod written before proposal 037 holds exactly one key; this is that path")
		assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, state.last[key])
	})
}

func dualProtocolEndpoint() *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{
		Ip:    "10.0.0.5",
		Port:  8080,
		Ports: []uint32{8080, 9000},
		PortProtocols: map[uint32]registryv1.PortProtocol{
			8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP,
			9000: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
		},
		Health: registryv1.ServiceEndpoint_HEALTH_HEALTHY,
	}
}
