package cniconflist

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aethermesh.dev/cni/conflist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	confName = "10-flannel.conflist"

	// flannelOnly is what kube-flannel's init container `cp -f`s over the
	// conflist: its ConfigMap template, with no aether entry.
	flannelOnly = `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel","delegate":{"hairpinMode":true}},{"type":"portmap","capabilities":{"portMappings":true}}]}`

	// aetherEntry is the plugin entry cni-install appends.
	aetherEntry = `{"name":"aether","type":"aether-cni","cniVersion":"0.0.1","agentCNIPath":"/run/aether/cni.sock"}`
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// chained returns the flannel conflist with aether appended, exactly as
// cni-install writes it.
func chained(t *testing.T) []byte {
	t.Helper()
	out, err := conflist.Insert([]byte(aetherEntry), []byte(flannelOnly))
	require.NoError(t, err)
	return out
}

func writeConf(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, content, 0o644))
	return path
}

// newReasserter returns a Reasserter over dir with its logger and metrics
// wired the way Start does (metrics stay nil: the no-op meter is fine, and every
// method is nil-safe).
func newReasserter(dir string) *Reasserter {
	r := &Reasserter{Dir: dir, Log: testLogger()}
	r.log = r.Log
	return r
}

// isChained reports whether the conflist at path currently carries the aether
// entry.
func isChained(t *testing.T, path string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	c, err := conflist.Parse(data)
	return err == nil && c.HasAether()
}

func pluginTypes(t *testing.T, data []byte) []string {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))
	raw, ok := m["plugins"].([]any)
	require.True(t, ok)
	types := make([]string, 0, len(raw))
	for _, p := range raw {
		types = append(types, p.(map[string]any)["type"].(string))
	}
	return types
}

func TestCheck(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		// content is what sits in the conflist before the check; nil writes no file.
		content []byte
		// primed seeds a last-known-good entry (as an earlier check would have).
		primed bool
		// wantTypes is the expected plugin order after the check; nil means the
		// file must be byte-identical to what was written.
		wantTypes []string
		// wantUntouched asserts the file bytes did not change.
		wantUntouched bool
	}{
		{
			name:          "missing entry is re-appended",
			content:       []byte(flannelOnly),
			primed:        true,
			wantTypes:     []string{"flannel", "portmap", "aether-cni"},
			wantUntouched: false,
		},
		{
			name:          "already chained is a byte-identical no-op",
			content:       nil, // filled in below with the chained conflist
			primed:        false,
			wantUntouched: true,
		},
		{
			name:          "invalid JSON is never touched",
			content:       []byte(`{not json`),
			primed:        true,
			wantUntouched: true,
		},
		{
			name:          "conflist with no primary CNI plugin is never touched",
			content:       []byte(`{"name":"cbr0","cniVersion":"0.3.1","plugins":[]}`),
			primed:        true,
			wantUntouched: true,
		},
		{
			name:          "missing entry with no known-good entry is never touched",
			content:       []byte(flannelOnly),
			primed:        false,
			wantUntouched: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			content := tc.content
			if content == nil {
				content = chained(t)
			}
			path := writeConf(t, dir, confName, content)

			r := newReasserter(dir)
			if tc.primed {
				r.entry = []byte(aetherEntry)
			}
			r.check(ctx)

			got, err := os.ReadFile(path)
			require.NoError(t, err)
			if tc.wantUntouched {
				assert.Equal(t, string(content), string(got), "the file must not have been rewritten")
				return
			}
			assert.Equal(t, tc.wantTypes, pluginTypes(t, got))
		})
	}

	t.Run("an absent conflist directory is a no-op", func(t *testing.T) {
		r := newReasserter(filepath.Join(t.TempDir(), "does-not-exist"))
		r.entry = []byte(aetherEntry)
		r.check(ctx) // must not panic, must not create anything
		_, err := os.Stat(r.Dir)
		assert.True(t, os.IsNotExist(err), "the re-asserter must never create the CNI config dir")
	})

	t.Run("an empty conflist directory is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		r := newReasserter(dir)
		r.entry = []byte(aetherEntry)
		r.check(ctx)
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries, "the re-asserter must never create a conflist")
	})

	t.Run("a chained conflist primes the known-good entry", func(t *testing.T) {
		dir := t.TempDir()
		writeConf(t, dir, confName, chained(t))
		r := newReasserter(dir)
		require.Empty(t, r.entry)

		r.check(ctx)
		require.NotEmpty(t, r.entry, "the observed entry must be cached for later re-assert")

		// A competing writer now strips it; the next check restores it from cache.
		writeConf(t, dir, confName, []byte(flannelOnly))
		r.check(ctx)
		got, err := os.ReadFile(filepath.Join(dir, confName))
		require.NoError(t, err)
		assert.Equal(t, []string{"flannel", "portmap", "aether-cni"}, pluginTypes(t, got))
		assert.Equal(t, string(chained(t)), string(got), "the restored file matches what cni-install writes")
	})

	t.Run("only the active (first) conflist is repaired", func(t *testing.T) {
		dir := t.TempDir()
		writeConf(t, dir, "10-flannel.conflist", []byte(flannelOnly))
		other := writeConf(t, dir, "99-other.conflist", []byte(`{"name":"other","cniVersion":"0.3.1","plugins":[{"type":"bridge"}]}`))

		r := newReasserter(dir)
		r.entry = []byte(aetherEntry)
		r.check(ctx)

		active, err := os.ReadFile(filepath.Join(dir, "10-flannel.conflist"))
		require.NoError(t, err)
		assert.Equal(t, []string{"flannel", "portmap", "aether-cni"}, pluginTypes(t, active))

		rest, err := os.ReadFile(other)
		require.NoError(t, err)
		assert.Equal(t, []string{"bridge"}, pluginTypes(t, rest), "non-active configs are left alone")
	})
}

