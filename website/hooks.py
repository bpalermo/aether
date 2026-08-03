"""MkDocs hooks that stage repository markdown into the site at build time.

Nothing under ``website/pages`` is a copy of a repository document. Files that
live in the repository (``README.md``, ``docs/**``, ``charts/README.md``,
``proxy/README.md``, ``AGENTS.md``) are read, transformed and injected as
*generated* MkDocs files here, so the site can never drift from the source of
truth and a stale duplicate can never be committed.

Adding a hand-picked page is one entry in :data:`STATIC_PAGES` plus a ``nav``
entry in ``mkdocs.yml``. Three sections need neither, because each is discovered
and navigated from its own source of truth:

* **Proposals** — every ``docs/proposals/NNN_slug.md`` is discovered, staged,
  indexed and navigated automatically, so dropping a new proposal file into the
  repository is the whole publishing step.
* **Conformance** — ``docs/conformance/gateway-api-features.md`` is the section
  front page, under a headline block whose numbers are *parsed* out of the newest
  ``baseline-*.md`` and out of ``.github/workflows/e2e.yaml``; every baseline is
  staged into a generated archive, newest first. Committing a new baseline
  publishes it and moves the headline.
* **API** — the ``/api/`` reference is not read from the repository at all. Bazel
  runs protoc over each proto package (``//bazel/protodoc``) and passes the
  rendered markdown in on ``AETHER_API_DOCS``; touching a ``.proto`` rebuilds the
  page.

The transform does five things:

1. drops whole ``##`` sections by title (the site has its own Getting Started,
   the licence lives in the footer, and the theme's right-rail supersedes the
   hand-rolled table of contents in ``getting-started.md``);
2. resolves every repository-relative link *from the directory of the document
   it appears in* — links to a document the site publishes become links between
   site pages, everything else becomes an absolute GitHub URL;
3. neutralises ``[[wiki refs]]`` — private-notebook syntax that must never be
   published as a live link — into a muted parenthetical;
4. injects the "design record" banner at the top of every proposal;
5. substitutes ``<!-- aether:readme-mermaid -->`` and
   ``<!-- aether:recent-proposals -->`` in any page, so the landing page shares
   one source with the README and with the proposals index.

Everything is fenced-code aware: nothing inside a ``` block is rewritten.
"""

from __future__ import annotations

import glob
import os
import posixpath
import re
import textwrap
from dataclasses import dataclass, field
from typing import TYPE_CHECKING

from mkdocs.structure.files import File

if TYPE_CHECKING:  # pragma: no cover - typing only
    from mkdocs.config.defaults import MkDocsConfig
    from mkdocs.structure.files import Files
    from mkdocs.structure.pages import Page

REPO_URL = "https://github.com/bpalermo/aether"
BLOB_URL = f"{REPO_URL}/blob/main/"
EDIT_URL = f"{REPO_URL}/edit/main/"

#: Repository file -> (site URI, titles of ``##`` sections to drop).
#:
#: The site URI decides the URL: ``docs/workloads.md`` is served at
#: ``/docs/workloads/``.
STATIC_PAGES: dict[str, tuple[str, tuple[str, ...]]] = {
    "README.md": ("architecture.md", ("Getting Started", "License")),
    # Docs.
    # getting-started.md keeps its hand-rolled table of contents for GitHub
    # readers; here the theme renders one in the right rail, so the section is
    # dropped rather than duplicated.
    "docs/getting-started.md": ("docs/getting-started.md", ("Table of contents",)),
    "docs/workload-requirements.md": ("docs/workloads.md", ()),
    "docs/configuration.md": ("docs/configuration.md", ()),
    "charts/README.md": ("docs/charts.md", ()),
    "docs/observability/README.md": ("docs/observability.md", ()),
    "docs/registry-backend-evolution.md": ("docs/registry.md", ()),
    # Development.
    "docs/runbook.md": ("dev/runbook.md", ()),
    "proxy/README.md": ("dev/proxy.md", ()),
    "AGENTS.md": ("dev/agents.md", ()),
}

#: Repository documents Bazel cannot reach, and the symlink that stands in.
#:
#: ``proxy/`` is listed in ``//.bazelignore`` — it is a sibling Bazel workspace
#: pinned to its own Bazel version — and a file under an ignored directory can
#: never be an action input, so no filegroup can carry ``proxy/README.md`` into
#: the sandbox. ``website/staged/proxy-README.md`` is a symlink to it: Bazel
#: reads through it, the site still renders the single copy of that document,
#: and everything else (links, ``edit_url``) keeps using the real path.
READ_THROUGH: dict[str, str] = {
    "proxy/README.md": "website/staged/proxy-README.md",
}

#: Where the proposals live, and the directory they are published under.
PROPOSALS_DIR = "docs/proposals"
PROPOSALS_URI = "proposals"

#: Where the conformance record lives, and the section it is published under.
CONFORMANCE_DIR = "docs/conformance"
CONFORMANCE_URI = "conformance"
#: The section front page: the honest supported-features list.
FEATURES_DOC = f"{CONFORMANCE_DIR}/gateway-api-features.md"
#: The nightly suite that turned conformance from a measurement into a gate.
E2E_WORKFLOW = ".github/workflows/e2e.yaml"

#: The generated protobuf reference, one page per proto package.
API_URI = "api"

#: The proto packages published under ``/api/``, in nav order. The key is the URL
#: segment and the key ``//website:BUILD`` passes on ``AETHER_API_DOCS``. The
#: blurb is the only thing on the page not derived from the protos themselves.
API_PAGES: dict[str, tuple[str, str]] = {
    "config": (
        "Config",
        "The CRD schemas. `MeshConfig`, `HTTPFilter`, `EdgeConfig` and `EndpointPolicy` are "
        "Kubernetes custom resources, but their `.spec` is not written in Go — it is these "
        "messages. `common/apis/config/v1` wraps them with the deepcopy and JSON glue "
        "Kubernetes needs and adds no fields, so what is documented here is exactly what a "
        "manifest may contain.",
    ),
    "registry": (
        "Registry",
        "The service registry: what an endpoint is, how a service is keyed, and the route and "
        "configuration projections that travel between clusters. Written through the "
        "registrar, read by every node agent.",
    ),
    "registrar": (
        "Registrar",
        "The gRPC contract between a node agent and its cluster's registrar — the versioned "
        "endpoint snapshot, the watch stream that fans changes out to agents, and the "
        "cross-cluster configuration listing.",
    ),
    "cni": (
        "CNI",
        "The gRPC contract between the CNI plugin and the node agent: what the plugin reports "
        "about a pod when the container runtime calls ADD, and what it withdraws at DEL.",
    ),
}

#: Injected at the top of every proposal. A proposal is a design record, not
#: documentation: it is published as written, and the reader has to be told that
#: before they read it as a description of the running system.
BANNER_BODY = (
    "**Design record.** This proposal is published as written, at its stated "
    "status. Later proposals may supersede parts of it, and implementation "
    "details drift. It documents the reasoning at a point in time, not the "
    "current behaviour of the system — for that, see the {docs_link}."
)

#: Placeholders replaced on any page that carries them.
MERMAID_MARKER = "<!-- aether:readme-mermaid -->"
RECENT_PROPOSALS_MARKER = "<!-- aether:recent-proposals -->"

#: How many proposals the landing page lists.
RECENT_PROPOSALS = 5

