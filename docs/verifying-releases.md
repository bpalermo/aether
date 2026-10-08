# Verifying a release

Every image and every Helm chart aether publishes can be checked against the
workflow that built it. Nothing here needs a credential: the registry
repositories and the attestations are public.

There are two independent pieces of evidence for each artifact.

| Evidence | What it says | Where it is stored | Tool |
| --- | --- | --- | --- |
| **cosign signature** (keyless) | The release workflow of `bpalermo/aether` signed this exact digest. | In the registry, next to the artifact (an OCI 1.1 referrer). | `cosign` |
| **SLSA build provenance** | Which commit, workflow file, run and runner produced this digest. | In the repository's attestation store on GitHub, looked up by digest. | `gh` |

Both are made with short-lived certificates from GitHub Actions' OIDC identity
(Sigstore's Fulcio) and recorded in the public Rekor transparency log. No
long-lived signing key exists.

The signature travels with the artifact: a mirror that copies referrers keeps
it. The provenance stays on GitHub: the artifacts are on quay.io, and an
attestation is bound to a digest, not to a location, so `gh` fetches it from
GitHub for whatever digest the registry serves.

## What is published

| Artifact | Repository | Signed by | Provenance |
| --- | --- | --- | --- |
| Images `agent`, `mesh-dns`, `proxy-supervisor`, `uds-csi`, `cni-install`, `registrar`, `controller`, `prober`, `udsecho` | `quay.io/aethermesh/<name>` | `publish.yaml`: the multi-arch index and each per-architecture manifest | `publish.yaml`: the index digest |
| Image `proxy` (Envoy) | `quay.io/aethermesh/proxy` | `proxy-release.yml`: index and each per-architecture manifest | not yet |
| Charts `aether`, `crds`, `prober`, `udsecho` | `oci://quay.io/aethermesh/chart-<name>` | `publish.yaml`: the chart manifest | `publish.yaml`: the chart manifest |

Charts are signed and everything in the provenance column is attested starting
with the first release after this page was added; older releases carry image
signatures only.

## Always verify a digest

A tag can be moved; a digest cannot. Resolve the tag once, then verify and
deploy that digest.

```bash
# An image tag -> its index digest.
crane digest quay.io/aethermesh/agent:dev-<full git sha>

# A chart version -> its manifest digest.
crane digest quay.io/aethermesh/chart-aether:<X.Y.Z>-<full git sha>
```

`oras resolve` and `docker buildx imagetools inspect` answer the same question.
The aether chart pins every image it deploys by digest, so verifying the chart
and then installing that chart digest covers the images it names.

## Check the signature (cosign)

```bash
cosign verify \
  --certificate-identity-regexp '^https://github\.com/bpalermo/aether/\.github/workflows/publish\.yaml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  quay.io/aethermesh/agent@sha256:<digest>
```

The same command verifies a chart; give it the chart reference
(`quay.io/aethermesh/chart-aether@sha256:<digest>`, no `oci://`).

For the proxy image the identity is the other workflow:
`^https://github\.com/bpalermo/aether/\.github/workflows/proxy-release\.yml@refs/heads/main$`.

`cosign verify` checks the one manifest it is given. For an image, that is the
index; the per-architecture manifests under it are signed as well, and
`cosign verify` has no `--recursive`. From a checkout of this repository, one
command walks the index and verifies every child with the pinned cosign:

```bash
bazel run //bazel/cosign:verify_image_signatures -- quay.io/aethermesh/agent@sha256:<index digest>
bazel run //bazel/cosign:verify_image_signatures -- --single quay.io/aethermesh/chart-aether@sha256:<digest>
```

## Check the provenance (gh)

```bash
gh attestation verify oci://quay.io/aethermesh/agent@sha256:<digest> \
  --repo bpalermo/aether \
  --signer-workflow github.com/bpalermo/aether/.github/workflows/publish.yaml \
  --source-digest <full git sha>
```

- `--repo` says whose attestation store to ask and whose builds to accept.
- `--signer-workflow` pins the workflow file. Without it any workflow of the
  repository would do.
- `--source-digest` pins the commit. Leave it out to accept any commit of the
  repository, and read the commit from the output instead
  (`--format json`, `.[].verificationResult.statement.predicate`). For an
  **image**, the commit is the one that first built that digest, which is not
  necessarily the one you deployed: see "Which commit built this digest" below.

A chart is verified the same way, with its own reference:

```bash
gh attestation verify oci://quay.io/aethermesh/chart-aether@sha256:<digest> \
  --repo bpalermo/aether \
  --signer-workflow github.com/bpalermo/aether/.github/workflows/publish.yaml
```

The provenance covers the digest a tag resolves to: an image index, or a chart
manifest. A per-architecture image manifest has a signature but is not an
attestation subject, so verify provenance against the index digest.

## Which commit built this digest

An image does not say. Nothing in an aether image depends on the commit: no
label, no annotation and no version linked into a binary (#1378). That is what
lets an image whose content did not change keep its digest from one commit to
the next, so that a deploy rolls only the components that changed. A label or
an annotation is part of the JSON document it sits in, and so of the digest;
the commit has to live outside it. Three things outside it bind a digest to a
commit:

| Where | What it names | How to read it |
| --- | --- | --- |
| The cosign signature's certificate | the commit of the workflow run that signed this digest | `cosign verify … -o json`, the certificate's `githubWorkflowSha` (or pin it: `--certificate-github-workflow-sha <full git sha>`) |
| The provenance attestation | the commit, workflow file, run and runner that **built** this digest | `gh attestation verify … --format json`, or pin it with `--source-digest <full git sha>` |
| The per-commit tag `dev-<full git sha>` | the digest that commit's publish produced | `crane digest quay.io/aethermesh/agent:dev-<full git sha>` |

They answer different questions once digests are stable.

- **The tag goes from a commit to a digest**, and every published commit has
  one. Many tags can name one digest: if commits A, B and C did not change the
  agent, `dev-A`, `dev-B` and `dev-C` all resolve to the same agent digest.
  "Which digest does commit C deploy" is always answerable this way.
- **The provenance goes from a digest to the commit that first built it.**
  Publishing attests a digest only when it is new, so an unchanged image's
  provenance names the **first** commit that produced that digest (A above), not
  the commit you deployed (C). `gh attestation verify --source-digest C` on an
  image that C did not change therefore **fails**, and that is the expected
  result, not a broken release: verify without `--source-digest` and read the
  commit from the output, or pin the commit on the **chart**, which is new for
  every commit and is attested every time.
- The same holds for the signature: an unchanged digest keeps the signature it
  already has.

So, to establish that what runs is what commit C published:

1. Verify the chart at C: `crane digest quay.io/aethermesh/chart-aether:<X.Y.Z>-<C>`,
   then `cosign verify` and `gh attestation verify --source-digest <C>` on that
   chart digest. The chart is per-commit and pins every image by digest.
2. For any image it pins, verify the signature and the provenance of the digest
   as above, without pinning the commit: the output names the earlier commit
   that built it, which is in C's history.

A binary's own `--version` is its build ID, not a commit: see the runbook, "Which
build is this binary".

## What this does and does not establish

In SLSA terms the provenance is generated by the build platform (GitHub
Actions), not by the build's own steps, and it is signed with an identity the
build's steps cannot choose: **Build Level 2**. It is not Level 3: the release
job itself holds the signing identity and the registry credential, where Level 3
wants the signing isolated from the build (in practice, a reusable workflow that
only signs).

- A valid signature and provenance tell you the artifact came out of this
  repository's release workflow at a given commit. They say nothing about
  whether that commit is one you want.
- There is no SBOM attestation yet.
- Nothing in a cluster checks any of this by itself. An admission controller
  (Kyverno, the Sigstore policy controller) reads the registry, so the cosign
  signatures are what it could enforce; this project has not tested one against
  them. Those tools do not look in GitHub's attestation store.

How the signing is built, and how the repository checks its own releases after
every publish, is in the [runbook](runbook.md) under "Verifying image
signatures".
