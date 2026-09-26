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
