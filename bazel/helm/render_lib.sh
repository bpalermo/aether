#!/usr/bin/env bash
# Sourced by the test runners //bazel/helm:defs.bzl generates. Not run directly.
#
# Two things every runner that might show a `helm template` render needs:
#
#   - mask_secrets: a render in which a document that might be a Secret shows
#     its identity and nothing else (the rule is below; it fails closed).
#     A chart can generate key material at render time (the aether chart's
#     self-signed webhook `tls.key`, #1382); it is a throwaway key, but a test
#     log is not the place for one, and a log that routinely holds key material
#     teaches its readers to stop noticing it.
#   - split_documents: the render as one file per document, with an index of
#     which template file and which object each one is. rules_helm's
#     `helm_template_test` keeps one document per template file (#1371).
#
# Portable on purpose: bash's own `[[ =~ ]]` (POSIX ERE) and POSIX awk. No
# `grep -P`, no `\s` in an ERE or an awk pattern (the review of #1365).

# The awk program behind both functions. It reads a render and prints it
# masked, line for line: the output has exactly as many lines as the input, so
# a line number means the same thing in both.
#
# The mask FAILS CLOSED, twice over.
#
# 1. Which documents are readable. It does not try to recognise a Secret, which
#    YAML lets a manifest spell in more ways than a line pattern can follow. A
#    document is printed as it is only when it is positively, plainly something
#    else:
#      - exactly one top-level `kind` line, of the plain form `kind: <Word>`
#        (the word optionally quoted, an optional trailing comment),
#      - whose word is neither `Secret` nor `List`,
#      - not written in flow style (it does not open with `{` or `[`), and with
#        no top-level explicit key (`? ...`) or merge key (`<<`),
#      - and with `kind: Secret` nowhere else in it (a Secret nested in another
#        object).
#    So a ConfigMap with a plain `kind: ConfigMap` line stays readable; an
#    excerpt of it would be useless otherwise.
#
# 2. What is printed of every other document. In a document that MIGHT be a
#    Secret, only these lines are ever printed, and only when they are a simple
#    `key: scalar` line (a plain or quoted word, no anchor, tag, alias, flow or
#    block value; a trailing comment is dropped):
#      - the `# Source:` comment helm puts first,
#      - `apiVersion:`, `kind:` and `type:` at the top level,
#      - `metadata:` itself, and `name:` and `namespace:` directly under it.
#    A document is "plainly something else" only when it has exactly one
#    top-level `kind: <Word>` line, the word is neither Secret nor a typed list
#    (anything ending in `List`), and no kind key anywhere inside it -- nested,
#    in a list item, in a flow mapping -- is Secret, a typed list, or written
#    with an anchor, a tag or an alias, and it has no explicit (`?`) or merge
#    (`<<`) key at any depth.
#    Every other line becomes `<masked>` at its own indentation. Nothing looks
#    for a `data` key: where the values are does not matter when no value line
#    is printed.
#
# With `-v stream=1` the input is what helm printed on stderr rather than a
# render: text before the first `---` is printed as it is, unless it already
# looks like a manifest (a `# Source:`, `apiVersion` or `kind` line), and any
# document after it goes through the rule above.
#
# With `-v dir=<directory>` it also writes, per document that has a `kind:`,
# `<dir>/doc.<n>` (as rendered), `<dir>/doc.<n>.masked`, and one line of
# `<dir>/index`: `<n>|<# Source: path>|<Kind>/<name>|<position in that file>`.
# shellcheck disable=SC2016 # an awk program, not a shell string.
_RENDER_AWK='
function indent_of(s,    i) {
  i = 1
  while (substr(s, i, 1) == " ") i++
  return i - 1
}
function uncomment(s) {
  sub(/[ \t]+#.*$/, "", s)
  return s
}
function unquote(s) {
  gsub(/^[ \t]+|[ \t]+$/, "", s)
  gsub(/^["\047]|["\047]$/, "", s)
  return s
}
function flush(    i, hide, secret, kinds, plain, word, content, unreadable, manifest, line, out, kind, name, in_meta, shown_meta, base) {
  secret = 0
  kinds = 0
  plain = 0
  unreadable = 0
  manifest = 0
  content = 0
  kind = ""
  name = ""
  in_meta = 0
  for (i = 1; i <= n; i++) {
    line = buf[i]
    if (line ~ /(^|[^A-Za-z0-9_])kind["\047]?[ \t]*:[ \t]*["\047]?Secret([^A-Za-z0-9_]|$)/) secret = 1
    if (line ~ /^["\047]?kind["\047]?[ \t]*:/) {
      kinds++
      if (line ~ /^kind:[ \t]*["\047]?[A-Za-z][A-Za-z0-9]*["\047]?[ \t]*(#.*)?$/) {
        word = unquote(uncomment(substr(line, 6)))
        # A typed list (SecretList, or any *List) holds items that carry no
        # kind line of their own: never plainly something else.
        if (word != "Secret" && word !~ /List$/) plain++
      }
    }
    # A kind key anywhere in the document (nested, in a list item, in a flow
    # mapping) whose value is not a plain word -- an anchor, a tag, an alias --
    # could be a Secret under any name: the document is not readable. So is a
    # nested typed list.
    if (line ~ /(^|[^A-Za-z0-9_])kind["\047]?[ \t]*:[ \t]*[&!*]/) unreadable = 1
    if (line ~ /^[ \t]+(-[ \t]+)?["\047]?kind["\047]?[ \t]*:/) {
      if (line !~ /^[ \t]+(-[ \t]+)?kind:[ \t]*["\047]?[A-Za-z][A-Za-z0-9]*["\047]?[ \t]*(#.*)?$/) unreadable = 1
      else if (line ~ /List["\047]?[ \t]*(#.*)?$/) unreadable = 1
    }
    # An explicit (`?`) or merge (`<<`) key at ANY depth: what it names or pulls
    # in cannot be read from the line.
    if (line ~ /^[ \t]*(-[ \t]+)?(\?|<<[ \t]*:)/) unreadable = 1
    if (line ~ /^(# Source: |["\047]?(apiVersion|kind)["\047]?[ \t]*:)/) manifest = 1
    if (!content && line !~ /^[ \t]*(#.*)?$/) {
      content = 1
      if (line ~ /^[ \t]*[{[]/) unreadable = 1
    }
    if (kind == "" && line ~ /^kind:/) kind = unquote(uncomment(substr(line, 6)))
    if (line ~ /^metadata:/) in_meta = 1
    else if (line ~ /^[^ \t#]/) in_meta = 0
    if (in_meta && name == "" && line ~ /^  name:/) name = unquote(uncomment(substr(line, 8)))
  }
  # Fail closed: readable only when plainly, positively not a Secret.
  hide = !(kinds == 1 && plain == 1 && !unreadable && !secret)
  # What helm says before any document is not a document.
  if (stream && chunk == 0 && !manifest) hide = 0
  chunk++
  shown_meta = 0
  for (i = 1; i <= n; i++) {
    line = buf[i]
    out = line
    if (hide && line !~ /^[ \t]*$/) {
      if (line ~ /^[^ \t]/) shown_meta = 0
      if (line ~ /^# Source: /) {
        out = line
      } else if (line ~ /^(apiVersion|kind|type):[ \t]*["\047]?[A-Za-z0-9][A-Za-z0-9._\/-]*["\047]?[ \t]*(#.*)?$/) {
        out = uncomment(line)
      } else if (line ~ /^metadata:[ \t]*(#.*)?$/) {
        out = "metadata:"
        shown_meta = 1
      } else if (shown_meta && line ~ /^  (name|namespace):[ \t]*["\047]?[A-Za-z0-9][A-Za-z0-9._\/-]*["\047]?[ \t]*(#.*)?$/) {
        out = uncomment(line)
      } else {
        out = sprintf("%" indent_of(line) "s<masked>", "")
      }
    }
    masked[i] = out
    print out
  }
  if (dir != "" && kind != "") {
    docs++
    position[source]++
    base = dir "/doc." docs
    for (i = 1; i <= n; i++) {
      print buf[i] > base
      print masked[i] > (base ".masked")
    }
    close(base)
    close(base ".masked")
    print docs "|" source "|" kind "/" name "|" position[source] > (dir "/index")
  }
  n = 0
}
/^---[ \t]*$/ {
  flush()
  print
  next
}
/^# Source: / { source = substr($0, 11) }
{ buf[++n] = $0 }
END { flush() }
'

# Every awk call goes through here. RENDER_LIB_AWK is a test seam
# (//bazel/helm:template_rules_test repeats itself with each awk the host has).
_awk() {
	"${RENDER_LIB_AWK:-awk}" "$@"
}

# mask_secrets: stdin is a render (or anything helm printed), stdout is the
# same text, masked by the fail-closed rule above.
mask_secrets() {
	_awk "$_RENDER_AWK"
}

# split_documents <render file> <directory>: writes the masked render to
# <directory>/render.masked and the per-document files and index described
# above.
split_documents() {
	rm -rf "$2"
	mkdir -p "$2"
	: >"$2/index"
	_awk -v dir="$2" "$_RENDER_AWK" "$1" >"$2/render.masked"
}

# show_bounded <max lines>: stdin, cut to that many lines and to 160 characters
# a line, saying so when it cut. For text that is already masked.
show_bounded() {
	_awk -v max="$1" '
		NR <= max { print (length($0) > 160 ? substr($0, 1, 160) " [...]" : $0) }
		END { if (NR > max) print "[... " (NR - max) " more lines not shown]" }'
}

# show_helm_failure <helm's stderr>: what helm said when it did not render.
# The runners keep helm's stdout (the render, which with --debug helm prints
# even when it fails) out of this, so it is an error message and a few debug
# lines, printed as they are. Should a helm ever put manifests on stderr, they
# go through the mask like any other (`stream=1` above). Cut to the last 30
# lines.
show_helm_failure() {
	printf '%s\n' "$1" | _awk -v stream=1 "$_RENDER_AWK" | _awk -v max=30 '
		{ line[NR] = (length($0) > 160 ? substr($0, 1, 160) " [...]" : $0) }
		END {
			if (NR > max) print "[... " (NR - max) " earlier lines not shown]"
			for (i = (NR > max ? NR - max + 1 : 1); i <= NR; i++) print line[i]
		}' >&2
}

# list_documents <directory>: the index, one readable line per document.
list_documents() {
	_awk -F'|' '{ printf "  %s (%s, document %d)\n", $3, $2, $4 }' "$1/index" | show_bounded 80
}

# translate_pattern <pattern>: the three escapes a pattern may use on top of
# POSIX ERE, which has no way to write any of them: `\n` (a line break), `\s`
# (whitespace) and `\S` (anything but whitespace). glibc's regcomp happens to
# take `\s` and `\S` as they are; BSD's does not, so they are spelled out as
# POSIX classes here. Sets $ere.
translate_pattern() {
	ere="${1//\\n/$'\n'}"
	ere="${ere//\\s/[[:space:]]}"
	ere="${ere//\\S/[^[:space:]]}"
}

# show_excerpt <pattern> <text> <masked file> <what the text is>: how far a
# pattern that did not match got, and what stands there instead.
#
# A pattern is usually several lines (`a\nb\nc`), pinning a block. This looks
# for the longest leading part of it that does match somewhere in <text> (all
# but the last line, then one line fewer, ...), and prints the lines where that
# part ends: the first line after it is the one the pattern did not expect.
# The text is searched as rendered; what is PRINTED comes from <masked file>,
# the same text line for line, masked.
show_excerpt() {
	local text="$2" file="$3" what="$4"
	local -a lines=()
	local rest="$1" ere lead matched before k i total first last
	while [[ "$rest" == *'\n'* ]]; do
		lines+=("${rest%%\\n*}")
		rest="${rest#*\\n}"
	done
	lines+=("$rest")
	total="${#lines[@]}"
	for ((k = total - 1; k >= 1; k--)); do
		lead="${lines[0]}"
		for ((i = 1; i < k; i++)); do
			lead+="\\n${lines[i]}"
		done
		# Nothing but line breaks (a pattern that opens with `\n`) matches
		# everywhere and locates nothing.
		[[ -n "${lead//\\n/}" ]] || continue
		translate_pattern "$lead"
		# Status 2 is a lead that is not an expression on its own (it stops
		# inside a group): try a shorter one.
		[[ "$text" =~ $ere ]] || continue
		matched="${BASH_REMATCH[0]}"
		before="${text%%"$matched"*}"
		first=$(($(printf '%s' "$before" | wc -l) + 1))
		last=$((first + $(printf '%s' "$matched" | wc -l)))
		# Count the pattern's lines as a reader does: `\na\nb\n` is two.
		local shown=0 of=0
		for ((i = 0; i < total; i++)); do
			[[ -n "${lines[i]}" ]] || continue
			of=$((of + 1))
			((i < k)) && shown=$((shown + 1))
		done
		local where="line $first"
		((last > first)) && where="lines $first-$last"
		echo "  the first $shown of the pattern's $of lines match ($where of $what); the rest does not match what follows:" >&2
		_awk -v last="$last" 'NR >= last - 7 && NR <= last + 8 {
			printf "    %5d%s %s\n", NR, (NR == last + 1 ? ">" : " "), (length($0) > 160 ? substr($0, 1, 160) " [...]" : $0)
		}' "$file" >&2
		return
	done
	if [[ "$total" -eq 1 ]]; then
		echo "  (it matches nowhere in $what)" >&2
	else
		echo "  (not even the pattern's first line matches anywhere in $what: ${lines[0]:-${lines[1]}})" >&2
	fi
}