_FENCE_RE = re.compile(r"^(?P<indent>\s*)(?P<fence>```+|~~~+)")
_LINK_RE = re.compile(r"(?<=\])\((?P<target>[^)\s]+)(?P<title>\s+\"[^\"]*\")?\)")
_REF_DEF_RE = re.compile(r"^(?P<prefix>\s{0,3}\[[^\]]+\]:\s+)(?P<target>\S+)", re.MULTILINE)
_WIKI_RE = re.compile(r"\[\[(?P<ref>[^\[\]]+)\]\]")
_MERMAID_BLOCK_RE = re.compile(r"^```mermaid\n.*?^```", re.MULTILINE | re.DOTALL)
_EXTERNAL_RE = re.compile(r"^(?:[a-z][a-z0-9+.-]*:|//|#)", re.IGNORECASE)

_PROPOSAL_FILE_RE = re.compile(r"^(?P<number>\d{3})_(?P<slug>.+)\.md$")


# --------------------------------------------------------------------------- #
# Repository access
# --------------------------------------------------------------------------- #


def _repo_root(config: MkDocsConfig) -> str:
    """Locate the repository root holding the files we stage.

    ``AETHER_REPO_ROOT`` is set by ``//website:build_site`` so the Bazel action
    reads the declared input rather than guessing. Falling back to the parent of
    ``website/`` keeps ``mkdocs serve`` working from a plain checkout.
    """
    override = os.environ.get("AETHER_REPO_ROOT")
    if override:
        return override
    config_dir = os.path.dirname(os.path.abspath(config.config_file_path))
    return os.path.dirname(config_dir)


def _read(config: MkDocsConfig, repo_path: str) -> str:
    read_path = READ_THROUGH.get(repo_path, repo_path)
    with open(os.path.join(_repo_root(config), read_path), encoding="utf-8") as handle:
        return handle.read()


# --------------------------------------------------------------------------- #
# The staging table
# --------------------------------------------------------------------------- #


@dataclass(frozen=True)
class Proposal:
    """One ``docs/proposals/NNN_slug.md``, parsed for the generated index."""

    number: str
    slug: str
    src_uri: str
    title: str
    #: The status line exactly as the proposal states it, links rewritten. Never
    #: replaced by the badge — the wording carries nuance the badge cannot.
    status_raw: str
    #: One of implemented / accepted / design / superseded / proposed.
    status_key: str
    date: str
    #: Relates:/Supersedes:/Follows: text, folded into the client-side search.
    refs: str
    #: Number of the proposal that supersedes this one, when it is parseable.
    superseded_by: str | None = None


@dataclass(frozen=True)
class Baseline:
    """One ``docs/conformance/baseline-*.md`` conformance run."""

    #: ``2026-06-27``, from the filename.
    date: str
    #: ``rev21``, from the filename; empty for the very first run.
    rev: str
    #: Sort key: revision number, or 0 for the unrevised first baseline.
    rev_number: int
    repo_path: str
    src_uri: str
    title: str
    #: The run's own one-line verdict, from its ``## TL;DR — …`` heading or its
    #: ``Headline:`` paragraph. Empty when the document states neither.
    verdict: str


@dataclass(frozen=True)
class ApiPage:
    """One generated protobuf reference page."""

    #: The key Bazel passes on ``AETHER_API_DOCS``, and the URL segment.
    key: str
    title: str
    #: ``aether.config.v1``, derived from the rendered file paths.
    proto_package: str
    #: ``api/aether/config/v1``, likewise.
    repo_dir: str
    #: The ``.proto`` files rendered on the page, in the order protoc saw them.
    proto_files: tuple[str, ...]
    src_uri: str
    markdown: str


@dataclass
class Staging:
    """Everything derived from the repository for one build."""

    #: repository path -> site source URI.
    pages: dict[str, str] = field(default_factory=dict)
    #: site source URI -> repository path (for ``edit_url``).
    sources: dict[str, str] = field(default_factory=dict)
    #: repository path -> ``##`` section titles to drop.
    drops: dict[str, tuple[str, ...]] = field(default_factory=dict)
    proposals: list[Proposal] = field(default_factory=list)
    #: Conformance baselines, newest first.
    baselines: list[Baseline] = field(default_factory=list)
    #: Generated protobuf reference pages, in :data:`API_PAGES` order.
    api: list[ApiPage] = field(default_factory=list)


#: Rebuilt by :func:`on_config` on every build (``mkdocs serve`` included).
_staging = Staging()


def _proposal_paths(config: MkDocsConfig) -> list[tuple[str, str, str]]:
    """``(number, slug, repository path)`` for every proposal, lowest first."""
    root = _repo_root(config)
    found: list[tuple[str, str, str]] = []
    for path in sorted(glob.glob(os.path.join(root, PROPOSALS_DIR, "*.md"))):
        name = os.path.basename(path)
        match = _PROPOSAL_FILE_RE.match(name)
        if match is None:
            raise RuntimeError(
                f"{PROPOSALS_DIR}/{name} is not named NNN_slug.md, so it has no stable "
                "proposal number or URL. Rename it, or move it out of the proposals "
                "directory."
            )
        found.append((match.group("number"), match.group("slug"), f"{PROPOSALS_DIR}/{name}"))
    if not found:
        raise RuntimeError(f"no proposals found under {PROPOSALS_DIR}")
    return found


def _build_staging(config: MkDocsConfig) -> Staging:
    staging = Staging()

    for repo_path, (src_uri, drops) in STATIC_PAGES.items():
        staging.pages[repo_path] = src_uri
        staging.sources[src_uri] = repo_path
        staging.drops[repo_path] = drops

    # Every proposal's URL is registered before any of them is parsed, so that
    # cross-references between proposals resolve in both directions.
    discovered: list[tuple[str, str, str, str]] = []
    for number, slug, repo_path in _proposal_paths(config):
        src_uri = f"{PROPOSALS_URI}/{number}-{slug.replace('_', '-')}.md"
        staging.pages[repo_path] = src_uri
        staging.sources[src_uri] = repo_path
        discovered.append((number, slug, repo_path, src_uri))

    for number, slug, repo_path, src_uri in discovered:
        staging.proposals.append(
            _parse_proposal(number, slug, repo_path, src_uri, _read(config, repo_path), staging)
        )

    # Conformance. The features document is the section front page; every
    # baseline is an archive page under it, at a URL that never moves.
    staging.pages[FEATURES_DOC] = f"{CONFORMANCE_URI}/index.md"
    staging.sources[f"{CONFORMANCE_URI}/index.md"] = FEATURES_DOC
    for baseline in _baselines(config):
        staging.pages[baseline.repo_path] = baseline.src_uri
        staging.sources[baseline.src_uri] = baseline.repo_path
        staging.baselines.append(baseline)
    # `/conformance/latest/` republishes whichever baseline is newest, so a link
    # to "the current run" keeps working. It is a second URL for a document that
    # already has a permanent one, so it is not registered in `pages`: a
    # cross-reference between baselines still resolves to the stable archive URL.
    staging.sources[f"{CONFORMANCE_URI}/latest.md"] = staging.baselines[0].repo_path

    staging.api.extend(_api_pages())

    return staging


