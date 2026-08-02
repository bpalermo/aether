"""MkDocs hooks that stage repository markdown into the site at build time.

Nothing under ``website/pages`` is a copy of a repository document. Files that
live in the repository (``README.md``, ``docs/**``, ``charts/README.md``,
``proxy/README.md``, ``AGENTS.md``) are read, transformed and injected as
*generated* MkDocs files here, so the site can never drift from the source of
truth and a stale duplicate can never be committed.

Adding a hand-picked page is one entry in :data:`STATIC_PAGES` plus a ``nav``
entry in ``mkdocs.yml``. The proposals series needs neither: every
``docs/proposals/NNN_slug.md`` is discovered, staged, indexed and navigated
automatically, so dropping a new proposal file into the repository is the whole
publishing step.

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

    return staging


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


def _inject_banner(markdown: str, src_uri: str) -> str:
    """Put the banner immediately after the H1, above the status block."""
    banner = _banner(src_uri)
    lines = markdown.splitlines(keepends=True)
    for index, line in enumerate(lines):
        if line.startswith("# "):
            head = "".join(lines[: index + 1])
            tail = "".join(lines[index + 1 :]).lstrip("\n")
            return f"{head}\n{banner}\n{tail}"
    return f"{banner}\n{markdown}"


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


# --------------------------------------------------------------------------- #
# Hooks
# --------------------------------------------------------------------------- #


def on_config(config: MkDocsConfig) -> MkDocsConfig:
    """Stage the repository, and extend ``nav`` with the generated proposals.

    The proposals section is appended here rather than written into
    ``mkdocs.yml`` so that adding ``docs/proposals/099_thing.md`` is the entire
    publishing step: it then appears in the nav, in the index and at its short
    link with no edit to the site at all.
    """
    global _staging
    _staging = _build_staging(config)

    entries: list[dict[str, str]] = [{"All proposals": f"{PROPOSALS_URI}/index.md"}]
    entries += [
        {f"{proposal.number} — {proposal.title}": proposal.src_uri}
        for proposal in sorted(_staging.proposals, key=lambda p: p.number)
    ]
    config.nav = list(config.nav or []) + [{"Proposals": entries}]
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
        files.append(File.generated(config, src_uri, content=content))

    files.append(
        File.generated(config, f"{PROPOSALS_URI}/index.md", content=_index_markdown(_staging))
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
    if repo_path:
        page.edit_url = EDIT_URL + repo_path
    elif page.file.src_uri == f"{PROPOSALS_URI}/index.md":
        # Generated from the directory, so "edit" means the directory listing.
        page.edit_url = f"{REPO_URL}/tree/main/{PROPOSALS_DIR}"
    return page


def on_page_markdown(markdown: str, page: Page, config: MkDocsConfig, files: Files) -> str:
    if MERMAID_MARKER in markdown:
        markdown = markdown.replace(MERMAID_MARKER, _architecture_diagram(config))
    if RECENT_PROPOSALS_MARKER in markdown:
        markdown = markdown.replace(
            RECENT_PROPOSALS_MARKER, _recent_proposals_markdown(_staging, page.file.src_uri)
        )
    return markdown
