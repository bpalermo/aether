#!/usr/bin/env python3
"""Build aethermesh.dev hermetically and prove it makes no third-party requests.

Used by both ``//website:site`` (which keeps the tar) and ``//website:site_test``
(which throws it away and only cares that the build and the assertions pass), so
the gate on a pull request and the artifact that gets deployed come out of the
same code path.

Steps:

1. ``mkdocs build`` with ``strict = true`` — any broken link, missing nav entry
   or plugin warning fails the build.
2. Install the pinned mermaid bundle into the site and repoint the CDN URL that
   Material bakes into its JavaScript at it.
3. Assert the generated site references no third-party host at all, and that the
   pages and the diagram we expect actually came out.
4. Write a byte-reproducible tar (sorted, fixed mtime/uid/gid/mode) of the site
   root, ready for ``upload-pages-artifact``.
"""

from __future__ import annotations

import argparse
import io
import logging
import os
import re
import sys
import tarfile
import tempfile
from pathlib import Path

# Hosts that must never appear anywhere in the generated site. aethermesh.dev is
# a .dev domain: HSTS-preloaded, and a page that pulls a font or a script from a
# CDN is a page that leaks its visitors to a third party.
FORBIDDEN_HOSTS = (
    "fonts.googleapis.com",
    "fonts.gstatic.com",
    "unpkg.com",
    "cdn.jsdelivr.net",
    "cdnjs.cloudflare.com",
    "ajax.googleapis.com",
    "polyfill.io",
    "api.github.com",
    "www.googletagmanager.com",
    "www.google-analytics.com",
    "gravatar.com",
)

#: External resources Material hard-codes into its bundle, and the same-origin
#: path each one is rewritten to. Every entry must match at least once — if a
#: theme upgrade renames one, the build fails rather than silently shipping a
#: CDN reference.
#:
#: The api.github.com entries belong to the repository-facts fetcher (stars,
#: forks, latest release). It is already dead code here because
#: overrides/partials/source.html drops the `data-md-component="source"` marker
#: that mounts it; repointing the endpoint as well means no future theme change
#: can quietly revive the call.
#: The self-hosted replacements must resolve correctly at ANY mount point (the
#: bpalermo.github.io/aether/ project-pages subpath today, the aethermesh.dev
#: root later) from ANY page depth. Neither a root-absolute "/assets/..." (404s
#: on the subpath — how the first deploy shipped a non-rendering diagram) nor a
#: bare relative "./..." (import() in a classic script resolves against the
#: DOCUMENT URL, not the script URL — browser-verified) can do that as a string
#: literal. So the quoted literal is replaced with a runtime EXPRESSION anchored
#: to the theme bundle's own <script src>, which exists on every page and lives
#: in the same directory as the self-hosted files. Keys that include quotes are
#: replaced verbatim (expression injection); bare keys keep their quotes.
_SELF_HOST_EXPR = (
    'new URL("./{name}",document.querySelector(\'script[src*="assets/javascripts/bundle"]\').src).href'
)
CDN_REWRITES = {
    '"https://unpkg.com/mermaid@11/dist/mermaid.min.js"': _SELF_HOST_EXPR.format(name="mermaid.min.js"),
    '"https://unpkg.com/resize-observer-polyfill"': _SELF_HOST_EXPR.format(name="resize-observer-polyfill.js"),
    "https://api.github.com/repos/": "/_third-party-disabled/repos/",
    "https://api.github.com/users/": "/_third-party-disabled/users/",
}

#: Written empty. It is only ever requested by a browser with no ResizeObserver,
#: which Material 9 does not support anyway; the point is that the request stays
#: on this origin.
POLYFILL_STUB = "assets/javascripts/resize-observer-polyfill.js"

MERMAID_DEST = "assets/javascripts/mermaid.min.js"

#: One page per section of the information architecture, plus the artefacts that
#: are easy to lose silently. A section that stops being staged fails the build
#: instead of quietly disappearing from the site.
REQUIRED_FILES = (
    "index.html",
    "architecture/index.html",
    # Docs.
    "docs/getting-started/index.html",
    "docs/workloads/index.html",
    "docs/configuration/index.html",
    "docs/charts/index.html",
    "docs/observability/index.html",
    "docs/registry/index.html",
    # Development.
    "dev/runbook/index.html",
    "dev/proxy/index.html",
    "dev/agents/index.html",
    # Proposals: the generated index, one known proposal, and its short link.
    "proposals/index.html",
    "proposals/018-gateway-api-gamma/index.html",
    "proposals/018/index.html",
    "CNAME",
    "robots.txt",
    "sitemap.xml",
    "search/search_index.json",
    MERMAID_DEST,
)

#: Terms that must survive into the search index. They come from pages staged in
#: phase 2/3, so their absence means the search plugin stopped indexing a whole
#: section — which no link check would catch.
REQUIRED_SEARCH_TERMS = ("uds-socket", "demand-scoped")