# --------------------------------------------------------------------------- #
# Conformance
#
# Two sources, both parsed, neither restated by hand: the newest baseline is
# where the numbers come from, and the nightly workflow is where the *gate*
# comes from. A published claim about conformance that nothing in the repository
# backs is exactly the failure mode this section exists to avoid, so anything
# unparseable raises rather than being filled in.
# --------------------------------------------------------------------------- #

_BASELINE_FILE_RE = re.compile(r"^baseline-(?P<date>\d{4}-\d{2}-\d{2})(?:-rev(?P<rev>\d+))?\.md$")

#: The suite prints one of these per profile per level. This is the only place
#: the counts are stated machine-readably, so it is the only place they are read.
_PROFILE_RESULT_RE = re.compile(
    r"^(?P<profile>GATEWAY-HTTP|MESH-HTTP)\s+(?P<level>core|extended):\s*"
    r"Passed:\s*(?P<passed>\d+)\s+Failed:\s*(?P<failed>\d+)",
    re.MULTILINE,
)
_SUITE_VERSION_RE = re.compile(r"gateway-api/conformance`?\s*@\s*\*\*(?P<version>v[\d.]+)\*\*")
_TLDR_RE = re.compile(r"^##\s+TL;DR\s*[—–-]\s*(?P<verdict>.+?)\s*$", re.MULTILINE)
_HEADLINE_RE = re.compile(
    r"^(?:\*\*)?Headline(?P<qualifier>[^:*]*)(?:\*\*)?:(?:\*\*)?\s*", re.MULTILINE
)
#: rev21's own warning against reading the document as a clean sweep. It is
#: quoted into the section header, and it is never removed from the page itself.
_CAVEAT_RE = re.compile(r"Do not read this doc as[^.]*\.", re.DOTALL)

#: `Hard gate (...)` in the nightly e2e workflow: the parenthetical is the job's
#: own statement of what it enforces.
_HARD_GATE_RE = re.compile(r"Hard gate \((?P<gate>[^)]*)\)")
_WORKFLOW_JOB_RE = re.compile(r"^  (?P<job>[a-z0-9][a-z0-9-]*):$", re.MULTILINE)
_GATEWAY_API_VERSION_RE = re.compile(
    r"^\s*GATEWAY_API_VERSION:\s*(?P<version>\S+)\s*$", re.MULTILINE
)


def _strip_inline(text: str) -> str:
    """Bare text for a table cell: links flattened, emphasis and code dropped.

    Unlike :func:`_plain` this keeps ``/`` and ``_``, because the thing being
    flattened is a score — ``33/33``, ``4 PASS / 3 FAIL`` — and a slash is load
    bearing.
    """
    text = _WIKI_RE.sub(lambda m: m.group("ref"), text)
    text = _MD_LINK_RE.sub(r"\1", text)
    text = re.sub(r"[`*]", "", text)
    return _cell(text)


def _paragraph_at(markdown: str, start: int) -> str:
    """The paragraph beginning at ``start``, unwrapped onto one line."""
    end = markdown.find("\n\n", start)
    body = markdown[start:] if end < 0 else markdown[start:end]
    return " ".join(body.split())


#: Every baseline titles itself "Gateway API conformance — <what happened>
#: (<date>)". The archive is already a table of conformance runs with a date
#: column, so both ends are noise repeated on every row.
_BASELINE_TITLE_RE = re.compile(
    r"^Gateway API conformance\s*[—–:-]\s*|\s*\(\d{4}-\d{2}-\d{2}\)\s*$"
)


def _clean_baseline_title(raw: str) -> str:
    return _BASELINE_TITLE_RE.sub("", _BASELINE_TITLE_RE.sub("", raw.strip())).strip()


def _first_sentence(text: str, limit: int = 180) -> str:
    """A verdict is one claim. Clip to it, and mark the clip when it is a clip."""
    match = re.search(r"\.(?:\s|$)", text)
    if match and match.start() < limit:
        return text[: match.start() + 1]
    if len(text) <= limit:
        return text
    return text[:limit].rsplit(" ", 1)[0] + " …"


def _baseline_verdict(markdown: str) -> str:
    """The run's own headline, preferring the TL;DR heading it usually carries.

    Falls back to the ``Headline:`` paragraph, and then to nothing: a baseline
    that states no verdict is listed by date and title, not by a verdict this
    file invented for it.
    """
    tldr = _TLDR_RE.search(markdown)
    if tldr:
        return _strip_inline(tldr.group("verdict"))
    headline = _HEADLINE_RE.search(markdown)
    if headline:
        qualifier = headline.group("qualifier").strip()
        body = _first_sentence(_strip_inline(_paragraph_at(markdown, headline.end())))
        return f"{qualifier.capitalize()}: {body}" if qualifier else body
    return ""


def _baselines(config: MkDocsConfig) -> list[Baseline]:
    """Every conformance baseline, newest first.

    Newest is decided by the date in the filename and then by the revision
    number, because a single day carries several revisions — nothing here is
    pinned to a particular run.
    """
    root = _repo_root(config)
    found: list[Baseline] = []
    for path in sorted(glob.glob(os.path.join(root, CONFORMANCE_DIR, "baseline-*.md"))):
        name = os.path.basename(path)
        match = _BASELINE_FILE_RE.match(name)
        if match is None:
            raise RuntimeError(
                f"{CONFORMANCE_DIR}/{name} is not named baseline-YYYY-MM-DD[-revN].md, so the "
                "archive cannot order it or give it a stable URL. Rename it, or move it out of "
                "the conformance directory."
            )
        repo_path = f"{CONFORMANCE_DIR}/{name}"
        markdown = _read(config, repo_path)
        heading = _H1_RE.search(markdown)
        if heading is None:
            raise RuntimeError(f"{repo_path} has no H1; the conformance archive has no title.")
        rev = match.group("rev") or ""
        slug = f"{match.group('date')}-rev{rev}" if rev else match.group("date")
        found.append(
            Baseline(
                date=match.group("date"),
                rev=f"rev{rev}" if rev else "",
                rev_number=int(rev) if rev else 0,
                repo_path=repo_path,
                src_uri=f"{CONFORMANCE_URI}/archive/{slug}.md",
                title=_clean_baseline_title(heading.group("title")),
                verdict=_baseline_verdict(markdown),
            )
        )
    if not found:
        raise RuntimeError(f"no conformance baselines found under {CONFORMANCE_DIR}")
    return sorted(found, key=lambda b: (b.date, b.rev_number), reverse=True)


def _profile_results(markdown: str, repo_path: str) -> dict[tuple[str, str], tuple[int, int]]:
    """``(profile, level) -> (passed, failed)`` as the suite printed it."""
    results = {
        (match.group("profile"), match.group("level")): (
            int(match.group("passed")),
            int(match.group("failed")),
        )
        for match in _PROFILE_RESULT_RE.finditer(markdown)
    }
    for key in (
        ("GATEWAY-HTTP", "core"),
        ("GATEWAY-HTTP", "extended"),
        ("MESH-HTTP", "core"),
    ):
        if key not in results:
            raise RuntimeError(
                f"{repo_path} carries no verbatim '{key[0]}  {key[1]}: Passed: N  Failed: N' line, "
                "so the conformance headline has no measured numbers to state. Paste the suite's "
                "own summary block into the baseline, or the site would have to invent them."
            )
    return results


