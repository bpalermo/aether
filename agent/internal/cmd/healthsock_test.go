package cmd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// socketClient is an HTTP client that dials the given Unix socket, as the
// agent-ready probe does.
func socketClient(path string) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}
}

func get(t *testing.T, c *http.Client, path string) (int, string) {
	t.Helper()
	resp, err := c.Get("http://agent" + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestHealthSocketServesTheReadinessVerdict: the pod-local socket answers what
// the TCP endpoint did — liveness always, readiness from the agent's own
// checks (the standby gate among them), with the failing check named on
// ?verbose so the kubelet's probe event says why.
func TestHealthSocketServesTheReadinessVerdict(t *testing.T) {
	dir, err := os.MkdirTemp("", "hs")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "sub", "h.sock")
	// A stale socket file from an earlier container of the pod is replaced.
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	ready, _ := newTestReadiness()
	standbyErr := errors.New("first snapshot not built yet")
	var failing atomic.Bool
	failing.Store(true)
	require.NoError(t, ready.add(newFakeReadyzAdder(), "standby", func(*http.Request) error {
		if failing.Load() {
			return standbyErr
		}
		return nil
	}))

	ln, err := listenHealthSocket(path)
	require.NoError(t, err)
	srv := &http.Server{Handler: newHealthMux(ready.probeChecks(nil)), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	c := socketClient(path)

	code, _ := get(t, c, "/healthz")
	assert.Equal(t, http.StatusOK, code, "liveness is independent of readiness")

	code, body := get(t, c, "/readyz?verbose")
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.Contains(t, body, "standby")

	failing.Store(false)
	code, _ = get(t, c, "/readyz")
	assert.Equal(t, http.StatusOK, code)
}

// TestProbeChecksCarryTheManagerBaseline: the socket must not be a weaker
// gate than the TCP endpoint it replaces, which also served the manager's
// ping and informer-cache sync.
func TestProbeChecksCarryTheManagerBaseline(t *testing.T) {
	ready, _ := newTestReadiness()
	require.NoError(t, ready.add(newFakeReadyzAdder(), "cni-chained", healthz.Ping))
	checks := ready.probeChecks(healthz.Ping)
	assert.Contains(t, checks, "readyz")
	assert.Contains(t, checks, "cache-sync")
	assert.Contains(t, checks, "cni-chained")
}
