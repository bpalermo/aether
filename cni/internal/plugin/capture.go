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
func installCaptureRedirect(netnsPath string, logger *zap.Logger) error {
	return withPodNetns(netnsPath, func() error { return programCaptureRedirect(logger) })
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
func programCaptureRedirect(logger *zap.Logger) error {
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
