package spire

import (
	"context"
	"testing"
	"time"

	"aethermesh.dev/common/spire/spiretest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewSourceWithTimeout_FollowsTheFirstSVIDClosely is issue #1123's own-SVID
// term. A restarted workload asks the SPIRE agent for its SVID before the agent
// can attest it (the new pod is not in the kubelet's list yet), and go-spiffe's
// default Workload API retry is LINEAR in whole seconds (1 s, 2 s, 3 s, ...): an
// SVID that became available at +1.5 s was picked up at +3 s, one at +4.5 s at
// +6 s. On kind the node agent's SVID wait was exactly 1.01 / 2.02 / 4.04 / 7.05
// s across 16 restarts, i.e. the retry ladder, not SPIRE; and the node agent
// opens no xDS socket until it has its SVID (#740), so every one of those
// seconds is spent with the node's proxy on no ADS stream.
//
// The retry is now 200 ms steps capped at 500 ms against the node-local socket,
// so the source follows the SVID within half a second.
func TestNewSourceWithTimeout_FollowsTheFirstSVIDClosely(t *testing.T) {
	for i := range 3 {
		fake, socket := spiretest.Start(t, "spiffe://"+spiretest.TrustDomain+"/ns/aether-system/sa/aether-agent")

		done := make(chan error, 1)
		go func() {
			src, err := NewSourceWithTimeout(context.Background(), socket, 30*time.Second)
			if src != nil {
				_ = src.Close()
			}
			done <- err
		}()

		// SPIRE starts issuing between two of the old ladder's attempts.
		time.Sleep(1500 * time.Millisecond)
		start := time.Now()
		fake.StartServing()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatalf("run %d: no source after SPIRE started serving", i)
		}
		took := time.Since(start)
		assert.Less(t, took, 700*time.Millisecond,
			"run %d: the first SVID must be picked up within one short retry step, not on a whole-second ladder (fetches: %d)", i, fake.Fetches())
	}
}

// TestFirstSVIDBackoff pins the retry ladder: short steps, a low cap, and a
// Reset that returns to the first step (go-spiffe resets after every
// successful update, so a later SPIRE agent restart starts from the bottom).
func TestFirstSVIDBackoff(t *testing.T) {
	b := firstSVIDBackoffStrategy{}.NewBackoff()
	var got []time.Duration
	for range 6 {
		got = append(got, b.Next())
	}
	assert.Equal(t, []time.Duration{
		200 * time.Millisecond, 400 * time.Millisecond, 500 * time.Millisecond,
		500 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond,
	}, got)
	b.Reset()
	assert.Equal(t, 200*time.Millisecond, b.Next())
}
