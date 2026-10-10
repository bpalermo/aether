---
hide:
  - toc
---

<div class="aether-hero" markdown>

# Aether is a Kubernetes service mesh data plane that runs one Envoy per node instead of one per pod, capturing traffic transparently and mTLS-ing every hop with SPIFFE identities.

Written in Go. Routed with the Gateway API. HTTP/3 between proxies. Each node
receives only the configuration its own pods actually use.
{ .aether-subline }

[Get started →](docs/getting-started.md){ .md-button .aether-button }
[Architecture](architecture.md){ .md-button .aether-button }

Apache-2.0 · images and charts on quay.io/aethermesh
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
A chained CNI plugin diverts each pod's outbound TCP and UDP to it, TPROXY-style,
so the original destination survives and workloads are unmodified: there is no
sidecar to inject, size, or restart.
[Proposal 038 →](proposals/038-udp-tproxy-capture.md)

</div>

<div class="aether-grid__cell" markdown>

**Demand-scoped configuration**

Each node receives only the clusters, registry watches, and endpoints its local
pods actually depend on, declared with the `config.aether.io/upstreams`
annotation, with on-demand CDS for the cold path. Config size tracks the node's
footprint, not the size of the mesh.
[Proposal 004 →](proposals/004-demand-scoped-distribution.md)

</div>

<div class="aether-grid__cell" markdown>

**Hitless proxy rollouts**

A small supervisor, PID 1 of the proxy container, takes Envoy through a cross-pod
hot restart with two-phase connection draining. In the most recent eight-hour
soak written up, 9,180,000 of 9,180,000 requests were answered `2xx` across 35
rollout steps of the proxy, the agent, the edge, mesh DNS, the CSI plugin and
the workloads; the
external prober's liveness tier recorded no failure, and its mesh-DNS tier
recorded 37 timeouts on one node.
[Proposal 001 →](proposals/001-proxy-hot-restart.md) ·
[Proposal 042 →](proposals/042-sortie-soak-driver.md)

</div>

<div class="aether-grid__cell" markdown>

**The Gateway API, natively**

No bespoke routing CRDs. East-west traffic uses GAMMA — `HTTPRoute`,
`GRPCRoute`, `TCPRoute`, `TLSRoute` and `UDPRoute` parented to a Service — and
north-south uses the same API against the optional edge gateway. The GATEWAY-HTTP profile is fully conformant, Core 33/33 and
Extended 10/10, and conformance is gated in CI.
[Proposal 018 →](proposals/018-gateway-api-gamma.md) ·
[Proposal 024 →](proposals/024-conformance-ci.md)

</div>

</div>

## What it does today

- **HTTP/3 between proxies.** HTTP requests cross the mesh over QUIC with the
  caller's own SPIFFE identity, with nothing to enable: a per-source cluster is
  fetched on first use. A weighted GAMMA split and a destination behind the
  east/west waypoint stay on HTTP/2.
  [How it works →](docs/getting-started.md#east-west-quic)
- **TCP and UDP capture.** Raw TCP rides the mesh as an mTLS passthrough and
  `UDPRoute` services are reached transparently. UDP is carried in plaintext:
  there is no DTLS.
  [Proposal 038 →](proposals/038-udp-tproxy-capture.md)
- **Identity from SPIRE, by pod reference.** The agent brokers every pod's
  X.509-SVID over the SPIFFE Broker API (SPIRE 1.15.2 or later), and an init
  container holds a mesh pod's application until its identity exists.
  [Proposal 036 →](proposals/036-spiffe-broker-api.md)
- **Retries that know what was delivered.** A request that never reached the
  application is retried on another endpoint whatever its method; one the
  application had received is replayed only when the method is idempotent.
  [What the mesh retries →](docs/workloads.md#what-the-mesh-retries-for-you)
- **Workloads that serve on a Unix socket.** The socket lives in a per-pod volume
  mounted by the mesh's own CSI node plugin, `csi.aether.io`; callers still dial
  the pod over mTLS.
  [Proposal 039 →](proposals/039-uds-csi-driver.md)
- **Agent rolls with a sub-second gap, opt-in.** A rolled node agent's successor
  can start beside it as a standby and take the node over when a lock is
  released.
  [Proposal 041 →](proposals/041-agent-surge-handoff.md)
- **An edge gateway and more than one cluster, both optional.** North-south
  ingress with HTTP/3 as an opt-in; endpoints, routes and a per-node tunnel
  across clusters on the etcd backend.
  [Edge →](docs/getting-started.md#edge) ·
  [Multi-cluster →](docs/getting-started.md#multi-cluster)
- **Signed releases.** Images and charts on quay.io/aethermesh, signed keyless
  with cosign.
  [Verifying a release →](docs/verifying-releases.md)

## The design record

Every decision above was argued in writing before it was built, and the argument
is published as it was written — status, dead ends and all. These are the five
most recent.

<!-- aether:recent-proposals -->

## Install

Two charts: the CRDs first, so they can be upgraded independently, then the
system. SPIRE 1.15.2 or later, with its Broker Endpoint enabled, comes first:
see the [prerequisites](docs/getting-started.md#prerequisites).

```bash
# Pick the published version: <X.Y.Z>-<full git sha of the release>.
VERSION=<X.Y.Z>-<commit>

# 1) CRDs (MeshConfig, HTTPFilter, EdgeConfig, EndpointPolicy) — install/upgrade first.
helm upgrade --install aether-crds \
  oci://quay.io/aethermesh/chart-crds \
  --version "$VERSION"

# 2) First install only: the namespace, labelled for Pod Security admission (the
#    agent, proxy, mesh-dns and uds-csi pods need hostNetwork, hostPath volumes
#    and NET_ADMIN).
kubectl create namespace aether-system
kubectl label namespace aether-system \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/audit=privileged \
  pod-security.kubernetes.io/warn=privileged

# 3) The system: agent + proxy + mesh-dns + registrar + controller.
helm upgrade --install aether \
  oci://quay.io/aethermesh/chart-aether \
  --version "$VERSION" \
  --namespace aether-system --create-namespace \
  --set clusterName=my-cluster \
  --set meshDomain=aether.internal
```

<div class="aether-note" markdown>

Aether is pre-1.0 and built by one person working with AI coding agents
(Anthropic's Claude), which write most of the code, tests and documentation
under that person's direction. It is soak-tested on a real cluster and gated on
Gateway API conformance in CI, but it has no support commitment.
Read the
[proposals](proposals/index.md) and the
[conformance baselines](https://github.com/bpalermo/aether/tree/main/docs/conformance)
before you run it.

</div>
