package spire

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"sync"
	"testing"

	"aethermesh.dev/common/spire/spiretest"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	brokerpb "github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// publishedSet is one observed SetSecrets call, reduced to what the ordering
// invariants are stated over: the SVID generation served for each identity and
// whether any trust bundle was served at all.
type publishedSet struct {
	svidVersion        map[string]int
	validationContexts int
}

// recordingStore is a SecretStore that records every published set in the order
// the pushes reached it. That order is exactly the order the real
// SnapshotCache.SetSecrets would apply them in — it replaces the whole secret
// map under its own lock — so a regression recorded here is a regression Envoy
// would have been served.
type recordingStore struct {
	mu     sync.Mutex
	pushes []publishedSet
}

func (s *recordingStore) SetSecrets(_ context.Context, secrets []*tlsv3.Secret) error {
	set := publishedSet{svidVersion: make(map[string]int, len(secrets))}
	for _, secret := range secrets {
		switch secret.Type.(type) {
		case *tlsv3.Secret_ValidationContext:
			set.validationContexts++
		case *tlsv3.Secret_TlsCertificate:
			set.svidVersion[secret.GetName()] = decodeSVIDVersion(secret)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushes = append(s.pushes, set)
	return nil
}

func (s *recordingStore) snapshot() []publishedSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]publishedSet(nil), s.pushes...)
}

// svidResponse builds a Broker API response for id whose certificate carries
// version in its serial number. An opaque marker is enough to tell one
// generation of an SVID from the next — which is the whole point: the assertion
// is about ordering, not crypto — but it must still be a real certificate,
// because the converter parses the chain now.
func svidResponse(t *testing.T, ca *spiretest.CA, id string, version int, federated map[string][]byte) *brokerpb.SubscribeToX509SVIDResponse {
	t.Helper()
	return &brokerpb.SubscribeToX509SVIDResponse{
		Svids:            []*brokerpb.X509SVID{ca.BrokerSVID(t, id, version)},
		FederatedBundles: federated,
	}
}

// decodeSVIDVersion reads back the version svidResponse encoded.
func decodeSVIDVersion(secret *tlsv3.Secret) int {
	block, _ := pem.Decode(secret.GetTlsCertificate().GetCertificateChain().GetInlineBytes())
	if block == nil {
		return -1
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return -1
	}
	return int(cert.SerialNumber.Int64()) - 1
}

// newOrderingTestBridge returns a bridge wired to store, with no broker client:
// the update handlers under test are driven directly.
func newOrderingTestBridge(store SecretStore) *Bridge {
	return NewBridge("/nonexistent/socket", store, nil, slog.New(slog.DiscardHandler))
}

