package plugin

import (
	"net"
	"testing"

	meshconst "github.com/bpalermo/aether/common/constants/mesh"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestDNSDNATExprs verifies the rule: <proto> + non-loopback daddr + dport 53 ->
// DNAT to hostIP:ProxyDNSResolverPort, for both UDP and TCP.
func TestDNSDNATExprs(t *testing.T) {
	hostIP := net.ParseIP("10.0.0.5").To4()
	port := uint16(meshconst.ProxyDNSResolverPort) // 18054
	for _, proto := range []byte{unix.IPPROTO_UDP, unix.IPPROTO_TCP} {
		exprs := dnsDNATExprs(proto, hostIP, port)
		require.Len(t, exprs, 10)

		assert.Equal(t, []byte{proto}, exprs[1].(*expr.Cmp).Data, "l4proto match")
		// loopback exclusion
		assert.Equal(t, []byte{0xff, 0x00, 0x00, 0x00}, exprs[3].(*expr.Bitwise).Mask)
		assert.Equal(t, expr.CmpOpNeq, exprs[4].(*expr.Cmp).Op)
		assert.Equal(t, []byte{127, 0, 0, 0}, exprs[4].(*expr.Cmp).Data)
		// dport 53
		assert.Equal(t, []byte{0x00, 0x35}, exprs[6].(*expr.Cmp).Data, "dport 53")
		// DNAT to hostIP:port
		assert.Equal(t, []byte(hostIP), exprs[7].(*expr.Immediate).Data, "dnat IP")
		assert.Equal(t, []byte{0x46, 0x86}, exprs[8].(*expr.Immediate).Data, "dnat port 18054")
		nat := exprs[9].(*expr.NAT)
		assert.Equal(t, expr.NATTypeDestNAT, nat.Type)
		assert.Equal(t, uint32(1), nat.RegAddrMin)
		assert.Equal(t, uint32(2), nat.RegProtoMin)
	}
}
