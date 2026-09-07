package registrar

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

// errPeerNoIdentity is verbatim what go-spiffe's server-side verifier produced
// on the rev210 upgrade roll (2026-09-07 20:03:45Z), when a registrar Pod was in
// its Service's endpoints before SPIRE had issued its SVID and an agent on
// main-worker-01 dialled it.
var errPeerNoIdentity = errors.New("x509svid: could not get X509 bundle")

// peerlessCreds is a TransportCredentials whose handshake fails the way an mTLS
// dial against an identity-less server fails: the connection is established, the
// handshake is attempted, and the peer cannot produce a bundle. gRPC renders
// that as `transport: authentication handshake failed: <err>` inside an
// Unavailable status, which is the string the client has to classify.
type peerlessCreds struct{}

func (peerlessCreds) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	_ = conn.Close()
	return nil, nil, errPeerNoIdentity
}

func (peerlessCreds) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	_ = conn.Close()
	return nil, nil, errPeerNoIdentity
}

func (peerlessCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls", SecurityVersion: "1.3"}
}

func (peerlessCreds) Clone() credentials.TransportCredentials { return peerlessCreds{} }

func (peerlessCreds) OverrideServerName(string) error { return nil }

// newPeerlessRegistry stands up a listener that accepts the TCP connection and
// then fails the mTLS handshake for want of a server identity, and points a
// registry client at it. The client's OWN identity is ready (IdentityReady
// returns true), so the deferral under test is about the SERVER's startup only.
func newPeerlessRegistry(t *testing.T) (*RegistrarRegistry, *lockedBuffer, *sdkmetric.ManualReader) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			// Accept and drop: the client's handshake fails on its own side
			// with the same error the server would have raised.
			_ = conn.Close()
		}
	}()

	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	r := NewRegistrarRegistry(log, Config{
		Address:     "passthrough:///registrar-test",
		ClusterName: "test-cluster",
		NodeName:    "test-node",
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(peerlessCreds{}),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		},
		IdentityReady: func() bool { return true }, // we have ours; the server does not
	})
	metrics, reader := newTestClientMetrics(t)
	r.metrics = metrics
	return r, logs, reader
}

// TestWatchLoop_DefersWhilePeerHasNoIdentity is PR 4 of #740. The agent holds
// its own SVID, so the pre-existing deferral (#741) does not apply; the failure
// comes from the REGISTRAR still waiting for SPIRE, which is the server's
// startup and not a client fault. It must be classified like #718's drain
// GOAWAY: INFO, no watch_errors, bounded retry — never the ERROR the path
// exists to report a registrar that will not serve the stream (#700).
func TestWatchLoop_DefersWhilePeerHasNoIdentity(t *testing.T) {
	r, logs, reader := newPeerlessRegistry(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, r.Initialize(ctx))
	defer func() { _ = r.Close() }()

	require.Eventually(t, func() bool {
		return findRecord(logRecords(t, logs), "registrar has no identity yet; retrying") != nil
	}, 30*time.Second, 10*time.Millisecond,
		"the peer's pending identity must be announced; logs:\n%s", logs.String())

	deferred := findRecord(logRecords(t, logs), "registrar has no identity yet; retrying")
	require.Equal(t, "INFO", deferred["level"])
	require.Equal(t, true, deferred["peer_identity_pending"])
	require.Contains(t, deferred, "backoff", "the deferral still waits before retrying")

	for _, rec := range logRecords(t, logs) {
		require.NotEqual(t, "ERROR", rec["level"],
			"the registrar's own startup must not be reported as a stream failure; logs:\n%s", logs.String())
		require.NotEqual(t, "failed to start watch stream, retrying", rec["msg"],
			"the #700 ERROR must be reserved for a registrar that will not serve; logs:\n%s", logs.String())
	}

	_, found := metricValue(t, reader, "aether.agent.registry.watch_errors")
	require.False(t, found, "the peer's startup must not count as a watch error")
}

// TestDeferPeerStreamBoundsTheBackoff pins the one behavioural difference from
// deferStream: nothing wakes this loop when the FAR side gets its SVID, so the
// backoff has to escalate — bounded at maxBackoff, which caps the added
// reconnect latency once the registrar is serving.
func TestDeferPeerStreamBoundsTheBackoff(t *testing.T) {
	r, _, _ := newPeerlessRegistry(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // waitBeforeRetry returns immediately; only the arithmetic is under test

	backoff := initialBackoff
	require.False(t, r.deferPeerStream(ctx, errPeerNoIdentity, &backoff),
		"a cancelled context stops the loop")
	require.Equal(t, initialBackoff, backoff, "a stopped loop leaves the backoff alone")

	// Now with a live context, so the doubling runs.
	live := t.Context()
	r.wake <- struct{}{} // cut the sleep short instead of waiting a second
	require.True(t, r.deferPeerStream(live, errPeerNoIdentity, &backoff))
	require.Equal(t, 2*initialBackoff, backoff, "the backoff escalates")

	backoff = maxBackoff
	r.wake <- struct{}{}
	require.True(t, r.deferPeerStream(live, errPeerNoIdentity, &backoff))
	require.Equal(t, maxBackoff, backoff, "the backoff is capped")
}
