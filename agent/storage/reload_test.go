package storage

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func values(in []*wrapperspb.StringValue) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, v.GetValue())
	}
	sort.Strings(out)
	return out
}

// TestReload_ReportsAnotherWritersChanges is the surge-handoff case (proposal
// 041): this process loaded the directory, then ANOTHER process (the old agent,
// which still owned the node) added, rewrote and removed records. Reload must
// report exactly that, with the removed record's PREVIOUS value — the caller
// tears down what it built from it — and leave the view equal to the disk.
func TestReload_ReportsAnotherWritersChanges(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	ours := NewCachedLocalStorage[*wrapperspb.StringValue](dir, newStringValue)
	require.NoError(t, ours.AddResource(ctx, "kept", wrapperspb.String("kept")))
	require.NoError(t, ours.AddResource(ctx, "changed", wrapperspb.String("before")))
	require.NoError(t, ours.AddResource(ctx, "deleted", wrapperspb.String("deleted-value")))

	// The other writer, through its own storage over the same directory.
	theirs := NewCachedLocalStorage[*wrapperspb.StringValue](dir, newStringValue)
	require.NoError(t, theirs.Initialize(ctx))
	require.NoError(t, theirs.AddResource(ctx, "added", wrapperspb.String("added")))
	require.NoError(t, theirs.AddResource(ctx, "changed", wrapperspb.String("after")))
	require.NoError(t, theirs.RemoveResource(ctx, "deleted"))

	d, err := ours.Reload(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"added"}, values(d.Added))
	assert.Equal(t, []string{"after"}, values(d.Updated))
	assert.Equal(t, []string{"deleted-value"}, values(d.Removed), "a removal carries the value the caller built from")
	assert.False(t, d.Empty())

	all, err := ours.GetAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"added", "after", "kept"}, values(all))

	again, err := ours.Reload(ctx)
	require.NoError(t, err)
	assert.True(t, again.Empty(), "a second reload with no writer in between finds nothing")
}

// TestReload_ACorruptFileLeavesTheViewAlone: a half-applied diff is worse than
// the previous consistent view, so a decode failure changes nothing.
func TestReload_ACorruptFileLeavesTheViewAlone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := NewCachedLocalStorage[*wrapperspb.StringValue](dir, newStringValue)
	require.NoError(t, s.AddResource(ctx, "a", wrapperspb.String("a")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.json"), []byte("{not json"), 0o600))

	_, err := s.Reload(ctx)
	require.Error(t, err)
	all, err := s.GetAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, values(all))
}

// TestReload_IgnoresWhatLoadAllIgnores keeps Reload's notion of a record the
// same as the initial load's: subdirectories (the observed-upstreams state/
// dir) and non-JSON files are not pod records.
func TestReload_IgnoresWhatLoadAllIgnores(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "state"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state", "x.json"), []byte(`"x"`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o600))

	s := NewCachedLocalStorage[*wrapperspb.StringValue](dir, newStringValue)
	require.NoError(t, s.Initialize(ctx))
	d, err := s.Reload(ctx)
	require.NoError(t, err)
	assert.True(t, d.Empty())
}
