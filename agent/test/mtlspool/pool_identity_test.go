// Package mtlspool runs a REAL aether-proxy Envoy and asks the one question
// unit tests and `envoy --mode validate` structurally cannot: when two source
// workloads with DIFFERENT ServiceAccounts share a node proxy and call the same
// upstream, does the destination verify the right client certificate on every
// request, or does upstream connection reuse carry a foreign identity?
//
// # Why this shape and no other
//
// Issue #831 observed, correctly, that aether stamped the source SPIFFE ID into
// filter state with set_filter_state's default FactoryKey "envoy.string", which
// builds a Router::StringAccessorImpl, and that
// CommonUpstreamTransportSocketFactory::hashKey folds a downstream shared
// filter-state object into the upstream pool hash ONLY if it implements
// Envoy::Hashable. So the source identity contributed zero bytes to the pool
// key. All of that was true. What saved aether was the NEXT link — pools were
// not shared, because every mesh cluster set
// connection_pool_per_downstream_connection.
//
// Issue #842 removed BOTH halves of that arrangement and replaced them with the
// correct one:
//
//   - the identity object is now built by "envoy.hashable_string"
//     (HashableString: StringAccessorImpl PLUS Hashable), so it DOES reach the
//     pool key and pools partition per (host, source identity);
//   - connection_pool_per_downstream_connection is therefore gone, and upstream
//     h2 connections are shared between pods of the same ServiceAccount;
//   - the per-identity transport_socket_matches / transport_socket_matcher are
//     gone too, replaced by one socket whose custom_tls_certificate_selector
//     (on_demand_secret + filter_state_override) derives the client certificate
//     name from the same filter-state object at handshake time.
//
// The tests below therefore parameterise exactly ONE thing — the object factory
// the listener stamps with — and change nothing else. A difference between the
// runs can only come from the pool key.
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
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exchangeRounds is the number of interleaved round trips AFTER the decisive
// first pair, so each source makes 11 requests in total.
const exchangeRounds = 10

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

// TestSourceIdentityIsNotPooledAcrossServiceAccounts is ASSERTION 1: with the
// production configuration — hashable identity filter state, shared with the
// upstream ONCE, per-connection certificate selector, and NO
// connection_pool_per_downstream_connection — every request from a source is
// verified by the destination as THAT source.
//
// Not merely as some legitimate mesh identity, and not as the node identity
// (which would mean the filter state never reached the upstream — issue
// #686/#825, a different defect).
func TestSourceIdentityIsNotPooledAcrossServiceAccounts(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	p := newPKI(t)
	dest := startDestination(t, p)
	h := startEnvoy(t, p, dest.addr, true /* envoy.hashable_string */)

	fromA, fromB := exchange(t, h, exchangeRounds)
	report(t, "A", fromA)
	report(t, "B", fromB)

	require.Len(t, fromA, exchangeRounds+1)
	require.Len(t, fromB, exchangeRounds+1)

	for i, o := range fromA {
		assert.Equalf(t, spiffeSourceA, o.peerURISAN,
			"request %d from source-a was verified by the destination as %q", i, o.peerURISAN)
	}
	for i, o := range fromB {
		assert.Equalf(t, spiffeSourceB, o.peerURISAN,
			"request %d from source-b was verified by the destination as %q "+
				"(if this is source-a's ID the upstream pool carried a foreign client "+
				"certificate — issue #831/#842; if it is the node ID the certificate "+
				"mapper fell back to default_value, i.e. the filter state never reached "+
				"the upstream — issue #686/#825)", i, o.peerURISAN)
	}

	assert.Empty(t, sharedConnections(fromA, fromB),
		"source-a and source-b must not share an upstream connection")
}

// TestUpstreamPoolsPartitionByIdentity is ASSERTION 2, and it is the half of
// #842 that the identity check above CANNOT see.
//
// Both the old configuration (connection_pool_per_downstream_connection) and
// the new one (identity in the pool key) attribute every request correctly, so
// "11/11 correct" alone would have passed before this change and proves nothing
// about what the change bought. The distinguishing observable is HOW MANY
// upstream connections the two sources used:
//
//	1 connection   -> pools shared with the identity absent from the key: the leak.
//	2 connections  -> pools partitioned by IDENTITY. What #842 delivers.
//	22 connections -> pools partitioned by DOWNSTREAM CONNECTION: the old flag,
//	                  i.e. a fresh TCP + mTLS handshake per downstream connection
//	                  and no upstream h2 multiplexing at all.
//
// Two also demonstrates the multiplexing that was previously impossible: each
// source's 11 requests ride ONE upstream connection.
//
// --concurrency 1 (startEnvoy) is what makes the exact number meaningful; pools
// are per worker thread, so with N workers the correct answer would be a range.
func TestUpstreamPoolsPartitionByIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	p := newPKI(t)
	dest := startDestination(t, p)
	h := startEnvoy(t, p, dest.addr, true /* envoy.hashable_string */)

	fromA, fromB := exchange(t, h, exchangeRounds)
	report(t, "A", fromA)
	report(t, "B", fromB)

	connsA := distinctConnections(fromA)
	connsB := distinctConnections(fromB)
	all := distinctConnections(append(append([]observation{}, fromA...), fromB...))
	t.Logf("upstream connections: source-a=%v source-b=%v total=%d", connsA, connsB, len(all))

	assert.Len(t, connsA, 1, "source-a's %d requests must multiplex onto ONE upstream connection", len(fromA))
	assert.Len(t, connsB, 1, "source-b's %d requests must multiplex onto ONE upstream connection", len(fromB))
	assert.Len(t, all, 2,
		"exactly two upstream connections, one per source IDENTITY: "+
			"1 would mean the identity is not in the pool key (the #831 leak), and "+
			"%d would mean pools are still keyed by the downstream connection "+
			"(connection_pool_per_downstream_connection back on)", len(fromA)+len(fromB))
}

