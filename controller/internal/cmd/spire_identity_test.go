package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"aethermesh.dev/common/spire"
	"aethermesh.dev/common/spire/spiretest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// recordingReadyz stands in for the controller-runtime manager's readiness
// registry, so the readiness table can be exercised without an apiserver.
type recordingReadyz struct {
	checks map[string]healthz.Checker
}

func newRecordingReadyz() *recordingReadyz {
	return &recordingReadyz{checks: map[string]healthz.Checker{}}
}

func (r *recordingReadyz) AddReadyzCheck(name string, check healthz.Checker) error {
	r.checks[name] = check
	return nil
}

// withSpireConfig sets the SPIRE-related globals for one test and restores them.
func withSpireConfig(t *testing.T, enabled bool, socket string, warnAfter time.Duration, logger *slog.Logger) {
	t.Helper()

	prevEnabled, prevSocket, prevWarn, prevLog := cfg.SpireEnabled, cfg.SpireWorkloadSocketPath, cfg.SpireWaitWarnAfter, l
	t.Cleanup(func() {
		cfg.SpireEnabled, cfg.SpireWorkloadSocketPath, cfg.SpireWaitWarnAfter, l = prevEnabled, prevSocket, prevWarn, prevLog
	})

	cfg.SpireEnabled = enabled
	cfg.SpireWorkloadSocketPath = socket
	cfg.SpireWaitWarnAfter = warnAfter
	l = logger
}

// TestBuildControllerBootstrapOptsReturnsImmediately is the regression test for
// #740 at the controller's call site — the earliest SPIRE call of the four,
// because it runs BEFORE manager.Bootstrap to build the webhook server. It used
// to block for up to 25s against an unreachable Workload API and then exit the
// process, so the controller was crash-looping through exactly the boot window in
// which the apiserver needed it.
//
// The negative control is the code this replaces: spire.NewSource on the same
// unreachable socket returns only after spire.SourceTimeout (25s) and with an
// error that the caller turned into a process exit.
func TestBuildControllerBootstrapOptsReturnsImmediately(t *testing.T) {
	withSpireConfig(t, true, spiretest.UnservedSocket(t), time.Minute, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	start := time.Now()
	src, opts, err := buildControllerBootstrapOpts(ctx)
	elapsed := time.Since(start)

	require.NoError(t, err, "an unreachable Workload API must not fail startup")
	require.NotNil(t, src)
	t.Cleanup(func() { _ = src.Close() })
	assert.Less(t, elapsed, 100*time.Millisecond, "startup must not wait for SPIRE")
	assert.False(t, src.HasSVID(), "no SVID can exist against a socket nobody serves")

	// The webhook server option is still installed: the certificate is resolved per
	// handshake (GetCertificate), so a source that is still waiting fails individual
	// handshakes — and the apiserver fails open, failurePolicy: Ignore — instead of
	// failing the process.
	require.Len(t, opts, 2, "scheme + webhook server options")
	o := &ctrl.Options{}
	opts[1](o)
	assert.NotNil(t, o.WebhookServer, "the webhook must be wired to serve with the SPIRE SVID")
}

// TestBuildControllerBootstrapOptsSpireDisabled pins the #421 path: with
// --spire-enabled=false there is no source, no log line and no webhook override —
// controller-runtime serves from the Helm-provisioned cert exactly as before.
func TestBuildControllerBootstrapOptsSpireDisabled(t *testing.T) {
	logs := &bytes.Buffer{}
	withSpireConfig(t, false, "/nonexistent/workload.sock", time.Minute,
		slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	src, opts, err := buildControllerBootstrapOpts(t.Context())

	require.NoError(t, err)
	assert.Nil(t, src, "SPIRE off must create no source at all")
	assert.Len(t, opts, 1, "SPIRE off must add no webhook server option")
	assert.Empty(t, logs.String(), "SPIRE off must log nothing")
}

// TestWireSpireReadiness is the controller's readiness table. The gate is
// load-bearing here in a way it is not on a DaemonSet: NotReady removes this
// replica from the webhook Service's endpoints.
func TestWireSpireReadiness(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/readyz", nil)
	require.NoError(t, err)

	t.Run("SPIRE disabled registers no check", func(t *testing.T) {
		readyz := newRecordingReadyz()
		require.NoError(t, wireSpireReadiness(readyz, nil))
		assert.Empty(t, readyz.checks, "with no identity to wait for the check must disappear entirely")
	})

	t.Run("inside the dwell the gate passes", func(t *testing.T) {
		readyz := newRecordingReadyz()
		src := spire.NewWaitingSource(spiretest.UnservedSocket(t), time.Hour, slog.New(slog.DiscardHandler))
		require.NoError(t, wireSpireReadiness(readyz, src))

		check, ok := readyz.checks[spire.ReadyCheckName]
		require.True(t, ok, "the gate must be registered as %q", spire.ReadyCheckName)
		assert.NoError(t, check(req), "a wait inside the dwell is a normal startup, not NotReady")
	})

	t.Run("past the dwell the gate fails", func(t *testing.T) {
		readyz := newRecordingReadyz()
		// A dwell of one nanosecond has already elapsed by the time the check runs.
		src := spire.NewWaitingSource(spiretest.UnservedSocket(t), time.Nanosecond, slog.New(slog.DiscardHandler))
		require.NoError(t, wireSpireReadiness(readyz, src))

		err := readyz.checks[spire.ReadyCheckName](req)
		require.Error(t, err, "past the dwell the replica must leave the webhook Service's endpoints")
		assert.Contains(t, err.Error(), "no SPIRE SVID after")
	})
}
