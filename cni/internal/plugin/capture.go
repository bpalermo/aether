package plugin

import (
	"encoding/binary"
	"fmt"

	commonconstants "github.com/bpalermo/aether/common/constants"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// captureTableName is the nft table the redirect rule lives in, inside the pod netns.
const captureTableName = "aether_capture"

// captureRedirectAllTableName is the nft table for the redirect-all mode (spike/M2a).
const captureRedirectAllTableName = "aether_capture_all"

// installCaptureRedirect programs, inside the pod's network namespace, an nftables
// REDIRECT of outbound TCP destined for a mesh ClusterIP:<meshPort> to the pod-local
// transparent-capture listener on <capturePort> (proposal 018, Phase 3a).
//
// The rule lives in the POD netns nat/output chain, so it fires before the packet
// egresses to the host (independent of host kube-proxy/flannel). The loopback
// exclusion (ip daddr != 127.0.0.0/8) leaves the explicit 127.0.0.1:<meshPort>
// fast-lane untouched — only ClusterIP traffic is captured. nftables is programmed
// over netlink (no iptables binary), so it works in the minimal CNI exec environment.
// The rule dies with the netns on pod teardown; no DEL cleanup is needed.
//
// It enters the netns by setns on a locked OS thread (matching netnsDialContext): the
// netlink socket nftables opens then lives in the pod netns. A failed restore leaves
// the thread locked so the Go runtime destroys it rather than reusing a poisoned one.
func installCaptureRedirect(netnsPath string, excludePorts []uint16, logger *zap.Logger) error {
	return withPodNetns(netnsPath, func() error { return programCaptureRedirect(excludePorts, logger) })
}

// programCaptureRedirect adds the nat/output REDIRECT rules in the CURRENT netns:
// one for outbound TCP and one for outbound UDP, both destined for the mesh
// port (ProxyOutboundPort). Both protocols redirect to ProxyCapturePort — the
// TCP and UDP listeners bind the SAME port number on independent sockets (TCP
// and UDP are separate socket families; the kernel routes by (protocol, port),
// so there is no collision). Both rules share the loopback exclusion so the
// pod-local explicit fast-lane (127.x:meshPort) is never intercepted.
//
// NOTE: the UDP redirect only takes effect when --l4-routes is enabled (the
// agent only generates the UDP capture listener when UDPRoute backends exist).
// The nft rule itself is always installed when capture is on — the absence of a
// bound UDP socket on ProxyCapturePort means redirected packets are silently
// dropped until the agent creates the listener, which is correct behaviour
// (UDPRoute without --l4-routes = no listener = datagrams discarded rather than
// sent to an unexpected destination).
func programCaptureRedirect(excludePorts []uint16, logger *zap.Logger) error {
	meshPort := uint16(commonconstants.ProxyOutboundPort)
	capturePort := uint16(commonconstants.ProxyCapturePort)

	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables netlink: %w", err)
	}

	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: captureTableName})
	chain := c.AddChain(&nftables.Chain{
		Name:     "output",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityNATDest,
	})
	// Excluded ports (proposal 022 M2-default): accept (RETURN) ahead of the
	// redirect so connections to these dports bypass capture entirely.
	for _, port := range excludePorts {
		c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: excludePortAcceptExprs(port)})
	}
	// TCP: outbound TCP to ClusterIP:meshPort → capturePort (Phase 3a).
	c.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: captureRedirectExprs(unix.IPPROTO_TCP, meshPort, capturePort),
	})
	// UDP: outbound UDP to ClusterIP:meshPort → capturePort (Phase 3b).
	// Datagrams arriving at ProxyCapturePort:UDP are handled by the udp_proxy
	// capture listener generated for each pod when UDPRoute backends are present.
	// The TCP and UDP listeners coexist on :18001 via independent protocol sockets.
	// No mesh mTLS — UDP is forwarded in plaintext (known limitation; no DTLS).
	c.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: captureRedirectExprs(unix.IPPROTO_UDP, meshPort, capturePort),
	})

	if err := c.Flush(); err != nil {
		return fmt.Errorf("apply capture redirect (nft flush): %w", err)
	}
	logger.Info("installed transparent-capture redirect",
		zap.Uint16("mesh_port", meshPort),
		zap.Uint16("capture_port", capturePort))
	return nil
}

