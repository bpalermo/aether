package mesh

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCaptureMarksAreDisjoint pins the relationship between the two fwmarks the
// capture path uses (proposal 038).
//
// The passthrough mark is matched by an `accept` that runs AHEAD of the divert
// rule; the divert mark is set by that rule and matched by the prerouting
// `tproxy`. If the two were equal, every diverted packet would take the
// passthrough accept and never be captured. If either were a bitmask superset
// of the other, an nft rule written with a mask (`meta mark & 0xae7e`) would
// match both. Neither is tolerable and neither would fail loudly, so both are
// asserted here rather than left to a comment.
func TestCaptureMarksAreDisjoint(t *testing.T) {
	const pt, dv = uint32(CapturePassthroughFwMark), uint32(CaptureDivertFwMark)

	assert.NotEqual(t, pt, dv, "the passthrough accept would swallow every diverted packet")
	assert.NotZero(t, pt, "mark 0 is 'unmarked' and matches nothing")
	assert.NotZero(t, dv, "mark 0 is 'unmarked' and matches nothing")

	// Neither is a bitmask superset of the other: a masked match on one must
	// never also match the other.
	assert.NotEqual(t, pt, pt&dv, "passthrough mark is a subset of the divert mark's bits")
	assert.NotEqual(t, dv, pt&dv, "divert mark is a subset of the passthrough mark's bits")

	// Same family, so `nft list ruleset` shows them as related. Cosmetic, but
	// the comment on both constants claims it, so it is checked.
	assert.Equal(t, pt&0xfff0, dv&0xfff0, "the two marks should share the 0xae7x family")

	// kube-proxy's masquerade and drop marks (0x4000, 0x8000) live in the host
	// netns and ours never leave the pod netns, so this is belt-and-braces —
	// but a future mark that happened to be exactly one of those values would
	// be confusing to read in a capture, so keep clear of them.
	for _, kp := range []uint32{0x4000, 0x8000} {
		assert.NotEqual(t, kp, dv, "divert mark collides with a kube-proxy mark value")
	}
}

// TestCaptureDivertRouteTableIsAboveMain pins that the divert's policy-routing
// table is a real, fixed table id and not one of the kernel's reserved ones
// (0 unspec, 253 default, 254 main, 255 local): `ip route add ... table 254`
// would write the divert's `local default dev lo` into the MAIN table and
// blackhole the pod.
func TestCaptureDivertRouteTableIsAboveMain(t *testing.T) {
	const tbl = CaptureDivertRouteTable
	assert.Greater(t, tbl, 0)
	assert.Less(t, tbl, 253, "253-255 are the kernel's default/main/local tables")
}
