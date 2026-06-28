# Proposal: Gateway API conformance in CI

**Status:** Design — 2026-06-28
**Relates:** proposal 018 (Gateway API / GAMMA — the API this certifies), proposal
022 (arbitrary-service interception — the blocker for MESH-HTTP), proposal 003
(edge proxy — the north-south data plane GATEWAY-HTTP exercises);
`docs/conformance/gateway-api-features.md`, `docs/conformance/baseline-*.md`,
`test/e2e/` (the existing kind harness this reuses); [[project_gateway_api_gamma]],
[[project_session_state_20260626]].

## Summary

The Gateway API conformance suite (`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**)
is run **manually, against the live `talos-main` cluster, from an uncommitted one-off
file** dropped into a gateway-api checkout (`/tmp/gateway-api/conformance/aether_rev20_test.go`).
That has produced 21 hand-recorded baselines (`docs/conformance/baseline-*.md`) and a
"GATEWAY-HTTP fully conformant 43/43" result — but nothing in the repo reproduces it,
nothing gates regressions, and the only person who can run it is whoever has the talos
kubeconfig and remembers the env-var incantation.

This proposes:

1. A **committed conformance runner** under `test/conformance/` — the `/tmp` test,
   cleaned up, plus the aether-specific MESH overlay manifests as committed files.
   Because the gateway-api conformance suite is a *separate, self-`replace`-ing Go
   module* (see Design — it cannot be imported as a normal dependency), the runner is a
   `//go:build conformance` **drop-in** copied into a gateway-api checkout at run time,
   not an in-module Go test. Reproducible by anyone with a kubeconfig, talos or not.
2. A **`workflow_dispatch` + nightly `schedule` GitHub Actions workflow**
   (`.github/workflows/conformance.yaml`) that stands up a **kind** cluster, builds and
   loads the aether images, installs aether's **edge** (north-south) path, installs the
   Gateway API CRDs, runs the **GATEWAY-HTTP** profile, and uploads the conformance
   report as an artifact.

**Feasibility verdict, up front, per profile:**

- **GATEWAY-HTTP — feasible in kind CI, recommended v1.** The edge path needs neither
  the CNI mesh interception nor SPIRE: the edge dials *unmeshed* conformance backends
  over **cleartext STRICT_DNS** (`BuildEdgeK8sCluster`, see Design), and the agent
  DaemonSet + CNI + registrar already come up on kind today in `test/e2e`. The one
  genuinely kind-specific piece is **LoadBalancer addressing** (the suite blocks on
  `Gateway.status.addresses`), solved with `cloud-provider-kind`.
- **MESH-HTTP — NOT feasible yet; do not gate on it.** It is not conformant *anywhere*
  (4/7 on talos, the 4 passing by kube-proxy coincidence — see proposal 022), it needs
  SPIRE for real mTLS identity and the arbitrary-service capture that 022 is still
  designing, and it depends on `kubectl exec` request timing under CNI redirect-all.
  We commit the runner and overlay so it is *reproducible*, but the workflow leaves it
  out (or, at most, behind a `continue-on-error` opt-in flag) until 022 lands.

So: **GATEWAY-HTTP-only in CI is the pragmatic v1**, and it is honestly the *whole* of
what is conformant today.

## Motivation

- **The runner is uncommitted and unreproducible.** `docs/conformance/baseline-2026-06-27-rev21.md`
  itself says: *"Same programmatic runner as rev2–rev20 (`conformance/aether_rev20_test.go`),
  not committed to the repo."* A conformance claim with no committed runner is a claim
  no one else can check and nothing protects.
- **No regression gate.** GATEWAY-HTTP reached 43/43 over a five-PR chain of subtle edge
  fixes (#380–#385, each exposing the next; see rev21). Every one of those is a class of
  bug a future edge change could silently reintroduce. There is currently no automated
  signal.
- **Talos-only is operationally narrow.** The badge run requires the talos kubeconfig,
  MetalLB, a live SPIRE, and manual env vars. CI should make the *certifiable* subset
  reproducible on a throwaway cluster.

## Design

### Where the cluster runs: kind (recommended), with `cloud-provider-kind`

`test/e2e/main_test.go` already proves the load-bearing facts on **kind**:

- aether's **CNI plugin installs** (`cni-install` init container writes
  `/opt/cni/bin` + `/etc/cni/net.d` via hostPath) and the **agent DaemonSet goes Ready**
  (`HostNetwork`, `NET_ADMIN`, the netns/registry hostPath mounts).
- the **registrar** comes up with `--registry-backend=kubernetes` (no DynamoDB/etcd).
- both run with **`--spire-enabled=false`**.

So "does the CNI + agent come up on kind?" is **already answered yes** — it is a merged,
green e2e job (`ci.yaml` → `e2e`). The conformance workflow reuses that exact harness
shape (same images via `make load-*-image` / `image_load`, same kind provider).

The one thing `test/e2e` does **not** exercise that GATEWAY-HTTP needs is **LoadBalancer
addressing**. The conformance suite blocks on `Gateway.status.addresses` being populated
(`GatewayMustHaveAddress`, which the talos runner already stretches to 180s because
MetalLB convergence is slow). On talos that address comes from MetalLB. kind has no
LoadBalancer by default. The standard kind answer — used by upstream Gateway API CI
itself — is **`cloud-provider-kind`** (a tiny userspace LB controller that assigns
docker-network IPs to `type: LoadBalancer` Services). We run it as a background process
in the workflow. (Alternative considered: `edge.perGatewayAddressing=false` +
`edge.service.type=NodePort`, but per-Gateway addressing is the default and the path
rev21 certifies, so we keep LoadBalancer and add the provider rather than diverge the
config under test.)

### Why GATEWAY-HTTP does **not** need SPIRE or the mesh CNI path

This is the crux of the feasibility call. The conformance backends are **plain
`echo` Services/Deployments** — they are *not* meshed (no `aether.io/managed` label, no
`:15008` inbound, no SVID). The edge handles exactly this case natively:

`agent/internal/xds/cache/edge.go` (`edgeClusterNameLocked`, ~L553–560):

> *"…a mesh-registered backend — use the existing mTLS mesh cluster path. If NOT in the
> registry, the backend is a plain k8s Service: return the cleartext cluster … dial the
> Service FQDN over cleartext instead of attempting a mesh mTLS connection to :15008."*

and `agent/internal/xds/proxy/edge.go` `BuildEdgeK8sCluster` builds a **`STRICT_DNS`,
cleartext, HTTP/1.1** cluster to the Service FQDN, **no TransportSocket**. So for
conformance's unmeshed backends the edge:

- needs **no SPIRE** (the mTLS branch is never taken),
- needs **no CNI interception** (the edge is a normal ingress Deployment dialing
  Service FQDNs through CoreDNS/kube-proxy),
- runs the edge as `agent edge` with **`--spire-enabled=false`**, the flag the e2e
  harness already passes to the agent/registrar.

This is what makes GATEWAY-HTTP-in-kind tractable: it is the **north-south edge data
plane only**, and that plane degrades cleanly to cleartext for non-mesh backends. The
risky subsystems (SPIRE, mesh CNI redirect) are simply **not on the GATEWAY-HTTP path**.

### How aether is installed in CI

Reuse the published-image-free path the e2e job uses:

1. `bazel run //agent/cmd/agent:image_load` (+ `cni-install`, `registrar`) to build and
   load images into the local docker daemon (BuildBuddy RBE warms the cache).
2. `kind create cluster`, then `kind load docker-image` for each (or
   `make load-*-image`). Images are referenced with `imagePullPolicy: Never`.
3. Install via the **Helm chart** (`charts/aether`) with edge enabled and SPIRE off:
   ```
   helm install aether charts/aether \
     --set spire.enabled=false \
     --set agent.spireEnabled=false \
     --set registrar.spireEnabled=false \
     --set edge.enabled=true \
     --set edge.spire... (disabled) \
     --set image pins to the loaded local tags
   ```
   The chart renders the GatewayClass `aether` bound to `gateway.aether.io/edge`, the
   edge Deployment/Service/ConfigMap, and the edge RBAC. The GatewayClass *name* the
   runner targets is `aether`; its controller is `gateway.aether.io/edge`.

   > Honesty note (refined after reading the chart): the chart gates **every**
   > component's `--spire-enabled` on the **single top-level `spire.enabled`**
   > (verified in `agent-daemonset.yaml`, `edge-deployment.yaml`,
   > `registrar-deployment.yaml`), so `--set spire.enabled=false` is a clean,
   > chart-native no-SPIRE install for the agent *and the edge* — no per-component
   > flags or hand-rolled manifests needed. The genuinely open item is narrower:
   > **pinning the chart's images to the kind-loaded local tags** (the chart defaults
   > to publish-digest image refs). That `--set repository/tag/pullPolicy=Never`
   > wiring is the one step marked TODO/aspirational in the draft workflow; the
   > `test/e2e` harness already does the equivalent with `imagePullPolicy: Never` +
   > local tags, so it is a known-shape fix, not an unknown.

