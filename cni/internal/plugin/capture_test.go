package plugin

import (
	"testing"

	commonconstants "github.com/bpalermo/aether/common/constants"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestCaptureRedirectExprs_TCP verifies the nft rule for TCP: tcp + non-loopback
// daddr + dport meshPort -> redirect to tcpCapturePort. A wrong loopback
// exclusion or port would silently break the explicit fast-lane or fail to capture.
func TestCaptureRedirectExprs_TCP(t *testing.T) {
	mesh := uint16(commonconstants.ProxyOutboundPort) // 18081
	cap := uint16(commonconstants.ProxyCapturePort)   // 18001
	exprs := captureRedirectExprs(unix.IPPROTO_TCP, mesh, cap)
	require.Len(t, exprs, 9)

	// l4proto == tcp
	require.IsType(t, &expr.Meta{}, exprs[0])
	assert.Equal(t, expr.MetaKeyL4PROTO, exprs[0].(*expr.Meta).Key)
	require.IsType(t, &expr.Cmp{}, exprs[1])
	assert.Equal(t, []byte{unix.IPPROTO_TCP}, exprs[1].(*expr.Cmp).Data)

	// loopback exclusion: mask /8 then NEQ 127.0.0.0
	require.IsType(t, &expr.Bitwise{}, exprs[3])
	assert.Equal(t, []byte{0xff, 0x00, 0x00, 0x00}, exprs[3].(*expr.Bitwise).Mask)
	require.IsType(t, &expr.Cmp{}, exprs[4])
	assert.Equal(t, expr.CmpOpNeq, exprs[4].(*expr.Cmp).Op)
	assert.Equal(t, []byte{127, 0, 0, 0}, exprs[4].(*expr.Cmp).Data)

	// tcp dport == 18081 (big-endian)
	require.IsType(t, &expr.Cmp{}, exprs[6])
	assert.Equal(t, expr.CmpOpEq, exprs[6].(*expr.Cmp).Op)
	assert.Equal(t, []byte{0x46, 0xA1}, exprs[6].(*expr.Cmp).Data) // 18081

	// redirect to 18001
	require.IsType(t, &expr.Immediate{}, exprs[7])
	assert.Equal(t, []byte{0x46, 0x51}, exprs[7].(*expr.Immediate).Data) // 18001
	assert.IsType(t, &expr.Redir{}, exprs[8])
}

// TestCaptureRedirectExprs_UDP verifies the nft rule for UDP: udp + non-loopback
// daddr + dport meshPort -> redirect to ProxyCapturePort (same port as TCP).
// The transport header dport is at the same offset (2) for UDP as for TCP, so
// the expression shape is identical except for the l4proto byte. Both TCP and
// UDP redirect to the same port number (18001) — the kernel differentiates them
// at the socket layer by protocol, so the listeners do not collide.
func TestCaptureRedirectExprs_UDP(t *testing.T) {
	mesh := uint16(commonconstants.ProxyOutboundPort) // 18081
	cap := uint16(commonconstants.ProxyCapturePort)   // 18001 (shared with TCP)
	exprs := captureRedirectExprs(unix.IPPROTO_UDP, mesh, cap)
	require.Len(t, exprs, 9)

	// l4proto == udp
	require.IsType(t, &expr.Meta{}, exprs[0])
	assert.Equal(t, expr.MetaKeyL4PROTO, exprs[0].(*expr.Meta).Key)
	require.IsType(t, &expr.Cmp{}, exprs[1])
	assert.Equal(t, []byte{unix.IPPROTO_UDP}, exprs[1].(*expr.Cmp).Data)

	// loopback exclusion unchanged
	require.IsType(t, &expr.Bitwise{}, exprs[3])
	assert.Equal(t, []byte{0xff, 0x00, 0x00, 0x00}, exprs[3].(*expr.Bitwise).Mask)
	require.IsType(t, &expr.Cmp{}, exprs[4])
	assert.Equal(t, expr.CmpOpNeq, exprs[4].(*expr.Cmp).Op)
	assert.Equal(t, []byte{127, 0, 0, 0}, exprs[4].(*expr.Cmp).Data)

	// udp dport == 18081 (big-endian)
	require.IsType(t, &expr.Cmp{}, exprs[6])
	assert.Equal(t, expr.CmpOpEq, exprs[6].(*expr.Cmp).Op)
	assert.Equal(t, []byte{0x46, 0xA1}, exprs[6].(*expr.Cmp).Data) // 18081

	// redirect to 18001 (ProxyCapturePort, shared with TCP listener)
	require.IsType(t, &expr.Immediate{}, exprs[7])
	assert.Equal(t, []byte{0x46, 0x51}, exprs[7].(*expr.Immediate).Data) // 18001
	assert.IsType(t, &expr.Redir{}, exprs[8])
}

// --- Redirect-all (spike/M2a) rule expression tests ---

// TestConntrackEstablishedAcceptExprs verifies the conntrack-bypass rule:
// ct state {established,related} → accept. This is the loop-prevention rule
// that prevents the redirect-all from trying to re-REDIRECT response traffic.
func TestConntrackEstablishedAcceptExprs(t *testing.T) {
	exprs := conntrackEstablishedAcceptExprs()
	// Ct load + Bitwise AND + Cmp NEQ 0 + Accept = 4 expressions.
	require.Len(t, exprs, 4)

	// First: load ct state into reg1.
	require.IsType(t, &expr.Ct{}, exprs[0])
	ct := exprs[0].(*expr.Ct)
	assert.Equal(t, expr.CtKeySTATE, ct.Key)
	assert.Equal(t, uint32(1), ct.Register)

	// Second: Bitwise AND with established|related mask (0x6).
	require.IsType(t, &expr.Bitwise{}, exprs[1])
	bw := exprs[1].(*expr.Bitwise)
	assert.Equal(t, []byte{0x06, 0x00, 0x00, 0x00}, bw.Mask)

	// Third: NEQ 0 (state bits set means established or related).
	require.IsType(t, &expr.Cmp{}, exprs[2])
	cmp := exprs[2].(*expr.Cmp)
	assert.Equal(t, expr.CmpOpNeq, cmp.Op)
	assert.Equal(t, []byte{0x00, 0x00, 0x00, 0x00}, cmp.Data)

	// Fourth: Accept verdict.
	require.IsType(t, &expr.Verdict{}, exprs[3])
	assert.Equal(t, expr.VerdictAccept, exprs[3].(*expr.Verdict).Kind)
}

// TestSkipCaptureSelfExprs verifies the rule that prevents re-redirecting
// connections already destined for the capture listener port itself.
func TestSkipCaptureSelfExprs(t *testing.T) {
	cap := uint16(commonconstants.ProxyCapturePort) // 18001
	exprs := skipCaptureSelfExprs(cap)
	// l4proto==tcp + dport==capturePort + accept = 5 expressions.
	require.Len(t, exprs, 5)

	// l4proto == tcp
	require.IsType(t, &expr.Meta{}, exprs[0])
	assert.Equal(t, expr.MetaKeyL4PROTO, exprs[0].(*expr.Meta).Key)
	require.IsType(t, &expr.Cmp{}, exprs[1])
	assert.Equal(t, []byte{unix.IPPROTO_TCP}, exprs[1].(*expr.Cmp).Data)

	// tcp dport == 18001
	require.IsType(t, &expr.Payload{}, exprs[2])
	require.IsType(t, &expr.Cmp{}, exprs[3])
	cmp := exprs[3].(*expr.Cmp)
	assert.Equal(t, expr.CmpOpEq, cmp.Op)
	assert.Equal(t, []byte{0x46, 0x51}, cmp.Data) // 18001 big-endian

	// Accept
	require.IsType(t, &expr.Verdict{}, exprs[4])
	assert.Equal(t, expr.VerdictAccept, exprs[4].(*expr.Verdict).Kind)
}

// TestRedirectAllTCPExprs verifies the broad-redirect rule: all non-loopback TCP
// is redirected to capturePort, with NO dport constraint (unlike the scoped rule).
func TestRedirectAllTCPExprs(t *testing.T) {
	cap := uint16(commonconstants.ProxyCapturePort) // 18001
	exprs := redirectAllTCPExprs(cap)
	// l4proto==tcp + loopback-excl (3 exprs) + redirect(2 exprs) = 7 total.
	require.Len(t, exprs, 7)

	// l4proto == tcp
	require.IsType(t, &expr.Meta{}, exprs[0])
	assert.Equal(t, expr.MetaKeyL4PROTO, exprs[0].(*expr.Meta).Key)
	require.IsType(t, &expr.Cmp{}, exprs[1])
	assert.Equal(t, []byte{unix.IPPROTO_TCP}, exprs[1].(*expr.Cmp).Data)

	// loopback exclusion: mask /8 then NEQ 127.0.0.0
	require.IsType(t, &expr.Payload{}, exprs[2])
	require.IsType(t, &expr.Bitwise{}, exprs[3])
	assert.Equal(t, []byte{0xff, 0x00, 0x00, 0x00}, exprs[3].(*expr.Bitwise).Mask)
	require.IsType(t, &expr.Cmp{}, exprs[4])
	assert.Equal(t, expr.CmpOpNeq, exprs[4].(*expr.Cmp).Op)
	assert.Equal(t, []byte{127, 0, 0, 0}, exprs[4].(*expr.Cmp).Data)

	// NO dport match between loopback and redirect — that's the key difference
	// from captureRedirectExprs which has 9 expressions (includes dport check).

	// redirect to capturePort
	require.IsType(t, &expr.Immediate{}, exprs[5])
	assert.Equal(t, []byte{0x46, 0x51}, exprs[5].(*expr.Immediate).Data) // 18001
	require.IsType(t, &expr.Redir{}, exprs[6])
}

// TestRedirectAllVsScopedDifference verifies that redirectAllTCPExprs has FEWER
// expressions than captureRedirectExprs: the scoped rule adds a dport match (2
// expressions: Payload + Cmp) that the redirect-all rule omits.
func TestRedirectAllVsScopedDifference(t *testing.T) {
	cap := uint16(commonconstants.ProxyCapturePort)
	mesh := uint16(commonconstants.ProxyOutboundPort)
	scoped := captureRedirectExprs(unix.IPPROTO_TCP, mesh, cap)
	broad := redirectAllTCPExprs(cap)
	// Scoped has 9, broad has 7: 2 fewer (the dport Payload + dport Cmp).
	assert.Equal(t, len(scoped)-2, len(broad), "redirect-all omits the dport match expressions")
}
