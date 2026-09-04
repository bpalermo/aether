package cniconflist

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staticChain is a ChainState frozen at a chosen status, so the dwell can be
// exercised by choosing Since rather than by sleeping.
type staticChain struct{ s ChainStatus }

func (c staticChain) ChainStatus() ChainStatus { return c.s }

func TestReadyChecker(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/readyz", nil)
	require.NoError(t, err)

	tests := []struct {
		name    string
		chain   ChainState
		wantErr bool
	}{
		{
			// The kill switch. --cni-conflist-reassert=false leaves nothing observing
			// the conflist, so this check must disappear along with the loop rather
			// than fence every node in the fleet.
			name:  "nil ChainState always passes",
			chain: nil,
		},
		{
			// Startup must never be delayed by this. The boot window is covered by the
			// taint gate, which HOLDS an existing taint until chaining is observed.
			name:  "unknown passes",
			chain: staticChain{},
		},
		{
			name:  "chained passes",
			chain: staticChain{s: ChainStatus{Observed: true, Chained: true, Since: time.Now().Add(-time.Hour)}},
		},
		{
			// A competing writer's in-place cp -f walks the file through a state that
			// parses as garbage. One check landing in that window must not fence the
			// node, so a fresh unchained observation still passes.
			name:  "unchained inside the dwell passes",
			chain: staticChain{s: ChainStatus{Observed: true, Since: time.Now().Add(-unchainedNotReady / 2)}},
		},
		{
			name:    "unchained past the dwell fails",
			chain:   staticChain{s: ChainStatus{Observed: true, Since: time.Now().Add(-unchainedNotReady - time.Second)}},
			wantErr: true,
		},
		{
			// Recovery needs no intervention: the next check that finds the entry back
			// flips Chained and the probe passes again on the very next poll.
			name:  "recovered after a long outage passes immediately",
			chain: staticChain{s: ChainStatus{Observed: true, Chained: true, Since: time.Now()}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ReadyChecker(tc.chain)(req)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "not chained")
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestUnchainedNotReadyDwell pins the relationship the constant exists for: at
// least two full re-check intervals, so a single check landing mid-rewrite can
// never fence a node.
func TestUnchainedNotReadyDwell(t *testing.T) {
	assert.GreaterOrEqual(t, unchainedNotReady, 2*DefaultCheckInterval)
}