#: Private-notebook syntax. hooks.py neutralises it into a muted parenthetical;
#: if a literal one ever reaches the HTML, a reader sees raw notebook syntax and
#: a link to a document that does not exist publicly.
_WIKI_RE = re.compile(r"\[\[[^\[\]]+\]\]")

#: `[[ ... ]]` is also shell. Code is quoted verbatim on purpose, so the wiki-ref
#: check looks only at prose.
_CODE_RE = re.compile(r"<pre\b.*?</pre>|<code\b.*?</code>", re.IGNORECASE | re.DOTALL)

TEXT_SUFFIXES = {".html", ".css", ".js", ".json", ".xml", ".txt", ".svg", ""}

#: `rel` values on <link> that cause the browser to fetch. `canonical` and
#: `alternate` are metadata and legitimately absolute.
_FETCHING_REL = (
    "stylesheet|preload|modulepreload|prefetch|preconnect|dns-prefetch"
    "|icon|shortcut icon|apple-touch-icon|manifest"
)

_SUBRESOURCE_RE = re.compile(
    r"""<(?:script|img|iframe|source|embed)\b[^>]*\bsrc\s*=\s*["']https?://"""
    rf"""|<link\b[^>]*\brel\s*=\s*["'](?:{_FETCHING_REL})["'][^>]*\bhref\s*=\s*["']https?://"""
    r"""|@import\s+(?:url\()?["']https?://"""
    r"""|url\(\s*["']?https?://""",
    re.IGNORECASE,
)


class BuildError(RuntimeError):
    pass


# --------------------------------------------------------------------------- #
# mkdocs
# --------------------------------------------------------------------------- #


def run_mkdocs(config_file: Path, repo_root: Path, site_dir: Path) -> None:
    # Read by website/hooks.py to locate README.md (and, later, docs/).
    os.environ["AETHER_REPO_ROOT"] = str(repo_root)
    os.environ.setdefault("SOURCE_DATE_EPOCH", "0")
    os.environ["TZ"] = "UTC"

    logging.basicConfig(level=logging.INFO, format="%(levelname)-8s -  %(message)s")

    from mkdocs.commands import build as mkdocs_build
    from mkdocs.config import load_config

    config = load_config(config_file=str(config_file), site_dir=str(site_dir), strict=True)
    config.plugins.on_startup(command="build", dirty=False)
    try:
        # `strict` is honoured inside build(): it installs a warning counter on
        # the mkdocs logger and raises on any warning, exactly as `mkdocs build
        # --strict` does.
        mkdocs_build.build(config)
    finally:
        config.plugins.on_shutdown()


# --------------------------------------------------------------------------- #
# Self-hosting
# --------------------------------------------------------------------------- #


def iter_text_files(site_dir: Path):
    for path in sorted(site_dir.rglob("*")):
        if path.is_file() and path.suffix.lower() in TEXT_SUFFIXES:
            yield path


def drop_source_maps(site_dir: Path) -> None:
    """Source maps are a development artefact — megabytes of unminified theme
    source, carrying the very CDN URLs the minified bundle no longer has. Drop
    them and the comments pointing at them."""
    for path in sorted(site_dir.rglob("*.map")):
        path.unlink()
    pattern = re.compile(r"(?://|/\*)# sourceMappingURL=\S+?(?:\*/)?\s*$", re.MULTILINE)
    for path in iter_text_files(site_dir):
        if path.suffix.lower() not in {".js", ".css"}:
            continue
        original = path.read_text(encoding="utf-8")
        stripped = pattern.sub("", original)
        if stripped != original:
            path.write_text(stripped, encoding="utf-8")


def self_host(site_dir: Path, mermaid: Path) -> None:
    (site_dir / MERMAID_DEST).parent.mkdir(parents=True, exist_ok=True)
    (site_dir / MERMAID_DEST).write_bytes(mermaid.read_bytes())
    (site_dir / POLYFILL_STUB).write_text("", encoding="utf-8")

    seen = {url: 0 for url in CDN_REWRITES}
    for path in iter_text_files(site_dir):
        try:
            original = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        rewritten = original
        for url, replacement in CDN_REWRITES.items():
            count = rewritten.count(url)
            if count:
                seen[url] += count
                rewritten = rewritten.replace(url, replacement)
        if rewritten != original:
            path.write_text(rewritten, encoding="utf-8")

    missing = [url for url, count in seen.items() if count == 0]
    if missing:
        raise BuildError(
            "expected to rewrite these CDN references but never found them — the "
            "theme probably changed, re-check how it loads them before dropping "
            f"the rule: {missing}"
        )


# --------------------------------------------------------------------------- #
# Assertions
# --------------------------------------------------------------------------- #


