# Proposal 038 Phase 0 — the TPROXY/netns spike

Settled 2026-09-25 on `main-worker-04` (kernel 6.18.34-talos). Kept because the
answer is load-bearing for proposal 038 Phase 1 and because a spike nobody can
re-run is a claim, not evidence.

## The question

Does a UDP socket created **inside** a pod netns (via `setns`) but **read from
another** netns still observe the pre-TPROXY destination?

That is aether's real shape: the proxy is `hostNetwork` and binds capture
listeners into each pod's netns via `network_namespace_filepath`, i.e. it creates
the socket in the pod netns and runs its event loop in the host netns.

## The answer

**Yes.** `ipi_addr` carries the original destination.

```
C1  in-netns create + read, mark-and-divert   ipi_addr=10.250.0.7   (the VIP)
C2  REDIRECT, in-netns read                   ipi_addr=127.0.0.1    (rewritten)
C3  no capture rule                           nothing delivered
MAIN create in-netns via setns, read outside  ipi_addr=10.250.0.7   (the VIP)
```

## Why the controls are the point

C2 is what makes MAIN mean anything: it proves `ipi_addr` reports the **real,
post-NAT** header rather than echoing the address that was sent. C3 only shows
that delivery needs a rule — a weaker claim.

The 2026-09-23 attempt produced an unusable answer because its control agreed
with its negative result. The FIRST run of this script lost C2 the same way and
more quietly: `redirect` is terminal in nftables, so a trailing `counter`
invalidated the rule, nothing installed, and C2 silently became a duplicate of C3
— while still passing an assertion written as "C2 must not report the VIP", which
a no-delivery `None` satisfies.

So the script now:

- raises on any failed setup command instead of logging and continuing;
- verifies with `nft list ruleset` that the rule is really there before measuring;
- treats *no delivery* in C2 as a **broken rig**, not a pass.

## It binds the SERVICE port, not the capture port

nftables `tproxy` is prerouting-only, so locally-originated pod egress uses
mark-and-divert — which does **not** rewrite the destination port. A listener on
any other port receives nothing. This is the constraint that forces
listener-per-port in Phase 2.

## Running it

```sh
kubectl -n aether-system create configmap tproxy-phase0 \
  --from-file=phase0.py=e2e/spike/udp-tproxy-phase0.py --dry-run=client -o yaml |
  kubectl apply -f -
kubectl apply -f e2e/spike/udp-tproxy-phase0-job.yaml
kubectl -n aether-system logs -l aether.io/spike=038-phase0
```

`aether-system` because it already enforces PodSecurity `privileged`; the rest of
the cluster enforces `baseline`, which refuses the capabilities this needs. The
pod runs with `NET_ADMIN` + `SYS_ADMIN` and **no host namespaces and no
`privileged: true`** — the pod's own netns is the "outer" one. `SYS_ADMIN` is the
single reason a rootless workstation rig cannot host this test: `setns()` back to
the original netns needs `CAP_SYS_ADMIN` in that namespace's user namespace.

Exit codes: `0` answered yes, `1` answered no (038 stops, #916 stands as a
documented limit), `2` rig broken — do not read the result.

---

# Phase 0b — the full TPROXY ruleset, both transports (`tproxy-phase0b.py`)

Settled 2026-09-26 on `main-worker-04` (kernel 6.18.34-talos), first run, exit 0.
This is the gate for the CNI change: PR 3 of the TPROXY plan is written against
**this exact ruleset**, which has now run on a Talos kernel.

## What Phase 0 had not established

Phase 0 proved that a transparent UDP socket created inside a pod netns via
`setns` and read from outside sees the pre-divert destination. It never
installed a prerouting `tproxy` rule (so it never proved `nft_tproxy` loads on
this kernel), never ran TCP, never sent a reply, and never exercised the one
rule that keeps the design from breaking every pod.

## The result

```
T1  TCP -> VIP:8080, ONE transparent listener on 18001
      accepted, getsockname() = 10.250.0.7:8080, client got the reply
      SO_ORIGINAL_DST on that socket = 10.250.0.7:8080
T2  TCP -> VIP:53          not captured; client timed out          (correct)
U1  UDP -> VIP:18082       ipi_addr = 10.250.0.7; reply sent FROM VIP:18082
                           reached a CONNECTED client
S1  inbound over a veth, redirect-all ON
      with    ct direction reply accept   ECHO=inbound   SERVED
      without ct direction reply accept   client TimeoutError, server TimeoutError
```

## Why S1 is the arm that matters

A `type route` chain sees **every** locally-generated packet. Under redirect-all
that includes the pod's own server replies to inbound clients — a SYN-ACK whose
`dport` is the client's ephemeral port. Without `ct direction reply accept` that
packet is marked, looped to `lo`, `tproxy`'d to the 18001 LISTEN socket, and
answered with a reset: every inbound connection to every redirect-all pod dies.
`ct state established,related accept` is **not** a substitute — in a route chain
it would also exempt the 2nd+ packets of the pod's *own* captured outbound flows,
sending them out `eth0` mid-connection. `direction reply` exempts exactly the
replies and nothing else.

S1 is run twice so the rule's absence is *seen* to break something. A run where
S1 passes without the rule is reported as a broken rig (exit 2), not a pass.

## Two facts worth keeping from T1

- **TCP keeps one listener.** `tproxy to :18001` looks the socket up by the
  target port while leaving the header intact, so the accepted socket's local
  endpoint is `VIP:8080` and the reply is correct with no conntrack NAT.
  Delivery-port rewrite is fine for TCP; it is not fine for UDP, whose reply
  source port is the socket's bound port — which is why U1 binds the dialed port.
- **`SO_ORIGINAL_DST` succeeds on a diverted flow.** The flow is conntrack-tracked
  but not NATed, so `getorigdst` finds the reply tuple and returns the original
  destination — the same answer `getsockname` gives. Envoy's `original_dst`
  filter therefore takes its normal branch; the `IP_TRANSPARENT` → `getsockname`
  fallback is a backstop, not the main path.

## Running it

```sh
kubectl -n aether-system create configmap tproxy-phase0b \
  --from-file=tproxy-phase0b.py=e2e/spike/tproxy-phase0b.py --dry-run=client -o yaml |
  kubectl apply -f -
kubectl apply -f e2e/spike/tproxy-phase0b-job.yaml
kubectl -n aether-system logs -l aether.io/spike=tproxy-phase0b
```

Same Job shape as Phase 0: `aether-system` (already PodSecurity `privileged`),
`NET_ADMIN` + `SYS_ADMIN`, **no host namespaces, no `privileged: true`**. Exit
`0` = every arm as designed including S1's red state; `1` = the design fails on
this kernel; `2` = rig broken.
