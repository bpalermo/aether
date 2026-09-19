package server

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockedBuffer is a log sink the background retry goroutine can write to while
// the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newSeverityServer(t *testing.T, watch IdentityGate) (*AgentXdsServer, *lockedBuffer) {
	t.Helper()
	srv, _ := newWaitServer(t, newDeadRegistry(), 50*time.Millisecond)
	out := &lockedBuffer{}
	srv.log = slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelWarn}))
	srv.retryBackoff = 5 * time.Millisecond
	srv.SetIdentityWatch(watch)
	return srv, out
}

// TestLocalOnlyStart_IsAnErrorWhenIdentityIsNotTheReason: a workload that holds
// its SVID and still cannot load the registry is on local-only config for a real
// reason, and says so at ERROR — the pre-#766 behaviour, unchanged.
func TestLocalOnlyStart_IsAnErrorWhenIdentityIsNotTheReason(t *testing.T) {
	held := newFakeIdentity()
	held.arrive()
	srv, out := newSeverityServer(t, held)

	require.NoError(t, srv.PreListen(t.Context()))

	assert.Contains(t, out.String(), `"level":"ERROR"`)
	assert.Contains(t, out.String(), "registry unavailable for initial snapshot")
}

// TestLocalOnlyStart_IsAWarningWhileIdentityIsPending is #766: the edge reaches
// the local-only path with no SVID yet, where no registrar handshake could have
// succeeded. That is the designed #740 wait — WARN, so a healthy roll keeps a
// clean ERROR log — and it stops being excused if identity arrives and the
// registry still cannot be loaded.
func TestLocalOnlyStart_IsAWarningWhileIdentityIsPending(t *testing.T) {
	pending := newFakeIdentity()
	srv, out := newSeverityServer(t, pending)

	require.NoError(t, srv.PreListen(t.Context()))

	assert.Contains(t, out.String(), "waits for its first SVID")
	assert.NotContains(t, out.String(), `"level":"ERROR"`, "a pending SVID is not a registry fault")

	// Still excused for as long as the SVID is pending, however often it retries.
	time.Sleep(100 * time.Millisecond)
	assert.NotContains(t, out.String(), `"level":"ERROR"`)

	pending.arrive()
	require.Eventually(t, func() bool {
		return strings.Contains(out.String(), "although this workload now has its SVID")
	}, 10*time.Second, 10*time.Millisecond, "a registry that stays unreachable after identity arrived is a fault after all")
	assert.Equal(t, 1, strings.Count(out.String(), `"level":"ERROR"`), "escalates once, not per retry")
}
