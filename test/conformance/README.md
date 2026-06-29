# Gateway API conformance runner

Committed, reproducible driver for the upstream Kubernetes **Gateway API conformance
suite** (`sigs.k8s.io/gateway-api/conformance` @ **v1.5.1**) against a live aether
cluster. This replaces the previously uncommitted one-off that lived in a gateway-api
checkout (see `docs/conformance/baseline-*.md`). Design + feasibility analysis:
`docs/proposals/024_conformance-ci.md`.

## What runs

| Test | Profile | Status | Run in CI? |
|---|---|---|---|
| `TestAetherGatewayHTTP` | GATEWAY-HTTP (north-south edge) | Fully conformant (43/43, talos rev21) | **Yes — gates** |
| `TestAetherMeshHTTP` | MESH-HTTP (east-west GAMMA) | **Not conformant** (blocked on proposal 022) | No (reproducible only) |

Both tests **skip** unless `AETHER_CONFORMANCE=1`, because they need a live cluster
reachable via the ambient kubeconfig.

## Why this is a "drop-in", not an in-tree Go test

`conformance_test.go` carries `//go:build conformance` and is **not compiled in the
aether module** (Gazelle skips it; `go test ./...` / `bazel test //...` ignore it). The
gateway-api **conformance suite is a separate Go module** whose `go.mod` has
`replace sigs.k8s.io/gateway-api => ../` — it is buildable **only from inside a
gateway-api source checkout** and cannot be consumed as an ordinary dependency
(`go get sigs.k8s.io/gateway-api/conformance@v1.5.1` fails to resolve the `apis/*`
imports). That is why the prior runner lived in a `/tmp` gateway-api checkout, and why
this committed copy is **copied into a checked-out gateway-api tree at run time**.

## Run it

Against any aether cluster with the **edge** enabled, the **Gateway API CRDs**
(standard channel, v1.5.1) installed, and GatewayClass `aether` present:

```bash
# 1. Check out the suite at the pinned version and drop the runner + overlay in.
git clone --depth 1 --branch v1.5.1 \
  https://github.com/kubernetes-sigs/gateway-api /tmp/gateway-api
cp test/conformance/conformance_test.go /tmp/gateway-api/conformance/aether_conformance_test.go
cp test/conformance/mesh/manifests.yaml /tmp/gateway-api/conformance/mesh/manifests.yaml

# 2. Run from inside the conformance module.
cd /tmp/gateway-api/conformance

# GATEWAY-HTTP (the conformant profile)
AETHER_CONFORMANCE=1 KUBECONFIG=/path/to/kubeconfig \
  go test . -tags conformance -run TestAetherGatewayHTTP -v -timeout 30m

# MESH-HTTP (not conformant yet; uses the embedded mesh overlay)
AETHER_CONFORMANCE=1 AETHER_CONFORMANCE_MESH=1 KUBECONFIG=/path/to/kubeconfig \
  go test . -tags conformance -run TestAetherMeshHTTP -v -timeout 30m
```

Useful env vars: `AETHER_NOCLEANUP=1` (leave base resources for debugging),
`AETHER_CONFORMANCE_REPORT=/path/report.yaml` (override the report output path).

In CI this is driven by `.github/workflows/conformance.yaml` (kind, no SPIRE,
GATEWAY-HTTP only), which performs the checkout + copy automatically.

## `mesh/manifests.yaml` — the aether mesh overlay

The suite's base mesh manifests do not exercise aether's mesh path. This overlay adds
the aether-specific adaptations the talos baseline runs applied by hand:

- `aether.io/managed: "true"` on the `gateway-conformance-mesh` namespace (mesh injection),
- **per-version ServiceAccounts** (`echo-v1`, `echo-v2`) on the echo Deployments —
  aether's mesh identity is the ServiceAccount, so the two versions need distinct ones.

The managed-namespace label is enough to capture the echo pods' real Service ports:
redirect-all is the chart default (`agent.captureRedirectAllDefault`, soak-validated),
so namespace injection labels the pods managed and they get redirect-all with **no
per-pod annotation** (the framework drives requests by `kubectl exec` into the echo
pods, and redirect-all routes those real ports through the mesh).

The Go test embeds this file (`//go:embed mesh/manifests.yaml`) and passes it as the
suite's `ManifestFS` for the MESH run, overriding the suite's embedded base.