def verify_proposals(site_dir: Path) -> list[str]:
    """The proposals series is generated, so check the shape, not a fixed list.

    Every proposal must have a row on the index and a working short link, and the
    index must be the only place either is written down.
    """
    problems: list[str] = []
    root = site_dir / "proposals"
    if not root.is_dir():
        return ["no proposals section was generated"]

    published = sorted(
        path.parent.name
        for path in root.glob("*/index.html")
        if re.fullmatch(r"\d{3}-.+", path.parent.name)
    )
    if not published:
        return ["the proposals section contains no proposal pages"]

    index = (root / "index.html").read_text(encoding="utf-8")
    for name in published:
        number = name[:3]
        if f'href="{name}/"' not in index and f'href="../{name}/"' not in index:
            problems.append(f"proposal {name} is published but has no row on /proposals/")
        redirect = root / number / "index.html"
        if not redirect.is_file():
            problems.append(f"proposal {name} has no short link at /proposals/{number}/")
        elif f"url=../{name}/" not in redirect.read_text(encoding="utf-8"):
            problems.append(f"short link /proposals/{number}/ does not point at {name}")

    if "aether-dot--" not in index:
        problems.append("the proposals index carries no status badges")
    return problems


def verify_search(site_dir: Path) -> list[str]:
    index = site_dir / "search" / "search_index.json"
    if not index.is_file():
        return ["no search index was generated"]
    content = index.read_text(encoding="utf-8")
    return [
        f"search index does not contain {term!r} — a staged section is not being indexed"
        for term in REQUIRED_SEARCH_TERMS
        if term not in content
    ]


def verify(site_dir: Path) -> None:
    problems: list[str] = []

    for relative in REQUIRED_FILES:
        if not (site_dir / relative).is_file():
            problems.append(f"missing expected output: {relative}")

    cname = site_dir / "CNAME"
    if cname.is_file() and cname.read_text(encoding="utf-8").strip() != "aethermesh.dev":
        problems.append("CNAME does not contain aethermesh.dev")

    index = site_dir / "index.html"
    if index.is_file():
        html = index.read_text(encoding="utf-8")
        if 'class="mermaid"' not in html:
            problems.append("landing page has no mermaid block — the architecture diagram is gone")
        if "graph TD" not in html:
            problems.append("landing page mermaid block is not the README architecture graph")
        if "aether-recent__row" not in html:
            problems.append("landing page lists no recent proposals — the generated block is gone")

    problems += verify_proposals(site_dir)
    problems += verify_search(site_dir)

    for path in iter_text_files(site_dir):
        try:
            content = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        relative = path.relative_to(site_dir)
        for host in FORBIDDEN_HOSTS:
            if host in content:
                problems.append(f"third-party host {host} referenced by {relative}")
        if path.suffix.lower() == ".html":
            match = _SUBRESOURCE_RE.search(content)
            if match:
                problems.append(f"external subresource in {relative}: {match.group(0)!r}")
            wiki = _WIKI_RE.search(_CODE_RE.sub("", content))
            if wiki:
                problems.append(f"unneutralised wiki ref in {relative}: {wiki.group(0)!r}")

    if problems:
        raise BuildError("site verification failed:\n  - " + "\n  - ".join(problems))


# --------------------------------------------------------------------------- #
# Packaging
# --------------------------------------------------------------------------- #


def write_tar(site_dir: Path, out: Path) -> None:
    """Tar the site root reproducibly: sorted, no owner, no timestamps."""
    out.parent.mkdir(parents=True, exist_ok=True)
    paths = sorted(p for p in site_dir.rglob("*") if p.is_file() or p.is_dir())

    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w", format=tarfile.GNU_FORMAT) as tar:
        for path in paths:
            info = tarfile.TarInfo(name=str(path.relative_to(site_dir)))
            info.mtime = 0
            info.uid = 0
            info.gid = 0
            info.uname = ""
            info.gname = ""
            if path.is_dir():
                info.type = tarfile.DIRTYPE
                info.mode = 0o755
                tar.addfile(info)
            else:
                data = path.read_bytes()
                info.type = tarfile.REGTYPE
                info.mode = 0o644
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
    out.write_bytes(buffer.getvalue())


# --------------------------------------------------------------------------- #


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True, help="path to mkdocs.yml")
    parser.add_argument("--repo-root", required=True, help="repository root holding README.md")
    parser.add_argument("--mermaid", required=True, help="path to the pinned mermaid bundle")
    parser.add_argument("--out", help="tar to write; defaults to a scratch file")
    args = parser.parse_args(argv)

    scratch = os.environ.get("TEST_TMPDIR") or tempfile.mkdtemp(prefix="aether-site-")
    site_dir = Path(scratch) / "site"
    out = Path(args.out).absolute() if args.out else Path(scratch) / "site.tar"

    run_mkdocs(Path(args.config).absolute(), Path(args.repo_root).absolute(), site_dir)
    drop_source_maps(site_dir)
    self_host(site_dir, Path(args.mermaid).absolute())
    verify(site_dir)
    write_tar(site_dir, out)

    print(f"site built and verified: {out}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except BuildError as error:
        print(f"error: {error}", file=sys.stderr)
        sys.exit(1)
