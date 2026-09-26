package plugin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"

	meshconst "aethermesh.dev/common/constants/mesh"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// captureTableName is the nftables table holding the pod's transparent-capture
// rules (proposal 038). ONE table, two chains, both transports.
const captureTableName = "aether_capture"

// ctDirReply is the kernel's IP_CT_DIR_REPLY (enum ip_conntrack_dir: ORIGINAL
// = 0, REPLY = 1), the byte `ct direction` loads into a register. x/sys/unix
// does not export it.
const ctDirReply byte = 1

// builtinDivertExcludedRanges are always carved out of capture, independent of
// per-pod annotations (proposal 022 M2-default). Link-local carries the cloud
// instance metadata service (169.254.169.254) and must be reached directly;
// multicast is never a unicast mesh destination.
var builtinDivertExcludedRanges = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
}

// installCaptureDivert programs TPROXY-style capture (proposal 038) in the pod's
// netns, for BOTH transports, replacing the nat REDIRECT of proposals 018/022.
//
// Everything lives in the netns and dies with it on pod teardown: the nft table,
// the policy-routing rule and the local route. No DEL cleanup is needed.
//
// It enters the netns by setns on a locked OS thread (withPodNetns): the
// nftables netlink socket AND the rtnetlink sockets vishvananda/netlink opens
// then live in the pod netns. That library's package-level calls open a fresh
// socket per call in the calling thread's current netns, which is exactly the
// property this depends on; a package-level handle created at init would have
// programmed the HOST's routing table.
//
// HOW IT WORKS. nft `tproxy` is prerouting-only, so locally-originated pod
// egress cannot be tproxy'd where it is generated. Instead:
//
//  1. OUTPUT (type route, priority mangle): a captured packet gets
//     CaptureDivertFwMark. `type route` re-runs the routing decision after the
//     mark changes, which is what makes step 2 apply.
//  2. `ip rule fwmark <mark> lookup <table>` + `ip route local default dev lo
//     table <table>`: the marked packet is delivered locally via lo, with its
//     IP header INTACT -- destination VIP and port preserved.
//  3. PREROUTING on lo (type filter, priority mangle): `tproxy` hands the packet
//     to the transparent capture socket. For TCP that is ONE listener on
//     ProxyCapturePort (the socket lookup is by the tproxy target port; the
//     accepted socket still carries the original destination, so replies are
//     correct with no conntrack NAT). For UDP there is no port rewrite -- a
//     datagram reply's source port is the socket's bound port, so the UDP
//     listener binds the port the client dialed (ProxyL4OutboundPort) and the
//     divert alone delivers to it.
//
// Every fact above was measured on a Talos node before this was written:
// e2e/spike/tproxy-phase0b.py.
func installCaptureDivert(netnsPath string, redirectAll bool, excludePorts []uint16, excludeRanges []netip.Prefix, logger *zap.Logger) error {
	return withPodNetns(netnsPath, func() error {
		if err := programCaptureDivert(redirectAll, excludePorts, excludeRanges, logger); err != nil {
			return err
		}
		return programDivertRouting(logger)
	})
}

