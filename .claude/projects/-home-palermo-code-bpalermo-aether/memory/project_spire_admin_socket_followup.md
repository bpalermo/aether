---
name: project-spire-admin-socket-followup
description: Pending follow-up PR to wire the agent's SPIRE admin socket / Delegated Identity API
metadata:
  type: project
---

The Helm-charts/SPIRE work (PRs #60, #62, #63, #64) is mostly done and the
**registrar** is validated working on the local `talos-main` cluster (SVID over
the `csi.spiffe.io` Workload API socket via go-spiffe `workloadapi`, mTLS gRPC
server up).

**Still pending — a separate PR:** the **agent** is not yet functional. It needs
the SPIRE **agent admin socket** (`/tmp/spire-agent/private/admin.sock`) for the
**Delegated Identity API**, which the agent uses to mint SVIDs for the
services/proxies (the `spire.NewBridge(SpireAdminSocketPath, …)` SDS bridge).

Scope of that PR:
- agent chart: mount the admin socket (hostPath) + the `csi.spiffe.io` workload
  socket; set `--spire-enabled`, `--spire-trust-domain`, `--spire-admin-socket`,
  `--spire-workload-socket` via a `spire:` values block (mirroring the registrar).
- **SPIRE-side prerequisite (outside the aether charts):** on `talos-main` the
  spire-agent admin socket is currently an emptyDir (pod-local, not exposed on a
  hostPath), and `authorized_delegates` lists `…/sa/aether-proxy`, not
  `aether-agent`. The admin socket must be exposed on a hostPath and the
  aether-agent SVID added as an authorized delegate, plus registration entries.

Cluster facts: trust domain `aether.internal`; SPIRE server in `spire-server` ns,
spire-agent + spire-spiffe-csi-driver in `spire-system`. Charts default the trust
domain to empty → omits `--spire-trust-domain` → binary uses the `ROOTCA`
sentinel (authorize-any). The `aether-agent-aws-credentials` secret in
`aether-system` is real and must be preserved across reinstalls.

Cleanup nit: an orphan `ghcr.io/bpalermo/aether/registrar:spire-test` image tag
was pushed during pre-merge prep; safe to delete (needs `write:packages`).
