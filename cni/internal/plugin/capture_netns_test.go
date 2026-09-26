//go:build linux

package plugin

// The proposal 038 integration gate: the divert ruleset against a REAL kernel,
// in a real network namespace, with real sockets. Everything the unit tests pin
// at the expression level is exercised here for the property that matters --
// packets go where the rules say -- and the one rule whose absence breaks every
// pod (`ct direction reply accept`) is REMOVED mid-test to prove it is
// load-bearing on this kernel, not just present.
//
// It needs root (netns creation, CAP_NET_ADMIN for nft/tproxy/IP_TRANSPARENT)
// and the nft_tproxy module, so it SKIPS when not root and is run in CI by the
// `netns` job, which builds the binary with Bazel and runs it under sudo. Under
// root it never skips: a kernel without tproxy FAILS, because a test that
// skips there would pass the exact deployment that ships uncaptured pods.
// Locally, `unshare -Urn --map-root-user <binary> -test.v` is root enough (see
// the BUILD file).
//
// This is the in-tree successor of e2e/spike/tproxy-phase0b.py, which measured
// the same arms on a Talos node before any of this was written.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	meshconst "aethermesh.dev/common/constants/mesh"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	// netnsVIP is a non-local "ClusterIP" nothing in the rig owns; only the
	// divert can make a packet addressed to it arrive anywhere.
	netnsVIP = "10.250.0.7"
	// netnsPodAddr / netnsPeerAddr are the two ends of the veth between the
	// "pod" netns and a "peer" netns (the inbound client for the S1 arm).
	netnsPodAddr  = "10.250.1.1"
	netnsPeerAddr = "10.250.1.2"
	netnsDialWait = 1500 * time.Millisecond
)

