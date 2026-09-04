package cmd

import (
	"testing"

	"aethermesh.dev/agent/internal/cniconflist"
	"aethermesh.dev/agent/internal/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewCNIConflistReasserterKillSwitch pins what --cni-conflist-reassert=false
// produces. Everything downstream keys off this being a nil pointer.
func TestNewCNIConflistReasserterKillSwitch(t *testing.T) {
	prev := cfg.CNIConflistReassert
	t.Cleanup(func() { cfg.CNIConflistReassert = prev })

	cfg.CNIConflistReassert = false
	assert.Nil(t, newCNIConflistReasserter(), "the kill switch must build no re-asserter at all")

	cfg.CNIConflistReassert = true
	require.NotNil(t, newCNIConflistReasserter())
}

// TestChainStateOf is the regression test for the typed-nil trap. A nil
// *Reasserter assigned straight into an interface field is a NON-nil interface
// holding a nil pointer: the consumer's nil check fails, the kill switch stops
// working, and the first ChainStatus() call dereferences nothing. This is the
// one place that conversion happens, so this is where it is pinned.
func TestChainStateOf(t *testing.T) {
	var nilReasserter *cniconflist.Reasserter

	// The trap, demonstrated so nobody "simplifies" chainStateOf away: the direct
	// assignment produces an interface that is NOT nil. It has to be compared with
	// `== nil` rather than assert.NotNil, because testify reflects into the
	// interface and reports the nil pointer inside it as nil — which is precisely
	// the confusion that makes this bug so easy to write.
	var direct cniconflist.ChainState = nilReasserter
	assert.False(t, direct == nil, //nolint:staticcheck // the point of the test is that this is not nil
		"a nil *Reasserter in an interface is non-nil — this is why chainStateOf exists")

	assert.True(t, chainStateOf(nilReasserter) == nil, //nolint:staticcheck // ditto, compared as an interface
		"the kill switch must yield a truly nil ChainState")
	assert.NotNil(t, chainStateOf(&cniconflist.Reasserter{}))
}

// TestTaintRemoverKillSwitchIsSocketOnly wires the taint gate exactly the way
// run() does with the re-assert loop switched off, and asserts the chaining
// condition drops out. An operator who turns the loop off gets the pre-#667
// socket-only gate — not a node that can never be untainted because nothing is
// left to observe the conflist.
func TestTaintRemoverKillSwitchIsSocketOnly(t *testing.T) {
	prev := cfg.CNIConflistReassert
	t.Cleanup(func() { cfg.CNIConflistReassert = prev })
	cfg.CNIConflistReassert = false

	reasserter := newCNIConflistReasserter()
	tr := &node.TaintRemover{}
	if chain := chainStateOf(reasserter); chain != nil {
		tr.Chain = chain
	}

	// Compared as an interface, not via assert.Nil: the gate's own kill-switch
	// check is `r.Chain == nil`, so that is the comparison worth asserting.
	assert.True(t, tr.Chain == nil, //nolint:staticcheck // interface identity is the property under test
		"with the loop off the gate must carry no ChainState")
}

// TestTaintRemoverWiresTheReasserter is the other half: with the loop on, the
// gate must actually be holding it, or #667 is silently un-fixed.
func TestTaintRemoverWiresTheReasserter(t *testing.T) {
	prev := cfg.CNIConflistReassert
	t.Cleanup(func() { cfg.CNIConflistReassert = prev })
	cfg.CNIConflistReassert = true

	reasserter := newCNIConflistReasserter()
	require.NotNil(t, reasserter)

	tr := &node.TaintRemover{}
	if chain := chainStateOf(reasserter); chain != nil {
		tr.Chain = chain
	}

	require.NotNil(t, tr.Chain)
	// A re-asserter that has not run yet reports unknown, which the gate reads as
	// not-chained — so the taint is held through the boot window.
	assert.False(t, tr.Chain.ChainStatus().Observed)
}