def _workflow_gate(workflow: str, job: str) -> str:
    """The ``Hard gate (...)`` the named job's own documentation claims.

    The comment sits above the job for one job and inside the body for another,
    so the search region is the job's comment block plus its body — bounded by
    the *next* job's comment block, or the gate below would be read as this
    job's.
    """
    body_at = workflow.find("\njobs:\n")
    if body_at < 0:
        raise RuntimeError(f"{E2E_WORKFLOW} has no `jobs:` block.")

    starts = [
        (match.group("job"), _comment_block_start(workflow, match.start()))
        for match in _WORKFLOW_JOB_RE.finditer(workflow, body_at)
    ]
    for index, (name, start) in enumerate(starts):
        if name != job:
            continue
        end = starts[index + 1][1] if index + 1 < len(starts) else len(workflow)
        gates = _HARD_GATE_RE.findall(workflow[start:end])
        if not gates:
            raise RuntimeError(
                f"the `{job}` job in {E2E_WORKFLOW} no longer documents a `Hard gate (...)`. The "
                "conformance page states what CI enforces on the workflow's own authority; it "
                "cannot state it if the workflow stopped saying so."
            )
        return gates[-1]
    raise RuntimeError(f"{E2E_WORKFLOW} has no `{job}` job; the conformance page cites it.")


def _comment_block_start(text: str, at: int) -> int:
    """Walk back from ``at`` over the contiguous comment lines above it."""
    start = at
    while start > 0:
        previous = text.rfind("\n", 0, start - 1) + 1
        if not text[previous:start].lstrip().startswith("#"):
            break
        start = previous
    return start


# --------------------------------------------------------------------------- #
# Transform
# --------------------------------------------------------------------------- #


def _split_fences(markdown: str) -> list[tuple[bool, str]]:
    """Split markdown into ``(is_code, text)`` runs, honouring fenced blocks."""
    segments: list[tuple[bool, str]] = []
    buffer: list[str] = []
    closing: str | None = None

    for line in markdown.splitlines(keepends=True):
        match = _FENCE_RE.match(line)
        if closing is None:
            if match:
                segments.append((False, "".join(buffer)))
                buffer = [line]
                closing = match.group("fence")[0] * 3
            else:
                buffer.append(line)
        else:
            buffer.append(line)
            if match and match.group("fence").startswith(closing):
                segments.append((True, "".join(buffer)))
                buffer = []
                closing = None

    segments.append((closing is not None, "".join(buffer)))
    return [segment for segment in segments if segment[1]]


def _map_prose(markdown: str, fn) -> str:
    return "".join(text if is_code else fn(text) for is_code, text in _split_fences(markdown))


def _drop_sections(markdown: str, titles: tuple[str, ...]) -> str:
    """Remove ``## <title>`` sections, up to the next heading of the same level."""
    if not titles:
        return markdown

    wanted = {title.strip().lower() for title in titles}
    kept: list[str] = []
    dropping = False

    for is_code, text in _split_fences(markdown):
        if is_code:
            if not dropping:
                kept.append(text)
            continue
        for line in text.splitlines(keepends=True):
            if line.startswith("# ") or (line.startswith("## ") and not line.startswith("###")):
                title = line.lstrip("#").strip().lower()
                dropping = line.startswith("## ") and title in wanted
            if not dropping:
                kept.append(line)

    return "".join(kept).rstrip() + "\n"


def _resolve(repo_path: str, target: str) -> str:
    """Resolve a link written inside ``repo_path`` to a repository-root path."""
    joined = posixpath.normpath(posixpath.join(posixpath.dirname(repo_path), target))
    # A link that climbs out of the repository is left alone rather than turned
    # into a nonsense GitHub URL.
    return target if joined.startswith("..") else joined


def _rewrite_link(target: str, repo_path: str, src_uri: str, staging: Staging) -> str:
    """Point one link at the site page that publishes it, or else at GitHub."""
    if _EXTERNAL_RE.match(target) or target.startswith("/"):
        return target

    path, _, fragment = target.partition("#")
    if not path:  # a bare in-page anchor
        return target

    resolved = _resolve(repo_path, path)
    published = staging.pages.get(resolved)
    if published is not None:
        # Relative between site pages, so mkdocs resolves and validates the
        # target — and its anchor — under `strict`, instead of the site shipping
        # an absolute URL nothing checks.
        rewritten = posixpath.relpath(published, posixpath.dirname(src_uri))
    else:
        rewritten = BLOB_URL + resolved

    return f"{rewritten}#{fragment}" if fragment else rewritten


def _rewrite_links(markdown: str, repo_path: str, src_uri: str, staging: Staging) -> str:
    def link(match: re.Match) -> str:
        target = _rewrite_link(match.group("target"), repo_path, src_uri, staging)
        return f"({target}{match.group('title') or ''})"

    def ref_def(match: re.Match) -> str:
        target = _rewrite_link(match.group("target"), repo_path, src_uri, staging)
        return f"{match.group('prefix')}{target}"

    return _REF_DEF_RE.sub(ref_def, _LINK_RE.sub(link, markdown))


def _neutralize_wiki_refs(markdown: str) -> str:
    return _WIKI_RE.sub(
        lambda m: f'<span class="aether-private-note">(private note: {m.group("ref").strip()})</span>',
        markdown,
    )


def transform(
    markdown: str,
    repo_path: str,
    src_uri: str,
    staging: Staging,
    drop_sections: tuple[str, ...] = (),
) -> str:
    """Apply the full staging transform to a repository document."""
    markdown = _drop_sections(markdown, drop_sections)
    markdown = _map_prose(markdown, lambda text: _rewrite_links(text, repo_path, src_uri, staging))
    return _map_prose(markdown, _neutralize_wiki_refs)


def _banner(src_uri: str) -> str:
    """The design-record admonition, as markdown, for a page at ``src_uri``."""
    docs_link = posixpath.relpath("docs/getting-started.md", posixpath.dirname(src_uri))
    body = BANNER_BODY.format(docs_link=f"[docs]({docs_link})")
    return '!!! note ""\n\n' + "".join(
        f"    {line}\n" for line in textwrap.wrap(body, width=84, break_long_words=False)
    )


def _inject_after_h1(markdown: str, block: str) -> str:
    """Put a generated block immediately below the document's own title."""
    lines = markdown.splitlines(keepends=True)
    for index, line in enumerate(lines):
        if line.startswith("# "):
            head = "".join(lines[: index + 1])
            tail = "".join(lines[index + 1 :]).lstrip("\n")
            return f"{head}\n{block}\n{tail}"
    return f"{block}\n{markdown}"


def _inject_banner(markdown: str, src_uri: str) -> str:
    """Put the banner immediately after the H1, above the status block."""
    return _inject_after_h1(markdown, _banner(src_uri))


# --------------------------------------------------------------------------- #
# Proposal parsing
#
# The corpus was hand-written over months and the header is not a schema: the
# status is `**Status:** ...` on some proposals and `Status: ...` on others, it
# wraps onto continuation lines, and it says things like "All findings fixed" or
# "Phases 1 + 1b merged". Everything below is deliberately tolerant, and the raw
# string is always published next to the badge, so nothing this normalisation
# flattens is lost.
# --------------------------------------------------------------------------- #