// TestCaptureDivertInNetns is the gate. Arms, in order:
//
//	programmed  the table, both chains, the fwmark rule and the local route exist
//	T1          TCP to VIP:8080 (redirect-all) lands on the transparent :18001
//	            listener with local address VIP:8080 -- the header was untouched
//	U1          UDP to VIP:18082 lands on the transparent :18082 socket with
//	            IP_PKTINFO dst = VIP; the reply sent FROM the VIP reaches a
//	            connected client (the reply's source must be what was dialled)
//	T2          TCP to VIP:53 is NOT diverted (the :53 accept is ahead of the marks)
//	X1          TCP to VIP:5432, the per-pod excluded port, is NOT diverted
//	S1          an inbound TCP connection from the peer netns to the pod's own
//	            server succeeds under redirect-all (the server's SYN-ACK is a
//	            reply and is exempt) -- then the ct rule is deleted and the same
//	            connection FAILS, proving the rule is what makes S1 pass
func TestCaptureDivertInNetns(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (netns creation, CAP_NET_ADMIN); the `netns` CI job runs this binary under sudo")
	}
	logger := zap.NewNop()

	pod := newTestNetns(t)
	peer := newTestNetns(t)

	// Rig: lo up in both; a veth between them with addresses; a route for the
	// VIP's /24 out of veth0 so an UNDIVERTED packet to the VIP leaves the pod
	// netns toward a peer that does not own it (ARP never resolves, the connect
	// times out) -- the "not captured" outcome, distinguishable from "captured".
	require.NoError(t, withPodNetns(pod.path, func() error {
		if err := linkUp("lo"); err != nil {
			return err
		}
		veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "veth0"}, PeerName: "veth1"}
		if err := netlink.LinkAdd(veth); err != nil {
			return fmt.Errorf("veth: %w", err)
		}
		v1, err := netlink.LinkByName("veth1")
		if err != nil {
			return err
		}
		if err := netlink.LinkSetNsFd(v1, int(peer.fd.Fd())); err != nil {
			return fmt.Errorf("move veth1 to peer: %w", err)
		}
		if err := addrUp("veth0", netnsPodAddr+"/24"); err != nil {
			return err
		}
		v0, err := netlink.LinkByName("veth0")
		if err != nil {
			return err
		}
		_, vipNet, _ := net.ParseCIDR("10.250.0.0/24")
		return netlink.RouteAdd(&netlink.Route{Dst: vipNet, LinkIndex: v0.Attrs().Index})
	}))
	require.NoError(t, withPodNetns(peer.path, func() error {
		if err := linkUp("lo"); err != nil {
			return err
		}
		return addrUp("veth1", netnsPeerAddr+"/24")
	}))

	// The thing under test: redirect-all on, 5432 excluded.
	err := installCaptureDivert(pod.path, true, []uint16{5432}, nil, logger)
	require.NoError(t, err, "installCaptureDivert rejected on this kernel: if the error names tproxy, nft_tproxy is missing -- that is a FAIL, not a skip, because the same kernel would ship uncaptured pods")

	t.Run("programmed", func(t *testing.T) {
		require.NoError(t, withPodNetns(pod.path, func() error {
			c, err := nftables.New()
			if err != nil {
				return err
			}
			table, output, divert, err := findCaptureChains(c)
			if err != nil {
				return err
			}
			out, err := c.GetRules(table, output)
			if err != nil {
				return err
			}
			// 1 passthrough + 1 ct + 3 ranges + 2 dns + 2 exclude(5432 both) + 3 marks + 1 any-tcp
			if got, want := len(out), 13; got != want {
				return fmt.Errorf("output chain has %d rules, want %d", got, want)
			}
			if _, ok := out[1].Exprs[0].(*expr.Ct); !ok {
				return fmt.Errorf("rule 2 of the output chain is not the ct direction accept")
			}
			div, err := c.GetRules(table, divert)
			if err != nil {
				return err
			}
			if len(div) != 2 {
				return fmt.Errorf("divert chain has %d rules, want 2", len(div))
			}
			// google/nftables v0.3.0 can MARSHAL expr.TProxy but has no decoder
			// for it, so on readback the tcp rule comes back without its tproxy
			// statement (8 exprs, not 9). Whether tproxy is really in the
			// kernel's rule is what arm T1 proves; here, only the chain's shape.
			if got, want := len(div[0].Exprs), len(tproxyTCPExprs(0, 0))-1; got != want {
				return fmt.Errorf("the tcp divert rule read back with %d exprs, want %d (9 written, minus the undecodable tproxy)", got, want)
			}

			rules, err := netlink.RuleList(unix.AF_INET)
			if err != nil {
				return err
			}
			var fwmark bool
			for _, r := range rules {
				if r.Mark == uint32(meshconst.CaptureDivertFwMark) && r.Table == meshconst.CaptureDivertRouteTable {
					fwmark = true
				}
			}
			if !fwmark {
				return fmt.Errorf("no `ip rule fwmark %#x lookup %d`", meshconst.CaptureDivertFwMark, meshconst.CaptureDivertRouteTable)
			}
			routes, err := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: meshconst.CaptureDivertRouteTable}, netlink.RT_FILTER_TABLE)
			if err != nil {
				return err
			}
			var local bool
			for _, r := range routes {
				if r.Type == unix.RTN_LOCAL && (r.Dst == nil || r.Dst.IP.Equal(net.IPv4zero)) {
					local = true
				}
			}
			if !local {
				return fmt.Errorf("table %d has no `local default dev lo`: %+v", meshconst.CaptureDivertRouteTable, routes)
			}
			return nil
		}))
	})

	// A transparent TCP listener on the capture port, alive for the rest of
	// the test (S1 needs it present, as production has it).
	tcpLn := transparentTCPListener(t, pod.path, meshconst.ProxyCapturePort)
	defer tcpLn.Close()

	t.Run("T1 tcp to VIP:8080 is diverted, header intact", func(t *testing.T) {
		accepted := make(chan net.Addr, 1)
		go func() {
			c, err := tcpLn.Accept()
			if err != nil {
				return
			}
			accepted <- c.LocalAddr()
			_ = c.Close()
		}()
		require.NoError(t, dialIn(pod.path, "tcp", netnsVIP+":8080"), "the SYN never reached the transparent listener: the divert is not delivering locally")
		select {
		case la := <-accepted:
			require.Equal(t, netnsVIP+":8080", la.String(), "the accepted socket's local address must be the ORIGINAL destination (what Envoy's filter-chain match and original_dst read); tproxy must not rewrite the header")
		case <-time.After(netnsDialWait):
			t.Fatal("dial succeeded but nothing was accepted on :18001")
		}
	})

	t.Run("U1 udp to VIP:18082 is diverted and the VIP-sourced reply returns", func(t *testing.T) {
		// Bind BEFORE the client sends (a datagram to an unbound port is
		// dropped, and "no reply" would then look like a divert failure);
		// only the receive loop runs concurrently.
		fd := bindTransparentUDP(t, pod.path, meshconst.ProxyL4OutboundPort)
		defer unix.Close(fd)
		got := make(chan string, 1)
		srvErr := make(chan error, 1)
		go func() { srvErr <- udpEchoOnce(fd, got) }()
		clientErr := withPodNetns(pod.path, func() error {
			c, err := net.DialTimeout("udp", fmt.Sprintf("%s:%d", netnsVIP, meshconst.ProxyL4OutboundPort), netnsDialWait)
			if err != nil {
				return err
			}
			defer c.Close()
			if _, err := c.Write([]byte("ping")); err != nil {
				return err
			}
			_ = c.SetReadDeadline(time.Now().Add(netnsDialWait))
			buf := make([]byte, 16)
			n, err := c.Read(buf)
			if err != nil {
				return fmt.Errorf("no reply on the CONNECTED client: %w (a reply not sourced from %s:%d is dropped by the kernel before the client sees it)", err, netnsVIP, meshconst.ProxyL4OutboundPort)
			}
			if string(buf[:n]) != "pong" {
				return fmt.Errorf("reply %q, want pong", buf[:n])
			}
			return nil
		})
		// Report the server's view BEFORE judging the client's: "no reply" has
		// two very different causes (the datagram never arrived, or the
		// VIP-sourced reply was refused on send) and only the server knows.
		var dst string
		select {
		case dst = <-got:
		case <-time.After(netnsDialWait):
		}
		var sErr error
		select {
		case sErr = <-srvErr:
		case <-time.After(netnsDialWait):
			sErr = errors.New("server did not finish")
		}
		require.NotEmpty(t, dst, "the datagram never reached the transparent :18082 socket (server: %v)", sErr)
		require.Equal(t, netnsVIP, dst, "IP_PKTINFO ipi_addr on the transparent socket must be the VIP the client dialled (what Envoy's udp_proxy matcher keys on)")
		require.NoError(t, sErr, "server side")
		require.NoError(t, clientErr, "client side")
	})

	t.Run("T2 tcp to VIP:53 is not diverted", func(t *testing.T) {
		err := dialIn(pod.path, "tcp", netnsVIP+":53")
		require.Error(t, err, "a :53 dial was CAPTURED: the dns accept must precede the marks, or the mesh-DNS DNAT at nat priority never sees it")
	})

	t.Run("X1 tcp to VIP:5432 (excluded port) is not diverted", func(t *testing.T) {
		err := dialIn(pod.path, "tcp", netnsVIP+":5432")
		require.Error(t, err, "an excluded port was CAPTURED under redirect-all")
	})

	t.Run("S1 inbound reply is exempt, and only because of the ct rule", func(t *testing.T) {
		srv := plainTCPListener(t, pod.path, 8080)
		defer srv.Close()
		go func() {
			for {
				c, err := srv.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()

		require.NoError(t, dialIn(peer.path, "tcp", netnsPodAddr+":8080"),
			"an inbound connection to the pod's own server failed under redirect-all WITH the ct rule: the server's SYN-ACK was diverted")

		// Now delete the ct direction reply rule and nothing else.
		require.NoError(t, withPodNetns(pod.path, func() error {
			c, err := nftables.New()
			if err != nil {
				return err
			}
			table, output, _, err := findCaptureChains(c)
			if err != nil {
				return err
			}
			rules, err := c.GetRules(table, output)
			if err != nil {
				return err
			}
			for _, r := range rules {
				if ct, ok := r.Exprs[0].(*expr.Ct); ok && ct.Key == expr.CtKeyDIRECTION {
					if err := c.DelRule(r); err != nil {
						return err
					}
					return c.Flush()
				}
			}
			return errors.New("ct direction rule not found in the output chain")
		}))

		err := dialIn(peer.path, "tcp", netnsPodAddr+":8080")
		require.Error(t, err, "the inbound connection SUCCEEDED without the ct direction reply rule: on this kernel the rule is not load-bearing, so this test is not exercising the failure it exists for (was the SYN-ACK really marked? is redirect-all on?)")
	})
}

// testNetns is a network namespace kept alive by a parked, thread-locked
// goroutine; path is what withPodNetns opens.
type testNetns struct {
	path string
	fd   *os.File
	stop chan struct{}
}

// newTestNetns creates a fresh netns via unshare on a locked thread and parks
// that thread for the test's lifetime, so /proc/self/task/<tid>/ns/net stays
// valid. No netns library, no mount: the same primitive the CNI itself relies
// on (the runtime hands it a path; here the path is a task's own ns link).
func newTestNetns(t *testing.T) *testNetns {
	t.Helper()
	ns := &testNetns{stop: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread dies with the goroutine
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			ready <- fmt.Errorf("unshare(CLONE_NEWNET): %w", err)
			return
		}
		ns.path = fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
		f, err := os.Open(ns.path)
		if err != nil {
			ready <- err
			return
		}
		ns.fd = f
		ready <- nil
		<-ns.stop
		_ = f.Close()
	}()
	require.NoError(t, <-ready)
	t.Cleanup(func() { close(ns.stop) })
	return ns
}

