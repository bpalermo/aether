package registrar

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errHandshakeBundle is how a failed mesh handshake reaches this loop. It is the
// SAME text whichever side was short of an identity — go-spiffe raises
// `could not get X509 bundle` in its own verifier, against the LOCAL bundle
// source (svid/x509svid/verify.go) — and after a pre-identity attempt the gRPC
// ClientConn hands it back from cache, so it is not even current. Local state is
// the only sound discriminator.
var errHandshakeBundle = status.Error(codes.Unavailable,
	`connection error: desc = "transport: authentication handshake failed: x509svid: could not get X509 bundle"`)

// newClassifyingRegistry builds a client with no connection at all: failStream
// is exercised directly, so the classification is tested without racing a dial.
func newClassifyingRegistry(t *testing.T, identityReady func() bool) (*RegistrarRegistry, *lockedBuffer) {
	t.Helper()

	logs := &lockedBuffer{}
	r := NewRegistrarRegistry(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Config{
		Address:       "passthrough:///registrar-test",
		ClusterName:   "test-cluster",
		NodeName:      "test-node",
		IdentityReady: identityReady,
	})
	metrics, _ := newTestClientMetrics(t)
	r.metrics = metrics
	return r, logs
}

// TestFailStream_ClassificationOrder is PR 5 of #740. The rev211 deploy roll
// (2026-09-07 20:47Z, main-worker-02) logged `registrar has no identity yet` for
// two conditions that were not that: at 20:47:27.338 the AGENT had no SVID, and
// at 20:47:27.912 the agent had one (85ms old) while its ClientConn was still
// recovering from the handshakes it had failed before. Both registrar replicas
// had been Ready with identities since 20:46:45.
//
// The three must be told apart in this order — ours, the reconnect, then the
// peer's — because that is the order in which the answers are trustworthy.
func TestFailStream_ClassificationOrder(t *testing.T) {
	tests := []struct {
		name          string
		identityReady bool
		identityAge   time.Duration // 0 = never notified
		wantLevel     string
		wantMsg       string
	}{
		{
			name:          "our own identity is pending",
			identityReady: false,
			wantLevel:     "INFO",
			wantMsg:       "watch stream deferred until this agent has an SVID",
		},
		{
			name:          "identity just arrived and the connection is catching up",
			identityReady: true,
			identityAge:   85 * time.Millisecond,
			wantLevel:     "INFO",
			wantMsg:       "registrar connection not yet re-established after identity; retrying",
		},
		{
			name:          "a settled identity leaves only the peer",
			identityReady: true,
			identityAge:   time.Hour,
			wantLevel:     "INFO",
			wantMsg:       "registrar has no identity yet; retrying",
		},
		{
			name:          "never notified: no window, pre-PR-5 classification",
			identityReady: true,
			wantLevel:     "INFO",
			wantMsg:       "registrar has no identity yet; retrying",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ready := tc.identityReady
			r, logs := newClassifyingRegistry(t, func() bool { return ready })
			if tc.identityAge != 0 {
				r.identityReadyAt.Store(time.Now().Add(-tc.identityAge).UnixNano())
			}

			backoff := time.Millisecond
			require.True(t, r.failStream(t.Context(), "failed to start watch stream, retrying", errHandshakeBundle, &backoff))

			rec := findRecord(logRecords(t, logs), tc.wantMsg)
			require.NotNil(t, rec, "expected %q; logs:\n%s", tc.wantMsg, logs.String())
			assert.Equal(t, tc.wantLevel, rec["level"])

			for _, r := range logRecords(t, logs) {
				assert.NotEqual(t, "ERROR", r["level"], "none of the three is a failure; logs:\n%s", logs.String())
			}
		})
	}
}

// TestFailStream_RealFailuresSurviveTheWindow is the negative control the ERROR
// path exists for (#700): a registrar that will not serve must still be an ERROR
// once identity is settled, and a dial failure that is not a handshake must not
// be swallowed by the reconnect window's transport check either.
func TestFailStream_RealFailuresSurviveTheWindow(t *testing.T) {
	r, logs := newClassifyingRegistry(t, func() bool { return true })
	r.identityReadyAt.Store(time.Now().Add(-time.Hour).UnixNano())

	backoff := time.Millisecond
	err := status.Error(codes.Internal, "registrar: watcher evicted")
	require.True(t, r.failStream(t.Context(), "failed to start watch stream, retrying", err, &backoff))

	rec := findRecord(logRecords(t, logs), "failed to start watch stream, retrying")
	require.NotNil(t, rec, "logs:\n%s", logs.String())
	assert.Equal(t, "ERROR", rec["level"])
}

// TestNotifyIdentityReady_StampsOnce pins the window's anchor: it measures the
// age of THIS connection's identity, so a later re-announcement must not reopen
// a window that closed seconds after boot.
func TestNotifyIdentityReady_StampsOnce(t *testing.T) {
	r, _ := newClassifyingRegistry(t, func() bool { return true })

	r.NotifyIdentityReady()
	first := r.identityReadyAt.Load()
	require.NotZero(t, first)

	time.Sleep(2 * time.Millisecond)
	r.NotifyIdentityReady()
	assert.Equal(t, first, r.identityReadyAt.Load(), "only the first identity is the anchor")
}
