package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// fakeReadyzAdder records what would have been registered with the manager.
type fakeReadyzAdder struct {
	checks map[string]healthz.Checker
	err    error
}

func newFakeReadyzAdder() *fakeReadyzAdder {
	return &fakeReadyzAdder{checks: map[string]healthz.Checker{}}
}

func (f *fakeReadyzAdder) AddReadyzCheck(name string, check healthz.Checker) error {
	if f.err != nil {
		return f.err
	}
	f.checks[name] = check
	return nil
}

// newTestReadiness returns an aggregate writing JSON logs into buf.
func newTestReadiness() (*agentReadiness, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return newAgentReadiness(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))), buf
}

// logLines parses the JSON log records emitted so far.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		out = append(out, rec)
	}
	return out
}

// TestAgentReadinessErr is the aggregate the node taint remover consults: the
// SAME verdict the kubelet computes from /readyz, so the two cannot disagree
// (issue #740, finding 2 — the remover used to see only a subset and cleared a
// taint the controller had armed for a check it could not see).
func TestAgentReadinessErr(t *testing.T) {
	errChain := errors.New("aether is not chained in the node conflist")
	errSVID := errors.New("no SPIRE SVID after 2m10s (socket /run/spire.sock)")

	t.Run("all passing", func(t *testing.T) {
		ready, _ := newTestReadiness()
		m := newFakeReadyzAdder()
		require.NoError(t, ready.add(m, "cni-chained", func(*http.Request) error { return nil }))
		require.NoError(t, ready.add(m, "spire-svid", func(*http.Request) error { return nil }))

		assert.NoError(t, ready.Err())
		assert.Len(t, m.checks, 2, "every check must still be registered with the manager")
	})

	t.Run("one failing names itself", func(t *testing.T) {
		ready, _ := newTestReadiness()
		m := newFakeReadyzAdder()
		require.NoError(t, ready.add(m, "cni-chained", func(*http.Request) error { return nil }))
		require.NoError(t, ready.add(m, "spire-svid", func(*http.Request) error { return errSVID }))

		err := ready.Err()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spire-svid")
		assert.Contains(t, err.Error(), "no SPIRE SVID")
	})

	t.Run("several failing are all reported", func(t *testing.T) {
		ready, _ := newTestReadiness()
		m := newFakeReadyzAdder()
		require.NoError(t, ready.add(m, "cni-chained", func(*http.Request) error { return errChain }))
		require.NoError(t, ready.add(m, "spire-svid", func(*http.Request) error { return errSVID }))

		err := ready.Err()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cni-chained")
		assert.Contains(t, err.Error(), "spire-svid")
	})

	t.Run("no checks is ready", func(t *testing.T) {
		// SPIRE off and the re-assert loop off: nothing to gate on.
		ready, _ := newTestReadiness()
		assert.NoError(t, ready.Err())
	})

	t.Run("a registration failure is named", func(t *testing.T) {
		ready, _ := newTestReadiness()
		m := newFakeReadyzAdder()
		m.err = errors.New("manager already started")

		err := ready.add(m, "spire-svid", func(*http.Request) error { return nil })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spire-svid")
	})
}

// TestAgentReadinessLogsTransitions covers the operator-visible half.
// controller-runtime's /readyz?verbose redacts checker errors — the cluster
// prints `spire-svid failed: reason withheld` — so the only place the actual
// reason can reach an operator is the log, once per transition rather than on
// every kubelet poll.
func TestAgentReadinessLogsTransitions(t *testing.T) {
	ready, buf := newTestReadiness()
	m := newFakeReadyzAdder()

	failing := errors.New("no SPIRE SVID after 2m10s (socket /run/spire.sock)")
	var fail bool
	require.NoError(t, ready.add(m, "spire-svid", func(*http.Request) error {
		if fail {
			return failing
		}
		return nil
	}))

	// Passing from the start logs nothing: the steady state must be silent.
	for range 3 {
		require.NoError(t, ready.Err())
	}
	assert.Empty(t, logLines(t, buf), "a check that never changed state must not log")

	// First failure: one WARN carrying the reason the endpoint withholds.
	fail = true
	require.Error(t, ready.Err())
	require.Error(t, ready.Err())
	require.Error(t, ready.Err())

	records := logLines(t, buf)
	require.Len(t, records, 1, "the failure must be logged ONCE, not once per poll")
	assert.Equal(t, "WARN", records[0]["level"])
	assert.Equal(t, "spire-svid readiness failing", records[0]["msg"])
	assert.Contains(t, records[0]["reason"], "no SPIRE SVID after 2m10s")

	// Recovery: one INFO, so the log brackets the outage.
	fail = false
	require.NoError(t, ready.Err())
	require.NoError(t, ready.Err())

	records = logLines(t, buf)
	require.Len(t, records, 2)
	assert.Equal(t, "INFO", records[1]["level"])
	assert.Equal(t, "spire-svid readiness passing", records[1]["msg"])
}

// TestAgentReadinessRegisteredCheckerLogsToo pins that the checker handed to the
// manager is the WRAPPED one: the kubelet's polls are what usually observe a
// transition first, and an operator reading the log should not have to wait for
// the taint remover's next reconcile to see why.
func TestAgentReadinessRegisteredCheckerLogsToo(t *testing.T) {
	ready, buf := newTestReadiness()
	m := newFakeReadyzAdder()
	require.NoError(t, ready.add(m, "cni-chained", func(*http.Request) error {
		return errors.New("aether is not chained in the node conflist")
	}))

	registered, ok := m.checks["cni-chained"]
	require.True(t, ok)
	require.Error(t, registered(nil))

	records := logLines(t, buf)
	require.Len(t, records, 1)
	assert.Equal(t, "cni-chained readiness failing", records[0]["msg"])
}