// captureRedirectExprs builds the rule:
//
//	meta l4proto <proto>                         (tcp or udp)
//	ip daddr & 255.0.0.0 != 127.0.0.0           (skip the loopback fast-lane)
//	<proto> dport <meshPort>                     (transport dport, offset 2)
//	redirect to :<capturePort>
//
// The transport dest port is at the same offset (2, len 2) for both TCP and UDP,
// so the same expression shape applies to both protocols — matching the approach
// used in dns.go for the mesh-DNS DNAT. The proto byte controls which L4 traffic
// is matched (unix.IPPROTO_TCP or unix.IPPROTO_UDP).
func captureRedirectExprs(proto byte, meshPort, capturePort uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		// ip daddr (IPv4 dest = offset 16, len 4), masked to /8, != 127.0.0.0
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{0xff, 0x00, 0x00, 0x00}, Xor: []byte{0x00, 0x00, 0x00, 0x00}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{127, 0, 0, 0}},
		// <proto> dport == meshPort (transport dest = offset 2, len 2, big-endian;
		// this offset is identical for TCP and UDP headers).
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: beUint16(meshPort)},
		// redirect to capturePort
		&expr.Immediate{Register: 1, Data: beUint16(capturePort)},
		&expr.Redir{RegisterProtoMin: 1},
	}
}

func beUint16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

// installCaptureRedirectAll programs, inside the pod's network namespace, an
// nftables REDIRECT of ALL outbound non-local TCP into the capture listener
// (:ProxyCapturePort). This is the M2a spike for proposal 022 "redirect-all +
// ORIGINAL_DST passthrough". Non-mesh destinations are forwarded in plain TCP
// by the Envoy passthrough_original_dst cluster; mesh destinations continue to
// route through the per-source mTLS path as with Phase 3a.
//
// EXPERIMENTAL: default false. Do NOT enable cluster-wide. The Envoy capture
// listener MUST carry the passthrough fallback filter chain (requires the agent
// --capture-redirect-all flag) or non-mesh egress will be dropped.
//
// Exclusions to prevent loops and proxy self-traffic:
//   - loopback (127.0.0.0/8): the 127.x fast-lane and the proxy's own loopback
//     conns are never intercepted.
//   - capture port (:ProxyCapturePort TCP): if Envoy itself originates a TCP
//     connection on the capture port (health checks dialing app clusters bound in
//     the pod netns come from 127.x, but belt-and-suspenders), we must not
//     re-redirect it — that would loop back into the capture listener.
//   - established / related connections: conntrack prevents mid-flow re-direction.
//     Without this exclusion, the response path of an already-established
//     connection would be RE-redirected to :18001 on each packet, breaking the
//     connection. Conntrack tracks established TCP state; ESTABLISHED skips the
//     redirect for them.
//
// The rule lives in a SEPARATE nft table (aether_capture_all) from the scoped
// rule (aether_capture) so both can be installed independently. When
// CaptureRedirectAllEnabled is true AND TransparentCaptureEnabled is false, only
// the broad rule is installed. When both are true, both tables exist and the
// redirect-all subsumes the scoped rule (all traffic is already captured).
func installCaptureRedirectAll(netnsPath string, excludePorts []uint16, logger *zap.Logger) error {
	return withPodNetns(netnsPath, func() error { return programCaptureRedirectAll(excludePorts, logger) })
}

// programCaptureRedirectAll adds the redirect-all nat/output rule in the CURRENT
// netns. Two rules are added in priority order:
//  1. ACCEPT established/related connections (conntrack bypass — avoids looping
//     the response path of outbound connections back into the capture listener).
//  2. REDIRECT all outbound TCP, excluding loopback and the capture port itself,
//     to ProxyCapturePort.
//
// The conntrack ACCEPT must come BEFORE the redirect in the same chain. We put
// it in a separate higher-priority "conntrack" chain to avoid coupling to
// rule position, but for a single-table approach both rules go in "output" with
// the conntrack rule first (lower rule index wins in nft).
func programCaptureRedirectAll(excludePorts []uint16, logger *zap.Logger) error {
	capturePort := uint16(commonconstants.ProxyCapturePort)

	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables netlink: %w", err)
	}

	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: captureRedirectAllTableName})
	chain := c.AddChain(&nftables.Chain{
		Name:     "output",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityNATDest,
	})

	// Rule 0a: skip the proxy's OWN forwarded egress (proposal 022 M2-default).
	// Envoy SO_MARKs the passthrough_original_dst cluster's upstream sockets with
	// CapturePassthroughFwMark; accepting it here, ahead of the redirect, prevents
	// the proxy's passthrough connection from being re-captured into a loop — the
	// "breaks all egress" failure mode. SO_MARK (not a UID match) because the proxy
	// runs as root. Harmless if the passthrough egresses proxy-side (never matches).
	c.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: passthroughMarkAcceptExprs(commonconstants.CapturePassthroughFwMark),
	})

	// Rule 0b: excluded ports (proposal 022 M2-default) — accept ahead of the broad
	// redirect so connections to these dports bypass the mesh. This is where
	// exclusions matter most: redirect-all otherwise captures every port.
	for _, port := range excludePorts {
		c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: excludePortAcceptExprs(port)})
	}

	// Rule 1: skip established/related connections (conntrack).
	// ct state {established, related} -> accept
	// This must come before the redirect rule so that response traffic for
	// outbound connections (e.g. curl responding) is not re-redirected.
	c.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: conntrackEstablishedAcceptExprs(),
	})

	// Rule 2: skip capture port itself to prevent re-entry loops.
	// tcp dport capturePort -> accept
	c.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: skipCaptureSelfExprs(capturePort),
	})

	// Rule 3: redirect all other outbound TCP (non-loopback) to capturePort.
	// meta l4proto tcp · ip daddr & /8 != 127.0.0.0 → redirect to capturePort
	c.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: redirectAllTCPExprs(capturePort),
	})

	if err := c.Flush(); err != nil {
		return fmt.Errorf("apply redirect-all capture (nft flush): %w", err)
	}
	logger.Info("installed redirect-all transparent-capture (spike/M2a)",
		zap.Uint16("capture_port", capturePort))
	return nil
}