func linkUp(name string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(l)
}

func addrUp(name, cidr string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	a, err := netlink.ParseAddr(cidr)
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(l, a); err != nil {
		return err
	}
	return netlink.LinkSetUp(l)
}

// findCaptureChains resolves the divert table and its two chains in the
// current netns.
func findCaptureChains(c *nftables.Conn) (table *nftables.Table, output, divert *nftables.Chain, err error) {
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, tb := range tables {
		if tb.Name == captureTableName {
			table = tb
		}
	}
	if table == nil {
		return nil, nil, nil, fmt.Errorf("table %s not found", captureTableName)
	}
	chains, err := c.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, ch := range chains {
		if ch.Table.Name != captureTableName {
			continue
		}
		switch ch.Name {
		case "output":
			output = ch
		case "divert":
			divert = ch
		}
	}
	if output == nil || divert == nil {
		return nil, nil, nil, fmt.Errorf("chains missing: output=%v divert=%v", output != nil, divert != nil)
	}
	return table, output, divert, nil
}

// dialIn dials from inside the netns with a short timeout and closes the
// connection. The dial itself must happen on the setns'd thread; the socket
// then belongs to that netns regardless of which goroutine touches it.
func dialIn(netnsPath, network, addr string) error {
	return withPodNetns(netnsPath, func() error {
		c, err := net.DialTimeout(network, addr, netnsDialWait)
		if err != nil {
			return err
		}
		return c.Close()
	})
}

