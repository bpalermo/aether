package install

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aethermesh.dev/cni/conflist"
	"github.com/containernetworking/cni/libcni"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discardLogger is the logger for the tests that assert on files, not on logs.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// syncBuffer is a mutex-guarded sink: the file watcher logs from its own
// goroutine, so the capture buffer must tolerate a concurrent write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

type logRecord struct {
	Level    string `json:"level"`
	Message  string `json:"message"`
	Filepath string `json:"filepath"`
	Error    string `json:"error"`
}

// captureLogger returns a logger shaped like the installer's real one (the
// common/log JSON handler renames slog's "msg" key to "message") plus a reader
// for the records it emitted.
func captureLogger() (*slog.Logger, func(*testing.T) []logRecord) {
	sink := &syncBuffer{}
	handler := slog.NewJSONHandler(sink, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.MessageKey {
				a.Key = "message"
			}
			return a
		},
	})
	read := func(t *testing.T) []logRecord {
		t.Helper()
		sink.mu.Lock()
		defer sink.mu.Unlock()
		var records []logRecord
		for line := range strings.SplitSeq(strings.TrimSpace(sink.buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec logRecord
			require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line is not JSON: %s", line)
			records = append(records, rec)
		}
		return records
	}
	return slog.New(handler), read
}

// findRecord returns the first captured record carrying the given message.
func findRecord(records []logRecord, message string) (logRecord, bool) {
	for _, rec := range records {
		if rec.Message == message {
			return rec, true
		}
	}
	return logRecord{}, false
}

// The conflist mutation itself (insert/parse/filename resolution) is shared with
// the agent's re-assert loop and tested in aethermesh.dev/cni/conflist. What is
// installer-specific — waiting for the primary CNI config to appear — is here.

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

func TestGetCNIConfigFilepath(t *testing.T) {
	ctx := context.Background()
	validList := `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`

	t.Run("returns the named file when it exists", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "10-flannel.conflist", validList)
		got, err := getCNIConfigFilepath(ctx, discardLogger(), "10-flannel.conflist", dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "10-flannel.conflist"), got)
	})

	t.Run("falls back from a missing .conf to its .conflist sibling", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "10-flannel.conflist", validList)
		got, err := getCNIConfigFilepath(ctx, discardLogger(), "10-flannel.conf", dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "10-flannel.conflist"), got)
	})

	t.Run("falls back from a missing .conflist to its .conf sibling", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "10-mynet.conf", `{"name":"mynet","type":"bridge"}`)
		got, err := getCNIConfigFilepath(ctx, discardLogger(), "10-mynet.conflist", dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "10-mynet.conf"), got)
	})

	t.Run("auto-discovers the config when no name is given", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "10-flannel.conflist", validList)
		got, err := getCNIConfigFilepath(ctx, discardLogger(), "", dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "10-flannel.conflist"), got)
	})
}

// TestWriteCNIConfigPersistsTheDurableEntry covers issue #680: cni-install leaves
// a durable copy of the entry it just chained, so the agent's re-assert loop can
// prime from disk when a competing writer beats it to the first check.
func TestWriteCNIConfigPersistsTheDurableEntry(t *testing.T) {
	ctx := context.Background()
	rendered := []byte(`{"name":"aether","cniVersion":"0.0.1","type":"aether-cni","agentCNIPath":"/run/aether/cni.sock"}`)

	newDir := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		writeFile(t, dir, "10-flannel.conflist", `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`)
		return dir
	}

	t.Run("writes the entry beside the conflist", func(t *testing.T) {
		dir := newDir(t)
		_, err := writeCNIConfig(ctx, discardLogger(), rendered, &InstallerConfig{MountedCNINetDir: dir})
		require.NoError(t, err)

		durable, err := os.ReadFile(conflist.EntryPath(dir))
		require.NoError(t, err)

		// The durable copy is exactly the entry that ended up chained, so a
		// re-assert from it reproduces this install byte for byte.
		merged, err := os.ReadFile(filepath.Join(dir, "10-flannel.conflist"))
		require.NoError(t, err)
		chain, err := conflist.Parse(merged)
		require.NoError(t, err)
		chained, present, err := chain.AetherEntry()
		require.NoError(t, err)
		require.True(t, present)
		assert.JSONEq(t, string(chained), string(durable))

		// And it is a valid priming input for the re-assert loop.
		parsed, err := conflist.ParseEntry(durable)
		require.NoError(t, err)
		assert.JSONEq(t, string(chained), string(parsed))
	})

	t.Run("is written with the conflist's mode", func(t *testing.T) {
		dir := newDir(t)
		_, err := writeCNIConfig(ctx, discardLogger(), rendered, &InstallerConfig{MountedCNINetDir: dir})
		require.NoError(t, err)

		info, err := os.Stat(conflist.EntryPath(dir))
		require.NoError(t, err)
		assert.Equal(t, confMode, info.Mode().Perm())
	})

	// The whole point of the filename: no CNI config loader may ever pick the
	// durable entry up as a network config of its own. libcni selects by
	// filepath.Ext against a caller-supplied extension list, and the widest list
	// in use anywhere is containerd go-cni's {.conf,.conflist,.json} — a superset
	// of kubelet's and of libcni's deprecated LoadConf. Ext(".aether-cni-entry")
	// matches none of them.
	t.Run("is invisible to every CNI config loader", func(t *testing.T) {
		dir := newDir(t)
		_, err := writeCNIConfig(ctx, discardLogger(), rendered, &InstallerConfig{MountedCNINetDir: dir})
		require.NoError(t, err)

		names, err := conflist.ConfigFilenames(dir)
		require.NoError(t, err)
		assert.Equal(t, []string{"10-flannel.conflist"}, names)

		for _, exts := range [][]string{
			{".conf", ".conflist"},          // aether, kubelet
			{".conf", ".conflist", ".json"}, // containerd go-cni
			{".conf", ".json"},              // libcni LoadConf
		} {
			files, err := libcni.ConfFiles(dir, exts)
			require.NoError(t, err)
			assert.NotContains(t, files, conflist.EntryPath(dir), "loader extensions %v must not select the durable entry", exts)
		}

		active, err := conflist.ActivePath(dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "10-flannel.conflist"), active)
	})
}

