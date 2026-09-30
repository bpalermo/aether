# Proposal 040: Publish to Quay (`quay.io/aethermesh`)

**Status:** Accepted 2026-09-27. Phases 1 (the abstraction, #998), 2 (the
cut-over) and 3 (talos-main runs from quay since rev243, 2026-09-27) are done.
Phase 4 (decommission ghcr) has not started: the repo still carries
`PROXY_PIN_LEGACY_REFERENCES = ["ghcr.io/bpalermo/aether/aether-proxy"]`
(`bazel/img/registry.bzl`), the publish-verify sweep's ghcr branch
(`.github/workflows/publish-verify.yaml`, `scripts/registry-lib.sh`) and its CI
fixtures, and a `ghcr.io/bpalermo/aether/*` step in
`docs/observability/profiling-symbols.md`; the remaining mentions are
migration notes for pre-1.0.0 releases, which stay.
**Author:** Bruno Palermo
**Date:** 2026-09-27
**Related:** #875 / #880 / #925 / #984 / #985 (signing and the publish-verify
sweep, which must keep working across the move), 035 (the `aethermesh.dev`
vanity identity the org name matches), 010 (the `//proxy` workspace).

## Context

Every aether image and chart is published to GitHub Container Registry under the
source repository's path: `ghcr.io/bpalermo/aether/<component>` and
`ghcr.io/bpalermo/aether/charts/<chart>`. We are moving them to the Quay
organisation **`aethermesh`**, for four reasons:

- **Flat, project-named coordinates.** `quay.io/aethermesh/agent` names the
  project, not the maintainer's GitHub account, and matches the `aethermesh.dev`
  identity (035). The org already scopes the names, so images carry no
  `aether-` prefix; charts carry `chart-` so the chart called `aether` does not
  take the org's most natural repository name.
- **The OCI 1.1 Referrers API.** Quay serves `GET /v2/<repo>/referrers/<digest>`;
  ghcr.io answers 404 (verified 2026-09-27 with a valid pull token). On ghcr.io
  cosign 3 therefore writes its signature as a *tag* (`sha256-<hex>`, the
  referrers fallback index); on Quay it attaches the Sigstore bundle as a real
  referrer. Observed on `quay.io/argoproj/argocd:latest` and
  `quay.io/cilium/cilium:stable`: artifactType
  `application/vnd.dev.sigstore.bundle.v0.3+json`, annotation
  `dev.sigstore.bundle.predicateType: https://sigstore.dev/cosign/sign/v1`, no
  fallback tag of either shape.
- **Sign in the push.** rules_img's `signing_config` with the
  `@rules_img_signer_cosign` plugin signs as part of `img deploy`, using an
  OIDC identity token (`$SIGSTORE_ID_TOKEN`, minted from the Actions OIDC
  provider). That closes the window between "pushed" and "signed" that today's
  separate `cosign sign` step leaves open, and it writes the signature as a
  referrer.
- **Credentials.** Pushes use a Quay **robot account**, held as two secrets of
  the `release` environment (`QUAY_USERNAME`, `QUAY_TOKEN`; deployment-branch
  policy `main`) — no long-lived personal token, nothing to rotate when a
  maintainer changes, and no run from any other ref can read them.

