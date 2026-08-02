---
hide:
  - toc
---

<div class="aether-hero" markdown>

# Aether is a Kubernetes service mesh data plane that runs one Envoy per node instead of one per pod, capturing traffic transparently and mTLS-ing every hop with SPIFFE identities.

Written in Go. Routed with the Gateway API. Each node receives only the
configuration its own pods actually use.
{ .aether-subline }

[Get started →](https://github.com/bpalermo/aether/blob/main/docs/getting-started.md){ .md-button .aether-button }
[Architecture](architecture.md){ .md-button .aether-button }

Apache-2.0 · pre-1.0 · images and charts on GHCR
{ .aether-fineprint }

</div>

## The shape of it

<!-- aether:readme-mermaid -->

Solid arrows are the workload data path; dashed arrows are control plane and
telemetry.
{ .aether-caption }

## Four decisions

<div class="aether-grid" markdown>

<div class="aether-grid__cell" markdown>

**One proxy per node**

A single `aether-proxy` DaemonSet carries the traffic for every managed pod on
the node, entering each pod's network namespace rather than living inside it.
Interception is set up by a chained CNI plugin, so workloads are unmodified and
there is no sidecar to inject, size, or restart.

</div>

<div class="aether-grid__cell" markdown>

**Demand-scoped configuration**

Each node receives only the clusters, registry watches, and endpoints its local
pods actually depend on, declared with the `config.aether.io/upstreams`
annotation, with on-demand CDS for the cold path. Config size tracks the node's
footprint, not the size of the mesh.
[Proposal 004 →](https://github.com/bpalermo/aether/blob/main/docs/proposals/004_demand-scoped-distribution.md)

</div>

<div class="aether-grid__cell" markdown>

**Hitless proxy rollouts**

The agent supervises Envoy through a cross-pod hot restart with two-phase
connection draining. Zero dropped requests across eight-hour soak tests driving
30-plus rollouts of the data plane, measured by an external prober rather than
the mesh's own telemetry.
[Proposal 001 →](https://github.com/bpalermo/aether/blob/main/docs/proposals/001_proxy-hot-restart.md)

</div>

<div class="aether-grid__cell" markdown>

**The Gateway API, natively**

No bespoke routing CRDs. East-west traffic uses GAMMA — `HTTPRoute` and
`GRPCRoute` parented to a Service — and north-south uses the same API against
the edge gateway. The GATEWAY-HTTP profile is fully conformant, Core 33/33 and
Extended 10/10, and conformance is gated in CI.
[Proposal 018 →](https://github.com/bpalermo/aether/blob/main/docs/proposals/018_gateway-api-gamma.md) ·
[Proposal 024 →](https://github.com/bpalermo/aether/blob/main/docs/proposals/024_conformance-ci.md)

</div>

</div>

## Install

Two charts: the CRDs first, so they can be upgraded independently, then the
system.

```bash
# Pick the published version (chart version == git commit of the release).
VERSION=0.x.0-<commit>

# 1) CRDs (MeshConfig, HTTPFilter, EdgeConfig, EndpointPolicy) — install/upgrade first.
helm upgrade --install aether-crds \
  oci://ghcr.io/bpalermo/aether/charts/crds \
  --version "$VERSION"

# 2) The system: agent + proxy + mesh-dns + registrar + controller.
helm upgrade --install aether \
  oci://ghcr.io/bpalermo/aether/charts/aether \
  --version "$VERSION" \
  --namespace aether-system --create-namespace \
  --set clusterName=my-cluster \
  --set meshDomain=aether.internal
```

<div class="aether-note" markdown>

Aether is pre-1.0 and built by one person. It is soak-tested on a real cluster
and gated on Gateway API conformance in CI, but it has no support commitment.
Read the
[proposals](https://github.com/bpalermo/aether/tree/main/docs/proposals) and the
[conformance baselines](https://github.com/bpalermo/aether/tree/main/docs/conformance)
before you run it.

</div>