// conntrackEstablishedAcceptExprs builds the nft rule:
//
//	ct state established,related accept
//
// This prevents the nat/output hook from seeing response packets for existing
// outbound connections — without it, the redirect rule would try to re-redirect
// ACK/data packets of an already-NATted connection, corrupting the flow.
//
// nftables conntrack state is expressed via CtStateBitMask:
//
//	established = 0x2 (NFT_CT_STATE_ESTABLISHED)
//	related     = 0x4 (NFT_CT_STATE_RELATED)
func conntrackEstablishedAcceptExprs() []expr.Any {
	// Conntrack state bitmask: established(2) | related(4) = 6.
	const ctStateEstablishedRelated = 0x00000006
	return []expr.Any{
		// load ct state into reg1
		&expr.Ct{Register: 1, SourceRegister: false, Key: expr.CtKeySTATE},
		// reg1 & ctStateEstablishedRelated != 0  (bitwise AND then NEQ 0)
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           []byte{0x06, 0x00, 0x00, 0x00},
			Xor:            []byte{0x00, 0x00, 0x00, 0x00},
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0x00, 0x00, 0x00, 0x00}},
		// ACCEPT
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// skipCaptureSelfExprs builds the nft rule:
//
//	meta l4proto tcp tcp dport capturePort accept
//
// This prevents Envoy (or the pod itself) from re-redirecting a connection that
// is already destined for the capture listener. Without this, a loopback
// connection to :capturePort in the pod netns would be redirected to itself,
// forming a redirect loop.
func skipCaptureSelfExprs(capturePort uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		// tcp dport == capturePort
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: beUint16(capturePort)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// excludePortAcceptExprs builds the rule:
//
//	meta l4proto tcp · tcp dport <port> · accept
//
// Placed ahead of the redirect, it carves a single outbound TCP destination port
// OUT of capture (proposal 022 M2-default, the exclude-outbound-ports annotation):
// connections to <port> bypass the mesh and reach their real destination directly.
func excludePortAcceptExprs(port uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		// tcp dport == port
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: beUint16(port)},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// passthroughMarkAcceptExprs builds the rule:
//
//	meta mark <fwmark> accept
//
// It matches the netfilter fwmark Envoy stamps (via SO_MARK) on the
// passthrough_original_dst cluster's upstream sockets and accepts ahead of the
// redirect, so the proxy's own forwarded egress is not re-captured (proposal 022
// M2-default). The mark is a u32 loaded into the register in host (little-endian)
// byte order, matching the conntrack-state encoding used above.
func passthroughMarkAcceptExprs(fwmark uint32) []expr.Any {
	mark := make([]byte, 4)
	binary.LittleEndian.PutUint32(mark, fwmark)
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: mark},
		&expr.Verdict{Kind: expr.VerdictAccept},
	}
}

// redirectAllTCPExprs builds the broad-redirect rule:
//
//	meta l4proto tcp · ip daddr & 255.0.0.0 != 127.0.0.0 → redirect to capturePort
//
// Unlike captureRedirectExprs, there is no dport match: ALL non-loopback outbound
// TCP is redirected. The loopback exclusion is identical to the scoped rule so
// the pod-local fast-lane (127.x:18081 → proxy) and Envoy's own app-cluster
// connections (127.x:appPort inside netns) are never re-captured.
func redirectAllTCPExprs(capturePort uint16) []expr.Any {
	return []expr.Any{
		// meta l4proto == tcp
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		// ip daddr (IPv4 dest = offset 16, len 4), masked to /8, != 127.0.0.0
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{0xff, 0x00, 0x00, 0x00}, Xor: []byte{0x00, 0x00, 0x00, 0x00}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{127, 0, 0, 0}},
		// redirect to capturePort (no dport constraint)
		&expr.Immediate{Register: 1, Data: beUint16(capturePort)},
		&expr.Redir{RegisterProtoMin: 1},
	}
}