(As built, phase 2 kept today's `image_push` + a separate keyless `cosign sign
--recursive` step rather than signing inside `img deploy`: that is the path the
quay-smoke gate measured on quay.io, and it already writes the signature as a
referrer there. See "Phases".)

## Mapping

| artifact | today (ghcr.io) | after phase 2 (quay.io) |
|---|---|---|
| agent image | `ghcr.io/bpalermo/aether/agent` | `quay.io/aethermesh/agent` |
| mesh-dns image | `ghcr.io/bpalermo/aether/mesh-dns` | `quay.io/aethermesh/mesh-dns` |
| proxy-supervisor image | `ghcr.io/bpalermo/aether/proxy-supervisor` | `quay.io/aethermesh/proxy-supervisor` |
| cni-install image | `ghcr.io/bpalermo/aether/cni-install` | `quay.io/aethermesh/cni-install` |
| registrar image | `ghcr.io/bpalermo/aether/registrar` | `quay.io/aethermesh/registrar` |
| controller image | `ghcr.io/bpalermo/aether/controller` | `quay.io/aethermesh/controller` |
| prober image | `ghcr.io/bpalermo/aether/prober` | `quay.io/aethermesh/prober` |
| udsecho image (validation workload) | `ghcr.io/bpalermo/aether/udsecho` | `quay.io/aethermesh/udsecho` |
| proxy image (`proxy-release.yml`) | `ghcr.io/bpalermo/aether/aether-proxy` | `quay.io/aethermesh/proxy` |
| aether chart | `ghcr.io/bpalermo/aether/charts/aether` | `quay.io/aethermesh/chart-aether` (1.0.0 onward) |
| crds chart | `ghcr.io/bpalermo/aether/charts/crds` | `quay.io/aethermesh/chart-crds` |
| prober chart | `ghcr.io/bpalermo/aether/charts/prober` | `quay.io/aethermesh/chart-prober` |
| udsecho chart | `ghcr.io/bpalermo/aether/charts/udsecho` | `quay.io/aethermesh/chart-udsecho` |

Not published, so not moved: `l4echo` (an e2e workload, built and kind-loaded
only), and `proxy-ready` / `mesh-dns-ready`, which are extra layers inside the
agent and mesh-dns images rather than images of their own.

## Design: one setting

`bazel/img/registry.bzl` holds the whole decision:

```python
IMAGE_REGISTRY = "quay.io"                   # phase 1: "ghcr.io"
IMAGE_NAMESPACE = "aethermesh"               # phase 1: "bpalermo/aether"
IMAGE_NAME_OVERRIDES = {}                    # phase 1: {"proxy": "aether-proxy"}
CHART_REPOSITORY_PREFIX = "chart-"           # phase 1: "charts/"
SIGNATURE_LAYOUT = "referrer"                # phase 2; absent before = "tag"
PROXY_PIN_LEGACY_REFERENCES = ["ghcr.io/bpalermo/aether/aether-proxy"]  # phase 2; [] in phase 4
```

with `image_repository(component)`, `image_reference(component)`,
`chart_repository(chart)`, `chart_registry_url(chart)`,
`proxy_pin_references()` and `registry_token_url(registry, repo)`. Bazel reads it directly (every
`go_multi_arch_image` call site, the chart pushes and template tests, the e2e
`go_test`'s `x_defs`, `//bazel/proxy_pin`). The `//proxy` workspace is a separate
Bazel module that cannot load from the root, so it carries a byte-identical copy
at `proxy/bazel/registry.bzl`. Everything that is not Bazel — workflows,
verifiers, e2e scripts — asks `scripts/image-registry.sh`, which parses the file
with a strict line-anchored grep (a setting it cannot parse is an error, never a
default). `scripts/check-registry-config.sh` keeps it that way on every PR, and
`//bazel/img:registry_test` pins the shell reader against the Starlark helpers.

The registry library (`scripts/registry-lib.sh`, formerly `ghcr-lib.sh`) is
registry-neutral: anonymous pull tokens from the right endpoint per host, the
same HEAD/GET primitives, and **referrers-based signature discovery** — a
signature is present in exactly one of three layouts: `legacy` (`.sig` tag),
`bundle` (fallback-index tag) or `referrer` (an OCI 1.1 referrer whose bundle
predicate is cosign's sign predicate). A referrers 404 means "no Referrers API"
and falls back to the tags; any other non-200 is inconclusive.

## Phases

1. **Abstraction (no behaviour change).** The single setting, still `ghcr.io`;
   every call site derived from it; the quay-capable library and its offline
   harnesses (a fake quay: `/v2/auth` tokens, a referrers index with a bundle
   referrer, a referrers 404 falling back to tags, a 5xx inconclusive, a paged
   referrers answer, and the verifier end to end against the phase-2 setting with
   referrer-only signatures). Real read-only proofs against ghcr.io and quay.io.
2. **Cut-over — DONE.** The gate went green first (`quay-smoke`, run
   36345331611: the robot pushes to a pre-created repository but cannot create
   one; cosign v3.1.2 keyless `sign --recursive` writes the signature as a
   `referrer`, no tag, on the index and every child; `verify_image_signatures`
   passes on quay). Then, in one PR:
   - **The flip.** `registry.bzl` (and its byte-identical proxy copy) says
     `quay.io` / `aethermesh` / no overrides / `chart-`, plus two new lines:
     `SIGNATURE_LAYOUT = "referrer"` (what the sweep asserts for every commit
     published under this setting) and `PROXY_PIN_LEGACY_REFERENCES` (below).
   - **Every chart major-bumped** (aether 0.95.x → `1.0.0`; crds, prober,
     udsecho → `1.0.0`): the default image repositories moved, which breaks
     anyone overriding `repository` by prefix. Consumers re-point at
     `oci://quay.io/aethermesh/chart-<name>` and upgrade with `helm get values
     -o yaml` → `-f` (never `--reuse-values`).
   - **Charts pushed with oras.** `helm push` cannot name the repository (it
     appends Chart.yaml's `name:`), Quay has no nested repositories, and a bare
     `prober` / `udsecho` would collide with those images. `chart_push`
     (`//bazel/helm:defs.bzl`) writes what `helm push` writes — the `.tgz` as
     the helm chart-content layer, Chart.yaml as JSON under the helm config
     media type (`//tools/chartconfig`) — with the pinned oras (`//tools/oras`,
     1.3.4, sha256-pinned archives in MODULE.bazel) to
     `chart_registry_url(<chart>)` = `quay.io/aethermesh/chart-<name>`, tagged
     with the packaged version; both tag shapes are kept (`<version>` and
     `<version>-<full sha>` for aether). Proved against a local registry:
     identical layer digest and config content to a real `helm push` of the
     same package, and `helm pull` / `helm template oci://…` work unchanged.
   - **Pushes and signing** run in jobs that declare `environment: release`
     and log in to the setting's host with the robot (`publish.yaml`, and
     `proxy-release.yml`'s `build-push`, `manifest` and `sign`). Images keep
     `image_push` + a separate `cosign sign --recursive` by digest (NOT the
     rules_img signer plugin: the smoke measured this path, and it already
     writes referrers on quay).
   - **The split sweep.** `verify-published-artifacts.sh` reads
     `bazel/img/registry.bzl` AS OF EACH PUSH HEAD — host, namespace,
     overrides, chart prefix, and the signature layout it must find there
     (`referrer` on quay; a file without the line promises `tag`) — so heads
     before the cut-over are checked on ghcr.io and heads at or after it on
     quay.io, with no sha or date written down: the cut-over commit is simply
     this PR's merge commit, the first whose file says quay.io. A post-flip head
     whose artifacts are only on ghcr.io is MISSING; a fallback tag on quay.io
     (or a referrer plus a tag) is MISSING. Commits older than the file (before
     #998) use its first version, which phase 1 introduced with no behaviour
     change.
   - **The proxy pin** is data, so the flip cannot move it: it names the ghcr.io
     image until the first proxy release after the flip — this PR's own merge
     triggers one — whose bump-chart PR rewrites `repository:` with `tag:` and
     `digest:`. Every pin reader accepts `proxy_pin_references()`, and the sweep
     looks a pin up on the registry the pin names.
3. **talos rollout.** `helm upgrade` on talos-main from the quay coordinates
   (values from `helm get values -o yaml`, never `--reuse-values`), then an 8h
   soak graded as usual.
4. **Decommission ghcr.** Once no supported release and no cluster references a
   ghcr coordinate: stop the sweep's ghcr branch, update the docs, and leave the
   ghcr packages in place read-only (deleting them would break every historical
   pin).

## Open questions (answered in phase 2)

- **Where does the signature land on quay?** `referrer`, and only a referrer:
  the quay-smoke gate (run 36345331611) measured cosign v3.1.2 keyless `sign
  --recursive` on our own index and every child — no fallback tag. Phase 2 kept
  that path (`image_push` + `cosign sign`), so the `img deploy` signer plugin's
  behaviour never needed measuring. `SIGNATURE_LAYOUT = "referrer"` makes it an
  assertion: a tag, or a tag plus a referrer, is MISSING in the sweep. Still to
  confirm by eye on the first real publish: `registry_referrers` lists the sign
  bundle and both tag lookups 404 on one digest (the double-write check).
- **Chart repository names.** `chart-<name>`, pushed with **oras** (not a chart
  rename, which would change `.Chart.Name` in every template, and not a rules_helm
  change): `chart_push` in `//bazel/helm:defs.bzl`, see phase 2.
- **Repository auto-creation.** **No**: the robot cannot create repositories.
  All 13 were pre-created public, with robot write. A new component needs its
  repository created the same way before its first publish.
- **Pull-rate limits.** Anonymous quay.io pulls are rate-limited per IP; the
  sweep's direct lookups (≈60 HEADs + 27 referrers GETs per commit) and CI's
  proxy-pin extraction must stay well under that. Still open: measure on the
  first sweep.