// programCaptureDivert adds the nft table in the CURRENT netns.
//
// RULE ORDER IS LOAD-BEARING, and the test that pins it (TestDivertRuleOrder)
// exists because the one rule that keeps this from breaking every pod is an
// early `accept`, not a mark:
//
//	output (type route, hook output, priority mangle)
//	  meta mark 0xae7e accept                 passthrough egress; defensive
//	  ct direction reply accept               THE load-bearing rule -- see below
//	  ip daddr 127.0.0.0/8 accept             pod-local fast lane, app clusters
//	  ip daddr 169.254.0.0/16 accept          metadata service
//	  ip daddr 224.0.0.0/4 accept             multicast
//	  tcp dport 53 accept / udp dport 53      the DNS DNAT runs later, at nat
//	  [exclude-outbound-ports, BOTH transports]
//	  [exclude-outbound-ip-ranges]
//	  udp dport 18082 mark set 0xae71         plaintext UDP, the L4 spelling
//	  tcp dport {18081,18082} mark set 0xae71 scoped TCP
//	  meta l4proto tcp mark set 0xae71        redirect-all only: any port
//	divert (type filter, hook prerouting, priority mangle)
//	  iif lo meta mark 0xae71 l4proto tcp tproxy to :18001 accept
//	  iif lo meta mark 0xae71 l4proto udp accept
//
// `ct direction reply accept`: a route chain sees EVERY locally-generated
// packet -- unlike the nat chain it replaces, which saw only a flow's first.
// That includes the pod's own server replies to inbound clients (a SYN-ACK
// whose dport is the client's ephemeral port) and Envoy's own replies on
// captured flows. Under redirect-all the any-port rule would mark them, they
// would loop to lo, tproxy's established lookup would miss, the listener lookup
// would hit the 18001 LISTEN socket, and a SYN-ACK arriving at a LISTEN socket
// is answered with a reset: every inbound connection to every redirect-all pod
// dies. Both are the REPLY direction of a tracked flow, so this rule exempts
// exactly them. `ct state established,related accept` is NOT a substitute: in
// a route chain it would also exempt the 2nd+ packets of the pod's own
// captured outbound flows, sending them out eth0 mid-connection. Measured:
// spike arm S1 connects with this rule and both ends time out without it.
//
// The :53 accepts sit ahead of the marks because aether_dns_capture DNATs :53
// at nat priority, AFTER this chain; a marked :53 packet would be diverted to
// lo before the DNAT could see it. Incidentally this makes TCP :53 under
// redirect-all deterministic -- it was previously contested by two nat chains
// at equal priority.
func programCaptureDivert(redirectAll bool, excludePorts []uint16, excludeRanges []netip.Prefix, logger *zap.Logger) error {
	mark := uint32(meshconst.CaptureDivertFwMark)
	capturePort := uint16(meshconst.ProxyCapturePort)
	httpMeshPort := uint16(meshconst.ProxyOutboundPort)
	l4MeshPort := uint16(meshconst.ProxyL4OutboundPort)

	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables netlink: %w", err)
	}

	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: captureTableName})
	output := c.AddChain(&nftables.Chain{
		Name:     "output",
		Table:    table,
		Type:     nftables.ChainTypeRoute,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityMangle,
	})
	divert := c.AddChain(&nftables.Chain{
		Name:     "divert",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityMangle,
	})

	for _, exprs := range divertOutputRules(redirectAll, excludePorts, excludeRanges, mark, httpMeshPort, l4MeshPort) {
		c.AddRule(&nftables.Rule{Table: table, Chain: output, Exprs: exprs})
	}
	for _, exprs := range divertPreroutingRules(mark, capturePort) {
		c.AddRule(&nftables.Rule{Table: table, Chain: divert, Exprs: exprs})
	}

	if err := c.Flush(); err != nil {
		// A rejected table is total: nothing of it is installed, the pod runs
		// UNCAPTURED, and CmdAdd treats this as a warning. Say so loudly and name
		// the likeliest cause, because the symptom downstream is "the mesh
		// silently does nothing for this pod".
		return fmt.Errorf("apply capture divert (nft flush; if this names tproxy, the nft_tproxy module may be unavailable on this kernel): %w", err)
	}
	logger.Info("installed transparent-capture divert (038)",
		zap.Bool("redirect_all", redirectAll),
		zap.Uint16("tcp_capture_port", capturePort),
		zap.Uint16("udp_capture_port", l4MeshPort),
		zap.Uint32("mark", mark))
	return nil
}

