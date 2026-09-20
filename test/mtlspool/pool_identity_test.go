// Package mtlspool runs a REAL aether-proxy Envoy and asks the one question
// unit tests and `envoy --mode validate` structurally cannot: when two source
// workloads with DIFFERENT ServiceAccounts share a node proxy and call the same
// upstream, does the destination verify the right client certificate on every
// request, or does upstream connection reuse carry a foreign identity?
//
// # Why this shape and no other
//
// Issue #831 observes, correctly, that aether stamps the source SPIFFE ID into
// filter state with set_filter_state's default FactoryKey "envoy.string", which
// builds a Router::StringAccessorImpl, and that
// CommonUpstreamTransportSocketFactory::hashKey folds a downstream shared
// filter-state object into the upstream pool hash ONLY if it implements
// Envoy::Hashable. So the source identity contributes zero bytes to the pool
// key. All of that is true, and it is why every existing test passes: they run
// one identity per node path, and one identity cannot leak into itself.
//
// What decides the outcome is the NEXT link — whether the pool is shared. The
// test therefore parameterises exactly that (cluster
// connection_pool_per_downstream_connection) and changes nothing else, so a
// difference between the two runs can only come from the pool key.
//
// # What the destination reports
//
// The destination reads the verified peer certificate's URI SAN off the TLS
// connection. That is the same value aether's inbound listener puts into XFCC:
// ingress.go sets forward_client_cert_details SANITIZE_SET with
// set_current_client_cert_details.uri, and Envoy fills that from the verified
// peer certificate. Measuring it directly costs one fewer Envoy in the harness
// and measures the same fact — and it is the fact every RBAC and ext_authz rule
// keyed on source identity ultimately rests on.
package mtlspool

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exchange drives the interleaved two-source conversation and returns what the
// destination saw, in order, tagged with which source actually made the call.
//
// The ORDER matters. Source A goes first and completes, which leaves an idle,
// pooled upstream HTTP/2 connection carrying A's client certificate. Source B's
// first call is therefore the decisive request: with a shared pool it finds
// that connection and multiplexes onto it. Everything after it is there to show
// the condition is steady rather than a one-off.
func exchange(t *testing.T, h *proxyHandle, rounds int) (fromA, fromB []observation) {
	t.Helper()

	a := newSourceClient("source-a", h.addrA)
	b := newSourceClient("source-b", h.addrB)

	// Warm A first: this is what creates the connection B might inherit.
	fromA = append(fromA, a.call(t))
	// The decisive request.
	fromB = append(fromB, b.call(t))

	for i := 0; i < rounds; i++ {
		fromA = append(fromA, a.call(t))
		fromB = append(fromB, b.call(t))
	}
	return fromA, fromB
}

func report(t *testing.T, label string, obs []observation) {
	t.Helper()
	for i, o := range obs {
		t.Logf("  %s req[%d]: destination verified %s on upstream connection %d", label, i, o.peerURISAN, o.connID)
	}
}

// TestSourceIdentityIsNotPooledAcrossServiceAccounts is the production
// configuration: every mesh service cluster the node proxy builds sets
// connection_pool_per_downstream_connection (proxy.NewServiceCluster /
// NewTCPServiceCluster, true for the node proxy, false only for the
// single-identity edge).
//
// Every request from a source must be verified by the destination as THAT
// source, not merely as some legitimate mesh identity, and not as the node
// identity (which would mean the transport_socket_matcher missed — issue
// #686/#825, a different defect).
func TestSourceIdentityIsNotPooledAcrossServiceAccounts(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	p := newPKI(t)
	dest := startDestination(t, p)
	h := startEnvoy(t, p, dest.addr, true /* connection_pool_per_downstream_connection */)

	fromA, fromB := exchange(t, h, 10)
	report(t, "A", fromA)
	report(t, "B", fromB)

	require.NotEmpty(t, fromA)
	require.NotEmpty(t, fromB)

	for i, o := range fromA {
		assert.Equalf(t, spiffeSourceA, o.peerURISAN,
			"request %d from source-a was verified by the destination as %q", i, o.peerURISAN)
	}
	for i, o := range fromB {
		assert.Equalf(t, spiffeSourceB, o.peerURISAN,
			"request %d from source-b was verified by the destination as %q "+
				"(if this is source-a's ID the upstream pool carried a foreign client "+
				"certificate — issue #831; if it is the node ID the transport socket "+
				"matcher missed — issue #686/#825)", i, o.peerURISAN)
	}

	// The mechanism, not just the outcome: with per-downstream pools the two
	// sources must never share an upstream connection, so the connection ids
	// they were served on are disjoint.
	assert.Empty(t, sharedConnections(fromA, fromB),
		"source-a and source-b must not share an upstream connection")
}

// TestSharedPoolLeaksSourceIdentity is the NEGATIVE CONTROL, and the reason the
// test above can be believed.
//
// It is the identical scenario with connection_pool_per_downstream_connection
// removed — which is precisely the configuration #831 assumes aether has. If
// this passes (i.e. a leak IS observed), then:
//
//   - the Envoy mechanism #831 describes is real and this harness detects it, so
//     the green result above is a property of aether's configuration rather than
//     of a test that cannot fail; and
//   - connection_pool_per_downstream_connection is load-bearing SECURITY
//     configuration on every mesh cluster, not a performance knob. Turning it
//     off is a workload-to-workload authorization boundary failure, silent by
//     construction: both identities are valid mesh workloads and the destination
//     has no way to object.
//
// A failure here does NOT mean aether is broken; it means the harness lost its
// discriminating power (most likely because the two sources stopped sharing a
// pool for some unrelated reason) and the assertions above stopped proving
// anything. Read it that way.
func TestSharedPoolLeaksSourceIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	p := newPKI(t)
	dest := startDestination(t, p)
	h := startEnvoy(t, p, dest.addr, false /* pool shared across downstream connections */)

	fromA, fromB := exchange(t, h, 10)
	report(t, "A", fromA)
	report(t, "B", fromB)

	leaked := 0
	for _, o := range fromB {
		if o.peerURISAN != spiffeSourceB {
			leaked++
		}
	}
	t.Logf("source-b requests verified as something other than source-b: %d/%d", leaked, len(fromB))

	require.NotZero(t, leaked,
		"negative control lost its power: with the source identity absent from the "+
			"upstream pool key and pools shared across downstream connections, at least "+
			"one source-b request must have been verified as a different identity. "+
			"If this is zero the two sources are no longer sharing a pool and "+
			"TestSourceIdentityIsNotPooledAcrossServiceAccounts proves nothing.")
	assert.Equal(t, spiffeSourceA, fromB[0].peerURISAN,
		"the leaked identity should be whichever source created the pooled connection")
	assert.NotEmpty(t, sharedConnections(fromA, fromB),
		"the leak is multiplexing: both sources' streams ride one upstream connection")
}

// sharedConnections returns the upstream connection ids that served BOTH
// sources' requests.
func sharedConnections(fromA, fromB []observation) []uint64 {
	seen := map[uint64]struct{}{}
	for _, o := range fromA {
		seen[o.connID] = struct{}{}
	}
	var shared []uint64
	dedup := map[uint64]struct{}{}
	for _, o := range fromB {
		if _, ok := seen[o.connID]; !ok {
			continue
		}
		if _, ok := dedup[o.connID]; ok {
			continue
		}
		dedup[o.connID] = struct{}{}
		shared = append(shared, o.connID)
	}
	return shared
}