// TestPushSecretsPublishesMonotonically is the S10/S11 regression test.
//
// Several goroutines push SDS in production (every per-pod subscription stream,
// the identity refresher and UnsubscribePod). Before the fix, pushSecrets
// snapshotted the secret map, released the lock, and then raced into SetSecrets
// — a whole-map replace — so a slow pusher could land an older set on top of a
// newer one and a just-rotated SVID silently disappeared from SDS until
// something else pushed.
//
// The assertions are stated over the sequence of published sets, not the final
// one: no identity's version may go backwards or vanish, and no set may be
// published without a validation context once one has been served.
func TestPushSecretsPublishesMonotonically(t *testing.T) {
	const (
		rounds    = 150
		idAName   = "spiffe://example.org/ns/aether-test/sa/svc-a"
		idBName   = "spiffe://example.org/ns/aether-test/sa/svc-b"
		netnsA    = "/proc/1/ns/net"
		netnsB    = "/proc/2/ns/net"
		netnsPeer = "/proc/3/ns/net"
	)

	ca := spiretest.NewCA(t)
	federated := map[string][]byte{"spiffe://peer.example": spiretest.NewCA(t).BundleDER()}
	// A second, larger bundle set the peer stream alternates with, so its pushes
	// really mutate the served validation contexts and race the SVID rotations
	// rather than collapsing into no-ops.
	federatedPlus := map[string][]byte{
		"spiffe://peer.example":  federated["spiffe://peer.example"],
		"spiffe://peer2.example": spiretest.NewCA(t).BundleDER(),
	}

	store := &recordingStore{}
	b := newOrderingTestBridge(store)
	ctx := t.Context()

	// Serve a trust bundle first so "a validation context was once served" holds
	// for every later push, and pre-seed both identities at version 0. The third
	// pod carries only federated bundles, which is what makes it the bundle
	// pusher of the old delegated shape.
	require.NoError(t, b.handleSVIDUpdate(ctx, netnsPeer, &brokerpb.SubscribeToX509SVIDResponse{FederatedBundles: federated}))
	require.NoError(t, b.handleSVIDUpdate(ctx, netnsA, svidResponse(t, ca, idAName, 0, nil)))
	require.NoError(t, b.handleSVIDUpdate(ctx, netnsB, svidResponse(t, ca, idBName, 0, nil)))

	// Errors are collected rather than asserted in the goroutines: testify's
	// require calls runtime.Goexit, which off the test goroutine would leak the
	// WaitGroup instead of failing the test.
	errs := make(chan error, 3*rounds)
	var wg sync.WaitGroup
	wg.Add(3)
	rotate := func(netns, id string) {
		defer wg.Done()
		for i := 1; i <= rounds; i++ {
			if err := b.handleSVIDUpdate(ctx, netns, svidResponse(t, ca, id, i, nil)); err != nil {
				errs <- err
			}
		}
	}
	go rotate(netnsA, idAName)
	go rotate(netnsB, idBName)
	go func() {
		defer wg.Done()
		for i := range rounds {
			set := federated
			if i%2 == 0 {
				set = federatedPlus
			}
			if err := b.handleSVIDUpdate(ctx, netnsPeer, &brokerpb.SubscribeToX509SVIDResponse{FederatedBundles: set}); err != nil {
				errs <- err
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	// Settle on a deterministic final state so the last-publication assertions
	// below are exact.
	require.NoError(t, b.handleSVIDUpdate(ctx, netnsPeer, &brokerpb.SubscribeToX509SVIDResponse{FederatedBundles: federated}))

	pushes := store.snapshot()
	require.NotEmpty(t, pushes)

	served := make(map[string]int)
	sawValidationContext := false
	for i, set := range pushes {
		for name, previous := range served {
			version, present := set.svidVersion[name]
			require.Truef(t, present, "push %d dropped SVID %s (previously served at v%d)", i, name, previous)
			require.GreaterOrEqualf(t, version, previous,
				"push %d regressed SVID %s from v%d to v%d: an older secret set was published on top of a newer one",
				i, name, previous, version)
		}
		for name, version := range set.svidVersion {
			served[name] = version
		}

		if set.validationContexts > 0 {
			sawValidationContext = true
			continue
		}
		require.Falsef(t, sawValidationContext,
			"push %d served no validation context after one had already been served: Envoy would lose every peer it can verify", i)
	}

	// The last publication must carry the newest inputs: nothing may be left
	// stranded in the bridge's map because an overlapping push was coalesced.
	last := pushes[len(pushes)-1]
	require.Equal(t, rounds, last.svidVersion[idAName], "final published set must carry svc-a's newest SVID")
	require.Equal(t, rounds, last.svidVersion[idBName], "final published set must carry svc-b's newest SVID")
	require.Equal(t, 1, last.validationContexts, "final published set must carry the federated trust bundle")

	b.mu.RLock()
	defer b.mu.RUnlock()
	require.Len(t, b.secrets, 3, "bridge must hold both SVIDs and the trust bundle")
	require.Equal(t, b.gen, b.publishedGen, "the store must hold the bridge's latest generation")
}

// TestHandleBundleUpdateKeepsBundlesOnConversionError covers S11: a malformed
// bundle for one trust domain used to wipe every validation context from the
// map before it failed, so the next push from any other goroutine served Envoy
// a secret set with nothing to verify peers against.
func TestHandleBundleUpdateKeepsBundlesOnConversionError(t *testing.T) {
	const netnsPeer = "/proc/3/ns/net"

	ca := spiretest.NewCA(t)
	store := &recordingStore{}
	b := newOrderingTestBridge(store)
	ctx := t.Context()

	require.NoError(t, b.handleSVIDUpdate(ctx, netnsPeer, &brokerpb.SubscribeToX509SVIDResponse{
		FederatedBundles: map[string][]byte{"spiffe://peer.example": spiretest.NewCA(t).BundleDER()},
	}))
	require.Equal(t, 1, store.snapshot()[0].validationContexts)

	err := b.handleSVIDUpdate(ctx, netnsPeer, &brokerpb.SubscribeToX509SVIDResponse{
		FederatedBundles: map[string][]byte{"spiffe://peer.example": []byte("not a DER certificate")},
	})
	require.Error(t, err, "a malformed bundle must be reported")

	b.mu.RLock()
	require.True(t, b.hasValidationContextLocked(), "the previously served trust bundle must survive a failed update")
	b.mu.RUnlock()

	// Whatever pushes next (an SVID rotation, an unsubscribe) must still carry
	// the bundle.
	require.NoError(t, b.handleSVIDUpdate(ctx, "/proc/1/ns/net",
		svidResponse(t, ca, "spiffe://example.org/ns/x/sa/a", 1, nil)))
	pushes := store.snapshot()
	require.Equal(t, 1, pushes[len(pushes)-1].validationContexts,
		"a push following a failed bundle update must still serve the cached validation context")
}

// TestHandleBundleUpdateSkipsEmptyBundleSet covers the other half of S11: an
// update that leaves no trust bundles at all is never a legitimate instruction
// to stop verifying peers, so the cached contexts are kept and counted.
func TestHandleBundleUpdateSkipsEmptyBundleSet(t *testing.T) {
	const netnsPeer = "/proc/3/ns/net"

	store := &recordingStore{}
	b := newOrderingTestBridge(store)
	reader := installTestBridgeMetrics(t, b)
	ctx := t.Context()

	require.NoError(t, b.handleSVIDUpdate(ctx, netnsPeer, &brokerpb.SubscribeToX509SVIDResponse{
		FederatedBundles: map[string][]byte{"spiffe://peer.example": spiretest.NewCA(t).BundleDER()},
	}))
	before := len(store.snapshot())

	// The peer's stream drops every bundle it was carrying, and nothing else
	// contributes one: the served contexts must be kept.
	require.NoError(t, b.handleSVIDUpdate(ctx, netnsPeer, &brokerpb.SubscribeToX509SVIDResponse{}))

	require.Len(t, store.snapshot(), before, "an empty bundle update must not publish anything")
	b.mu.RLock()
	require.True(t, b.hasValidationContextLocked(), "the cached trust bundle must be kept")
	b.mu.RUnlock()

	require.Equal(t, int64(1), counterValue(t, reader, "aether.agent.sds_push.empty_bundle_skipped"))
	require.Equal(t, int64(0), counterValue(t, reader, "aether.agent.sds_push.stale_rejected"))
}

// TestBridgeMetricsCountersSeededAtZero pins the #717 lesson: the OTel SDK
// exports a counter only after its first Add, and all four of these are zero in
// a healthy agent — so without seeding, "never registered" and "zero" look the
// same to a grading query.
func TestBridgeMetricsCountersSeededAtZero(t *testing.T) {
	reader := installTestBridgeMetrics(t, newOrderingTestBridge(&recordingStore{}))
	for _, name := range []string{
		"aether.agent.sds_push.stale_rejected",
		"aether.agent.sds_push.empty_bundle_skipped",
		"aether.agent.spire.broker.reference_not_found",
		"aether.agent.spire.broker.permission_denied",
	} {
		value, found := lookupCounter(t, reader, name)
		require.Truef(t, found, "%s not exported before its first increment", name)
		require.Equalf(t, int64(0), value, "%s must be seeded at zero", name)
	}
}

// installTestBridgeMetrics points the bridge's instruments at a ManualReader so
// a test can read them; NewBridge otherwise rides the global (no-op) provider.
func installTestBridgeMetrics(t *testing.T, b *Bridge) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := newBridgeMetrics(provider.Meter("test"))
	require.NoError(t, err)
	b.metrics = m
	return reader
}

func counterValue(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	value, found := lookupCounter(t, reader, name)
	require.Truef(t, found, "counter %s not exported", name)
	return value
}

func lookupCounter(t *testing.T, reader *sdkmetric.ManualReader, name string) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.Truef(t, ok, "metric %s is %T, want Sum[int64]", name, m.Data)
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total, true
		}
	}
	return 0, false
}
