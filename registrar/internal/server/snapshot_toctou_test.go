package server

import (
	"sync"
	"testing"

	registrarv1 "aethermesh.dev/api/aether/registrar/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/require"
)

// TestDiffAndReplace_NeverErasesARegistrationSilently pins the invariant that
// made Diff + Replace a TOCTOU (#772, S13): the sync loop is not the snapshot's
// only writer — an agent's RegisterEndpoint reaches it through Apply on a gRPC
// handler goroutine.
//
// With the two halves as separate lock acquisitions, a registration landing
// between them is broadcast as ENDPOINT_ADDED by Apply and then erased by a
// replacement computed before it existed, with NO compensating REMOVED event.
// Watchers then hold an endpoint the snapshot does not have, and nothing in the
// snapshot repairs it.
//
// The invariant asserted here holds for both orderings and is exactly what the
// split violated: an endpoint that is absent from the snapshot afterwards must
// have been announced as REMOVED.
func TestDiffAndReplace_NeverErasesARegistrationSilently(t *testing.T) {
	const (
		syncedService = "default/svc-a"
		agentService  = "default/svc-b"
		agentIP       = "10.0.0.9"
	)

	for i := range 500 {
		s := NewSnapshot()
		// The world the external registry last reported, and what this sync
		// cycle is about to install: it knows nothing of the agent's endpoint.
		external := map[string]map[registryv1.Service_Protocol][]*registryv1.ServiceEndpoint{
			syncedService: {registryv1.Service_PROTOCOL_HTTP: {makeEndpoint("10.0.0.1")}},
		}
		_, _ = s.Replace(external)

		var (
			mu     sync.Mutex
			events []*registrarv1.WatchEndpointsResponse
			wg     sync.WaitGroup
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			diff, _, transitions := s.DiffAndReplace(external)
			mu.Lock()
			events = append(append(events, diff...), transitions...)
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			<-start
			// The agent registering a local pod (server.go RegisterEndpoint).
			_, _ = s.Apply([]*registrarv1.WatchEndpointsResponse{{
				Type:        registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_ADDED,
				ServiceName: agentService,
				Protocol:    registryv1.Service_PROTOCOL_HTTP,
				Endpoint:    makeEndpoint(agentIP),
			}})
		}()
		close(start)
		wg.Wait()

		present := false
		for _, ep := range s.GetAll(registryv1.Service_PROTOCOL_HTTP)[agentService] {
			if ep.GetIp() == agentIP {
				present = true
			}
		}

		mu.Lock()
		announced := false
		for _, e := range events {
			if e.GetServiceName() == agentService &&
				e.GetType() == registrarv1.WatchEndpointsResponse_EVENT_TYPE_ENDPOINT_REMOVED {
				announced = true
			}
		}
		mu.Unlock()

		require.Falsef(t, !present && !announced,
			"iteration %d: the registration was erased from the snapshot and no REMOVED event was broadcast", i)
	}
}