_H1_RE = re.compile(r"^#\s+(?P<title>.+?)\s*$", re.MULTILINE)
_TITLE_PREFIX_RE = re.compile(r"^(?:proposal\s*\d{0,3}\s*[:—–-]|\d{3}\s*[:—–-])\s*", re.IGNORECASE)
_FIELD_RE = re.compile(r"^\s*(?:\*\*)?(?P<name>[A-Z][A-Za-z /-]{0,24}):(?:\*\*)?\s*(?P<value>.*)$")
_BLOCK_START_RE = re.compile(r"^(?:#|>|\||```|~~~|[-*+]\s|\d+\.\s)")
_DATE_RE = re.compile(r"\b(\d{4}-\d{2}-\d{2})\b")
#: The head of a status is the claim, before the qualification introduced by a
#: dash or a semicolon: `Implemented — the CRD was later retired` is Implemented,
#: and the retirement is nuance that stays in the raw string.
_STATUS_HEAD_RE = re.compile(r"\s+[—–]\s+|\s+-\s+|;")
_SUPERSEDER_RE = re.compile(
    r"\((?P<link>\d{3})_[^)]*\.md\)|proposals?\s+(?P<bare>\d{3})", re.IGNORECASE
)

#: Checked in order against the head of the status; first hit wins. `superseded`
#: is first because "Implemented, then superseded" is superseded — and it does
#: not match "supersedes", which is the opposite claim.
_STATUS_RULES: tuple[tuple[str, str], ...] = (
    (r"supersed(ed|ing)", "superseded"),
    (r"implement|shipped|merged|validated|fixed|complete", "implemented"),
    (r"accepted|approved", "accepted"),
    (r"design|draft|spike|investigation|assessment", "design"),
    (r"proposed|idea|open", "proposed"),
)

#: Badge label per normalised status. The five buckets, in index order.
STATUS_LABELS: dict[str, str] = {
    "implemented": "Implemented",
    "accepted": "Accepted",
    "design": "Design",
    "proposed": "Proposed",
    "superseded": "Superseded",
}


def _header_block(markdown: str) -> list[str]:
    """The lines before the first ``##`` — where the metadata lives."""
    lines: list[str] = []
    for line in markdown.splitlines():
        if line.startswith("## "):
            break
        lines.append(line)
    return lines


def _field(lines: list[str], *names: str) -> str:
    """Read a ``**Name:** value`` field, following its continuation lines.

    A continuation is any non-empty line that is not itself a field and does not
    open a block (heading, quote, list, table, fence). ``*emphasis*`` and
    ``**bold**`` open plenty of continuation lines, so a leading asterisk only
    ends the field when it actually opens a list item.
    """
    wanted = {name.lower() for name in names}
    collected: list[str] = []
    for index, line in enumerate(lines):
        match = _FIELD_RE.match(line)
        if match is None or match.group("name").strip().lower() not in wanted:
            continue
        collected.append(match.group("value").strip())
        for follow in lines[index + 1 :]:
            stripped = follow.strip()
            if not stripped or _FIELD_RE.match(follow) or _BLOCK_START_RE.match(stripped):
                break
            collected.append(stripped)
        break
    return " ".join(collected).strip()


def _classify_status(status: str) -> str:
    head = _STATUS_HEAD_RE.split(status, maxsplit=1)[0] if status else ""
    for candidate in (head, status):
        for pattern, key in _STATUS_RULES:
            if re.search(pattern, candidate, re.IGNORECASE):
                return key
    # Unrecognisable wording: a proposal that states a status we cannot place is
    # still a design record, and the raw string is on the page either way.
    return "design"


def _superseder(status: str, number: str) -> str | None:
    at = status.lower().find("supersed")
    if at < 0:
        return None
    for match in _SUPERSEDER_RE.finditer(status[at:]):
        found = match.group("link") or match.group("bare")
        if found and found != number:
            return found
    return None


def _clean_title(raw: str) -> str:
    return _TITLE_PREFIX_RE.sub("", raw).strip()


def _parse_proposal(
    number: str, slug: str, repo_path: str, src_uri: str, markdown: str, staging: Staging
) -> Proposal:
    heading = _H1_RE.search(markdown)
    if heading is None:
        raise RuntimeError(f"{repo_path} has no H1; the proposals index has no title to show.")

    header = _header_block(markdown)
    status_raw = _field(header, "Status")
    if not status_raw:
        raise RuntimeError(
            f"{repo_path} states no Status in its header block. Every proposal states its "
            "status, and the index badge is derived from it."
        )

    date = _field(header, "Date")
    date_match = _DATE_RE.search(date) or _DATE_RE.search("\n".join(header))
    refs = " ".join(
        value
        for value in (_field(header, name) for name in ("Relates", "Supersedes", "Follows"))
        if value
    )

    # The status is republished on the index page, so the links inside it are
    # rewritten relative to the index, not to the proposal.
    status_for_index = _rewrite_links(status_raw, repo_path, f"{PROPOSALS_URI}/index.md", staging)

    return Proposal(
        number=number,
        slug=slug,
        src_uri=src_uri,
        title=_clean_title(heading.group("title")),
        status_raw=_neutralize_wiki_refs(status_for_index),
        status_key=_classify_status(status_raw),
        date=date_match.group(1) if date_match else "",
        refs=refs,
        superseded_by=_superseder(status_raw, number),
    )


# --------------------------------------------------------------------------- #
# Generated pages
# --------------------------------------------------------------------------- #


_MD_LINK_RE = re.compile(r"\[([^\]]*)\]\([^)]*\)")


def _cell(text: str) -> str:
    """A markdown table cell holds no bare pipe and no line break."""
    return " ".join(text.split()).replace("|", "\\|")


def _plain(text: str) -> str:
    """Strip markdown to bare words, for text that is only ever searched.

    The hidden haystack in each row is never rendered, so any surviving markup
    would be dead weight at best — and a relative link mkdocs would rightly
    flag as dangling at worst.
    """
    text = _WIKI_RE.sub(lambda m: m.group("ref"), text)
    text = _MD_LINK_RE.sub(r"\1", text)
    text = re.sub(r"[_/]", " ", text)
    text = re.sub(r"[`*\[\]<>&]", "", text)
    return _cell(text)


