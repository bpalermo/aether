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

**A digest is signed and attested once**, by the publish run that first pushed
it. A later commit that publishes the same digest again (an image whose content
did not change) adds a tag and nothing else: no second signature, no second
attestation. So the signature and the provenance of a digest name the commit
that **built it first**, which is not always the commit whose tag you resolved.
"[Which commit built this digest](#which-commit-built-this-digest)" below has
the consequences and the commands.

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
- `--source-digest` pins the commit **that built the digest**. For a chart
  that is the commit in its version. For an image it is the commit that first
  published that digest, which can be older than the commit whose
  `dev-<full git sha>` tag you resolved: pin it only when you know it, and
  otherwise leave it out and read the commit from the output (next section).
- `--limit` is how many attestations of the digest `gh` fetches and checks;
  the default is 30. A digest published by this repository has one, so the
  default is enough. Should a digest ever collect more than 30 (nothing in
  this repository does that today), pass `--limit 1000`, the maximum: the
  statement you pin with `--source-digest` is otherwise missed whenever it is
  not among the 30 fetched. The publish workflow's own check always passes it.

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

Three things bind a digest to a commit, and none of them is inside the image.
Do not look for the commit in the image itself: the Go images (every image
except `proxy`) no longer have the `org.opencontainers.image.revision` label
(#1378), because a label is part of the digest, and a commit in the digest is
what made every image change with every commit and every deploy roll every
workload. The `proxy` image is built separately and still carries that label,
with the Envoy pin beside it; its commit can be read from the image as well.

| Where | What it names | How to read it |
| --- | --- | --- |
| The provenance attestation | the commit, workflow file and run that **first built** this digest | `gh attestation verify … --format json` (below) |
| The cosign signature's certificate | the commit of the run that **first signed** this digest: the same run | pin it with `cosign verify … --certificate-github-workflow-sha <full git sha>`; a wrong value fails with `expected GithubWorkflowSHA to be "…", got "<the commit>"` |
| The tag `dev-<full git sha>` | the digest that commit's publish **resolved to** | `crane digest quay.io/aethermesh/agent:dev-<full git sha>` |

The tag goes from a commit to a digest, and every published commit has one:
if commits A, B and C did not change the agent, `dev-A`, `dev-B` and `dev-C`
all resolve to the same agent digest. The attestation and the signature go the
other way, from a digest to exactly one commit, the first: A.

```bash
gh attestation verify oci://quay.io/aethermesh/agent@sha256:<digest> \
  --repo bpalermo/aether \
  --signer-workflow github.com/bpalermo/aether/.github/workflows/publish.yaml \
  --format json |
  jq -r '.[].verificationResult
         | [.statement.predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit,
            .statement.predicate.runDetails.metadata.invocationId,
            .verifiedTimestamps[0].timestamp]
         | @tsv'
```

That prints the commit, the workflow run and the time the transparency log
witnessed it. `.signature.certificate.sourceRepositoryDigest` holds the same
commit, from the certificate rather than from the statement.

What follows from "once, by the first commit":

- `gh attestation verify --source-digest <C>` on an **image** passes only when
  C is the commit that first built that digest. It fails for a later commit
  that published the same digest unchanged, and that failure does not mean the
  image is not genuine. Verify without `--source-digest` (the repository and
  the signer workflow are still pinned) and read the commit.
- **Every chart is new for every commit**: its version carries the commit and
  so does its `appVersion`. A chart's signature and provenance therefore always
  name the commit in its version, and `--source-digest <C>` is the right check
  for `chart-<name>:<X.Y.Z>-<C>`.
- So, to establish that what runs is what commit C published: resolve and
  verify the chart at C with `--source-digest <C>`; the chart pins every image
  by digest. Then verify each image digest it pins without a commit. Each was
  built by C or by an earlier commit of this repository, and the output says
  which.

The publish workflow checks the same two ways after every run: a digest it
attested in that run against that commit, and a digest it left alone against
the workflow only. The run's summary lists every artifact with its digest,
whether the digest is new, the commit its provenance names and the tag.

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