// divertOutputRules returns the OUTPUT chain's rules in order. Split out of
// programCaptureDivert so the ORDER is a testable value rather than a side
// effect of nft calls.
func divertOutputRules(redirectAll bool, excludePorts []uint16, excludeRanges []netip.Prefix, mark uint32, httpMeshPort, l4MeshPort uint16) [][]expr.Any {
	rules := [][]expr.Any{
		passthroughMarkAcceptExprs(meshconst.CapturePassthroughFwMark),
		ctDirectionReplyAcceptExprs(),
		excludeIPRangeAcceptExprs(netip.MustParsePrefix("127.0.0.0/8")),
	}
	for _, r := range builtinDivertExcludedRanges {
		rules = append(rules, excludeIPRangeAcceptExprs(r))
	}
	rules = append(rules,
		dportAcceptExprs(unix.IPPROTO_TCP, 53),
		dportAcceptExprs(unix.IPPROTO_UDP, 53),
	)
	// Per-pod exclusions cover BOTH transports now that UDP is captured (they
	// were TCP-only under REDIRECT, invisibly, because redirect-all never
	// captured UDP).
	for _, port := range excludePorts {
		rules = append(rules, dportAcceptExprs(unix.IPPROTO_TCP, port), dportAcceptExprs(unix.IPPROTO_UDP, port))
	}
	for _, r := range excludeRanges {
		rules = append(rules, excludeIPRangeAcceptExprs(r))
	}
	rules = append(rules,
		markSetDportExprs(unix.IPPROTO_UDP, l4MeshPort, mark),
		markSetDportExprs(unix.IPPROTO_TCP, httpMeshPort, mark),
		markSetDportExprs(unix.IPPROTO_TCP, l4MeshPort, mark),
	)
	if redirectAll {
		rules = append(rules, markSetAnyTCPExprs(mark))
	}
	return rules
}

// divertPreroutingRules returns the prerouting chain's rules in order.
func divertPreroutingRules(mark uint32, tcpCapturePort uint16) [][]expr.Any {
	return [][]expr.Any{
		tproxyTCPExprs(mark, tcpCapturePort),
		divertUDPAcceptExprs(mark),
	}
}

// programDivertRouting installs the policy-routing pair in the CURRENT netns:
//
//	ip rule add fwmark <CaptureDivertFwMark> lookup <CaptureDivertRouteTable>
//	ip route add local 0.0.0.0/0 dev lo table <CaptureDivertRouteTable>
//
// Two zero-value traps in vishvananda/netlink are avoided on purpose. A Rule
// built as a struct literal sends FRA_SUPPRESS_PREFIXLEN=0, which suppresses
// the default route in the target table -- the divert then silently does
// nothing -- and Goto 0; netlink.NewRule() sets the sentinels. A Route with a
// nil Dst is rejected outright. Both add calls use exclusive-create flags, so a
// re-run (a CNI ADD replayed for the same netns) tolerates EEXIST on the rule
// and replaces the route.
func programDivertRouting(logger *zap.Logger) error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find lo in the pod netns: %w", err)
	}

	rule := netlink.NewRule()
	rule.Family = unix.AF_INET
	rule.Mark = uint32(meshconst.CaptureDivertFwMark)
	rule.Table = meshconst.CaptureDivertRouteTable
	if err := netlink.RuleAdd(rule); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("add fwmark rule: %w", err)
	}

	_, all, _ := net.ParseCIDR("0.0.0.0/0")
	route := &netlink.Route{
		Dst:       all,
		Type:      unix.RTN_LOCAL,
		Scope:     unix.RT_SCOPE_HOST,
		LinkIndex: lo.Attrs().Index,
		Table:     meshconst.CaptureDivertRouteTable,
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("add local route in table %d: %w", meshconst.CaptureDivertRouteTable, err)
	}
	logger.Info("installed divert policy routing",
		zap.Uint32("mark", uint32(meshconst.CaptureDivertFwMark)),
		zap.Int("table", meshconst.CaptureDivertRouteTable))
	return nil
}

// ---------------------------------------------------------------------------
// expression builders
// ---------------------------------------------------------------------------