def _index_markdown(staging: Staging) -> str:
    """The proposals index: one dense table over the files, filtered in the page.

    Nothing here is hand-maintained. The rows, the counts on the filter chips and
    the five buckets all come from the proposal files themselves.
    """
    proposals = sorted(staging.proposals, key=lambda p: p.number, reverse=True)
    counts = {key: 0 for key in STATUS_LABELS}
    for proposal in proposals:
        counts[proposal.status_key] += 1

    chips = [
        '<button type="button" class="aether-chip is-on" data-status="all">All '
        f"<span>{len(proposals)}</span></button>"
    ]
    chips += [
        f'<button type="button" class="aether-chip" data-status="{key}">'
        f'<span class="aether-dot aether-dot--{key}"></span>{label} '
        f"<span>{counts[key]}</span></button>"
        for key, label in STATUS_LABELS.items()
        if counts[key]
    ]

    by_number = {proposal.number: proposal for proposal in staging.proposals}
    rows: list[str] = []
    for proposal in proposals:
        href = posixpath.relpath(proposal.src_uri, PROPOSALS_URI)
        status = proposal.status_raw
        target = by_number.get(proposal.superseded_by or "")
        if target is not None:
            link = posixpath.relpath(target.src_uri, PROPOSALS_URI)
            if f"({link})" not in status:
                status = f"{status} → [{target.number}]({link})"
        # The hidden span is invisible but part of textContent, so the free-text
        # filter also searches the slug and the Relates:/Supersedes:/Follows: refs.
        haystack = _plain(f"{proposal.slug.replace('_', ' ')} {proposal.refs}")
        rows.append(
            f"| [{proposal.number}]({href}) "
            f"| [{_cell(proposal.title)}]({href})"
            f'<span class="aether-hay" hidden>{haystack}</span> '
            f'| <span class="aether-dot aether-dot--{proposal.status_key}" '
            f'data-status="{proposal.status_key}" '
            f'title="{STATUS_LABELS[proposal.status_key]}"></span> {_cell(status)} '
            f"| {proposal.date or '—'} |"
        )

    lowest = min(proposals, key=lambda p: p.number)
    return (
        # The page is one table under one heading: a right-rail table of
        # contents would hold a single entry and take a third of the width the
        # table wants.
        "---\n"
        "hide:\n"
        "  - toc\n"
        "---\n"
        "\n"
        "# Proposals\n"
        "\n"
        "Every design decision in aether is written down before it is built, and each\n"
        "record is published as it was written. The status is the proposal's own wording;\n"
        "the dot beside it is that wording sorted into five buckets, nothing more.\n"
        "\n"
        # No `markdown` attribute: the control panel is HTML through and
        # through, and passing it through raw keeps markdown from wrapping the
        # input in a paragraph.
        '<div class="aether-filter">\n'
        '<input type="search" class="aether-filter__q" placeholder="Filter proposals…"'
        ' aria-label="Filter proposals" autocomplete="off" spellcheck="false">\n'
        '<div class="aether-chips" role="group" aria-label="Filter by status">\n'
        f"{chr(10).join(chips)}\n"
        "</div>\n"
        '<p class="aether-filter__count" role="status" aria-live="polite"></p>\n'
        "</div>\n"
        "\n"
        '<div class="aether-proposals" markdown="1">\n'
        "\n"
        "| # | Proposal | Status | Date |\n"
        "|---|---|---|---|\n"
        f"{chr(10).join(rows)}\n"
        "\n"
        "</div>\n"
        "\n"
        f"A number is enough to cite one: `/{PROPOSALS_URI}/{lowest.number}/` redirects to\n"
        "the full URL of that proposal, and keeps working when its title changes.\n"
    )


def _redirect_html(proposal: Proposal) -> str:
    """A dependency-free short link: ``/proposals/018/`` -> the full URL."""
    target = f"../{posixpath.basename(proposal.src_uri)[: -len('.md')]}/"
    title = f"Proposal {proposal.number}"
    return (
        "<!doctype html>\n"
        '<html lang="en">\n'
        "<head>\n"
        '<meta charset="utf-8">\n'
        f'<meta http-equiv="refresh" content="0; url={target}">\n'
        f'<link rel="canonical" href="{target}">\n'
        '<meta name="robots" content="noindex, follow">\n'
        f"<title>{title}</title>\n"
        "</head>\n"
        "<body>\n"
        f'<p>{title} is at <a href="{target}">{target}</a>.</p>\n'
        "</body>\n"
        "</html>\n"
    )


def _recent_proposals_markdown(staging: Staging, src_uri: str) -> str:
    """The landing page's design-record block. Generated, never hand-listed.

    Each row is a *markdown* link, not raw HTML: mkdocs only resolves and
    validates links it parsed itself, so an ``<a href>`` written here would ship
    a raw ``.md`` path that `strict` never looks at.
    """
    base = posixpath.dirname(src_uri)
    recent = sorted(staging.proposals, key=lambda p: p.number, reverse=True)[:RECENT_PROPOSALS]
    index = posixpath.relpath(f"{PROPOSALS_URI}/index.md", base)

    rows = [
        f'[<span class="aether-recent__n">{proposal.number}</span>'
        f'<span class="aether-recent__t">{proposal.title}</span>'
        f'<span class="aether-dot aether-dot--{proposal.status_key}" '
        f'title="{STATUS_LABELS[proposal.status_key]}"></span>]'
        f"({posixpath.relpath(proposal.src_uri, base)})"
        "{ .aether-recent__row }"
        for proposal in recent
    ]
    body = "\n\n".join([*rows, f"[All proposals →]({index}){{ .aether-recent__all }}"])
    return f'<div class="aether-recent" markdown>\n\n{body}\n\n</div>'


def _architecture_diagram(config: MkDocsConfig) -> str:
    match = _MERMAID_BLOCK_RE.search(_read(config, "README.md"))
    if match is None:
        raise RuntimeError(
            "README.md no longer contains a ```mermaid block; the landing page "
            "architecture diagram has no source."
        )
    return match.group(0)


def _admonition(title: str, paragraphs: list[str]) -> str:
    """A Material admonition from wrapped prose paragraphs."""
    body = "\n\n".join(paragraphs)
    lines: list[str] = []
    for paragraph in body.split("\n\n"):
        lines += textwrap.wrap(paragraph, width=84, break_long_words=False) + [""]
    return f'!!! info "{title}"\n\n' + "".join(f"    {line}\n".rstrip() + "\n" for line in lines)


def _conformance_headline(config: MkDocsConfig, staging: Staging) -> str:
    """The state-of-conformance block above the supported-features list.

    Every number is read out of the repository: the scores come from the newest
    baseline's verbatim suite output, and the gates from the nightly workflow's
    own description of itself. The document that produced the scores is linked
    next to them, caveat and all, because "Core 33/33" without "and only for the
    edge profile" is the overclaim this block exists to prevent.
    """
    latest = staging.baselines[0]
    markdown = _read(config, latest.repo_path)
    results = _profile_results(markdown, latest.repo_path)
    workflow = _read(config, E2E_WORKFLOW)

    def score(profile: str, level: str) -> str:
        passed, failed = results[(profile, level)]
        return f"{passed}/{passed + failed}"

    suite = _SUITE_VERSION_RE.search(markdown)
    suite_note = f" against the upstream suite at {suite.group('version')}" if suite else ""
    ci_suite = _GATEWAY_API_VERSION_RE.search(workflow)
    ci_note = f" at {ci_suite.group('version')}" if ci_suite else ""

    run = f"{latest.rev or 'the first baseline'} ({latest.date})"
    mesh_passed, mesh_failed = results[("MESH-HTTP", "core")]
    paragraphs = [
        f"**GATEWAY-HTTP (north-south) — Core {score('GATEWAY-HTTP', 'core')} and Extended "
        f"{score('GATEWAY-HTTP', 'extended')}.** Measured on a real cluster{suite_note} by "
        f"[{run}](latest.md), the newest run in the archive. The same profile runs nightly in "
        f"CI{ci_note}, where `.github/workflows/e2e.yaml` calls it a "
        f'"Hard gate ({_workflow_gate(workflow, "gateway-http")})".',
        "**MESH-HTTP (east-west GAMMA) — a hard CI gate at "
        f'{_workflow_gate(workflow, "mesh-http")}.** The nightly workflow runs the mesh profile '
        "through the full capture path on its own cluster, and a regression fails it. The "
        "committed baselines predate that gate and are not where the mesh profile stands today: "
        f"{run} still records MESH-HTTP core at {mesh_passed} passed / {mesh_failed} failed, "
        "blocked at the time on proposal 022.",
    ]

    caveat = _CAVEAT_RE.search(" ".join(markdown.replace("\n>", "\n").split()))
    if caveat:
        paragraphs.append(
            f"{run} states its own caveat, and it is still the right way to read this page: "
            f"*{_strip_inline(caveat.group(0))}*"
        )

    return _admonition("Where conformance stands", paragraphs)