4. Install the **Gateway API CRDs** (standard channel, v1.5.1 to match the suite) —
   aether does **not** own these (`edge/README.md`: *"the Gateway API CRDs are installed
   out-of-band"*).

### The committed runner — `test/conformance/` (a drop-in, NOT an in-module Go test)

**Feasibility finding (validated this session — corrects the brief's assumption.)** The
gateway-api **conformance suite is a *separate Go module***:
`sigs.k8s.io/gateway-api/conformance` has its **own `go.mod`** whose first directives are:

```
module sigs.k8s.io/gateway-api/conformance
replace sigs.k8s.io/gateway-api => ../
require sigs.k8s.io/gateway-api v0.0.0-00010101000000-000000000000
```

That `replace … => ../` makes it buildable **only from inside a gateway-api source
checkout**. It cannot be consumed as an ordinary dependency:
`go get sigs.k8s.io/gateway-api/conformance@v1.5.1` resolves but then **fails** every
`apis/*` import (`invalid version: unknown revision 000000000000`) — I reproduced this.
Equivalently, `bazel run @rules_go//go get` + `make gazelle` yields a target that does
not build (the `conformance/` subtree is absent from the extracted gateway-api Bazel
repo). **This is why the prior runner lived in a `/tmp/gateway-api/conformance/`
checkout** — it ran *as part of the conformance module*, not as an external consumer.

So the committed form is a **drop-in source file**, not a Bazel/Go test in the aether
module:

- `test/conformance/conformance_test.go` carries **`//go:build conformance`**, so
  Gazelle skips it (no BUILD.bazel generated — verified) and `go test ./...` /
  `bazel test //...` never compile it. No new aether-module dependency, no go.mod churn.
- CI (and the README runbook) **check out gateway-api@v1.5.1, copy the runner +
  `mesh/manifests.yaml` overlay into `conformance/`, and `go test -tags conformance`
  from inside that module.** This is the honest, working mechanism; the
  "import-as-a-test-dependency" path in the brief is not achievable with the current
  upstream module layout.

It keeps the suite options verbatim from the talos runner: `GatewayClassName: "aether"`,
  `AllowCRDsMismatch: true`, the GATEWAY-HTTP profile inferring features from
  `GatewayClass.status.supportedFeatures`, and the talos timeouts
  (`GatewayMustHaveAddress=180s`, `MaxTimeToConsistency=60s`, `RequestTimeout=10s`).
- gate both tests behind an env var (`AETHER_CONFORMANCE=1`) so a plain `bazel test //...`
  / `go test ./...` skips them — they need a live cluster, so they must NOT run as unit
  tests. This mirrors the existing `AETHER_REV20=1` gate and the `testing.Short()`
  convention in this repo.
- write the report to a path the workflow uploads.

The **MESH overlay** (`test/conformance/mesh/`) commits the aether adaptations the talos
runs applied by hand to the suite's base mesh manifests: the `aether.io/managed=true`
namespace label, per-version ServiceAccounts for `echo-v1`/`echo-v2` (aether identity =
ServiceAccount), and the `capture.aether.io/redirect-all=true` pod annotation. Committed
so MESH-HTTP is reproducible the day 022 unblocks it — even though the workflow does not
run it yet.

### Which profiles run, and how results surface

- **GATEWAY-HTTP**: runs, **gates** (job fails on any Core/Extended failure).
- **MESH-HTTP**: **not run** in v1 (commit the runner+overlay only). When 022 lands, add
  a second job, initially `continue-on-error: true` (informational), promoted to a gate
  once it is actually conformant.
- **Surfacing**: upload the suite's YAML conformance report
  (`confv1.ConformanceReport`) as a workflow artifact; stream the suite's own
  `Core/Extended … Passed/Failed` summary to the job log; on failure, `kind export logs`
  + edge/agent/registrar pod logs (copied from the e2e job's failure-collection step).

### Triggers / cost

`workflow_dispatch` (on-demand badge runs) + nightly `schedule`. **Not** on every PR:
it builds three images, stands up kind + cloud-provider-kind, and the suite alone runs
~110s plus cluster bring-up — too heavy for the per-PR gate, and the per-PR `e2e` job
already covers the edge data path functionally. Nightly catches conformance regressions
within a day; `workflow_dispatch` reproduces a badge run on demand.

## Tensions / honesty

- **Image pinning is the soft spot (not SPIRE).** The chart gates all components'
  `--spire-enabled` on the single `spire.enabled`, so the no-SPIRE install is
  chart-native (`--set spire.enabled=false`). The remaining open item is pinning the
  chart's images to the **kind-loaded local tags** (the chart defaults to publish
  digests); the draft workflow leaves that `--set repository/tag/pullPolicy=Never` as a
  TODO. `test/e2e` already does the equivalent, so the shape is known. The **edge**
  no-SPIRE path itself is still unproven by a green CI job (it is exercised conceptually:
  cleartext backends + `--spire-enabled=false` in `cmd/edge.go`) — validating it on a
  `workflow_dispatch` run is the main task before the workflow gates.
- **`cloud-provider-kind` reliability.** LB IP assignment in kind is generally robust but
  is the most likely flake source; the 180s `GatewayMustHaveAddress` budget absorbs slow
  convergence. If it proves flaky, the NodePort fallback (`perGatewayAddressing=false`)
  is the escape hatch, at the cost of testing a non-default addressing mode.
- **kind vs talos parity.** kind runs a different kernel/CNI substrate than talos. The
  e2e job shows the agent/CNI come up; GATEWAY-HTTP doesn't use the mesh CNI path anyway,
  so parity risk is low for *this* profile (and high for MESH, another reason to defer it).
- **MESH-HTTP is not conformant and this proposal does not pretend otherwise.** Per
  rev21 and proposal 022, MESH is 4/7 with the 4 passing by kube-proxy coincidence. No
  workflow can make it pass before 022 ships the arbitrary-service capture. Gating MESH
  now would mean a permanently-red job; we commit the runner instead and wait.

## Verification

- **Proven (from existing committed code, read this session):** CNI install + agent
  DaemonSet + registrar come up on kind with `--spire-enabled=false` (`test/e2e`,
  merged/green); the edge uses cleartext STRICT_DNS for non-mesh backends
  (`edge.go`/`BuildEdgeK8sCluster`); the talos runner's exact suite options
  (`/tmp/.../aether_rev20_test.go`); GATEWAY-HTTP is 43/43 on talos (rev21).
- **Assumed / to validate when the workflow first runs:** the **edge** installs cleanly
  on kind with SPIRE fully off; `cloud-provider-kind` populates `Gateway.status.addresses`
  within budget; the v1.5.1 CRDs + `AllowCRDsMismatch` reconcile against aether's
  GatewayClass; the suite green in kind matches the talos 43/43.

A run is "good" when the GATEWAY-HTTP job logs `Core tests succeeded. Extended tests
succeeded.` and uploads a `ConformanceReport` artifact with 33 Core + 10 Extended passes.

## Sequencing

1. Commit the drop-in runner (`test/conformance/`, `//go:build conformance`) + MESH
   overlay. No aether-module dependency or go.mod change (see the runner section).
   (This PR — draft.)
2. Wire the **kind-loaded image pins** into the install step and validate the no-SPIRE
   edge install on `workflow_dispatch` (the one aspirational gap; SPIRE-off is already
   chart-native via `spire.enabled=false`).
3. Flip the workflow's edge-install + GATEWAY-HTTP run from aspirational to gating; turn
   on the nightly schedule.
4. (After proposal 022) add the MESH-HTTP job, `continue-on-error` → gate.
