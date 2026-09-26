package plugin

import (
	"net/netip"
	"testing"

	"aethermesh.dev/cni/config"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	meshconst "aethermesh.dev/common/constants/mesh"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// ruleKind classifies one OUTPUT-chain rule by what it does, so the order test
// can reason about the chain without matching on expression counts.
type ruleKind int

const (
	kindAccept ruleKind = iota
	kindMark
	kindOther
)

func classify(exprs []expr.Any) ruleKind {
	last := exprs[len(exprs)-1]
	if v, ok := last.(*expr.Verdict); ok && v.Kind == expr.VerdictAccept {
		return kindAccept
	}
	if m, ok := last.(*expr.Meta); ok && m.Key == expr.MetaKeyMARK && m.SourceRegister {
		return kindMark
	}
	return kindOther
}

// isCtDirectionReply reports whether a rule is the `ct direction reply accept`.
func isCtDirectionReply(exprs []expr.Any) bool {
	if len(exprs) != 3 {
		return false
	}
	ct, ok := exprs[0].(*expr.Ct)
	return ok && ct.Key == expr.CtKeyDIRECTION
}

// TestDivertRuleOrder is THE gate for the divert chain (proposal 038): the
// `ct direction reply accept` must precede every mark rule. A route chain sees
// every locally-generated packet including the pod's own server replies; a
// reply that reaches a mark rule under redirect-all is diverted to lo, hits the
// 18001 LISTEN socket, and is reset -- every inbound connection to every pod
// dies. The spike (e2e/spike/tproxy-phase0b.py, arm S1) proved this on a node;
// this pins the order the proof depends on.
//
// It also pins: every accept precedes every mark (a mark rule followed by an
// accept is dead code -- the packet is already marked), the :53 accepts exist
// for both transports ahead of the marks (the DNS DNAT runs at nat, after this
// chain), and 127/8 is accepted so Envoy's pod-local app dials are never
// captured.
func TestDivertRuleOrder(t *testing.T) {
	for _, redirectAll := range []bool{false, true} {
		t.Run(map[bool]string{false: "scoped", true: "redirect-all"}[redirectAll], func(t *testing.T) {
			rules := divertOutputRules(redirectAll, []uint16{5432}, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
				uint32(meshconst.CaptureDivertFwMark), uint16(meshconst.ProxyOutboundPort), uint16(meshconst.ProxyL4OutboundPort))
			require.NotEmpty(t, rules)

			ctIdx, firstMark := -1, -1
			for i, r := range rules {
				if isCtDirectionReply(r) {
					require.Equal(t, -1, ctIdx, "ct direction reply must appear exactly once")
					ctIdx = i
				}
				switch classify(r) {
				case kindMark:
					if firstMark == -1 {
						firstMark = i
					}
				case kindAccept:
					assert.Equal(t, -1, firstMark, "rule %d: an accept after a mark rule is dead code", i)
				default:
					t.Fatalf("rule %d is neither an accept nor a mark: %#v", i, r)
				}
			}
			require.NotEqual(t, -1, ctIdx, "the ct direction reply accept is missing -- every inbound reply would be diverted")
			require.NotEqual(t, -1, firstMark, "no mark rule: nothing is captured")
			assert.Less(t, ctIdx, firstMark, "ct direction reply accept must precede the first mark rule")

			// Passthrough mark accept is first (defensive; proposal 022).
			assert.Equal(t, expr.MetaKeyMARK, rules[0][0].(*expr.Meta).Key)

			// Mark rules: UDP 18082, TCP 18081, TCP 18082, and under redirect-all
			// one more with no dport at all.
			var marks [][]expr.Any
			for _, r := range rules {
				if classify(r) == kindMark {
					marks = append(marks, r)
				}
			}
			wantMarks := 3
			if redirectAll {
				wantMarks = 4
			}
			require.Len(t, marks, wantMarks)
			assertMarkDport(t, marks[0], unix.IPPROTO_UDP, meshconst.ProxyL4OutboundPort)
			assertMarkDport(t, marks[1], unix.IPPROTO_TCP, meshconst.ProxyOutboundPort)
			assertMarkDport(t, marks[2], unix.IPPROTO_TCP, meshconst.ProxyL4OutboundPort)
			if redirectAll {
				assert.Len(t, marks[3], 4, "the any-port rule has no dport match")
				assert.Equal(t, []byte{unix.IPPROTO_TCP}, marks[3][1].(*expr.Cmp).Data)
			}
			// No mark rule for the identity-bearing UDP ports until their
			// listeners exist (038 Phase 4): a divert with no socket behind it
			// blackholes what today leaves the pod normally.
			for _, m := range marks {
				if len(m) == 6 {
					port := m[3].(*expr.Cmp).Data
					// 18008 (inbound) and 18009 (east-west gateway) are agent-side
					// constants (proxy.defaultInboundPort, proxy.DefaultEastWestTunnelPort)
					// the CNI does not import; the literals are the contract here.
					assert.NotEqual(t, beUint16(18008), port, "18008 must not be diverted before its QUIC listener lands")
					assert.NotEqual(t, beUint16(18009), port, "18009 must never be diverted")
				}
			}

			// Both :53 accepts, both transports, ahead of the marks.
			var dns53 int
			for _, r := range rules[:firstMark] {
				if len(r) == 5 && string(r[3].(*expr.Cmp).Data) == string(beUint16(53)) {
					dns53++
				}
			}
			assert.Equal(t, 2, dns53, "tcp and udp dport 53 accepts must both precede the marks")

			// The per-pod port exclusion is present for BOTH transports.
			var excl5432 []byte
			for _, r := range rules[:firstMark] {
				if len(r) == 5 && string(r[3].(*expr.Cmp).Data) == string(beUint16(5432)) {
					excl5432 = append(excl5432, r[1].(*expr.Cmp).Data[0])
				}
			}
			assert.ElementsMatch(t, []byte{unix.IPPROTO_TCP, unix.IPPROTO_UDP}, excl5432, "exclude-outbound-ports must cover both transports")

			// Loopback carve-out present ahead of the marks.
			var lo bool
			for _, r := range rules[:firstMark] {
				if len(r) == 4 {
					if c, ok := r[2].(*expr.Cmp); ok && string(c.Data) == string([]byte{127, 0, 0, 0}) {
						lo = true
					}
				}
			}
			assert.True(t, lo, "127.0.0.0/8 must be accepted ahead of the marks")
		})
	}
}

func assertMarkDport(t *testing.T, r []expr.Any, proto byte, port int) {
	t.Helper()
	require.Len(t, r, 6)
	assert.Equal(t, []byte{proto}, r[1].(*expr.Cmp).Data)
	assert.Equal(t, uint32(2), r[2].(*expr.Payload).Offset)
	assert.Equal(t, beUint16(uint16(port)), r[3].(*expr.Cmp).Data, "port %d big-endian", port)
	assert.Equal(t, leUint32(uint32(meshconst.CaptureDivertFwMark)), r[4].(*expr.Immediate).Data)
	m := r[5].(*expr.Meta)
	assert.Equal(t, expr.MetaKeyMARK, m.Key)
	assert.True(t, m.SourceRegister, "the final meta must SET the mark, not read it")
}

// TestDivertPreroutingRules pins the prerouting half: TCP gets `tproxy to :18001`
// (one listener for every captured port; the accepted socket keeps the original
// destination), UDP gets NO port rewrite (the listener binds the dialed port
// because a datagram reply's source port is the socket's bound port). Both are
// scoped to iif lo + the divert mark so a packet that arrives on eth0 with a
// stale mark is never tproxy'd.
func TestDivertPreroutingRules(t *testing.T) {
	rules := divertPreroutingRules(uint32(meshconst.CaptureDivertFwMark), uint16(meshconst.ProxyCapturePort))
	require.Len(t, rules, 2)

	tcp, udp := rules[0], rules[1]
	for name, r := range map[string][]expr.Any{"tcp": tcp, "udp": udp} {
		assert.Equal(t, expr.MetaKeyIIFNAME, r[0].(*expr.Meta).Key, name)
		assert.Equal(t, ifname("lo"), r[1].(*expr.Cmp).Data, name)
		assert.Equal(t, expr.MetaKeyMARK, r[2].(*expr.Meta).Key, name)
		assert.Equal(t, leUint32(uint32(meshconst.CaptureDivertFwMark)), r[3].(*expr.Cmp).Data, name)
		assert.Equal(t, expr.MetaKeyL4PROTO, r[4].(*expr.Meta).Key, name)
		assert.Equal(t, expr.VerdictAccept, r[len(r)-1].(*expr.Verdict).Kind, name)
	}

	require.Len(t, tcp, 9)
	assert.Equal(t, []byte{unix.IPPROTO_TCP}, tcp[5].(*expr.Cmp).Data)
	assert.Equal(t, beUint16(uint16(meshconst.ProxyCapturePort)), tcp[6].(*expr.Immediate).Data, "tproxy target port 18001, big-endian")
	tp, ok := tcp[7].(*expr.TProxy)
	require.True(t, ok, "tcp rule must carry a tproxy statement")
	assert.Equal(t, byte(unix.NFPROTO_IPV4), tp.Family)
	assert.Equal(t, uint32(1), tp.RegPort)

	require.Len(t, udp, 7)
	assert.Equal(t, []byte{unix.IPPROTO_UDP}, udp[5].(*expr.Cmp).Data)
	for _, e := range udp {
		_, isTproxy := e.(*expr.TProxy)
		assert.False(t, isTproxy, "udp must NOT be tproxy'd to another port: the reply source port would not match the dialed port")
	}
}

// TestCtDirectionReplyAcceptExprs pins the encoding of the load-bearing rule:
// ct direction is a one-byte key and reply = 1 (IP_CT_DIR_REPLY). `ct state
// established` is a DIFFERENT key and is wrong here (it would also exempt the
// 2nd+ packets of the pod's own captured outbound flows).
func TestCtDirectionReplyAcceptExprs(t *testing.T) {
	exprs := ctDirectionReplyAcceptExprs()
	require.Len(t, exprs, 3)
	ct := exprs[0].(*expr.Ct)
	assert.Equal(t, expr.CtKeyDIRECTION, ct.Key)
	assert.NotEqual(t, expr.CtKeySTATE, ct.Key, "ct state is not a substitute for ct direction in a route chain")
	assert.Equal(t, []byte{1}, exprs[1].(*expr.Cmp).Data, "IP_CT_DIR_REPLY == 1")
	assert.Equal(t, expr.VerdictAccept, exprs[2].(*expr.Verdict).Kind)
}

// TestDportAcceptExprs verifies the transport-generic dport accept used for the
// :53 carve-outs and the per-pod exclusions on both transports.
func TestDportAcceptExprs(t *testing.T) {
	for _, proto := range []byte{unix.IPPROTO_TCP, unix.IPPROTO_UDP} {
		exprs := dportAcceptExprs(proto, 53)
		require.Len(t, exprs, 5)
		assert.Equal(t, expr.MetaKeyL4PROTO, exprs[0].(*expr.Meta).Key)
		assert.Equal(t, []byte{proto}, exprs[1].(*expr.Cmp).Data)
		assert.Equal(t, expr.PayloadBaseTransportHeader, exprs[2].(*expr.Payload).Base)
		assert.Equal(t, uint32(2), exprs[2].(*expr.Payload).Offset)
		assert.Equal(t, []byte{0x00, 0x35}, exprs[3].(*expr.Cmp).Data)
		assert.Equal(t, expr.VerdictAccept, exprs[4].(*expr.Verdict).Kind)
	}
}

// TestBuiltinDivertExcludedRanges verifies the always-on carve-outs cover IPv4
// link-local (the cloud metadata service) and multicast (proposal 022); the
// loopback /8 is a separate, always-present rule.
func TestBuiltinDivertExcludedRanges(t *testing.T) {
	got := make(map[netip.Prefix]bool)
	for _, r := range builtinDivertExcludedRanges {
		got[r] = true
	}
	assert.True(t, got[netip.MustParsePrefix("169.254.0.0/16")], "link-local must be excluded (IMDS 169.254.169.254)")
	assert.True(t, got[netip.MustParsePrefix("224.0.0.0/4")], "multicast must be excluded")
	assert.True(t, netip.MustParsePrefix("169.254.0.0/16").Contains(netip.MustParseAddr("169.254.169.254")))
}

// TestEncodingHelpers pins the byte orders the kernel expects: transport ports
// are network order in the payload, marks are host order in a meta register,
// interface names are IFNAMSIZ NUL-padded.
func TestEncodingHelpers(t *testing.T) {
	assert.Equal(t, []byte{0x46, 0xa2}, beUint16(18082))
	assert.Equal(t, []byte{0x71, 0xae, 0x00, 0x00}, leUint32(0xae71))
	lo := ifname("lo")
	assert.Len(t, lo, unix.IFNAMSIZ)
	assert.Equal(t, "lo", string(lo[:2]))
	assert.Equal(t, byte(0), lo[2])
}

func TestPodRedirectAll(t *testing.T) {
	annoTrue := map[string]string{aetherannotations.AnnotationCaptureRedirectAll: "true"}
	annoFalse := map[string]string{aetherannotations.AnnotationCaptureRedirectAll: "false"}
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

// TestPodExcludedOutboundPorts verifies the exclude-outbound-ports annotation
// (proposal 022 M2-default) parses into a deduplicated port list and degrades
// gracefully on malformed/blank/out-of-range entries.
func TestPodExcludedOutboundPorts(t *testing.T) {
	anno := func(v string) config.AetherConf {
		m := map[string]string{aetherannotations.AnnotationCaptureExcludeOutboundPorts: v}
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
		m := map[string]string{aetherannotations.AnnotationCaptureExcludeOutboundIPRanges: v}
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

// TestPassthroughMarkAcceptExprs verifies the self-exclusion rule matches the
// proxy's fwmark (little-endian u32) and accepts, so SO_MARK'd passthrough egress
// bypasses the redirect (proposal 022 M2-default).
func TestPassthroughMarkAcceptExprs(t *testing.T) {
	exprs := passthroughMarkAcceptExprs(meshconst.CapturePassthroughFwMark)
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

// TestCaptureRedirectExprs_TCPMeshPort pins the mesh's well-known TCP spelling
// (proposal 037) into the scoped redirect.
//
// This is what makes <svc>:18082 reachable WITHOUT redirect-all: scoped capture
// redirects it alongside ProxyOutboundPort. A dial to a service's own
// application port has no such rule and is captured only under redirect-all.
//
// Asserting the encoded dport rather than just "a rule exists" is the point — a
// rule built for the wrong port installs cleanly, captures nothing, and looks