// ctDirectionReplyAcceptExprs builds:
//
//	ct direction reply accept
//
// The rule that keeps the divert from capturing the pod's own replies (see
// programCaptureDivert). nftables encodes ct direction as a one-byte value:
// 0 = original, 1 = reply (IP_CT_DIR_REPLY).
func ctDirectionReplyAcceptExprs() []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, SourceRegister: false, Key: expr.CtKeyDIRECTION},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{ctDirReply}},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// dportAcceptExprs builds:
//
//	meta l4proto <proto> · th dport <port> · accept
//
// Used for the :53 carve-outs (the DNS DNAT runs after this chain) and for the
// per-pod exclude-outbound-ports annotation, on both transports. The transport
// dport sits at offset 2 for TCP and UDP alike, so one builder serves both.
func dportAcceptExprs(proto byte, port uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: beUint16(port)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// excludePortAcceptExprs is dportAcceptExprs for TCP, kept under its historical
// name for the tests and call sites that predate UDP capture.
func excludePortAcceptExprs(port uint16) []expr.Any {
	return dportAcceptExprs(unix.IPPROTO_TCP, port)
}

// excludeIPRangeAcceptExprs builds:
//
//	ip daddr & <netmask> == <network> · accept
//
// Destination-based, so it carves out both transports. Also used for the
// loopback /8 (the pod-local fast lane and Envoy's own app-cluster dials) and
// the built-in special ranges. IPv4 only (the table is TableFamilyIPv4).
func excludeIPRangeAcceptExprs(prefix netip.Prefix) []expr.Any {
	network := prefix.Masked().Addr().As4()
	mask := net.CIDRMask(prefix.Bits(), 32)
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0x00, 0x00, 0x00, 0x00}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: network[:]},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// passthroughMarkAcceptExprs builds:
//
//	meta mark <fwmark> accept
//
// Accepts the proxy's own forwarded egress (SO_MARK'd by the
// passthrough_original_dst cluster) ahead of the marks. In practice it never
// matches -- the proxy's sockets live in the host netns -- and it is kept as a
// defensive first rule (proposal 022). The mark is a u32 loaded into the
// register in host byte order.
func passthroughMarkAcceptExprs(fwmark uint32) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: leUint32(fwmark)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// markSetDportExprs builds:
//
//	meta l4proto <proto> · th dport <port> · meta mark set <mark>
//
// The scoped capture rules. No loopback carve-out is needed here: 127.0.0.0/8
// is accepted earlier in the chain.
func markSetDportExprs(proto byte, port uint16, mark uint32) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: beUint16(port)},
		&expr.Immediate{Register: 1, Data: leUint32(mark)},
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1, SourceRegister: true},
	}
}

// markSetAnyTCPExprs builds the redirect-all rule:
//
//	meta l4proto tcp · meta mark set <mark>
//
// ALL outbound TCP not accepted earlier. Its safety rests entirely on the
// accepts above it -- `ct direction reply` most of all.
func markSetAnyTCPExprs(mark uint32) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		&expr.Immediate{Register: 1, Data: leUint32(mark)},
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1, SourceRegister: true},
	}
}

// tproxyTCPExprs builds the prerouting rule:
//
//	iif lo · meta mark <mark> · meta l4proto tcp · tproxy to :<port> · accept
//
// Socket lookup by the target port with the IP header untouched: the single TCP
// capture listener on ProxyCapturePort receives connections to every captured
// destination and sees the original destination as the accepted socket's local
// endpoint. tproxy assigns only to a socket with IP_TRANSPARENT (the capture
// listener sets `transparent: true`, PR 2).
func tproxyTCPExprs(mark uint32, port uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname("lo")},
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: leUint32(mark)},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		&expr.Immediate{Register: 1, Data: beUint16(port)},
		&expr.TProxy{Family: unix.NFPROTO_IPV4, TableFamily: unix.NFPROTO_IPV4, RegPort: 1},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// divertUDPAcceptExprs builds the prerouting rule:
//
//	iif lo · meta mark <mark> · meta l4proto udp · accept
//
// No tproxy statement: the UDP capture listener binds the port the client
// dialed, so plain local delivery of the intact packet reaches it. A port
// rewrite here would be a no-op at best and, if the port differed, would break
// the reply (the reply's source port is the socket's bound port and a
// connected client drops a mismatch).
func divertUDPAcceptExprs(mark uint32) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname("lo")},
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: leUint32(mark)},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_UDP}},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// beUint16 encodes a port for a transport-header comparison (network order).
func beUint16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

// leUint32 encodes a mark for a meta register (host order; nftables registers
// hold meta values in host byte order, as the passthrough accept always has).
func leUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

// ifname encodes an interface name for an IIFNAME comparison: the kernel
// compares a fixed IFNAMSIZ (16) byte buffer, NUL-padded.
func ifname(name string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return b
}
