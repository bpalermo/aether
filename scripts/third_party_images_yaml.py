#!/usr/bin/env python3
"""The YAML reader of scripts/third-party-images.sh (#1525).

`check` used to read YAML line by line, and every spelling of `image:` a line
reader does not know was a blind spot (an alias, a block scalar behind node
properties, a flow mapping broken across lines, a reference under a child key).
This reads a YAML file with a YAML parser and hands the scan what the file
says, one record per scalar, so the scan judges values and never YAML syntax.

Usage:
    third_party_images_yaml.py <out dir>      paths on stdin, relative to the
                                              current directory, one per line

For every path, one status line on stdout:

    parsed<TAB><path>                     records are in <out dir>/<path>
    template<TAB><path>                   not YAML, and it holds `{{`: a Go
                                          template (a Helm chart's). The scan
                                          reads it line by line, like YAML
                                          embedded in any other language
    problem<TAB><path><TAB><line><TAB><what>
                                          not YAML and no template: a finding,
                                          never a pass

A record is `<line><TAB><kind><TAB><text>`; an empty line ends a group of
records that belong together. The kinds:

    v   what a scalar says, comments and quoting already gone:
          image: "<value>"   the value of a key that ends in `image` (any
                             case), also when it is reached through an alias,
                             stands on a later line or is a block scalar; and a
                             scalar that is a direct child of such a key's
                             mapping or sequence (`image:` / `  ref: x`), except
                             under the keys an image is split over (PART_KEYS)
          : <value>          any other scalar, key or value: text in which a
                             known image, a `--image` flag or JSON may stand
    s   a source line of a block scalar that is no image value (a script, an
        embedded manifest): read like a line of a here-document

Exit 0 when every path got its status line; anything else is a failure of the
reader itself, and the scan then stops (exit 2) instead of passing.

Needs PyYAML. Under Bazel it is the hash-checked wheel of the documentation
site's lock (//scripts:third_party_images_yaml); outside Bazel it is the
`python3` on PATH and its `yaml` module.
"""

import os
import sys

import yaml

# The keys an image is split over (the Helm values spelling, put together by a
# template). Their values are parts and not a reference.
PART_KEYS = frozenset(["registry", "repository", "tag", "digest", "pullPolicy"])

BLOCK_STYLES = ("|", ">")


def is_image_key(node):
    return isinstance(node, yaml.ScalarNode) and node.value.lower().endswith("image")


class Reader:
    def __init__(self, text):
        self.lines = text.split("\n")
        self.records = []
        # A node's own marks begin at its anchor or tag; the token's begin at
        # the scalar. Both end where the scalar ends.
        self.token_line = {}
        for token in yaml.scan(text, Loader=yaml.SafeLoader):
            if isinstance(token, yaml.ScalarToken):
                self.token_line[token.end_mark.index] = token.start_mark.line
        for document in yaml.compose_all(text, Loader=yaml.SafeLoader):
            # Per document: an anchor does not outlive its document, and the
            # identity of a node means nothing once its document is gone.
            self.seen = set()
            if document is not None:
                self.walk(document, False)

    def group(self, *records):
        for line, kind, text in records:
            self.records.append("%d\t%s\t%s" % (line + 1, kind, text))
        self.records.append("")

    def scalar(self, node, image):
        line = self.token_line.get(node.end_mark.index, node.start_mark.line)
        if node.style in BLOCK_STYLES:
            end = node.end_mark.line + (1 if node.end_mark.column > 0 else 0)
            body = range(line + 1, min(end, len(self.lines)))
            if not image:
                self.group(*[(i, "s", self.lines[i]) for i in body])
                return
            for i in body:
                if self.lines[i].strip():
                    # The scan's own block-scalar rule: the first line that
                    # is not blank is the value, whole.
                    self.group((line, "v", "image: |"), (i, "v", self.lines[i].strip()))
                    return
            return
        value = " ".join(node.value.split())
        if not value:
            return
        if image:
            self.group((line, "v", 'image: "%s"' % value))
        else:
            self.group((line, "v", ": " + value))

    def walk(self, node, image):
        if isinstance(node, yaml.ScalarNode):
            self.scalar(node, image)
            return
        # An alias reaches a node twice, and may reach itself.
        if (id(node), image) in self.seen:
            return
        self.seen.add((id(node), image))
        if isinstance(node, yaml.SequenceNode):
            for child in node.value:
                self.walk(child, image and isinstance(child, yaml.ScalarNode))
        elif isinstance(node, yaml.MappingNode):
            for key, value in node.value:
                self.walk(key, False)
                under = is_image_key(key)
                if image and isinstance(value, yaml.ScalarNode):
                    under = under or not (isinstance(key, yaml.ScalarNode) and key.value in PART_KEYS)
                self.walk(value, under)


def read(path, out_dir):
    try:
        with open(path, encoding="utf-8") as handle:
            text = handle.read()
    except (OSError, UnicodeDecodeError) as error:
        return "problem\t%s\t0\tcannot be read (%s)" % (path, type(error).__name__)
    try:
        records = Reader(text).records
    except yaml.YAMLError as error:
        if "{{" in text:
            return "template\t" + path
        mark = getattr(error, "problem_mark", None)
        what = " ".join(str(getattr(error, "problem", None) or error).split())
        return "problem\t%s\t%d\tis not YAML a parser can read (%s)" % (path, mark.line + 1 if mark else 0, what)
    out = os.path.join(out_dir, path)
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, "w", encoding="utf-8") as handle:
        handle.write("".join(record + "\n" for record in records))
    return "parsed\t" + path


def main(argv):
    if len(argv) != 2:
        sys.stderr.write("usage: third_party_images_yaml.py <out dir>   (paths on stdin)\n")
        return 2
    for path in sys.stdin.read().split("\n"):
        if path:
            print(read(path, argv[1]))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
