package plugin

import (
	"net/netip"
	"testing"

	"github.com/bpalermo/aether/cni/config"
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

// TestPodRedirectAll verifies the redirect-all decision precedence (proposal 022):
// an explicit per-pod annotation ("true"=force on / "false"=force off) overrides
// the node default; otherwise CaptureRedirectAllDefault (the M2-default flip)
// applies. This is the safety gate that keeps infra/opt-out pods off
// redirect-all even when the node default is on.
func TestPodRedirectAll(t *testing.T) {
	annoTrue := map[string]string{commonconstants.AnnotationCaptureRedirectAll: "true"}
	annoFalse := map[string]string{commonconstants.AnnotationCaptureRedirectAll: "false"}
	annoOther := map[string]string{"some.other/annotation": "true"}
	withAnno := func(m map[string]string) config.AetherConf {
		return config.AetherConf{RuntimeConfig: &config.RuntimeConfig{PodAnnotations: &m}}
	}

	tests := []struct {
		name string
		conf config.AetherConf
		want bool
	}{
		{name: "annotation true forces on (default off)", conf: withAnno(annoTrue), want: true},
		{
			name: "annotation false forces off even with default on",
			conf: config.AetherConf{CaptureRedirectAllDefault: true, RuntimeConfig: &config.RuntimeConfig{PodAnnotations: &annoFalse}},
			want: false,
		},
		{name: "no annotation, default on -> on", conf: config.AetherConf{CaptureRedirectAllDefault: true}, want: true},
		{name: "unrelated annotation, default off -> off", conf: withAnno(annoOther), want: false},
		{name: "nil pod annotations, default off -> off", conf: config.AetherConf{RuntimeConfig: &config.RuntimeConfig{}}, want: false},
		{name: "nil runtime config, default off -> off", conf: config.AetherConf{}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, podRedirectAll(tc.conf))
		})
	}
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

// TestPodExcludedOutboundPorts verifies the exclude-outbound-ports annotation
// (proposal 022 M2-default) parses into a deduplicated port list and degrades
// gracefully on malformed/blank/out-of-range entries.
func TestPodExcludedOutboundPorts(t *testing.T) {
	anno := func(v string) config.AetherConf {
		m := map[string]string{commonconstants.AnnotationCaptureExcludeOutboundPorts: v}
		return config.AetherConf{RuntimeConfig: &config.RuntimeConfig{PodAnnotations: &m}}
	}
	tests := []struct {
		name string
		conf config.AetherConf
		want []uint16
	}{
		{name: "single port", conf: anno("5432"), want: []uint16{5432}},
		{name: "list with whitespace", conf: anno("5432, 9000 ,80"), want: []uint16{5432, 9000, 80}},
		{name: "dedup", conf: anno("80,80,443"), want: []uint16{80, 443}},
		{name: "skips blank/zero/non-numeric/over-range", conf: anno("80,,0,foo,70000,443"), want: []uint16{80, 443}},
		{name: "empty value", conf: anno(""), want: nil},
		{name: "nil annotations", conf: config.AetherConf{RuntimeConfig: &config.RuntimeConfig{}}, want: nil},
		{name: "nil runtime config", conf: config.AetherConf{}, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, podExcludedOutboundPorts(tc.conf))
		})
	}
}

// TestPodExcludedOutboundIPRanges verifies the exclude-outbound-ip-ranges
// annotation (proposal 022 M2-default) parses into a deduplicated, network-masked,
// IPv4-only prefix list and degrades gracefully on malformed/blank/non-IPv4 entries.
func TestPodExcludedOutboundIPRanges(t *testing.T) {
	anno := func(v string) config.AetherConf {
		m := map[string]string{commonconstants.AnnotationCaptureExcludeOutboundIPRanges: v}
		return config.AetherConf{RuntimeConfig: &config.RuntimeConfig{PodAnnotations: &m}}
	}
	mk := func(s string) netip.Prefix { return netip.MustParsePrefix(s) }
	tests := []struct {
		name string
		conf config.AetherConf
		want []netip.Prefix
	}{
		{name: "single cidr", conf: anno("10.0.0.0/8"), want: []netip.Prefix{mk("10.0.0.0/8")}},
		{name: "bare addr -> /32", conf: anno("192.168.1.5"), want: []netip.Prefix{mk("192.168.1.5/32")}},
		{name: "list with whitespace", conf: anno("10.0.0.0/8, 192.168.1.0/24 ,172.16.0.0/12"), want: []netip.Prefix{mk("10.0.0.0/8"), mk("192.168.1.0/24"), mk("172.16.0.0/12")}},
		{name: "host bits masked off", conf: anno("10.1.2.3/8"), want: []netip.Prefix{mk("10.0.0.0/8")}},
		{name: "dedup after masking", conf: anno("10.1.2.3/8,10.4.5.6/8"), want: []netip.Prefix{mk("10.0.0.0/8")}},
		{name: "skips blank/garbage/ipv6", conf: anno("10.0.0.0/8,,foo,fd00::/8,300.0.0.0/8,192.168.1.5"), want: []netip.Prefix{mk("10.0.0.0/8"), mk("192.168.1.5/32")}},
		{name: "empty value", conf: anno(""), want: nil},
		{name: "nil annotations", conf: config.AetherConf{RuntimeConfig: &config.RuntimeConfig{}}, want: nil},
		{name: "nil runtime config", conf: config.AetherConf{}, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, podExcludedOutboundIPRanges(tc.conf))
		})
	}
}