// TestWriteCNIConfigLogsThroughTheInstallerLogger covers issue #696: the install
// used to log these lines through controller-runtime's global logger, which
// nothing in cni/ ever binds, so the two confirmations an operator greps the
// cni-install init container for never reached stdout.
func TestWriteCNIConfigLogsThroughTheInstallerLogger(t *testing.T) {
	ctx := context.Background()
	rendered := []byte(`{"name":"aether","cniVersion":"0.0.1","type":"aether-cni","agentCNIPath":"/run/aether/cni.sock"}`)

	dir := t.TempDir()
	writeFile(t, dir, "10-flannel.conflist", `{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`)

	logger, records := captureLogger()
	_, err := writeCNIConfig(ctx, logger, rendered, &InstallerConfig{MountedCNINetDir: dir})
	require.NoError(t, err)

	emitted := records(t)

	wrote, ok := findRecord(emitted, "Wrote CNI config")
	require.True(t, ok, "the CNI config confirmation must reach the installer's logger; got %+v", emitted)
	assert.Equal(t, "INFO", wrote.Level)
	assert.Equal(t, filepath.Join(dir, "10-flannel.conflist"), wrote.Filepath)

	durable, ok := findRecord(emitted, "Wrote the durable aether CNI entry")
	require.True(t, ok, "the durable-entry confirmation must reach the installer's logger; got %+v", emitted)
	assert.Equal(t, "INFO", durable.Level)
	assert.Equal(t, conflist.EntryPath(dir), durable.Filepath)
}

// TestWriteDurableEntryLogsItsFailures pins the other half of #696: the durable
// entry's write is deliberately swallowed (losing the re-assert safety net must
// never fail an install that already meshed the node), so the ERROR line is the
// operator's only signal that it did not land.
func TestWriteDurableEntryLogsItsFailures(t *testing.T) {
	ctx := context.Background()
	rendered := []byte(`{"name":"aether","cniVersion":"0.0.1","type":"aether-cni","agentCNIPath":"/run/aether/cni.sock"}`)
	merged, err := conflist.Insert(rendered, []byte(`{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`))
	require.NoError(t, err)

	t.Run("logs at ERROR when the entry cannot be written", func(t *testing.T) {
		// A regular file standing in for the conf dir: the write fails with
		// ENOTDIR for root and non-root alike, unlike a mode-based denial.
		notADir := filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o644))

		logger, records := captureLogger()
		writeDurableEntry(ctx, logger, notADir, merged)

		rec, ok := findRecord(records(t), "failed to write the durable aether CNI entry; the re-assert loop cannot prime from disk")
		require.True(t, ok, "the failed durable-entry write must be logged")
		assert.Equal(t, "ERROR", rec.Level)
		assert.Equal(t, conflist.EntryPath(notADir), rec.Filepath)
		assert.NotEmpty(t, rec.Error, "the write error must be carried on the record")
	})

	t.Run("logs at ERROR when the entry cannot be extracted", func(t *testing.T) {
		logger, records := captureLogger()
		writeDurableEntry(ctx, logger, t.TempDir(), []byte(`{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel"}]}`))

		rec, ok := findRecord(records(t), "failed to extract the aether entry from the CNI config just written; the re-assert loop cannot prime from disk")
		require.True(t, ok, "a conflist without an aether entry must be logged")
		assert.Equal(t, "ERROR", rec.Level)
		assert.NotEmpty(t, rec.Error, "the extraction error must be carried on the record")
	})
}
