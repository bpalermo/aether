# Proposal: Vanity Go module path `aethermesh.dev`

**Status:** Accepted — 2026-08-08 (apex `aethermesh.dev`, served by the docs site).
The serving half (go-import meta + build guard) ships first; the module rename
is the follow-up PR and is gated on the meta being live.
**Author:** Bruno Palermo
**Relates:** the aethermesh.dev website (the serving surface), proposal 010
(the sibling proxy workspace — C++, unaffected).

## Problem Statement

The Go module is named after its hosting, `github.com/bpalermo/aether`. The
project now has its own domain and website; the import path is the one public
name that still points at repository plumbing rather than the project. A module
path is forever once anything depends on it, so if it is ever going to change,
it changes now — before the vanity domain appears in docs and the module
gathers external importers.

## Decision

`module aethermesh.dev` — the apex, no `go.` prefix, no `/aether` suffix.

- **Apex over `go.` subdomain** (tailscale.com / gvisor.dev precedent, vs
  go.uber.org): the `go.` host would exist only to serve two static files and
  needs its own Pages repo, CNAME, and certificate. The docs site already
  serves the apex and already has a fail-closed verification culture to keep
  the meta tag honest. Imports read better: `aethermesh.dev/common/udspath`.
- **Bare apex over `aethermesh.dev/aether`**: the `/aether` segment would
  restate the project name that is already the domain. All Go code lives in
  this one module (the proxy workspace is C++), so the whole host can map to
  the one repository.

## Mechanism

`go get` resolves a custom import path by fetching
`https://aethermesh.dev/<pkg>?go-get=1` and reading a `go-import` meta tag; it
parses the tag regardless of HTTP status. Two tags in the site's shared
`extrahead` block therefore cover every URL on the host:

```html
<meta name="go-import" content="aethermesh.dev git https://github.com/bpalermo/aether">
<meta name="go-source" content="aethermesh.dev https://github.com/bpalermo/aether https://github.com/bpalermo/aether/tree/main{/dir} https://github.com/bpalermo/aether/blob/main{/dir}/{file}#L{line}">
```

- Real pages serve the meta at 200; every other path — which is what package
  subpaths like `/agent/internal/...` are — is served by the site's `404.html`,
  which extends the same template and carries the same tags.
- `go-source` gives pkg.go.dev working "view source" links into GitHub.
- The meta sits **outside** the `{% if page %}` guard in
  `website/overrides/main.html`: static templates (404.html) render with
  `page = None`, and they are the load-bearing case.

**The coupling risk, and its guard.** Module resolution now depends on a docs
site that gets redesigned. The mitigation is the same pattern as every other
invariant this site holds: `build_site.py` fails the build — and with it the
required `//website:site_test` gate — if either `index.html` or `404.html`
stops carrying the exact `go-import` tag. proxy.golang.org caching insulates
existing users from transient outages either way; only first-fetches of new
versions and `GOPROXY=direct` reach the site.

## The rename (follow-up PR)

Atomic, mechanical, wide:

1. `go.mod`: `module aethermesh.dev`; `# gazelle:prefix aethermesh.dev` in the
   root BUILD.bazel.
2. Rewrite every `github.com/bpalermo/aether` import across `**/*.go`.
3. `option go_package` in every proto under `api/`, then regenerate.
4. `make gazelle` (rewrites every `importpath`) + `make tidy`.
5. Sweep docs, CLAUDE.md, website content, e2e scripts.

CI never network-fetches its own module, so the build does not depend on DNS —
but the moment the rename merges, the vanity path is the **only** installable
path, which is why the meta must be verified live first:

```sh
curl -s 'https://aethermesh.dev/common/udspath?go-get=1' | grep go-import
```

## Consequences

- `go get github.com/bpalermo/aether` is frozen at pre-rename tags; new
  versions exist only under `aethermesh.dev`. There is no dual-path option for
  a Go module. README gets a note.
- A future second module (say, a carved-out `api`) would live at
  `aethermesh.dev/api` and need its own meta tag with the longer prefix; the
  single-tag setup maps the whole host to this one repository until then.

## Verification

- Build-time: the `verify_go_import` check in `build_site.py` (index + 404).
- Post-deploy: the `curl … ?go-get=1` gate above, on a real package path.
- Post-rename: `GOPROXY=direct go mod download aethermesh.dev@latest`, then the
  default proxy path, then pkg.go.dev indexing with working source links.

## Alternatives considered (and rejected)

- **`go.aethermesh.dev`**: a second Pages repo + DNS record + certificate to
  serve two files that never change. Isolation from site redesigns is its one
  real advantage; the build guard buys the same property without the
  infrastructure.
- **Keep `github.com/bpalermo/aether`**: free, but permanent — the import path
  is the one name that cannot be migrated gradually, and its cost only grows
  with adoption.
