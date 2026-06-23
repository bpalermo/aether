package plugin

import (
	"fmt"
	"net"

	commonconstants "github.com/bpalermo/aether/common/constants"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// dnsNATTableName is the nft table the DNS DNAT rules live in (pod netns).
const dnsNATTableName = "aether_dns_capture"

// installDNSRedirect programs, inside the pod's network namespace, an nftables DNAT of
// the pod's outbound DNS (UDP+TCP dst-port 53, non-loopback) directly to the node
// agent's mesh-DNS resolver at hostIP:ProxyDNSResolverPort (proposal 018, mesh-global
// FQDN). No per-pod Envoy DNS listener is needed: the agent is host-network, so the
// pod reaches its resolver via the node IP, and conntrack rewrites the reply's source
// back to the pod's original nameserver.
//
// No loop-prevention is needed: the resolver's own forward to kube-dns originates in
// the HOST netns and never traverses this pod-netns rule. The loopback exclusion
// leaves a pod-local resolver (127.x) alone. The rule dies with the netns; best-effort.
func installDNSRedirect(netnsPath, hostIP string, logger *zap.Logger) error {
	ip := net.ParseIP(hostIP).To4()
	if ip == nil {
		return fmt.Errorf("invalid host IP %q for mesh-DNS DNAT", hostIP)
	}
	return withPodNetns(netnsPath, func() error { return programDNSDNAT(ip, logger) })
}

func programDNSDNAT(hostIP net.IP, logger *zap.Logger) error {
	port := uint16(commonconstants.ProxyDNSResolverPort)

	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables netlink: %w", err)
	}

	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: dnsNATTableName})
	chain := c.AddChain(&nftables.Chain{
		Name:     "output",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityNATDest,
	})
	// DNS is UDP-primary with a TCP fallback (large answers), so DNAT both.
	c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: dnsDNATExprs(unix.IPPROTO_UDP, hostIP, port)})
	c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: dnsDNATExprs(unix.IPPROTO_TCP, hostIP, port)})

	if err := c.Flush(); err != nil {
		return fmt.Errorf("apply mesh-DNS DNAT (nft flush): %w", err)
	}
	logger.Info("installed mesh-DNS DNAT", zap.String("resolver", fmt.Sprintf("%s:%d", hostIP, port)))
	return nil
}

// dnsDNATExprs builds: meta l4proto <proto> · ip daddr & /8 != 127.0.0.0 · <proto>
// dport 53 · dnat to hostIP:port. The transport dest port is at offset 2 for UDP+TCP.
func dnsDNATExprs(proto byte, hostIP net.IP, port uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{proto}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{0xff, 0x00, 0x00, 0x00}, Xor: []byte{0x00, 0x00, 0x00, 0x00}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{127, 0, 0, 0}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: beUint16(53)},
		// dnat to hostIP (reg1) : port (reg2)
		&expr.Immediate{Register: 1, Data: hostIP},
		&expr.Immediate{Register: 2, Data: beUint16(port)},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2},
	}
}
