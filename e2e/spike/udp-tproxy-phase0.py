#!/usr/bin/env python3
"""Proposal 038 Phase 0.

THE ONE QUESTION: does a UDP socket created INSIDE a target netns (via setns) but
READ from another netns still observe the pre-TPROXY destination address?

That is aether's real shape: the proxy is hostNetwork and binds capture listeners
into each pod's netns via network_namespace_filepath, i.e. it creates the socket
in the pod netns and runs its event loop in the host netns.

Envoy reads the destination from IP_PKTINFO's ipi_addr (addressFromMessage), which
Linux fills from the IP header's daddr. So this tests the kernel half directly, on
the socket Envoy would use, without Envoy's config surface in the way.

WHY IT BINDS THE SERVICE PORT, NOT THE CAPTURE PORT: nftables `tproxy` is
prerouting-only, so locally-originated pod egress uses mark-and-divert instead --
which does NOT rewrite the destination port. A listener on any other port receives
nothing (038's third spike row, the one that forces listener-per-port).

CONTROLS, because the previous attempt produced an unusable answer when its
control agreed with its negative result:
  C1  socket created AND read inside the netns   -> must see the VIP
  C2  REDIRECT instead of mark-and-divert        -> must see 127.0.0.1
  MAIN socket created inside, read from outside  -> the question
A run where C1 does not see the VIP proves nothing about MAIN.
"""

import ctypes
import os
import socket
import struct
import subprocess
import sys

libc = ctypes.CDLL("libc.so.6", use_errno=True)
CLONE_NEWNET = 0x40000000
IP_TRANSPARENT = 19

NETNS = "inner"
NETNS_PATH = f"/var/run/netns/{NETNS}"
VIP = "10.250.0.7"          # stands in for a mesh ClusterIP; routed, not assigned
SVC_PORT = 18081
MARK = 0x1
TABLE = 100


class SetupFailed(RuntimeError):
    """A rule did not install. Raised rather than logged: the first attempt at
    this spike printed an nft error, carried on with an EMPTY ruleset, and the
    REDIRECT control silently became a no-capture control that still satisfied
    its own assertion. A control that did not run must abort the run."""


def sh(cmd, check=True):
    r = subprocess.run(cmd, shell=True, capture_output=True, text=True)
    if r.returncode != 0:
        print(f"  ! `{cmd.splitlines()[0]}` -> rc={r.returncode} {r.stderr.strip()}", flush=True)
        if check:
            raise SetupFailed(cmd.splitlines()[0])
    return r


