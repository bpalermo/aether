package plugin

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"

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
	target, err := os.Open(netnsPath)
	if err != nil {
		return fmt.Errorf("opening netns %s: %w", netnsPath, err)
	}
	defer func() { _ = target.Close() }()

	runtime.LockOSThread()
	origin, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("opening current netns: %w", err)
	}
	defer func() { _ = origin.Close() }()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("entering netns %s: %w", netnsPath, err)
	}

	progErr := programCaptureRedirect(logger)

	if err := unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET); err != nil {
		// Thread poisoned (stuck in the pod netns): keep it locked and let the
		// goroutine's thread be destroyed rather than reused.
		return fmt.Errorf("restoring host netns: %w", err)
	}
	runtime.UnlockOSThread()
	return progErr
}

// programCaptureRedirect adds the nat/output REDIRECT rule in the CURRENT netns.
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
	c.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: captureRedirectExprs(meshPort, capturePort),
	})

	if err := c.Flush(); err != nil {
		return fmt.Errorf("apply capture redirect (nft flush): %w", err)
	}
	logger.Info("installed transparent-capture redirect",
		zap.Uint16("mesh_port", meshPort), zap.Uint16("capture_port", capturePort))
	return nil
}

// captureRedirectExprs builds the rule:
//
//	meta l4proto tcp
//	ip daddr & 255.0.0.0 != 127.0.0.0      (skip the loopback fast-lane)
//	tcp dport <meshPort>
//	redirect to :<capturePort>
func captureRedirectExprs(meshPort, capturePort uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		// ip daddr (IPv4 dest = offset 16, len 4), masked to /8, != 127.0.0.0
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{0xff, 0x00, 0x00, 0x00}, Xor: []byte{0x00, 0x00, 0x00, 0x00}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{127, 0, 0, 0}},
		// tcp dport == meshPort (transport dest = offset 2, len 2, big-endian)
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
