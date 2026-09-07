package spire

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// svidOnlySource serves an SVID but no bundle for its trust domain — the half
// identity that readiness silently accepted before #740's finding 3.
type svidOnlySource struct {
	svid *x509svid.SVID
}

func (s *svidOnlySource) GetX509SVID() (*x509svid.SVID, error) { return s.svid, nil }

func (s *svidOnlySource) GetX509BundleForTrustDomain(td spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return nil, fmt.Errorf("x509bundle: no X.509 bundle found for trust domain: %q", td.Name())
}

func (s *svidOnlySource) Updated() <-chan struct{} { return nil }

var _ SVIDSource = (*svidOnlySource)(nil)

// TestFirstIdentityRequiresBothHalves pins what "has identity" means: an mTLS
// peer needs this workload's own SVID AND the bundle it verifies the other side
// against, so a source that can produce only one of them is not ready — and
// neither is the WaitingSource built on it, since firstIdentity is the sole gate
// on publishing the acquired source.
//
// This is issue #740's finding 3. On 2026-09-07 readyz reported `spire-svid ok`
// and svid_ready went to 1 three seconds after the SVID landed, while every mTLS
// client kept failing `x509svid: could not get X509 bundle` for another 2m11s.
// Readiness was answering a narrower question than the one its consumers ask.
func TestFirstIdentityRequiresBothHalves(t *testing.T) {
	t.Run("bundle missing is not an identity", func(t *testing.T) {
		svid := &x509svid.SVID{
			ID: spiffeid.RequireFromString("spiffe://" + testTrustDomain + "/ns/aether-system/sa/aether-agent"),
		}

		_, err := firstIdentity(&svidOnlySource{svid: svid})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "X.509 bundle")
		assert.Contains(t, err.Error(), testTrustDomain, "the failure must name the trust domain it looked for")
	})

	t.Run("both halves present yields the trust domain", func(t *testing.T) {
		// The real Workload API path: what a WaitingSource publishes answers both,
		// and the trust domain it reports is the SVID's own.
		fake, socket := startFakeWorkloadAPI(t)
		fake.startServing()

		w, _ := newTestWaitingSource(t, socket, time.Minute)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = w.Start(ctx) }()

		require.Eventually(t, w.HasSVID, 30*time.Second, 10*time.Millisecond)

		td, err := firstIdentity(w)
		require.NoError(t, err)
		assert.Equal(t, testTrustDomain, td)
	})
}