// TestStartConverges is the end-to-end test of the runnable: a real fsnotify
// watcher over a temp dir, a flannel-only conflist written over the chained one
// mid-watch (the incident), and the file converging back to chained within the
// settle window — with the periodic re-check too slow to be what fixed it.
func TestStartConverges(t *testing.T) {
	dir := t.TempDir()
	path := writeConf(t, dir, confName, []byte(flannelOnly))

	r := &Reasserter{
		Dir:         dir,
		Interval:    time.Hour, // the watcher, not the ticker, must do the work
		SettleDelay: 50 * time.Millisecond,
		Log:         testLogger(),
	}
	// Prime the known-good entry before the loop goroutine exists, standing in for
	// the chained conflist cni-install leaves behind at agent start.
	r.entry = []byte(aetherEntry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// The initial check repairs the file. Its completion also proves the fsnotify
	// watch is established: Start adds the watch before the first check.
	require.Eventually(t, func() bool {
		return isChained(t, path)
	}, 5*time.Second, 10*time.Millisecond, "the initial check must chain aether")

	// kube-flannel's init container: cp -f its template over the conflist.
	require.NoError(t, os.WriteFile(path, []byte(flannelOnly), 0o644))

	require.Eventually(t, func() bool {
		return isChained(t, path)
	}, 5*time.Second, 20*time.Millisecond, "the watcher must re-assert the stripped entry")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(chained(t)), string(got), "the converged file matches what cni-install writes")

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "Start must never fail the manager")
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