// TestExcludeIPRangeAcceptExprs verifies the exclusion rule masks the destination
// IPv4 address to the prefix and accepts on a network match (so the redirect that
// follows never sees it). It matches no L4 proto — the carve-out is destination-based.
func TestExcludeIPRangeAcceptExprs(t *testing.T) {
	exprs := excludeIPRangeAcceptExprs(netip.MustParsePrefix("10.0.0.0/8"))
	require.Len(t, exprs, 4)
	// ip daddr payload (network header offset 16, len 4).
	require.IsType(t, &expr.Payload{}, exprs[0])
	assert.Equal(t, uint32(16), exprs[0].(*expr.Payload).Offset)
	assert.Equal(t, uint32(4), exprs[0].(*expr.Payload).Len)
	// reg1 &= /8 netmask.
	require.IsType(t, &expr.Bitwise{}, exprs[1])
	assert.Equal(t, []byte{0xff, 0x00, 0x00, 0x00}, exprs[1].(*expr.Bitwise).Mask)
	// reg1 == network (10.0.0.0).
	require.IsType(t, &expr.Cmp{}, exprs[2])
	assert.Equal(t, expr.CmpOpEq, exprs[2].(*expr.Cmp).Op)
	assert.Equal(t, []byte{10, 0, 0, 0}, exprs[2].(*expr.Cmp).Data)
	require.IsType(t, &expr.Verdict{}, exprs[3])
	assert.Equal(t, expr.VerdictAccept, exprs[3].(*expr.Verdict).Kind)
}

// TestBuiltinRedirectAllExcludedRanges verifies the always-on redirect-all carve-outs
// cover IPv4 link-local (the cloud metadata service) and multicast (proposal 022),
// which the /8 loopback mask in redirectAllTCPExprs does not exclude.
func TestBuiltinRedirectAllExcludedRanges(t *testing.T) {
	got := make(map[netip.Prefix]bool)
	for _, r := range builtinRedirectAllExcludedRanges {
		got[r] = true
	}
	assert.True(t, got[netip.MustParsePrefix("169.254.0.0/16")], "link-local must be excluded (IMDS 169.254.169.254)")
	assert.True(t, got[netip.MustParsePrefix("224.0.0.0/4")], "multicast must be excluded")
	// Sanity: the metadata IP falls inside the excluded link-local prefix.
	assert.True(t, netip.MustParsePrefix("169.254.0.0/16").Contains(netip.MustParseAddr("169.254.169.254")))
}

// TestPassthroughMarkAcceptExprs verifies the self-exclusion rule matches the
// proxy's fwmark (little-endian u32) and accepts, so SO_MARK'd passthrough egress
// bypasses the redirect (proposal 022 M2-default).
func TestPassthroughMarkAcceptExprs(t *testing.T) {
	exprs := passthroughMarkAcceptExprs(commonconstants.CapturePassthroughFwMark)
	require.Len(t, exprs, 3)
	require.IsType(t, &expr.Meta{}, exprs[0])
	assert.Equal(t, expr.MetaKeyMARK, exprs[0].(*expr.Meta).Key)
	require.IsType(t, &expr.Cmp{}, exprs[1])
	assert.Equal(t, expr.CmpOpEq, exprs[1].(*expr.Cmp).Op)
	// 0xae7e little-endian = 0x7e 0xae 0x00 0x00.
	assert.Equal(t, []byte{0x7e, 0xae, 0x00, 0x00}, exprs[1].(*expr.Cmp).Data)
	require.IsType(t, &expr.Verdict{}, exprs[2])
	assert.Equal(t, expr.VerdictAccept, exprs[2].(*expr.Verdict).Kind)
}

// TestExcludePortAcceptExprs verifies the exclusion rule matches TCP to the given
// dport and accepts (so the redirect that follows never sees it).
func TestExcludePortAcceptExprs(t *testing.T) {
	exprs := excludePortAcceptExprs(5432)
	require.Len(t, exprs, 5)
	require.IsType(t, &expr.Meta{}, exprs[0])
	assert.Equal(t, []byte{unix.IPPROTO_TCP}, exprs[1].(*expr.Cmp).Data)
	// dport payload (transport header offset 2) compared to 5432 big-endian.
	require.IsType(t, &expr.Payload{}, exprs[2])
	assert.Equal(t, uint32(2), exprs[2].(*expr.Payload).Offset)
	assert.Equal(t, []byte{0x15, 0x38}, exprs[3].(*expr.Cmp).Data) // 5432 = 0x1538
	require.IsType(t, &expr.Verdict{}, exprs[4])
	assert.Equal(t, expr.VerdictAccept, exprs[4].(*expr.Verdict).Kind)
}