func setIPTransparent(_, _ string, rc syscall.RawConn) error {
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TRANSPARENT, 1)
	}); err != nil {
		return err
	}
	return serr
}

// transparentTCPListener binds 0.0.0.0:port with IP_TRANSPARENT inside the
// netns -- what Envoy's capture listener does with `transparent: true`.
func transparentTCPListener(t *testing.T, netnsPath string, port int) net.Listener {
	t.Helper()
	var ln net.Listener
	require.NoError(t, withPodNetns(netnsPath, func() error {
		lc := net.ListenConfig{Control: setIPTransparent}
		l, err := lc.Listen(context.Background(), "tcp4", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			return err
		}
		ln = l
		return nil
	}))
	return ln
}

func plainTCPListener(t *testing.T, netnsPath string, port int) net.Listener {
	t.Helper()
	var ln net.Listener
	require.NoError(t, withPodNetns(netnsPath, func() error {
		l, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			return err
		}
		ln = l
		return nil
	}))
	return ln
}

// bindTransparentUDP binds a UDP socket on 0.0.0.0:port inside the netns with
// IP_TRANSPARENT and IP_PKTINFO -- what Envoy's udp_proxy listener does with
// `transparent: true` -- and returns the raw fd.
func bindTransparentUDP(t *testing.T, netnsPath string, port int) int {
	t.Helper()
	var fd int
	require.NoError(t, withPodNetns(netnsPath, func() error {
		s, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
		if err != nil {
			return err
		}
		if err := unix.SetsockoptInt(s, unix.IPPROTO_IP, unix.IP_TRANSPARENT, 1); err != nil {
			return err
		}
		if err := unix.SetsockoptInt(s, unix.IPPROTO_IP, unix.IP_PKTINFO, 1); err != nil {
			return err
		}
		if err := unix.Bind(s, &unix.SockaddrInet4{Port: port}); err != nil {
			return err
		}
		fd = s
		return nil
	}))
	return fd
}

// udpEchoOnce receives one datagram on fd, reports its destination address on
// got, and replies "pong" FROM that destination address (ipi_spec_dst) -- the
// exact reply shape Envoy's udp_proxy produces: source port = the socket's
// bound port, source address = the received datagram's destination.
func udpEchoOnce(fd int, got chan<- string) error {
	tv := unix.NsecToTimeval(int64(netnsDialWait))
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	buf := make([]byte, 64)
	oob := make([]byte, 128)
	n, oobn, _, from, err := unix.Recvmsg(fd, buf, oob, 0)
	if err != nil {
		return fmt.Errorf("recvmsg: %w", err)
	}
	cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return err
	}
	var dst net.IP
	for _, m := range cmsgs {
		if m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_PKTINFO {
			pi := (*unix.Inet4Pktinfo)(unsafe.Pointer(&m.Data[0]))
			dst = net.IPv4(pi.Addr[0], pi.Addr[1], pi.Addr[2], pi.Addr[3])
		}
	}
	if dst == nil {
		return errors.New("no IP_PKTINFO on the received datagram")
	}
	got <- dst.String()
	if string(buf[:n]) != "ping" {
		return fmt.Errorf("received %q, want ping", buf[:n])
	}
	// Reply from the VIP: ipi_spec_dst is the SOURCE address selector on send.
	var spec [4]byte
	copy(spec[:], dst.To4())
	reply := unix.PktInfo4(&unix.Inet4Pktinfo{Spec_dst: spec})
	return unix.Sendmsg(fd, []byte("pong"), reply, from, 0)
}
