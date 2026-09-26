#!/usr/bin/env python3
"""TPROXY capture, Phase 0b: the FULL ruleset, on a node, both transports.

Phase 0 (udp-tproxy-phase0.py) proved one thing: a transparent UDP socket
created inside a pod netns via setns and read from outside observes the
pre-divert destination. It did not install a prerouting `tproxy` rule, did not
run TCP, did not send a reply, and did not exercise the rule that keeps the
design from breaking every pod. This does all of that, with the exact ruleset
the CNI will program, so PR 3 is written against a shape that has run on a
Talos kernel rather than a shape that looks right.

TOPOLOGY
  outer  = this pod's own netns; the "host" side where the proxy's event loop
           runs. Sockets are CREATED in `inner` via setns and read from here.
  inner  = the "pod" netns: lo, dummy0 10.251.0.1/24 with the default route,
           veth vi 10.252.0.1/24 to `peer`. The ruleset lives here.
  peer   = a client netns on the other end of the veth (vp 10.252.0.2/24),
           so an INBOUND connection to a server in `inner` can be made.
  VIP    = 10.250.0.7, a stand-in ClusterIP: routed (via the default), never
           assigned anywhere.

ARMS
  T1  TCP capture on ONE listener. Client in `inner` connects to VIP:8080.
      Transparent listener bound 0.0.0.0:18001 in `inner` must accept it with
      getsockname() == VIP:8080 (the header survived; delivery-port rewrite via
      `tproxy to :18001` is fine for TCP). It replies; the client must receive
      the reply, which proves the src-VIP reply path with no conntrack NAT.
      Also records what getsockopt(SO_ORIGINAL_DST) returns on that socket.
  T2  TCP to VIP:53 is NOT diverted (the :53 accept precedes the marks so the
      DNS DNAT at nat priority still sees it). The 18001 listener must accept
      nothing; the client must time out rather than be captured.
  U1  UDP reply from the VIP. Transparent UDP socket bound 0.0.0.0:18082 in
      `inner`, read from outer. A CONNECTED client in `inner` sends to
      VIP:18082 and must receive the reply -- which the server sends with
      IP_PKTINFO spec_dst=VIP so its source is VIP:18082, the only source a
      connected socket will accept.
  S1  THE LOAD-BEARING ONE. A plain TCP server in `inner`; a client in `peer`
      connects to it over the veth. Under redirect-all, the server's SYN-ACK is
      locally-generated TCP with dport = the client's ephemeral port. Without
      `ct direction reply accept` it is marked, looped to lo, `tproxy`'d to the
      18001 LISTEN socket, and answered with a reset: every inbound connection
      to every redirect-all pod would die. So S1 runs TWICE: with the rule
      (must connect) and with the rule removed (must FAIL). A run in which S1
      passes without the rule means the rig is not exercising the hazard and
      proves nothing.

EXIT  0 = every arm as designed, including S1's red state
      1 = the design does not work on this kernel (an arm failed with the rule)
      2 = rig broken -- a control did not run or did not discriminate
"""

import ctypes
import os
import socket
import struct
import subprocess
import sys
import time

libc = ctypes.CDLL("libc.so.6", use_errno=True)
CLONE_NEWNET = 0x40000000
IP_TRANSPARENT = 19
SO_ORIGINAL_DST = 80

INNER, PEER = "inner", "peer"
NS = lambda n: f"/var/run/netns/{n}"  # noqa: E731

VIP = "10.250.0.7"
INNER_IP, PEER_IP = "10.252.0.1", "10.252.0.2"
DUMMY_IP = "10.251.0.1"
MARK, TABLE = 0xAE71, 100
CAPTURE_PORT, UDP_PORT = 18001, 18082
SERVER_PORT = 9090


class SetupFailed(RuntimeError):
    """A rule or link did not install. Raised, never logged-and-continued: the
    first Phase 0 run printed an nft error, carried on with an empty ruleset,
    and its negative control silently became a no-op."""


def sh(cmd, check=True):
    r = subprocess.run(cmd, shell=True, capture_output=True, text=True)
    if r.returncode != 0:
        print(f"  ! `{cmd.splitlines()[0][:90]}` rc={r.returncode} {r.stderr.strip()[:300]}", flush=True)
        if check:
            raise SetupFailed(cmd.splitlines()[0])
    return r


