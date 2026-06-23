package plugin

import (
	"testing"

	commonconstants "github.com/bpalermo/aether/common/constants"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestDNSRedirectExprs verifies the rule: <proto> + non-loopback daddr + dport 53 ->
// redirect to the mesh-DNS port, for both UDP and TCP.
func TestDNSRedirectExprs(t *testing.T) {
	dnsPort := uint16(commonconstants.ProxyDNSCapturePort) // 18053
	for _, proto := range []byte{unix.IPPROTO_UDP, unix.IPPROTO_TCP} {
		exprs := dnsRedirectExprs(proto, dnsPort)
		require.Len(t, exprs, 9)

		assert.Equal(t, []byte{proto}, exprs[1].(*expr.Cmp).Data, "l4proto match")
		// loopback exclusion
		assert.Equal(t, []byte{0xff, 0x00, 0x00, 0x00}, exprs[3].(*expr.Bitwise).Mask)
		assert.Equal(t, expr.CmpOpNeq, exprs[4].(*expr.Cmp).Op)
		assert.Equal(t, []byte{127, 0, 0, 0}, exprs[4].(*expr.Cmp).Data)
		// dport 53 (big-endian) -> redirect 18053
		assert.Equal(t, []byte{0x00, 0x35}, exprs[6].(*expr.Cmp).Data, "dport 53")
		assert.Equal(t, []byte{0x46, 0x85}, exprs[7].(*expr.Immediate).Data, "redirect to 18053")
		assert.IsType(t, &expr.Redir{}, exprs[8])
	}
}
