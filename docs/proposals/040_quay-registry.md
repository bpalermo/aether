# Proposal 040: Publish to Quay (`quay.io/aethermesh`)

**Status:** Accepted 2026-09-27. Phase 1 (the abstraction) is the PR that adds
this file; phases 2–4 follow.
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
- **Credentials.** Pushes use a Quay **robot account** (`aethermesh+publisher`),
  held as two repository secrets (`QUAY_USERNAME`, `QUAY_PASSWORD`) — no
  long-lived personal token, and nothing to rotate when a maintainer changes.

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
| aether chart | `ghcr.io/bpalermo/aether/charts/aether` | `quay.io/aethermesh/chart-aether` |
| crds chart | `ghcr.io/bpalermo/aether/charts/crds` | `quay.io/aethermesh/chart-crds` |
| prober chart | `ghcr.io/bpalermo/aether/charts/prober` | `quay.io/aethermesh/chart-prober` |
| udsecho chart | `ghcr.io/bpalermo/aether/charts/udsecho` | `quay.io/aethermesh/chart-udsecho` |

Not published, so not moved: `l4echo` (an e2e workload, built and kind-loaded
only), and `proxy-ready` / `mesh-dns-ready`, which are extra layers inside the
agent and mesh-dns images rather than images of their own.

## Design: one setting

`bazel/img/registry.bzl` holds the whole decision:

```python
IMAGE_REGISTRY = "ghcr.io"                        # phase 2: "quay.io"
IMAGE_NAMESPACE = "bpalermo/aether"               # phase 2: "aethermesh"
IMAGE_NAME_OVERRIDES = {"proxy": "aether-proxy"}  # phase 2: {}
CHART_REPOSITORY_PREFIX = "charts/"               # phase 2: "chart-"
```

with `image_repository(component)`, `image_reference(component)`,
`chart_repository(chart)`, `chart_registry_url()` and
`registry_token_url(registry, repo)`. Bazel reads it directly (every
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
2. **Cut-over.** Flip `registry.bzl` (and its proxy copy); **major-bump every
   chart** (the default image repositories move, which is a breaking change for
   anyone overriding `repository` by prefix); `publish.yaml` and
   `proxy-release.yml` log in to quay.io with the robot account and push there.
   Images move from `image_push` + a separate `cosign sign` step to `img deploy`
   with a `signing_config` using the `@rules_img_signer_cosign` plugin and
   `$SIGSTORE_ID_TOKEN` from the Actions OIDC provider. The charts need a push
   that can name the repository (`chart_registry_url()` fails loudly on a flat
   prefix until then — see open questions). publish-verify sweeps **quay.io for
   commits at or after the cut-over and ghcr.io for older push heads** (the
   verifier can read the setting as of each commit, the way it already reads
   chart versions and the release tag). The pinned aether-proxy digest moves with
   the first proxy release after the flip.
3. **talos rollout.** `helm upgrade` on talos-main from the quay coordinates
   (values from `helm get values -o yaml`, never `--reuse-values`), then an 8h
   soak graded as usual.
4. **Decommission ghcr.** Once no supported release and no cluster references a
   ghcr coordinate: stop the sweep's ghcr branch, update the docs, and leave the
   ghcr packages in place read-only (deleting them would break every historical
   pin).

## Open questions

- **Does `img deploy` on quay write the signature as a referrer only?** rules_img
  0.3.21's `signing_config` docstring says the signature is "pushed to the
  image's repository as an OCI referrer"; whether it also writes a fallback tag
  on a registry that serves the API is in a prebuilt Go binary and unverified. A
  tag *and* a referrer would read as `both` (the double-write defect) in the
  sweep. `cosign verify` (v3) discovers a referrer by itself either way. Confirm
  on the first publish: `registry_referrers` + both tag lookups on one digest.
- **Chart repository names.** `helm push` derives the last path segment from
  `Chart.yaml`'s `name:`, so `oci://quay.io/aethermesh` publishes chart `aether`
  as `aethermesh/aether`, not `chart-aether`. Options: rename the charts
  (`name: chart-aether` changes `.Chart.Name` in every template), or push the
  packaged `.tgz` with a tool that names the target (`oras push`/`crane`), or a
  rules_helm change. Decide before the flip.
- **Repository auto-creation.** Can the robot account create a repository on
  first push, or must each of the 13 repositories be pre-created (and made
  public) in the org? Pre-creating is safer: a first push that auto-creates a
  *private* repository fails every anonymous pull and the sweep's witness check.
- **Pull-rate limits.** Anonymous quay.io pulls are rate-limited per IP; the
  sweep's direct lookups (≈60 HEADs + 27 referrers GETs per commit) and CI's
  proxy-pin extraction must stay well under that. Measure on the first sweep.