// TestPrimeFromDurableEntry covers the priming window of issue #680: a competing
// writer strips the conflist between cni-install's write and this loop's first
// check, so there is nothing healthy to observe and the durable copy cni-install
// left behind is the only known-good entry left on the node.
func TestPrimeFromDurableEntry(t *testing.T) {
	ctx := context.Background()

	// durableEntry is what cni-install persists: the entry as it sits in the
	// conflist it chained (Insert drops the entry's own cniVersion).
	const durableEntry = `{"name":"aether","type":"aether-cni","agentCNIPath":"/run/aether/cni.sock"}`

	// liveEntry is deliberately distinguishable from the durable one, so a test
	// can tell which source a repair came from.
	const liveEntry = `{"name":"aether","type":"aether-cni","agentCNIPath":"/run/aether/observed.sock"}`

	writeDurable := func(t *testing.T, dir, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(conflist.EntryPath(dir), []byte(content), 0o644))
	}

	t.Run("an unchained conflist is repaired from the durable entry", func(t *testing.T) {
		dir := t.TempDir()
		path := writeConf(t, dir, confName, []byte(flannelOnly))
		writeDurable(t, dir, durableEntry)

		var logs logCapture
		r := newReasserter(dir)
		r.log = logs.logger()
		require.Empty(t, r.entry, "nothing was ever observed: this is the priming window")

		r.check(ctx)

		assert.True(t, isChained(t, path), "the durable entry must repair the conflist")
		assert.Equal(t, []string{"flannel", "portmap", "aether-cni"}, pluginTypes(t, mustRead(t, path)))
		assert.True(t, r.ChainStatus().Observed)
		assert.True(t, r.ChainStatus().Chained, "the repair must publish a chained state")
		assert.Contains(t, logs.String(), "primed known-good entry from durable file")

		// Priming lasts for the rest of the process: a second strip is repaired
		// without the durable file being there at all.
		require.NoError(t, os.Remove(conflist.EntryPath(dir)))
		writeConf(t, dir, confName, []byte(flannelOnly))
		r.check(ctx)
		assert.True(t, isChained(t, path), "the primed entry survives the durable file going away")
	})

	t.Run("no durable entry keeps the fail-closed path", func(t *testing.T) {
		dir := t.TempDir()
		path := writeConf(t, dir, confName, []byte(flannelOnly))

		var logs logCapture
		r := newReasserter(dir)
		r.log = logs.logger()
		r.check(ctx)

		assert.Equal(t, flannelOnly, string(mustRead(t, path)), "the conflist must not have been rewritten")
		assert.Empty(t, r.entry)
		assert.True(t, r.ChainStatus().Observed)
		assert.False(t, r.ChainStatus().Chained)
		assert.Contains(t, logs.String(), "no durable aether entry on disk to prime from")
		assert.Contains(t, logs.String(), "no known-good entry could be recovered")
	})

	t.Run("a garbage durable entry is ignored and logged", func(t *testing.T) {
		for _, garbage := range []string{`{not json`, ``, `{"name":"cbr0","type":"flannel"}`, `[{"type":"aether-cni"}]`} {
			dir := t.TempDir()
			path := writeConf(t, dir, confName, []byte(flannelOnly))
			writeDurable(t, dir, garbage)

			var logs logCapture
			r := newReasserter(dir)
			r.log = logs.logger()
			r.check(ctx)

			assert.Equal(t, flannelOnly, string(mustRead(t, path)), "a garbage durable entry must never be chained (%q)", garbage)
			assert.Empty(t, r.entry)
			assert.False(t, r.ChainStatus().Chained)
			assert.Contains(t, logs.String(), "level=WARN", "an ignored durable entry must be logged loudly (%q)", garbage)
			assert.Contains(t, logs.String(), "the durable aether entry is unusable")
		}
	})

	t.Run("an observed entry wins over the durable one", func(t *testing.T) {
		dir := t.TempDir()
		live, err := conflist.Insert([]byte(liveEntry), []byte(flannelOnly))
		require.NoError(t, err)
		path := writeConf(t, dir, confName, live)
		writeDurable(t, dir, durableEntry)

		r := newReasserter(dir)
		r.check(ctx)
		require.Contains(t, string(r.entry), "observed.sock", "a healthy check must prime from what the node actually accepted")

		// The competing writer strips it: the repair must use the OBSERVED entry.
		// The durable file is a last resort, never a second source of truth.
		writeConf(t, dir, confName, []byte(flannelOnly))
		r.check(ctx)

		repaired := string(mustRead(t, path))
		assert.Contains(t, repaired, "observed.sock")
		assert.NotContains(t, repaired, "/run/aether/cni.sock", "the durable entry must not have been consulted")
		assert.Equal(t, string(live), repaired)
	})
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// logCapture is a slog sink a test can assert on.
type logCapture struct {
	buf bytes.Buffer
}

func (c *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&c.buf, nil))
}

func (c *logCapture) String() string { return c.buf.String() }

func TestStartWithoutConflistDir(t *testing.T) {
	// A dir that cannot be watched degrades to periodic re-checks and must not
	// fail the manager.
	r := &Reasserter{
		Dir:         filepath.Join(t.TempDir(), "absent"),
		Interval:    20 * time.Millisecond,
		SettleDelay: 10 * time.Millisecond,
		Log:         testLogger(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	assert.NoError(t, r.Start(ctx))
}