def setns(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        if libc.setns(fd, CLONE_NEWNET) != 0:
            err = ctypes.get_errno()
            raise OSError(err, f"setns({path}): {os.strerror(err)}")
    finally:
        os.close(fd)


def build_netns(capture):
    """Create `inner` with a route for VIP and the given capture mode."""
    sh(f"ip netns del {NETNS}", check=False)
    sh(f"ip netns add {NETNS}")
    sh(f"ip -n {NETNS} link set lo up")
    # A real egress route, so sendto() to the VIP resolves before the mark is set.
    sh(f"ip -n {NETNS} link add dummy0 type dummy")
    sh(f"ip -n {NETNS} addr add 10.251.0.1/24 dev dummy0")
    sh(f"ip -n {NETNS} link set dummy0 up")
    sh(f"ip -n {NETNS} route add default dev dummy0")

    if capture == "divert":
        # Mark and divert: the packet is delivered locally with its IP header
        # INTACT, which is the whole point -- the VIP survives to ipi_addr.
        # `type route` (not filter) so the mark change re-runs the route lookup.
        rules = f"""
table ip div {{
  chain output {{
    type route hook output priority mangle; policy accept;
    ip daddr != 127.0.0.0/8 udp dport {SVC_PORT} meta mark set {MARK} counter
  }}
}}
"""
        sh(f"ip netns exec {NETNS} nft -f - <<'EOF'\n{rules}\nEOF")
        sh(f"ip netns exec {NETNS} ip rule add fwmark {MARK} lookup {TABLE}")
        sh(f"ip netns exec {NETNS} ip route add local default dev lo table {TABLE}")
    elif capture == "redirect":
        # The status quo. REDIRECT REWRITES the destination, so ipi_addr can only
        # ever report the post-NAT address. This is the negative control.
        rules = f"""
table ip red {{
  chain output {{
    type nat hook output priority dstnat; policy accept;
    ip daddr != 127.0.0.0/8 udp dport {SVC_PORT} counter redirect to :{SVC_PORT}
  }}
}}
"""
        sh(f"ip netns exec {NETNS} nft -f - <<'EOF'\n{rules}\nEOF")
    elif capture == "none":
        return
    else:
        raise ValueError(capture)

    # The rule must actually be THERE. `nft list ruleset` in the netns is the
    # only honest confirmation; a printed error with an empty ruleset is how the
    # first run lost its negative control.
    listed = sh(f"ip netns exec {NETNS} nft list ruleset", check=True).stdout
    if "dport 18081" not in listed:
        raise SetupFailed(f"{capture}: ruleset installed nothing:\n{listed}")
    print(f"  [rules for {capture} confirmed installed]", flush=True)


def make_socket():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    # IP_TRANSPARENT is what lets a socket accept a datagram addressed to an IP
    # it does not own. A missing capability fails LOUDLY here.
    s.setsockopt(socket.IPPROTO_IP, IP_TRANSPARENT, 1)
    s.setsockopt(socket.IPPROTO_IP, socket.IP_PKTINFO, 1)
    s.bind(("0.0.0.0", SVC_PORT))
    return s


def read_dst(s, timeout=4.0):
    """Return (payload, peer, ipi_addr, ipi_spec_dst) or None on timeout."""
    s.settimeout(timeout)
    try:
        data, anc, _flags, peer = s.recvmsg(2048, socket.CMSG_SPACE(64))
    except socket.timeout:
        return None
    ipi_addr = ipi_spec = None
    for level, typ, cdata in anc:
        if level == socket.IPPROTO_IP and typ == socket.IP_PKTINFO:
            # struct in_pktinfo { int ipi_ifindex; in_addr ipi_spec_dst; in_addr ipi_addr; }
            _ifidx, spec, addr = struct.unpack("=i4s4s", cdata[:12])
            ipi_spec = socket.inet_ntoa(spec)
            ipi_addr = socket.inet_ntoa(addr)
    return data, peer, ipi_addr, ipi_spec


SENDER = (
    "python3 -c \"import socket;s=socket.socket(2,2);"
    f"s.sendto(b'phase0',('{VIP}',{SVC_PORT}))\""
)


def send_from_inner():
    return sh(f"ip netns exec {NETNS} {SENDER}", check=False)


def report(name, res):
    if res is None:
        print(f"  {name}: NO DATAGRAM DELIVERED", flush=True)
        return None
    _data, peer, ipi_addr, ipi_spec = res
    verdict = "VIP PRESERVED" if ipi_addr == VIP else f"LOST (saw {ipi_addr})"
    print(f"  {name}: ipi_addr={ipi_addr}  ipi_spec_dst={ipi_spec}  from={peer[0]}  -> {verdict}",
          flush=True)
    return ipi_addr


def case_inside(capture):
    """C1: create AND read the socket inside the netns."""
    build_netns(capture)
    outer = os.open("/proc/self/ns/net", os.O_RDONLY)
    try:
        setns(NETNS_PATH)
        s = make_socket()
        # Sender must run in the netns too; we are already in it, so a bare fork.
        pid = os.fork()
        if pid == 0:
            try:
                c = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
                c.sendto(b"phase0", (VIP, SVC_PORT))
            finally:
                os._exit(0)
        os.waitpid(pid, 0)
        res = read_dst(s)
        s.close()
        return res
    finally:
        if libc.setns(outer, CLONE_NEWNET) != 0:
            print("  ! could not setns back to the original netns", flush=True)
        os.close(outer)


def case_bound_from_outside(capture):
    """MAIN: create the socket inside the netns, read it from outside."""
    build_netns(capture)
    outer = os.open("/proc/self/ns/net", os.O_RDONLY)
    setns(NETNS_PATH)
    s = make_socket()
    if libc.setns(outer, CLONE_NEWNET) != 0:
        os.close(outer)
        raise OSError("setns back failed")
    os.close(outer)
    # We are back outside; drive the sender into the netns via `ip netns exec`.
    send_from_inner()
    res = read_dst(s)
    s.close()
    return res


def main():
    print(f"kernel={os.uname().release}  node={os.environ.get('NODE_NAME','?')}", flush=True)
    print(f"VIP={VIP} service_port={SVC_PORT}\n", flush=True)

    print("C1  socket created AND read INSIDE the netns (mark-and-divert)")
    c1 = report("C1", case_inside("divert"))

    print("\nC2  REDIRECT instead of mark-and-divert (negative control:")
    print("    proves ipi_addr reports the POST-NAT header, i.e. that the")
    print("    measurement tracks reality and does not echo the sent address)")
    c2 = report("C2", case_inside("redirect"))

    print("\nC3  no capture rule at all")
    c3 = report("C3", case_bound_from_outside("none"))

    print("\nMAIN  socket created INSIDE via setns, read from OUTSIDE  <-- the question")
    main_addr = report("MAIN", case_bound_from_outside("divert"))

    print("\n================ VERDICT ================", flush=True)
    if c1 != VIP:
        print("RIG BROKEN: C1 did not preserve the VIP, so MAIN proves nothing.")
        print("This is the same failure mode as the 2026-09-23 attempt. Do not")
        print("read MAIN as an answer.")
        rc = 2
    elif c2 is None:
        print("RIG BROKEN: C2 (REDIRECT) delivered NOTHING, so the negative")
        print("control did not run. C3 proves delivery needs a rule; only C2")
        print("proves ipi_addr tracks the REAL header rather than echoing what")
        print("was sent. Without it MAIN proves nothing.")
        rc = 2
    elif c2 == VIP:
        print("RIG BROKEN: C2 (REDIRECT) also 'preserved' the VIP, which is")
        print("impossible -- REDIRECT rewrites the header. The test cannot")
        print("distinguish, so MAIN proves nothing.")
        rc = 2
    elif main_addr == VIP:
        print("ANSWER: YES. A transparent socket created inside a pod netns via")
        print("setns and read from another netns DOES observe the pre-TPROXY")
        print("destination. Proposal 038 Phase 1 is unblocked.")
        rc = 0
    elif main_addr is None:
        print("ANSWER: NO -- nothing was delivered at all to the")
        print("externally-read socket, while C1 (same rules, in-netns read)")
        print("worked. 038 stops here and #916 stands as a documented limit.")
        rc = 1
    else:
        print(f"ANSWER: NO. MAIN saw {main_addr}, not the VIP, while C1 saw the")
        print("VIP under the same rules. The netns-bound socket loses the")
        print("original destination. 038 stops and #916 stands as a limit.")
        rc = 1
    print(f"C1={c1} C2={c2} C3={c3} MAIN={main_addr}", flush=True)
    sh(f"ip netns del {NETNS}", check=False)
    return rc


if __name__ == "__main__":
    sys.exit(main())
