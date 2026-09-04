package cniconflist

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChainStatusNeverStarted pins the contract the taint gate and the readiness
// check depend on: a Reasserter that has never run reports UNKNOWN, not
// "chained". Getting this backwards would make the boot window — the moment a
// node is least likely to be able to mesh anything — look healthy.
func TestChainStatusNeverStarted(t *testing.T) {
	r := &Reasserter{Dir: t.TempDir(), Log: testLogger()}

	s := r.ChainStatus()
	assert.False(t, s.Observed, "a Reasserter that never checked must report unknown")
	assert.False(t, s.Chained)
	assert.True(t, s.Since.IsZero())
}

// TestChainStatusAfterCheck walks every terminal path of check() and asserts the
// published state matches what the node can actually do. Because check() repairs
// synchronously, Chained=false after a check means UNREPAIRABLE right now.
func TestChainStatusAfterCheck(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		// content is what sits in the conflist before the check; nil means "write
		// the correctly chained conflist".
		content []byte
		// noFile writes nothing at all, leaving an empty config dir.
		noFile bool
		// primed seeds a last-known-good entry, as an earlier check would have.
		primed      bool
		wantChained bool
	}{
		{
			name:        "chained conflist publishes chained",
			primed:      false,
			wantChained: true,
		},
		{
			// The "agent restarted into an already-stripped conflist" state: nothing
			// primed the entry cache, so the loop cannot repair and this node will
			// keep creating pods outside the mesh until cni-install runs again.
			name:        "stripped conflist with no known-good entry publishes unchained",
			content:     []byte(flannelOnly),
			primed:      false,
			wantChained: false,
		},
		{
			// The repair path: the loop puts the entry back inside the same check,
			// so the state it publishes is the POST-repair one.
			name:        "stripped conflist the loop can repair publishes chained",
			content:     []byte(flannelOnly),
			primed:      true,
			wantChained: true,
		},
		{
			name:        "conflist with no primary CNI plugin publishes unchained",
			content:     []byte(`{"name":"cbr0","cniVersion":"0.3.1","plugins":[]}`),
			primed:      true,
			wantChained: false,
		},
		{
			name:        "unparseable conflist publishes unchained",
			content:     []byte(`{not json`),
			primed:      true,
			wantChained: false,
		},
		{
			name:        "empty config dir publishes unchained",
			noFile:      true,
			primed:      true,
			wantChained: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if !tc.noFile {
				content := tc.content
				if content == nil {
					content = chained(t)
				}
				writeConf(t, dir, confName, content)
			}

			r := newReasserter(dir)
			if tc.primed {
				r.entry = []byte(aetherEntry)
			}
			r.check(ctx)

			s := r.ChainStatus()
			require.True(t, s.Observed, "a completed check must always publish")
			assert.Equal(t, tc.wantChained, s.Chained)
			assert.False(t, s.Since.IsZero(), "an observed status must carry a Since")
		})
	}
}

// TestChainStatusSince covers the reason Since exists at all: consumers dwell on
// it before acting, so it must mark when the state was FIRST seen, not when it
// was last re-confirmed. A Since that reset on every 60s re-check would restart
// the dwell forever and the readiness check would never fire.
func TestChainStatusSince(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := writeConf(t, dir, confName, chained(t))

	r := newReasserter(dir)
	r.check(ctx)
	first := r.ChainStatus()
	require.True(t, first.Chained)

	// Re-checking the same state must not move Since.
	r.check(ctx)
	assert.Equal(t, first.Since, r.ChainStatus().Since, "Since must be preserved across an unchanged check")

	// A transition must move it. The conflist is removed outright rather than
	// merely stripped, because the first check primed the entry cache from it —
	// a stripped file would just be repaired inside the same check and never
	// publish unchained at all.
	require.NoError(t, os.Remove(path))
	r.check(ctx)
	stripped := r.ChainStatus()
	require.False(t, stripped.Chained)
	assert.False(t, stripped.Since.Before(first.Since), "Since must not go backwards on a transition")
	assert.NotEqual(t, first, stripped)

	// And back again, so the state is not sticky.
	writeConf(t, dir, confName, chained(t))
	r.check(ctx)
	assert.True(t, r.ChainStatus().Chained)
}

// Reasserter must satisfy ChainState — the whole point of A1 is that the taint
// gate can be handed one.
var _ ChainState = (*Reasserter)(nil)