// TestCertificateMapperSelectsPerConnectionCertificate is ASSERTION 3: the
// filter_state_override mapper turned each connection's filter-state value into
// the right SECRET NAME, and the on-demand selector fetched and presented it.
//
// This is a separate property from pooling. Pool partitioning alone guarantees
// only that two sources do not SHARE a connection; it says nothing about which
// certificate either connection ended up carrying. A mapper that always
// returned its default_value would still give two disjoint pools — and every
// request from both sources would arrive as the node identity. So the thing
// asserted here is that each upstream connection presented exactly one
// identity, that it is the ORIGINATING pod's, and that default_value was never
// reached.
//
// It also pins, end to end through a real SDS server, the assumption the whole
// mechanism rests on: the mapper returns the filter-state string VERBATIM as
// the secret name, so this only works because aether's SDS secret names are
// SPIFFE IDs.
func TestCertificateMapperSelectsPerConnectionCertificate(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	p := newPKI(t)
	dest := startDestination(t, p)
	h := startEnvoy(t, p, dest.addr, true /* envoy.hashable_string */)

	fromA, fromB := exchange(t, h, exchangeRounds)
	report(t, "A", fromA)
	report(t, "B", fromB)

	for _, tc := range []struct {
		label string
		obs   []observation
		want  string
	}{
		{"source-a", fromA, spiffeSourceA},
		{"source-b", fromB, spiffeSourceB},
	} {
		sans := map[string]struct{}{}
		for _, o := range tc.obs {
			sans[o.peerURISAN] = struct{}{}
		}
		assert.Lenf(t, sans, 1, "%s's connection presented more than one identity: %v", tc.label, sans)
		assert.Containsf(t, sans, tc.want,
			"%s's upstream connection must present %s's own certificate", tc.label, tc.label)
		assert.NotContainsf(t, sans, spiffeNode,
			"%s presented the node identity: the mapper fell back to default_value, "+
				"which means the %s filter-state object never reached "+
				"TransportSocketOptions::downstreamSharedFilterStateObjects() "+
				"(SharedWithUpstream not ONCE?) or is named something other than the "+
				"mapper's hardcoded lookup key", tc.label, tc.label)
	}
}

// TestSharedPoolLeaksSourceIdentity is the NEGATIVE CONTROL, and the reason the
// three tests above can be believed.
//
// It is the identical scenario with the identity filter state built by the
// non-hashable "envoy.string" factory instead of "envoy.hashable_string"
// (sourceFilterStates -> downgradeToNonHashable rewrites exactly that one
// field of production's own output). The object is still stamped, still shared
// with the upstream, and still found by the certificate mapper — HashableString
// and StringAccessorImpl both satisfy its dynamic_cast to
// Router::StringAccessor — so certificate SELECTION is unaffected. The only
// thing that changes is whether
// CommonUpstreamTransportSocketFactory::hashKey can see the value, because that
// gate is a dynamic_cast to Envoy::Hashable.
//
// If this passes (i.e. a leak IS observed), then:
//
//   - the Envoy mechanism #831 describes is real and this harness detects it, so
//     the green results above are a property of aether's configuration rather
//     than of a test that cannot fail; and
//   - "envoy.hashable_string" is load-bearing SECURITY configuration, not a
//     performance knob. Reverting it — while
//     connection_pool_per_downstream_connection stays off — is a
//     workload-to-workload authorization boundary failure, silent by
//     construction: both identities are valid mesh workloads, the handshake
//     succeeds, and the destination has no way to object.
//
// A failure here does NOT mean aether is broken; it means the harness lost its
// discriminating power (most likely because the two sources stopped sharing a
// pool for some unrelated reason — a per-source SNI, or more than one worker)
// and the assertions above stopped proving anything. Read it that way.
func TestSharedPoolLeaksSourceIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real Envoy; skipped under -test.short")
	}

	p := newPKI(t)
	dest := startDestination(t, p)
	h := startEnvoy(t, p, dest.addr, false /* envoy.string: NOT Hashable */)

	fromA, fromB := exchange(t, h, exchangeRounds)
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
			"upstream pool key and pools no longer keyed by the downstream connection, "+
			"at least one source-b request must have been verified as a different "+
			"identity. If this is zero the two sources are no longer sharing a pool and "+
			"the positive tests prove nothing.")
	assert.Equal(t, spiffeSourceA, fromB[0].peerURISAN,
		"the leaked identity should be whichever source created the pooled connection")
	assert.NotEmpty(t, sharedConnections(fromA, fromB),
		"the leak is multiplexing: both sources' streams ride one upstream connection")
	assert.Len(t, distinctConnections(append(append([]observation{}, fromA...), fromB...)), 1,
		"without the identity in the pool key the two sources collapse onto ONE upstream connection")
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

// distinctConnections returns the sorted set of upstream connection ids that
// served the given observations.
func distinctConnections(obs []observation) []uint64 {
	seen := map[uint64]struct{}{}
	for _, o := range obs {
		seen[o.connID] = struct{}{}
	}
	ids := make([]uint64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