def _archive_slug(baseline: Baseline) -> str:
    return posixpath.basename(baseline.src_uri)[: -len(".md")]


def _conformance_archive_markdown(staging: Staging) -> str:
    """The generated index over every conformance run, newest first.

    Nothing is hand-maintained: the rows, the count and the verdicts all come
    from the baseline files. Each link is a *markdown* path so that `strict`
    resolves and validates it, exactly as on the proposals index.
    """
    rows = [
        f"| [{baseline.date}]({_archive_slug(baseline)}.md) "
        f"| {baseline.rev or '—'} "
        f"| [{_cell(baseline.title)}]({_archive_slug(baseline)}.md) "
        f"| {baseline.verdict or '—'} |"
        for baseline in staging.baselines
    ]
    return (
        # One wide table under one heading; a right-rail table of contents would
        # hold a single entry and take a third of the width the table wants.
        "---\n"
        "hide:\n"
        "  - toc\n"
        "---\n"
        "\n"
        "# Conformance archive\n"
        "\n"
        "Every Gateway API conformance run aether has written up, newest first — "
        f"{len(staging.baselines)} of them, from the first diagnostic run that could not reach "
        "the traffic phase at all to the fully conformant one. Each is published as it was "
        "written, at the score it recorded that day. The verdict column is the run's own "
        "headline, not a re-reading of it.\n"
        "\n"
        "The newest run is also served at [/"
        f"{CONFORMANCE_URI}/latest/](../latest.md); what CI enforces *today* is on the "
        "[supported features](../index.md) page.\n"
        "\n"
        '<div class="aether-proposals" markdown="1">\n'
        "\n"
        "| Date | Revision | Run | Verdict |\n"
        "|---|---|---|---|\n"
        f"{chr(10).join(rows)}\n"
        "\n"
        "</div>\n"
    )


def _latest_baseline_note(staging: Staging) -> str:
    """The one generated block on ``/conformance/latest/``."""
    latest = staging.baselines[0]
    slug = _archive_slug(latest)
    return _admonition(
        "Newest conformance run",
        [
            "This page follows whichever run is newest, so it moves. This one is "
            f"{latest.rev or 'the first baseline'}, and it keeps a URL of its own that never "
            f"will: [/{CONFORMANCE_URI}/archive/{slug}/](archive/{slug}.md) — cite that one. "
            f"The [archive](archive/index.md) has all {len(staging.baselines)}."
        ],
    )


# --------------------------------------------------------------------------- #
# The protobuf reference
#
# protoc-gen-doc's markdown is written for a standalone file, not for a page in
# a themed site: it opens with its own H1 and a table of contents the right rail
# already renders, closes with the same scalar-type appendix on every page, and
# links types at anchors that only exist once that appendix is kept. Everything
# below trims it to a page body. None of it touches the content: names, types
# and the comments lifted out of the .proto are exactly as protoc rendered them.
# --------------------------------------------------------------------------- #

#: The first per-file section. Everything above it is the generator's own front
#: matter — H1, `#top` anchor, table of contents.
_PROTOC_BODY_START_RE = re.compile(r'^<a name="[^"]+"></a>\n<p align="right">', re.MULTILINE)
#: "back to top" links, whose target is dropped with the front matter.
_PROTOC_TOP_LINK_RE = re.compile(r'^<p align="right"><a href="#top">Top</a></p>\n', re.MULTILINE)
_PROTOC_ANCHOR_RE = re.compile(r'<a name="(?P<id>[^"]+)"\s*/?></a>|<a name="(?P<self>[^"]+)"\s*/>')
_PROTOC_SCALARS = "\n## Scalar Value Types"
_PROTO_FILE_HEADING_RE = re.compile(r"^##\s+(?P<path>\S+\.proto)\s*$", re.MULTILINE)
_ANCHOR_LINK_RE = re.compile(r"\[(?P<text>[^\]]*)\]\(#(?P<anchor>[^)]*)\)")
_BLANK_RUN_RE = re.compile(r"\n{3,}")


def _squash_row(parts: list[str]) -> str:
    text = ""
    for part in parts:
        if part == "<br>":
            text += "<br>"
        elif text and not text.endswith("<br>"):
            text += " " + part
        else:
            text += part
    return text


def _join_table_rows(markdown: str) -> str:
    """Fold a wrapped table row back onto one line.

    A ``.proto`` comment is prose and often several paragraphs; protoc-gen-doc
    drops it into a table cell with its line breaks intact, which ends the table
    at the first one. Continuation lines are pulled back into the row and the
    paragraph breaks become ``<br>``.
    """
    out: list[str] = []
    pending: list[str] | None = None
    for line in markdown.split("\n"):
        stripped = line.strip()
        if pending is None:
            if line.startswith("|") and not stripped.endswith("|"):
                pending = [stripped]
            else:
                out.append(line)
            continue
        if not stripped:
            pending.append("<br>")
        else:
            pending.append(stripped)
            if stripped.endswith("|"):
                out.append(_squash_row(pending))
                pending = None
    if pending is not None:
        out.append(_squash_row(pending))
    return "\n".join(out)


def _flatten_dangling_links(markdown: str) -> str:
    """Turn a link to an anchor this page does not define into plain code.

    Type columns link every field type, including the scalars documented in the
    dropped appendix and the well-known types no page here renders. Those are
    dangling anchors, which `strict` is right to reject; the ones that do resolve
    — a message defined on this page — stay links.
    """
    known = {anchor for pair in _PROTOC_ANCHOR_RE.findall(markdown) for anchor in pair if anchor}
    return _ANCHOR_LINK_RE.sub(
        lambda m: m.group(0) if m.group("anchor") in known else f"`{m.group('text')}`",
        markdown,
    )


def _clean_protoc_doc(markdown: str, path: str) -> str:
    """protoc-gen-doc's markdown, trimmed to something that is a page body."""
    start = _PROTOC_BODY_START_RE.search(markdown)
    if start is None:
        raise RuntimeError(
            f"{path} does not look like protoc-gen-doc markdown — no per-file section was "
            "found. The template or the plugin changed; re-check the trimming below before "
            "publishing the output."
        )
    body = markdown[start.start() :]
    appendix = body.find(_PROTOC_SCALARS)
    if appendix >= 0:
        body = body[:appendix]
    body = _PROTOC_TOP_LINK_RE.sub("", body)
    body = _join_table_rows(body)
    body = _flatten_dangling_links(body)
    # The template pads sections with lines holding a single space, which
    # markdown reads as content.
    body = "\n".join(line.rstrip() for line in body.split("\n"))
    return _BLANK_RUN_RE.sub("\n\n", body).strip() + "\n"


