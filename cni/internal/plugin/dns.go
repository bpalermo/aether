package plugin

import (
	"fmt"

	commonconstants "github.com/bpalermo/aether/common/constants"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// dnsRedirectTableName is the nft table the DNS redirect rules live in (pod netns).
const dnsRedirectTableName = "aether_dns_capture"

// installDNSRedirect programs, inside the pod's network namespace, an nftables REDIRECT
// of the pod's outbound DNS (UDP+TCP dst-port 53, non-loopback) to the pod-local
// mesh-DNS listener on ProxyDNSCapturePort (proposal 018, mesh-global FQDN). Envoy's
// dns_filter answers <svc>.<meshDomain> and forwards the rest to the upstream resolver.
//
// No loop-prevention is needed: the node agent (and the Envoy it supervises) is
// host-network, so the dns_filter's forward to kube-dns originates in the HOST netns
// and never traverses this pod-netns redirect. The loopback exclusion leaves any
// pod-local resolver (127.x) alone. The rule dies with the netns; best-effort.
func installDNSRedirect(netnsPath string, logger *zap.Logger) error {
	return withPodNetns(netnsPath, func() error { return programDNSRedirect(logger) })
}

func programDNSRedirect(logger *zap.Logger) error {
	dnsPort := uint16(commonconstants.ProxyDNSCapturePort)

	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables netlink: %w", err)
	}

	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: dnsRedirectTableName})
	chain := c.AddChain(&nftables.Chain{
		Name:     "output",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityNATDest,
	})
	// DNS is UDP-primary with a TCP fallback (large answers / zone transfers), so
	// redirect both.
	c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: dnsRedirectExprs(unix.IPPROTO_UDP, dnsPort)})
	c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: dnsRedirectExprs(unix.IPPROTO_TCP, dnsPort)})

	if err := c.Flush(); err != nil {
		return fmt.Errorf("apply mesh-DNS redirect (nft flush): %w", err)
	}
	logger.Info("installed mesh-DNS redirect", zap.Uint16("dns_port", dnsPort))
	return nil
}

// dnsRedirectExprs builds: meta l4proto <proto> · ip daddr & /8 != 127.0.0.0 ·
// <proto> dport 53 · redirect to redirectPort. The transport dest port is at offset 2
// for both UDP and TCP.
func dnsRedirectExprs(proto byte, redirectPort uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{0xff, 0x00, 0x00, 0x00}, Xor: []byte{0x00, 0x00, 0x00, 0x00}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{127, 0, 0, 0}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: beUint16(53)},
		&expr.Immediate{Register: 1, Data: beUint16(redirectPort)},
		&expr.Redir{RegisterProtoMin: 1},
	}
}
