"""MkDocs hooks that stage repository markdown into the site at build time.

Nothing under ``website/pages`` is a copy of a repository document. Files that
live in the repository (starting with ``README.md``) are read, transformed and
injected as *generated* MkDocs files here, so the site can never drift from the
source of truth and a stale duplicate can never be committed.

Adding a page in a later phase is one entry in :data:`STAGED_PAGES` plus a ``nav``
entry in ``mkdocs.yml``.

The transform does four things:

1. drops whole ``##`` sections by title (the site has its own Getting Started and
   the licence lives in the footer);
2. rewrites repository-relative links to absolute GitHub URLs, unless the target
   is a page the site itself publishes (see :data:`SITE_LINKS`);
3. neutralises ``[[wiki refs]]`` — private-notebook syntax that must never be
   published as a live link — into a muted parenthetical;
4. substitutes ``<!-- aether:readme-mermaid -->`` in any page with the
   architecture diagram lifted from the README, so the landing page and the
   architecture page share one source.

Everything is fenced-code aware: nothing inside a ``` block is rewritten.
"""

from __future__ import annotations

import os
import re
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
STAGED_PAGES: dict[str, tuple[str, tuple[str, ...]]] = {
    "README.md": ("architecture.md", ("Getting Started", "License")),
}

#: Repository paths that the site publishes itself. Links to these are rewritten
#: to the site page instead of to GitHub. Empty in phase 1 — the docs and
#: proposal sections are not published yet, so every link still points at the
#: repository.
SITE_LINKS: dict[str, str] = {}

#: Placeholder replaced with the README's architecture diagram.
MERMAID_MARKER = "<!-- aether:readme-mermaid -->"

_FENCE_RE = re.compile(r"^(?P<indent>\s*)(?P<fence>```+|~~~+)")
_LINK_RE = re.compile(r"(?<=\])\((?P<target>[^)\s]+)(?P<title>\s+\"[^\"]*\")?\)")
_REF_DEF_RE = re.compile(r"^(?P<prefix>\s{0,3}\[[^\]]+\]:\s+)(?P<target>\S+)", re.MULTILINE)
_WIKI_RE = re.compile(r"\[\[(?P<ref>[^\[\]]+)\]\]")
_MERMAID_BLOCK_RE = re.compile(r"^```mermaid\n.*?^```", re.MULTILINE | re.DOTALL)
_EXTERNAL_RE = re.compile(r"^(?:[a-z][a-z0-9+.-]*:|//|#)", re.IGNORECASE)


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
    with open(os.path.join(_repo_root(config), repo_path), encoding="utf-8") as handle:
        return handle.read()


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


def _rewrite_link(target: str) -> str:
    if _EXTERNAL_RE.match(target) or target.startswith("/"):
        return target
    path = target.lstrip("./")
    path, _, fragment = path.partition("#")
    if path in SITE_LINKS:
        rewritten = SITE_LINKS[path]
    else:
        rewritten = BLOB_URL + path
    return f"{rewritten}#{fragment}" if fragment else rewritten


def _absolutize_links(markdown: str) -> str:
    markdown = _LINK_RE.sub(
        lambda m: f"({_rewrite_link(m.group('target'))}{m.group('title') or ''})", markdown
    )
    return _REF_DEF_RE.sub(
        lambda m: f"{m.group('prefix')}{_rewrite_link(m.group('target'))}", markdown
    )


def _neutralize_wiki_refs(markdown: str) -> str:
    return _WIKI_RE.sub(
        lambda m: f'<span class="aether-private-note">(private note: {m.group("ref").strip()})</span>',
        markdown,
    )


def transform(markdown: str, drop_sections: tuple[str, ...] = ()) -> str:
    """Apply the full staging transform to a repository document."""
    markdown = _drop_sections(markdown, drop_sections)
    markdown = _map_prose(markdown, _absolutize_links)
    return _map_prose(markdown, _neutralize_wiki_refs)


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


def on_files(files: Files, config: MkDocsConfig) -> Files:
    for repo_path, (src_uri, drop_sections) in STAGED_PAGES.items():
        content = transform(_read(config, repo_path), drop_sections)
        files.append(File.generated(config, src_uri, content=content))
    return files


def on_pre_page(page: Page, config: MkDocsConfig, files: Files) -> Page:
    for repo_path, (src_uri, _) in STAGED_PAGES.items():
        if page.file.src_uri == src_uri:
            page.edit_url = EDIT_URL + repo_path
    return page


def on_page_markdown(markdown: str, page: Page, config: MkDocsConfig, files: Files) -> str:
    if MERMAID_MARKER in markdown:
        markdown = markdown.replace(MERMAID_MARKER, _architecture_diagram(config))
    return markdown
