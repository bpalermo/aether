package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLogger returns a JSON slog logger writing into buf, so a test can assert on
// the LEVEL a message was emitted at (which is the whole point of the fix below).
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// levelOfRecordContaining returns the level of the first log record whose message
// contains want, or "" when no record matched.
func levelOfRecordContaining(t *testing.T, buf *bytes.Buffer, want string) string {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		if msg, _ := rec["msg"].(string); strings.Contains(msg, want) {
			level, _ := rec["level"].(string)
			return level
		}
	}
	return ""
}

// TestResolveUpstreamsFlagsWin: explicit --mesh-dns-upstream flags bypass resolv.conf.
func TestResolveUpstreamsFlagsWin(t *testing.T) {
	upstreams = []string{"10.96.0.10:53"}
	t.Cleanup(func() { upstreams = nil })

	var buf bytes.Buffer
	assert.Equal(t, []string{"10.96.0.10:53"}, resolveUpstreams(captureLogger(&buf), "/nonexistent"))
	assert.Empty(t, buf.String(), "no resolv.conf read, nothing to report")
}

// TestResolveUpstreamsFromResolvConf: with no flags the node resolv.conf supplies the
// upstreams, reported at INFO.
func TestResolveUpstreamsFromResolvConf(t *testing.T) {
	upstreams = nil
	path := filepath.Join(t.TempDir(), "resolv.conf")
	require.NoError(t, os.WriteFile(path, []byte("nameserver 10.96.0.10\n"), 0o644))

	var buf bytes.Buffer
	assert.Equal(t, []string{"10.96.0.10"}, resolveUpstreams(captureLogger(&buf), path))
	assert.Equal(t, "INFO", levelOfRecordContaining(t, &buf, "upstream defaulted from resolv.conf"))
}

// TestResolveUpstreamsEmptyIsLoud: no flags and an unreadable/empty resolv.conf leaves
// the resolver with NO upstream, which makes every non-mesh query (cluster.local and
// external) a forward_error — a full DNS outage for every managed pod on the node,
// since the CNI DNATs all :53 here. Behaviour is unchanged (fail open, mesh names
// still resolve) but it must be reported at ERROR, not the INFO "upstreams=[]" line
// that hid it before issue #586.
func TestResolveUpstreamsEmptyIsLoud(t *testing.T) {
	upstreams = nil
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty-resolv.conf")
	require.NoError(t, os.WriteFile(empty, []byte("# no nameserver here\nsearch cluster.local\n"), 0o644))

	for name, path := range map[string]string{
		"missing":        filepath.Join(dir, "does-not-exist"),
		"no nameservers": empty,
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			assert.Empty(t, resolveUpstreams(captureLogger(&buf), path), "fail open: no upstream, still serving")
			assert.Equal(t, "ERROR", levelOfRecordContaining(t, &buf, "NO upstream resolver"),
				"an empty upstream set is a node-wide DNS outage and must be loud")
		})
	}
}