def setns(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        if libc.setns(fd, CLONE_NEWNET) != 0:
            e = ctypes.get_errno()
            raise OSError(e, f"setns({path}): {os.strerror(e)}")
    finally:
        os.close(fd)


class InNetns:
    """Context manager: run the body with the calling thread in `path`, then
    return to the original netns. Sockets created inside stay bound to it."""

    def __init__(self, path):
        self.path = path

    def __enter__(self):
        self.orig = os.open("/proc/self/ns/net", os.O_RDONLY)
        setns(self.path)
        return self

    def __exit__(self, *exc):
        if libc.setns(self.orig, CLONE_NEWNET) != 0:
            print("  ! could not setns back to the original netns", flush=True)
        os.close(self.orig)


# ----------------------------------------------------------------------------
# topology + ruleset
# ----------------------------------------------------------------------------

RULESET = f"""
table ip aether_capture {{
  chain output {{
    type route hook output priority mangle; policy accept;
    meta mark 0xae7e accept
    ct direction reply accept comment "CT_DIRECTION_REPLY"
    ip daddr 127.0.0.0/8 accept
    ip daddr 169.254.0.0/16 accept
    ip daddr 224.0.0.0/4 accept
    tcp dport 53 accept
    udp dport 53 accept
    udp dport {UDP_PORT} meta mark set {MARK}
    tcp dport {{ 18081, 18082 }} meta mark set {MARK}
    meta l4proto tcp meta mark set {MARK}
  }}
  chain divert {{
    type filter hook prerouting priority mangle; policy accept;
    iif lo meta mark {MARK} meta l4proto tcp tproxy to :{CAPTURE_PORT} accept
    iif lo meta mark {MARK} meta l4proto udp accept
  }}
}}
"""


def build_topology():
    for n in (INNER, PEER):
        sh(f"ip netns del {n}", check=False)
        sh(f"ip netns add {n}")
        sh(f"ip -n {n} link set lo up")
    # inner: a real egress route so sendto()/connect() to the VIP resolves.
    sh(f"ip -n {INNER} link add dummy0 type dummy")
    sh(f"ip -n {INNER} addr add {DUMMY_IP}/24 dev dummy0")
    sh(f"ip -n {INNER} link set dummy0 up")
    sh(f"ip -n {INNER} route add default dev dummy0")
    # veth inner<->peer for the inbound-server arm.
    sh("ip link add vi type veth peer name vp")
    sh(f"ip link set vi netns {INNER}")
    sh(f"ip link set vp netns {PEER}")
    sh(f"ip -n {INNER} addr add {INNER_IP}/24 dev vi")
    sh(f"ip -n {INNER} link set vi up")
    sh(f"ip -n {PEER} addr add {PEER_IP}/24 dev vp")
    sh(f"ip -n {PEER} link set vp up")
    # policy routing for the divert (the CNI will do this via rtnetlink).
    sh(f"ip netns exec {INNER} ip rule add fwmark {MARK} lookup {TABLE}")
    sh(f"ip netns exec {INNER} ip route add local 0.0.0.0/0 dev lo table {TABLE}")


def install_ruleset(with_ct_reply=True):
    rules = RULESET
    if not with_ct_reply:
        rules = "\n".join(l for l in rules.splitlines() if "CT_DIRECTION_REPLY" not in l)
    sh(f"ip netns exec {INNER} nft flush ruleset", check=False)
    r = sh(f"ip netns exec {INNER} nft -f - <<'EOF'\n{rules}\nEOF", check=False)
    if r.returncode != 0:
        if "tproxy" in (r.stderr or "").lower() or "not supported" in (r.stderr or "").lower():
            raise SetupFailed(f"nft rejected the ruleset -- nft_tproxy may be unavailable on this kernel: {r.stderr.strip()[:200]}")
        raise SetupFailed(f"nft -f failed: {r.stderr.strip()[:200]}")
    listed = sh(f"ip netns exec {INNER} nft list ruleset").stdout
    if "tproxy to :18001" not in listed:
        raise SetupFailed(f"tproxy rule not present after install:\n{listed}")
    has_ct = "ct direction reply" in listed
    if has_ct != with_ct_reply:
        raise SetupFailed(f"ct direction reply presence={has_ct}, wanted {with_ct_reply}")
    print(f"  [ruleset installed, ct-direction-reply={'on' if with_ct_reply else 'OFF'}]", flush=True)


# ----------------------------------------------------------------------------
# sockets
# ----------------------------------------------------------------------------

def transparent_tcp_listener():
    """Created INSIDE inner, returned to outer: the proxy's shape."""
    with InNetns(NS(INNER)):
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        s.setsockopt(socket.IPPROTO_IP, IP_TRANSPARENT, 1)
        s.bind(("0.0.0.0", CAPTURE_PORT))
        s.listen(8)
    return s


def transparent_udp_socket():
    with InNetns(NS(INNER)):
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        s.setsockopt(socket.IPPROTO_IP, IP_TRANSPARENT, 1)
        s.setsockopt(socket.IPPROTO_IP, socket.IP_PKTINFO, 1)
        s.bind(("0.0.0.0", UDP_PORT))
    return s


def original_dst(conn):
    try:
        raw = conn.getsockopt(socket.IPPROTO_IP, SO_ORIGINAL_DST, 16)
        port, addr = struct.unpack("!2xH4s8x", raw)
        return f"{socket.inet_ntoa(addr)}:{port}"
    except OSError as e:
        return f"ERR {os.strerror(e.errno)}"


def in_ns_python(ns, code):
    """Run a python snippet inside a netns; return (rc, stdout)."""
    r = subprocess.run(["ip", "netns", "exec", ns, "python3", "-c", code],
                       capture_output=True, text=True, timeout=20)
    return r.returncode, (r.stdout or "").strip(), (r.stderr or "").strip()


# ----------------------------------------------------------------------------
# arms
# ----------------------------------------------------------------------------

def arm_t1():
    """TCP to VIP:8080 lands on the single transparent listener; reply returns."""
    ln = transparent_tcp_listener()
    ln.settimeout(5)
    client = subprocess.Popen(
        ["ip", "netns", "exec", INNER, "python3", "-c",
         f"import socket,sys\n"
         f"s=socket.socket();s.settimeout(5)\n"
         f"try:\n s.connect(('{VIP}',8080));s.sendall(b'hello');print('REPLY='+s.recv(16).decode())\n"
         f"except Exception as e: print('CLIENT_ERR='+repr(e))\n"],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        conn, peer = ln.accept()
    except socket.timeout:
        client.kill()
        return {"accepted": False}
    local = conn.getsockname()
    od = original_dst(conn)
    data = conn.recv(16)
    conn.sendall(b"world")
    conn.close()
    out, _ = client.communicate(timeout=10)
    ln.close()
    return {"accepted": True, "local": f"{local[0]}:{local[1]}", "peer": peer[0],
            "orig_dst": od, "payload": data, "client": out.strip()}


def arm_t2():
    """TCP to VIP:53 must NOT be captured: listener sees nothing, client times out."""
    ln = transparent_tcp_listener()
    ln.settimeout(4)
    rc, out, _ = in_ns_python(INNER,
        f"import socket\ns=socket.socket();s.settimeout(3)\n"
        f"try:\n s.connect(('{VIP}',53));print('CONNECTED')\n"
        f"except Exception as e: print('ERR='+type(e).__name__)")
    captured = True
    try:
        ln.accept()
    except socket.timeout:
        captured = False
    ln.close()
    return {"captured": captured, "client": out}


def arm_u1():
    """UDP to VIP:18082: transparent socket receives; reply from VIP reaches a CONNECTED client."""
    srv = transparent_udp_socket()
    srv.settimeout(5)
    client = subprocess.Popen(
        ["ip", "netns", "exec", INNER, "python3", "-c",
         f"import socket\ns=socket.socket(2,2);s.settimeout(5);s.connect(('{VIP}',{UDP_PORT}))\n"
         f"s.send(b'ping')\n"
         f"try: print('REPLY='+s.recv(16).decode())\n"
         f"except Exception as e: print('CLIENT_ERR='+repr(e))\n"],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        data, anc, _f, peer = srv.recvmsg(64, socket.CMSG_SPACE(64))
    except socket.timeout:
        client.kill()
        srv.close()
        return {"received": False}
    ipi_addr = None
    for lvl, typ, cdata in anc:
        if lvl == socket.IPPROTO_IP and typ == socket.IP_PKTINFO:
            _i, _spec, addr = struct.unpack("=i4s4s", cdata[:12])
            ipi_addr = socket.inet_ntoa(addr)
    # Reply FROM the VIP: spec_dst selects the source address; sport is the bound 18082.
    pktinfo = struct.pack("=i4s4s", 0, socket.inet_aton(VIP), b"\0" * 4)
    srv.sendmsg([b"pong"], [(socket.IPPROTO_IP, socket.IP_PKTINFO, pktinfo)], 0, peer)
    out, _ = client.communicate(timeout=10)
    srv.close()
    return {"received": True, "ipi_addr": ipi_addr, "payload": data, "client": out.strip()}


def arm_s1():
    """Inbound: a client in `peer` connects to a plain server in `inner` over the veth."""
    # Server runs INSIDE inner (plain, not transparent), as an app would.
    server = subprocess.Popen(
        ["ip", "netns", "exec", INNER, "python3", "-c",
         f"import socket\nl=socket.socket();l.setsockopt(1,2,1);l.bind(('{INNER_IP}',{SERVER_PORT}));l.listen(1);l.settimeout(8)\n"
         f"try:\n c,_=l.accept();c.sendall(c.recv(16));c.close();print('SERVED')\n"
         f"except Exception as e: print('SERVER_ERR='+type(e).__name__)\n"],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    time.sleep(0.5)
    rc, out, err = in_ns_python(PEER,
        f"import socket\ns=socket.socket();s.settimeout(4)\n"
        f"try:\n s.connect(('{INNER_IP}',{SERVER_PORT}));s.sendall(b'inbound');print('ECHO='+s.recv(16).decode())\n"
        f"except Exception as e: print('CLIENT_ERR='+type(e).__name__)")
    sout, _ = server.communicate(timeout=10)
    return {"client": out, "server": sout.strip()}


def report(name, r):
    print(f"  {name}: {r}", flush=True)


def main():
    print(f"kernel={os.uname().release} node={os.environ.get('NODE_NAME','?')}", flush=True)
    print(f"VIP={VIP} capture_port={CAPTURE_PORT} udp_port={UDP_PORT} mark={MARK:#x} table={TABLE}\n", flush=True)

    build_topology()
    install_ruleset(with_ct_reply=True)

    # The 18001 transparent listener must exist for S1's negative control to
    # have a socket to hit; arm_t1/arm_t2 create their own and close them, so
    # keep one open for the whole S1 pair.
    print("T1  TCP capture, single listener, reply from VIP")
    t1 = arm_t1(); report("T1", t1)
    print("\nT2  TCP :53 must not be captured")
    t2 = arm_t2(); report("T2", t2)
    print("\nU1  UDP capture on the dialed port, reply from VIP to a connected client")
    u1 = arm_u1(); report("U1", u1)

    print("\nS1  inbound server reply under redirect-all, WITH ct direction reply")
    bg = transparent_tcp_listener()   # stays open: the socket a diverted SYN-ACK would hit
    s1_with = arm_s1(); report("S1(with)", s1_with)

    print("\nS1' same, with `ct direction reply accept` REMOVED  <-- must FAIL")
    install_ruleset(with_ct_reply=False)
    s1_without = arm_s1(); report("S1(without)", s1_without)
    bg.close()

    print("\n================ VERDICT ================", flush=True)
    t1_ok = t1.get("accepted") and t1.get("local") == f"{VIP}:8080" and t1.get("client") == "REPLY=world"
    t2_ok = (not t2.get("captured")) and t2.get("client", "").startswith("ERR=")
    u1_ok = u1.get("received") and u1.get("ipi_addr") == VIP and u1.get("client") == "REPLY=pong"
    s1w_ok = s1_with.get("client") == "ECHO=inbound"
    s1wo_broke = s1_without.get("client") != "ECHO=inbound"

    if not s1w_ok:
        print("DESIGN FAILS: the inbound server reply did not get through even WITH")
        print("`ct direction reply accept`. Redirect-all under divert breaks inbound.")
        rc = 1
    elif not s1wo_broke:
        print("RIG BROKEN: removing `ct direction reply` did NOT break the inbound")
        print("reply, so this rig is not exercising the hazard. MAIN results are")
        print("not evidence about that rule. Do not proceed on this run.")
        rc = 2
    elif not (t1_ok and u1_ok and t2_ok):
        print("DESIGN FAILS on this kernel:")
        if not t1_ok: print("  T1: TCP single-listener capture / VIP reply did not work as designed")
        if not u1_ok: print("  U1: UDP reply from the VIP did not reach the connected client")
        if not t2_ok: print("  T2: TCP :53 was captured (or the client did not time out)")
        rc = 1
    else:
        print("ANSWER: YES. Full ruleset works on this kernel for BOTH transports:")
        print("  T1 one transparent TCP listener on 18001 sees VIP:8080 and replies;")
        print(f"     SO_ORIGINAL_DST on that socket = {t1.get('orig_dst')}")
        print("  T2 :53 is not diverted;  U1 UDP reply from VIP:18082 reaches a connected client;")
        print("  S1 inbound replies survive WITH `ct direction reply` and BREAK without it")
        print("     -- the red state for the rule that keeps this from taking down every pod.")
        print("PR 3 may be written against this exact ruleset.")
        rc = 0
    print(f"T1={t1_ok} T2={t2_ok} U1={u1_ok} S1_with={s1w_ok} S1_without_broke={s1wo_broke}", flush=True)

    for n in (INNER, PEER):
        sh(f"ip netns del {n}", check=False)
    return rc


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SetupFailed as e:
        print(f"\nRIG BROKEN (setup): {e}", flush=True)
        sys.exit(2)
