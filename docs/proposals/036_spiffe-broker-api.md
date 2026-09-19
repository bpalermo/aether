# Proposal: Replace SPIRE's Delegated Identity API with the SPIFFE Broker API

**Status:** Accepted — 2026-09-18; phases 0 (k8s-talos-main#79), 1 (#802) and 2
(k8s-talos-main#96) are implemented, deployed and validated on talos-main, including
an 8-hour soak; phase 3 (`enforced` access policy) is platform-side and pending
([k8s-talos-main#77](https://github.com/bpalermo/k8s-talos-main/issues/77)).
The measured results are in the two Outcome sections below.
**Author:** Bruno Palermo
**Relates:** the mesh mTLS model (per-pod app inbound, every hop mTLS), the
SPIRE startup decoupling (#740), the monotonic SDS publication (#784), the
inbound identity discriminator (#638). Supersedes the design discussion in #800.

## Context

The node agent mints a per-pod X.509-SVID for every managed pod on its node and
serves it to the node proxy over SDS. It does that through SPIRE's proprietary
**Delegated Identity API** on the SPIRE agent's **admin socket**
(`agent/internal/spire/{client,bridge,converter}.go`), authorised by the SPIRE
agent's `authorizedDelegates` list, with a selector set the agent builds by hand
(`PodSelectors`: `k8s:ns`, `k8s:sa`, `k8s:pod-name`, `k8s:pod-uid`).

That design has three costs:

1. **The delegate, not SPIRE, decides the selector set.** A registration entry
   keyed on anything else — pod labels, container image, sigstore verification —
   can never match, because SPIRE never attests the pod; it only compares the
   selectors the agent supplied.
2. **The admin socket is all-or-nothing.** An authorised delegate may request any
   selector set, hence any identity registered on the node.
3. **It is SPIRE-specific.** `github.com/spiffe/spire-api-sdk` is in the module
   for this one package, and the wiring (admin socket host mount, delegate
   allow-list) has no equivalent on another SPIFFE provider.

The **SPIFFE Broker API** ([spiffe/spiffe#340](https://github.com/spiffe/spiffe/pull/340),
merged 2026-06-16: `SPIFFE_Broker_API.md`, `SPIFFE_Broker_Endpoint.md`,
`brokerapi.proto`) standardises exactly this role. A *broker* presents a
**workload reference**; the SPIFFE provider resolves and attests the referenced
workload itself and streams its SVIDs and bundles. The endpoint is gRPC over UDS
or TCP, requires **mutual TLS with X.509-SVIDs in both directions**, authorises
brokers by SPIFFE ID, and rejects any request lacking the metadata key
`broker.spiffe.io: true` (an SSRF guard).

Everything needed exists in the versions already deployed (checked 2026-09-18):

| Piece | State |
|---|---|
| SPIRE agent | implements the Broker API since 1.15.2 under `experimental { broker { … } }`; talos-main runs 1.15.3 |
| k8s workload attestor | resolves `KubernetesObjectReference` and `WorkloadPIDReference`; `experimental.broker.access_policy` = `enforced` (SubjectAccessReview, verb `impersonate-via-spire`, username = broker SPIFFE ID) or `permissive`; `pod_reference_scope` = `agent_node` (default) or `cluster` |
| Helm chart | `spire-0.30.2` (the GitOps pin) exposes `sockets.broker`, `brokerAPI.brokers.<key>` and `workloadAttestors.k8s.brokerAPI` |
| Go stubs | `github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker`, present in the pinned v2.8.1 |

## Decision

Replace the Delegated Identity path with the Broker API. This is a
**replacement, not an opt-in**: the delegated client, the admin-socket plumbing
and the `spire-api-sdk` dependency are deleted, not kept behind a flag.

The design points below are part of the decision, so the implementation does not
reopen them:

- **Reference.** `KubernetesObjectReference{type: {plural: "pods", group: "core"},
  key: {namespace, name}, uid}` — key *and* UID, so SPIRE verifies the UID of the
  pod it resolved. `pod_reference_scope` stays `agent_node`: a node agent only
  brokers pods on its own node, and that scope uses the kubelet pod list as a
  fast path. `WorkloadPIDReference` is not used.
- **Transport.** UDS only, never TCP. A client interceptor adds
  `broker.spiffe.io: true` to every unary and streaming call.
- **Authentication.** The client certificate is the agent's own SVID from
  `common/spire.WaitingSource`, so the #740 semantics carry over unchanged: no
  SVID yet means the bridge waits, it never exits. The server is authorised as a
  SPIFFE ID in our own trust domain under `/spire/agent/`, because the endpoint
  specification requires the broker to authenticate the provider.
- **Trust bundles.** The Broker bundle stream needs a workload reference, which a
  node with zero managed pods does not have. Validation contexts are built from
  the agent's own Workload API bundle set (available as soon as identity is) plus
  the union of the `federated_bundles` carried on each pod's SVID stream. #784's
  guarantees stay: single-writer monotonic publication, and never replacing a
  non-empty validation-context set with an empty one.
- **Errors.** `NotFound` / `FailedPrecondition` on a reference (the pod is not in
  the kubelet list yet at CNI ADD, or no entry exists yet) → retry on the
  existing jittered backoff and count it. `PermissionDenied` → ERROR plus a
  metric, slow retry (the provider's policy may be non-static). `Unauthenticated`
  → refresh from `WaitingSource` and redial. `InvalidArgument` → a programming
  error: ERROR, no retry. `Unavailable` → backoff, exactly today's SPIRE-outage
  behaviour.
- **Access policy.** Ship on an **explicit `permissive`**, never on the chart's
  `auto`. `auto` was expected to resolve to `permissive` while broker access is
  node-local UDS; it does not. The chart's `spire-agent.broker-access-policy`
  helper escalates `auto` to `enforced` for any allowed reference type other than
  `WorkloadPIDReference` — which is exactly the `KubernetesObjectReference` this
  design uses (and likewise for `pod_reference_scope: cluster`,
  `allowOverTCP: true`, or an empty `allowedReferenceTypes`). `enforced` makes
  SPIRE run a `SubjectAccessReview` per referenced pod, whose RBAC grant the
  chart only renders when the broker also sets
  `workloadAttestors.k8s.brokerAPI.brokers.<key>.impersonation.clusterWidePodsOnly`.
  `auto` on its own therefore fails **closed**: every pod gets `PermissionDenied`.
  `enforced` is a later, separate hardening step, and it moves both settings at
  once.
- **Unchanged.** SDS secret naming and the #638 inbound/outbound discriminator,
  the node SVID path, the xDS hold while identity is pending, and the edge,
  controller and registrar — they use the plain Workload API.

## Consequences

**Gained.** SPIRE attests the pod itself, so every selector the k8s attestor can
produce (labels, image, sigstore) is *available* to registration entries. In
practice that only matters for entries written with such selectors:
`spire-controller-manager` emits entries keyed on exactly one selector,
`k8s:pod-uid:<uid>`, which the delegated path's hand-built set already carried —
so for ClusterSPIFFEID-managed entries nothing matches now that did not match
before (measured on talos-main, see Outcome). The concrete gains are the other
three: the admin socket and `authorizedDelegates` disappear, impersonation can be
RBAC-scoped per pod once `enforced` is on, and the agent speaks a SPIFFE standard
rather than a SPIRE API, with `spire-api-sdk` out of the module.

**Cost.** SPIRE marks the Broker API **experimental**: its configuration may
change before it is promoted (upstream tracks open questions in
spiffe/spire#7151, #7007 and #7264), and the go-spiffe package lives under
`exp/`. A reference must resolve *at request time*, so CNI ADD can race the
kubelet pod list — a retry path the selector-based flow never needed. And a build
after the switch requires SPIRE ≥ 1.15.2 with the broker enabled: a breaking
change for anyone installing the chart.

**Mitigations.** The platform pins chart 0.30.2 / SPIRE 1.15.3; any SPIRE bump
gates on the e2e SPIRE harnesses. The retry path is specified above and measured
in validation. The breaking change is called out in the release notes and
`docs/getting-started.md`.

## Phasing

0. **Platform, additive.** Enable the broker socket on the SPIRE agents
   (`sockets.broker.{enabled, mountOnHost}`), register the aether agent's SPIFFE
   ID (`spiffe://<trust-domain>/ns/aether-system/sa/aether-agent`) as a broker
   limited to `KubernetesObjectReference` over UDS, and enable the k8s attestor's
   broker block with an explicit `accessPolicy: permissive` (see the access-policy
   decision above — `auto` fails closed here). The same broker key must appear in
   both `brokerAPI.brokers.<key>` and
   `workloadAttestors.k8s.brokerAPI.brokers.<key>`. The admin socket stays, so an
   aether rollback remains trivial.
1. **aether, one hard-switch change.** Broker client behind a small interface;
   `SubscribePod` takes a pod reference instead of selectors and `PodSelectors`
   is deleted; the bundle loop becomes a Workload API bundle watcher plus per-pod
   federated bundles; the converter takes the broker `X509SVID`;
   `--spire-broker-socket` replaces `--spire-admin-socket`; the chart's
   `spire.adminSocket.*` becomes `spire.brokerSocket.*`; `spire-api-sdk` leaves
   `go.mod`; a fake Broker server in `common/spire/spiretest` enforces the
   metadata header and mTLS; the e2e harnesses move from SPIRE chart 0.28.4
   (SPIRE 1.14.5, no Broker API) to 0.30.2 with the broker values; docs updated.
2. **Platform cleanup, after one 8-hour soak on phase 1.** Remove
   `authorizedDelegates` and `sockets.admin`. From here a `helm rollback` of
   aether to a pre-switch revision no longer works.
3. **Hardening.** `accessPolicy: enforced` **and**
   `workloadAttestors.k8s.brokerAPI.brokers.<key>.impersonation.clusterWidePodsOnly`
   in ONE change — the flag is what makes the chart render the ClusterRole/Binding
   granting `impersonate-via-spire` on pods to the agent's SPIFFE ID, so either
   alone is broken (`enforced` without it denies every pod; the grant without
   `enforced` is inert). Then measure the SubjectAccessReview load and the CNI ADD
   latency, and consider granting it only in mesh-managed namespaces.

## Validation (phase 1, on talos-main)

A hitless roll graded by the external prober (zero errors per node); the SDS
secret count equals the managed pod count; the #638 mismatch counters stay zero
across a proxy roll; workload churn with `reference_not_found` retries observed
and converging; an SVID rotation observed on a live stream; the #740 SPIRE-outage
tiers (agents never exit, xDS held, recovery on attestation); a registration
entry keyed on a pod **label** receiving its identity — the capability the
delegated path could not provide; then the 8-hour soak before phase 2.

## Outcome (phase 1, 2026-09-18)

Deployed to talos-main as rev217 (`0.92.24-9fc27c8`, #802) on top of phase 0.
Every per-pod SVID on the cluster is now minted over the Broker API.

| Measure | Result |
|---|---|
| Agent roll | 2 m 19 s; per-node time until every local pod had its SVID again: 1.5 – 2.2 s; 39/39 subscriptions |
| `permission_denied` / `reference_not_found` | 0 / 0 fleet-wide (both counters exist, seeded) |
| External prober across roll, churn and a SPIRE agent restart | 0 errors, both tiers, all five nodes |
| Workload churn (8 new pods) | time-to-SVID mean 195 ms (60 – 391 ms); the bounded first receive never fired |
| SPIRE agent restart on one node (~34 s socket outage) | all streams resubscribed 17.18 s in, against 17.4 s for the delegated bridge; the agent never went NotReady |
| #638 mismatch, `sds_push_*`, Envoy `ssl_*`, LDS/CDS rejects | 0 |

What it did **not** show, recorded so the next validation covers it:

- **The selector gain is not observable here** (see Consequences): controller-manager
  entries carry only `k8s:pod-uid`. Proving it needs a hand-written entry with a
  different selector, which is a platform exercise.
- **The `NotFound` race at CNI ADD never fired** — 8 of 8 references resolved on
  the first attempt, so the retry path and the bounded first receive are covered
  by unit tests only. A burst scale-up or a node drain is the way to provoke it.
- **No rotation was observed.** `default_x509_svid_ttl` on this cluster is 4 h
  (half-life ~2 h); the 8-hour soak spans several and is the decisive check (done:
  see the next section).
- **`PermissionDenied` was never provoked** — `permissive` was in force; phase 3's
  `enforced` path is untested.
- A startup race logs one ERROR per managed pod when the stored-pod resubscribe
  runs before the agent's own SVID has landed (the mTLS dial has no client
  certificate yet). It self-heals in about a second at no cost; the severity is
  wrong, not the behaviour (#766).

## Outcome (8-hour soak and phase 2, 2026-09-19)

An 8-hour churned soak (31 rolls, incl. two agent rolls and a concurrent agent +
proxy + service roll) on rev218 (`0.92.24-15332e2`), then phase 2.

| Measure | Result |
|---|---|
| Pod-SVID rotation | **4 cycles** at ~2 h (half-life of the 4 h TTL), on workloads churn never rolled; prober delta in every rotation bucket: 0. The clock is SPIRE-side: the cadence survived both agent rolls, because a resubscribe through the Broker returns SPIRE's cached SVID |
| Signing-CA rotation | SPIRE's 24 h CA activated ~02:41Z, inside the window; rotation cycles 3 and 4 and every re-mint after it were issued by the new CA at zero cost. The X.509 bundle is the upstream root and does not move on that cycle |
| `permission_denied` / `reference_not_found` / #638 / `sds_push_*` / xDS rejects | 0 for 8 h |
| SLI | liveness 0 / 719,998; mesh-DNS 42 timeouts / 1,439,996, none identity-related; k6 0.00505 % |
| **Phase 2** (admin socket and `authorizedDelegates` removed, k8s-talos-main#96) | `spire-agent` rolled node by node; every pod resubscribed in 17.2 – 17.8 s per node, 0 prober errors, 0 `permission_denied`; a cold agent start with no admin socket on the node is clean. The rev216 rollback path is gone with it |

What the soak changed in the design's favour, and what it exposed:

- **#804:** a pod pruned by the ghost sweep kept its Broker subscription, which then
  retried `NotFound` for the agent's lifetime. The delegated bridge leaked the same
  way, silently — the Broker's explicit `NotFound` and its counter are what made it
  visible. Fixed in #805. It needs every CNI DEL for the pod to miss the agent (the
  runtime re-issues DEL at sandbox removal), which is why it is rare and why it
  cannot be forced from a harness; the regression test is in-process.
- **The agent had no signal for a healthy rotation**; the cycles above were read
  from Envoy's per-secret version gauges. #806 adds
  `aether.agent.spire.svid_updates{identity, update}` and `bundle_updates`.
  `update="rotated"` means *the certificate changed*: a restarted SPIRE agent
  re-mints a whole node's SVIDs and counts as `rotated` too.
- **#766** is closed (#807): the designed retries around a missing SVID or a
  restarting SPIRE agent log at WARN and escalate only when the excuse lapses. A
  SPIRE agent restart used to cost four ERROR lines per managed pod; it costs none.
- **The `NotFound` race at CNI ADD has still never fired** — not in a replica burst,
  not in 31 rolls. It stays covered by unit tests only.
- Upstream, cosmetic: every `spire-agent` logs one ERROR on its first Broker
  subscription (`unrecognized service for connection metrics: spiffe.broker.API`).

Still open: **phase 3** — `access_policy: enforced` together with the
`impersonate-via-spire` grant, in one change — and the selector gain, which remains
theoretical while controller-manager entries carry only `k8s:pod-uid`.

## Rejected alternatives

- **Opt-in flag with the delegated path kept as default.** Doubles the identity
  code (two clients, two converters, two test suites) for the lifetime of the
  flag, and the point of the change is to delete the admin-socket path. The
  additive phase 0 already gives a rollback window without carrying both in code.
- **`WorkloadPIDReference`.** The agent does not run in the pods' PID namespace,
  the spec requires the workload's own PID rather than the sandbox's, and a PID is
  weaker evidence than a UID-verified pod reference.
- **Broker over TCP from a central component.** Moves per-pod key material off
  the node, contradicts the node-local design of the agent, and would need
  `pod_reference_scope: cluster`.
- **Per-pod Workload API access (SPIFFE CSI driver in every pod).** Requires
  changing workloads; the mesh's premise is that they are untouched.
- **Wait for SPIRE to promote the API out of experimental.** The pinned versions
  implement the merged specification today, the platform pins those versions, and
  the selector limitation is a present functional gap.