def _api_pages() -> list[ApiPage]:
    """The reference pages Bazel generated for this build, in nav order.

    ``AETHER_API_DOCS`` carries ``key=path`` entries. It is absent under a plain
    ``mkdocs serve``, where protoc has not run; the section then does not exist
    at all rather than existing empty, and `//website:build_site` asserts that
    the real build did produce it.
    """
    raw = os.environ.get("AETHER_API_DOCS", "")
    if not raw:
        return []

    supplied: dict[str, str] = {}
    for entry in raw.split(os.pathsep):
        if not entry:
            continue
        key, separator, path = entry.partition("=")
        if not separator or not path:
            raise RuntimeError(f"AETHER_API_DOCS entry {entry!r} is not key=path.")
        supplied[key] = path

    if set(supplied) != set(API_PAGES):
        raise RuntimeError(
            "AETHER_API_DOCS does not match the published packages: expected "
            f"{sorted(API_PAGES)}, got {sorted(supplied)}. Add the proto_doc target to "
            "//website:API_DOCS and the page to API_PAGES together, or the nav and the "
            "generator disagree."
        )

    pages: list[ApiPage] = []
    for key, (title, _) in API_PAGES.items():
        path = supplied[key]
        with open(path, encoding="utf-8") as handle:
            body = _clean_protoc_doc(handle.read(), path)
        proto_files = tuple(_PROTO_FILE_HEADING_RE.findall(body))
        if not proto_files:
            raise RuntimeError(
                f"{path} documents no .proto file; /{API_URI}/{key}/ would be empty."
            )
        repo_dir = posixpath.dirname(proto_files[0])
        pages.append(
            ApiPage(
                key=key,
                title=title,
                # `api/aether/config/v1` is the package `aether.config.v1`; both
                # come out of the paths protoc itself printed.
                proto_package=repo_dir.removeprefix(f"{API_URI}/").replace("/", "."),
                repo_dir=repo_dir,
                proto_files=proto_files,
                src_uri=f"{API_URI}/{key}.md",
                markdown=body,
            )
        )
    return pages


def _api_markdown(page: ApiPage) -> str:
    """One reference page: a generated header, then protoc's own rendering."""
    _, blurb = API_PAGES[page.key]
    files = "\n".join(
        f"- [`{posixpath.basename(path)}`]({BLOB_URL}{path})" for path in page.proto_files
    )
    note = _admonition(
        "Generated from the schema",
        [
            f"Rendered by protoc from `{page.repo_dir}/` when this site was built, so it cannot "
            "drift from the compiled schema — there is no committed copy of this page. Field "
            "constraints are carried as `buf.validate` options and are **not** shown here; read "
            "the `.proto` for those.",
        ],
    )
    return (
        f"# {page.title} API\n"
        "\n"
        f"`{page.proto_package}` — {blurb}\n"
        "\n"
        f"{files}\n"
        "\n"
        f"{note}"
        "\n"
        f"{page.markdown}"
    )


# --------------------------------------------------------------------------- #
# Hooks
# --------------------------------------------------------------------------- #


def on_config(config: MkDocsConfig) -> MkDocsConfig:
    """Stage the repository, and extend ``nav`` with the generated sections.

    The three discovered sections are appended here rather than written into
    ``mkdocs.yml`` so that adding ``docs/proposals/099_thing.md``, committing a
    new ``docs/conformance/baseline-*.md``, or adding a field to a ``.proto`` is
    the entire publishing step: each then appears in the nav, in its index and at
    its stable URL with no edit to the site at all.
    """
    global _staging
    _staging = _build_staging(config)

    conformance: list[dict[str, str]] = [
        {"Supported features": f"{CONFORMANCE_URI}/index.md"},
        {"Latest run": f"{CONFORMANCE_URI}/latest.md"},
        {"Archive": f"{CONFORMANCE_URI}/archive/index.md"},
    ]
    conformance += [
        {f"{baseline.date} {baseline.rev}".strip(): baseline.src_uri}
        for baseline in _staging.baselines
    ]

    entries: list[dict[str, str]] = [{"All proposals": f"{PROPOSALS_URI}/index.md"}]
    entries += [
        {f"{proposal.number} — {proposal.title}": proposal.src_uri}
        for proposal in sorted(_staging.proposals, key=lambda p: p.number)
    ]

    sections: list[dict[str, object]] = [{"Conformance": conformance}]
    if _staging.api:
        sections.append({"API": [{page.title: page.src_uri} for page in _staging.api]})
    sections.append({"Proposals": entries})

    config.nav = list(config.nav or []) + sections
    return config


def on_files(files: Files, config: MkDocsConfig) -> Files:
    for repo_path, src_uri in _staging.pages.items():
        content = transform(
            _read(config, repo_path),
            repo_path,
            src_uri,
            _staging,
            _staging.drops.get(repo_path, ()),
        )
        if src_uri.startswith(f"{PROPOSALS_URI}/"):
            content = _inject_banner(content, src_uri)
        elif src_uri == f"{CONFORMANCE_URI}/index.md":
            content = _inject_after_h1(content, _conformance_headline(config, _staging))
        files.append(File.generated(config, src_uri, content=content))

    files.append(
        File.generated(config, f"{PROPOSALS_URI}/index.md", content=_index_markdown(_staging))
    )
    files.append(
        File.generated(
            config,
            f"{CONFORMANCE_URI}/archive/index.md",
            content=_conformance_archive_markdown(_staging),
        )
    )
    # `/conformance/latest/` republishes the newest run under a name that does
    # not move. It is the one page on the site that deliberately has a second
    # URL; the note says so and points at the permanent one.
    latest = _staging.baselines[0]
    latest_uri = f"{CONFORMANCE_URI}/latest.md"
    files.append(
        File.generated(
            config,
            latest_uri,
            content=_inject_after_h1(
                transform(_read(config, latest.repo_path), latest.repo_path, latest_uri, _staging),
                _latest_baseline_note(_staging),
            ),
        )
    )

    for api_page in _staging.api:
        files.append(
            File.generated(config, api_page.src_uri, content=_api_markdown(api_page))
        )

    # Short stable links. Emitted as plain HTML rather than as markdown pages so
    # they stay out of the nav, the search index and the sitemap — and so the
    # site needs no redirect plugin, and therefore no new dependency.
    for proposal in _staging.proposals:
        files.append(
            File.generated(
                config,
                f"{PROPOSALS_URI}/{proposal.number}/index.html",
                content=_redirect_html(proposal),
            )
        )
    return files


def on_pre_page(page: Page, config: MkDocsConfig, files: Files) -> Page:
    repo_path = _staging.sources.get(page.file.src_uri)
    api_page = next((p for p in _staging.api if p.src_uri == page.file.src_uri), None)
    if repo_path:
        page.edit_url = EDIT_URL + repo_path
    elif api_page is not None:
        # Nothing to edit on the page: the source is the proto package.
        page.edit_url = f"{REPO_URL}/tree/main/{api_page.repo_dir}"
    elif page.file.src_uri == f"{PROPOSALS_URI}/index.md":
        # Generated from the directory, so "edit" means the directory listing.
        page.edit_url = f"{REPO_URL}/tree/main/{PROPOSALS_DIR}"
    elif page.file.src_uri == f"{CONFORMANCE_URI}/archive/index.md":
        page.edit_url = f"{REPO_URL}/tree/main/{CONFORMANCE_DIR}"
    return page


def on_page_markdown(markdown: str, page: Page, config: MkDocsConfig, files: Files) -> str:
    if MERMAID_MARKER in markdown:
        markdown = markdown.replace(MERMAID_MARKER, _architecture_diagram(config))
    if RECENT_PROPOSALS_MARKER in markdown:
        markdown = markdown.replace(
            RECENT_PROPOSALS_MARKER, _recent_proposals_markdown(_staging, page.file.src_uri)
        )
    return markdown
