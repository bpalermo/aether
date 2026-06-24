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
// daddr + dport meshPort -> redirect to udpCapturePort. The transport header dport
// is at the same offset (2) for UDP as for TCP, so the expression shape is identical
// except for the l4proto byte and the redirect target port.
func TestCaptureRedirectExprs_UDP(t *testing.T) {
	mesh := uint16(commonconstants.ProxyOutboundPort)  // 18081
	cap := uint16(commonconstants.ProxyUDPCapturePort) // 18002
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

	// redirect to 18002
	require.IsType(t, &expr.Immediate{}, exprs[7])
	assert.Equal(t, []byte{0x46, 0x52}, exprs[7].(*expr.Immediate).Data) // 18002
	assert.IsType(t, &expr.Redir{}, exprs[8])
}
