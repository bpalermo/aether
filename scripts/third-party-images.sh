#!/usr/bin/env bash
# Third-party container images: pinned by digest, from one inventory, with a
# way to see which pins are behind (#1400, #1401).
#
# An image this repository does not build (curl, an echo server, etcd, the kind
# node) is named in chart values, e2e harnesses, manifests and test fixtures. A
# tag can be re-pushed; a digest cannot. So every such reference carries the
# digest of the image's multi-arch INDEX, and scripts/third-party-images.txt is
# the one list of them:
#
#   pin <name> <tag> <digest>
#
# A reference is written `<name>:<tag>@<digest>` (what the kubelet, containerd,
# `docker run`, `kubectl run --image` and kind's `--image` all accept: the
# digest selects the image, the tag is for the reader), or `<name>@<digest>`
# where a consumer wants no tag.
#
# Usage:
#   scripts/third-party-images.sh check
#       Offline; the test (.github/workflows/ci.yaml, `shell` job). Scans the
#       tracked and untracked-but-not-ignored files under SCAN_PATHS and fails on
#         - an image referenced by tag only, or by no tag at all
#         - a digest the inventory does not list for that name
#         - a tag that disagrees with the inventory's tag for that digest
#         - a pin, or an `allow` or `skip` line, that nothing uses any more
#       so a new image cannot arrive unpinned or unlisted.
#   scripts/third-party-images.sh list
#       Offline. Every pin and the files that use it.
#   scripts/third-party-images.sh outdated [--newer-tags] [<name>...]
#       Asks each pin's registry what its tag points at now. Anonymous: public
#       repositories only, no credential is read from anywhere. Reports, per pin,
#       `current`, `MOVED` (the tag was re-pushed: the new digest is printed),
#       `NOT-MULTI-ARCH` (the pinned index lacks linux/amd64 or linux/arm64) or
#       `ERROR` (the registry did not answer; never reported as current), and
#       says when the index no longer lists linux/amd64 and linux/arm64. With
#       --newer-tags it also lists the registry's tags that sort after the
#       pinned one and have its shape (8.22.0 -> 8.23.0, not `latest`).
#       Exit 0: every pin is current. 1: a pin is behind. 2: a pin could not be
#       checked.
#   scripts/third-party-images.sh resolve <name>:<tag>...
#       Prints the `pin` line for each, after checking the index lists both
#       architectures. How a pin is added or moved; see docs/runbook.md,
#       "Refreshing third-party image pins".
#
# Environment (tests set these; a normal run needs none):
#   THIRD_PARTY_ROOT       the tree to scan (default: this script's repository)
#   THIRD_PARTY_INVENTORY  the inventory (default: scripts/third-party-images.txt
#                          under the root)
#   JQ, CURL               the jq and curl to run (`outdated`, `resolve` only)
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${THIRD_PARTY_ROOT:-$(cd -- "$HERE/.." && pwd)}"
INVENTORY="${THIRD_PARTY_INVENTORY:-$ROOT/scripts/third-party-images.txt}"
JQ="${JQ:-jq}"
CURL="${CURL:-curl}"

# Where an image reference can reach a cluster or a container runtime from:
# the charts, the e2e harnesses and their manifests, the Go e2e and conformance
# suites, and the one Go constant the testcontainers integration tests pull.
# Go unit tests elsewhere name images that are never pulled (`app:v1` in a pod
# fixture) and are deliberately not scanned.
SCAN_PATHS=(charts e2e test registry/etcdtest)

# Not scanned inside SCAN_PATHS: Bazel files and hermetic shell tests (their
# image strings are patterns and fixtures: `kindest/node:$V@sha256:[0-9a-f]{64}`
# in e2e/kind_pin_test.sh is a regular expression, and nothing a `*_test.sh`
# names is ever pulled), and Markdown (prose). A Go `_test.go` IS scanned: the
# e2e and conformance suites are Go tests, and they start what they name.
is_scanned_file() {
	case "$1" in
	*/BUILD.bazel | *.bzl | *.md | *_test.sh) return 1 ;;
	esac
	return 0
}

die() {
	echo "third-party-images: $*" >&2
	exit 2
}

# --- the inventory -------------------------------------------------------------

declare -A PIN_TAG=()    # "<name>@<digest>" -> tag
declare -A PIN_DIGEST=() # "<name>:<tag>" -> digest
declare -A PIN_USES=()   # "<name>@<digest>" -> newline-separated "path:line"
declare -a PIN_ORDER=()
declare -A ALLOW=()      # "<path> <ref>" -> 1
declare -A ALLOW_USED=() # "<path> <ref>" -> 1
declare -a ALLOW_ORDER=()
declare -a SKIP=()
declare -a SKIP_UNUSED=() # the `skip` prefixes no scanned file is under

NAME_RE='^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-]+[a-z0-9]+)*)+$|^[a-z0-9]+([._-][a-z0-9]+)*$'
TAG_RE='^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$'
DIGEST_RE='^sha256:[0-9a-f]{64}$'

load_inventory() {
	[ -f "$INVENTORY" ] || die "no inventory at $INVENTORY"
	local n=0 kind a b c extra key
	while IFS= read -r line || [ -n "$line" ]; do
		n=$((n + 1))
		line="${line%%#*}"
		read -r kind a b c extra <<<"$line" || true
		[ -n "${kind:-}" ] || continue
		case "$kind" in
		pin)
			[ -n "${c:-}" ] && [ -z "${extra:-}" ] || die "$INVENTORY:$n: want 'pin <name> <tag> <digest>'"
			[[ "$a" =~ $NAME_RE ]] || die "$INVENTORY:$n: '$a' is not an image name"
			[[ "$b" =~ $TAG_RE ]] || die "$INVENTORY:$n: '$b' is not a tag"
			[[ "$c" =~ $DIGEST_RE ]] || die "$INVENTORY:$n: '$c' is not sha256:<64 hex>"
			key="$a@$c"
			[ -z "${PIN_TAG[$key]:-}" ] || die "$INVENTORY:$n: $key is listed twice"
			# One tag has one digest: a second line for it is a refresh left half
			# done, with both digests still in use and nothing saying so.
			[ -z "${PIN_DIGEST["$a:$b"]:-}" ] || die "$INVENTORY:$n: $a:$b is already pinned (to ${PIN_DIGEST["$a:$b"]}); a tag has one digest, replace that line"
			PIN_DIGEST["$a:$b"]="$c"
			PIN_TAG[$key]="$b"
			PIN_ORDER+=("$key")
			;;
		allow)
			[ -n "${b:-}" ] && [ -z "${c:-}" ] || die "$INVENTORY:$n: want 'allow <path> <reference>'"
			key="$a $b"
			ALLOW[$key]=1
			ALLOW_ORDER+=("$key")
			;;
		skip)
			[ -n "${a:-}" ] && [ -z "${b:-}" ] || die "$INVENTORY:$n: want 'skip <path prefix>'"
			SKIP+=("$a")
			;;
		*) die "$INVENTORY:$n: unknown line kind '$kind' (pin, allow, skip)" ;;
		esac
	done <"$INVENTORY"
}

# --- the scan ------------------------------------------------------------------

# Every file under SCAN_PATHS, relative to the root. In a git work tree: tracked
# files plus untracked ones git does not ignore, so a new manifest is judged
# before it is committed. Elsewhere (a test's fixture tree): every file.
scanned_files() {
	local present=() p
	for p in "${SCAN_PATHS[@]}"; do
		[ -e "$ROOT/$p" ] && present+=("$p")
	done
	[ "${#present[@]}" -gt 0 ] || return 0
	if git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1 &&
		[ "$(git -C "$ROOT" rev-parse --show-toplevel 2>/dev/null)" = "$ROOT" ]; then
		git -C "$ROOT" ls-files --cached --others --exclude-standard -- "${present[@]}" |
			while IFS= read -r p; do [ -f "$ROOT/$p" ] && printf '%s\n' "$p"; done
	else
		(cd "$ROOT" && find "${present[@]}" -type f)
	fi | LC_ALL=C sort -u
}

is_skipped() {
	local prefix
	for prefix in "${SKIP[@]}"; do
		case "$1" in "$prefix"*) return 0 ;; esac
	done
	return 1
}

# Prints "<path>\t<line>\t<reference>" for every literal image reference.
#
# Two nets, because no single pattern knows every way an image is named:
#   1. the value after an `image` key or flag, in any of the spellings in use:
#      YAML `image: x` (a block key, a list item, with a trailing comment, in
#      a flow mapping `[{name: p, image: x}]`, also one broken across lines,
#      as a block scalar `image: >-` with the value on the next line, as a
#      plain or quoted scalar alone on the line after a bare `image:`, behind an
#      anchor or a tag `image: &a x`; an alias `image: *a` is refused, since
#      its value is written where the scan cannot follow), JSON `"image": "x"`
#      (also inside a shell string, `--overrides='{"image":"x"}'` or
#      `--overrides="{\"image\":\"x\"}"`), `--image=x` / `--image x`, a shell
#      `FOO_IMAGE="x"` or `FOO_IMAGE="${FOO_IMAGE:-x}"`, Go `Image: "x"` and
#      `Image = "x"`;
#   2. any `<name>:<tag>` or `<name>@<digest>` of a name the inventory already
#      knows, wherever it stands, so a known image cannot come back by tag,
#      under a digest the inventory does not list, or under a computed tag or
#      digest (`name:$TAG`, `name@$DIGEST`) behind a key net 1 does not read.
# Comments are not read, neither a comment line nor the comment that ends a
# line of code (` # ...`; ` // ...` in Go and JavaScript): a comment may name
# the tag, and a pin that only a comment still names is used by nothing. A `#`
# or `//` inside quotes, or with no white space before it (`${var#prefix}`, a
# URL), is not a comment. A value that is not a literal (`$VAR`,
# `{{ .Values.x }}`, a Bazel `{@//label}` stamp) is somebody else's value and is
# judged where it is written down.
#
# The scan is line by line and not a YAML parser, so in a YAML file (`*.yaml`,
# `*.yml`) it fails closed: an `image:` that follows a `{`, `[` or `,` outside
# quotes is read as a flow-mapping key whether or not the `{` is on that line.
# The price: a YAML line that is no flow mapping and still reads
# `..., image: word` outside quotes is a finding; quote the text.
#
# What this cannot see: an image no pin names yet, written where no `image` key
# or flag introduces it (a positional `docker run <image>`, a list of bare
# names). Give such a reference a `*_IMAGE` variable, as e2e/etcd-image.sh does.
# Also not read, on purpose:
#   - an image split over sibling keys (`image:` / `  repository: x` /
#     `  tag: y`, the Helm values spelling, put together by a template): the
#     parts are not a reference on any one line. No third-party image in this
#     tree is written that way (the chart's split values are the images this
#     repository builds); write a third-party one as a single reference.
#   - YAML embedded in another language (a here-document in a shell script, a
#     Go raw string) when it holds a flow mapping broken across lines, with an
#     unquoted `image:` value that is not first on its line and whose `{` is on
#     an earlier one. There an unquoted `image:` in mid-line with no `{` before
#     it on the same line is how prose reads ("3 node(s) but 2 image: line(s)"),
#     and reading it brings those back as findings. A quoted value is read;
#   - a comment that follows a quote left open on its line (an apostrophe in a
#     here-document's text): the line is read whole, comment included;
#   - a block scalar whose value is folded over several lines, or whose first
#     line is a comment-looking `#` line: only the first non-blank line after
#     `image: >-` / `image: |` is taken;
#   - an image assembled from parts (`"$REPO:$TAG"`, a Go `fmt.Sprintf`): not a
#     literal. A repository written out under a computed tag (`x/y:$TAG`) IS
#     read, by net 1 behind an image key or flag for any name and by net 2
#     anywhere for a known one.
extract_references() { # <names file>; file list on stdin
	local files=() f
	while IFS= read -r f; do
		is_scanned_file "$f" || continue
		is_skipped "$f" && continue
		files+=("$f")
	done
	[ "${#files[@]}" -gt 0 ] || return 0
	(cd "$ROOT" && awk -v names_file="$1" '
		BEGIN {
			while ((getline n < names_file) > 0) if (n != "") names[n] = 1
			close(names_file)
		}
		function emit(ref) {
			if (!((FILENAME, FNR, ref) in seen)) {
				seen[FILENAME, FNR, ref] = 1
				printf "%s\t%d\t%s\n", FILENAME, FNR, ref
			}
		}
		# Only a literal image reference is judged: a lower-case repository, then
		# an optional :tag and @digest. That leaves out $VAR, {{ .Values.x }}, a
		# {@//label} stamp, a Go identifier, a regex, and a switch named *_IMAGE.
		function literal(ref) {
			if (ref !~ /^[a-z0-9][a-z0-9._\/-]*(:[0-9]+\/[a-z0-9._\/-]+)?(:[A-Za-z0-9_][A-Za-z0-9._-]*)?(@[^${]*)?$/) return 0
			return ref !~ /^(0|1|true|false|yes|no)$/
		}
		# Where the comment that ends line s begins (0: it has none), and, in
		# OPEN_QUOTE, the quote s ends inside of ("" for none). A comment opens
		# with `#` after white space, or with `//` anywhere when slash_comments
		# (Go and JavaScript need no space before it), outside quotes; a
		# backslash takes the next character with it, so `\"` closes nothing. Quotes are not followed across lines: one left open hides a
		# comment on its own line only, and that line then stays read in full.
		function comment_start(s, slash_comments,    i, n, c, q, prev) {
			n = length(s)
			q = ""
			prev = ""
			OPEN_QUOTE = ""
			for (i = 1; i <= n; i++) {
				c = substr(s, i, 1)
				if (c == "\\" && q != "\047" && q != "`") {
					i++
					prev = c
					continue
				}
				if (q != "") {
					if (c == q) q = ""
				} else if (c == "\"" || c == "\047" || (slash_comments && c == "`")) {
					q = c
				} else if (slash_comments ? substr(s, i, 2) == "//" : (prev ~ /[ \t]/ && c == "#")) {
					return i
				}
				prev = c
			}
			OPEN_QUOTE = q
			return 0
		}
		FNR == 1 {
			block = 0
			yaml = (FILENAME ~ /\.ya?ml$/)
			slash_comments = (FILENAME ~ /\.(go|js)$/)
		}
		# The line after `image: >-` / `image: |` is the value (a block scalar).
		# So is the line after a bare `image:` (block == 2), when it holds one
		# scalar and nothing else: there a comment line is passed over, and
		# node properties, quotes and a trailing comment are not the value. A
		# nested mapping (`image:` / `  repository: x`) is not a literal.
		block == 2 && /^[ \t]*#/ { next }
		block && !/^[ \t]*$/ {
			ref = $0
			if (block == 2) {
				if ((i = comment_start(ref, 0)) > 0) ref = substr(ref, 1, i - 1)
				sub(/^[ \t]+/, "", ref)
				while (match(ref, /^[&!][^ \t]*[ \t]+/)) ref = substr(ref, RLENGTH + 1)
				gsub(/^["\047]|["\047]?[ \t]*$/, "", ref)
			}
			block = 0
			gsub(/^[ \t]+|[ \t]+$/, "", ref)
			if (literal(ref)) emit(ref)
		}
		/^[ \t]*(#|\/\/)/ { next }
		{
			code = $0
			if ((i = comment_start(code, slash_comments)) > 0) code = substr(code, 1, i - 1)
			# JSON inside a double-quoted shell argument writes every quote as
			# `\"` (`--overrides="{\"image\":\"x\"}"`): read as the JSON it is.
			if (!yaml) gsub(/\\"/, "\"", code)
			rest = code
			before_key = ""
			at_start = 1
			while (match(rest, /([Ii][Mm][Aa][Gg][Ee]["\047]?[ \t]*[:=][ \t]*|--image[ \t]+)/)) {
				key = substr(rest, RSTART, RLENGTH)
				lead = substr(rest, 1, RSTART - 1)
				before_key = before_key lead
				rest = substr(rest, RSTART + RLENGTH)
				# A YAML key opens its line (`image: x`, `- image: x`, `"image": x`).
				key_opens_line = (at_start && lead ~ /^[ \t]*(-[ \t]+)?["\047]?$/)
				# ...or stands in a flow mapping: after its `{` or a `,`, inside
				# braces opened on this line (`containers: [{name: p, image: x}]`).
				in_flow = (before_key ~ /\{/ && before_key ~ /[{,][ \t]*["\047]?$/)
				# In a YAML file the `{` may be on an earlier line, and a flow
				# sequence takes a single pair (`[image: x]`): fail closed, any
				# key that follows `{`, `[` or `,` outside quotes is a flow key.
				if (!in_flow && yaml && before_key ~ /[{[,][ \t]*["\047]?$/) {
					outside = before_key
					sub(/["\047]$/, "", outside)
					comment_start(outside, 0)
					in_flow = (OPEN_QUOTE == "")
				}
				before_key = before_key key
				at_start = 0
				if (key ~ /:[ \t]*$/ && key_opens_line && rest ~ /^[>|][-+0-9]*[ \t]*(#.*)?$/) {
					block = 1
					break
				}
				# Nothing after the key but node properties: the value, if it is
				# a scalar, is on the next line.
				if (key ~ /:[ \t]*$/ && key_opens_line && rest ~ /^([&!][^ \t]*[ \t]*)*$/) {
					block = 2
					break
				}
				ref = rest
				if (key ~ /:[ \t]*$/ && (key_opens_line || in_flow)) {
					# YAML node properties stand before the value and are not it:
					# `image: &probe x`, `image: !!str x`.
					while (match(ref, /^[&!][^ \t]*[ \t]+/)) ref = substr(ref, RLENGTH + 1)
					# An alias (`image: *probe`) is a value written where this scan
					# cannot follow it: emitted as it stands, which no pin matches.
					if (match(ref, /^\*[A-Za-z0-9_.-]+/)) {
						emit(substr(ref, 1, RLENGTH))
						continue
					}
				}
				quoted = sub(/^["\047]/, "", ref)
				sub(/^\$\{[A-Za-z_][A-Za-z0-9_]*:-/, "", ref)
				if (!match(ref, /^[^] \t"\047}),;]+/)) continue
				ref = substr(ref, 1, RLENGTH)
				# `image:` anywhere else in a line is prose ("no image: line") unless
				# a quoted value follows (Go: `{Name: "a", Image: "b"}`).
				if (key ~ /:[ \t]*$/ && !key_opens_line && !in_flow && !quoted) continue
				if (literal(ref)) {
					emit(ref)
				} else if (match(rest, /^["\047]?[a-z0-9][a-z0-9._\/-]*(:[0-9]+\/[a-z0-9._\/-]+)?(:(\$|\{\{)|(:[A-Za-z0-9_][A-Za-z0-9._-]*)?@(\$|\{\{))[^ \t"\047]*/)) {
					# A repository written out under a computed tag (`x/y:$TAG`,
					# `x/y:{{ .Values.tag }}`) or a computed digest (`x/y:1@$D`,
					# `x/y@$D`) is an image this file names, and it is not
					# pinned: emitted as it stands, whether or not the inventory
					# knows the name.
					ref = substr(rest, 1, RLENGTH)
					sub(/^["\047]/, "", ref)
					emit(ref)
				}
			}
			for (n in names) {
				rest = code
				while ((i = index(rest, n)) > 0) {
					before = (i > 1) ? substr(rest, i - 1, 1) : ""
					if (i > 2 && substr(rest, i - 2, 2) == ":-") before = ""  # ${VAR:-name:tag}
					sep = substr(rest, i + length(n), 1)
					rest = substr(rest, i + length(n))
					if (sep != ":" && sep != "@") continue                    # not a reference
					if (before ~ /[A-Za-z0-9._\/-]/) continue                 # a longer name
					if (sep == "@") {
						# By digest alone (`name@sha256:...`, `name@$DIGEST`): the
						# whole @ token, as below.
						match(rest, /^@[^ \t"\047}),;]*/)
						emit(n substr(rest, 1, RLENGTH))
						continue
					}
					rest = substr(rest, 2)
					if (match(rest, /^(\$|\{\{)[^ \t"\047]*/)) {
						# A known image under a computed tag is not pinned either.
						emit(n ":" substr(rest, 1, RLENGTH))
						continue
					}
					if (!match(rest, /^[A-Za-z0-9_][A-Za-z0-9._-]*/)) continue
					tag = substr(rest, 1, RLENGTH)
					after = substr(rest, RLENGTH + 1)
					# The WHOLE @ token, so `@sha256:<64 hex>-x` is judged as
					# written (and refused) and not as its valid prefix.
					if (match(after, /^@[^ \t"\047}),;]*/)) {
						emit(n ":" tag substr(after, 1, RLENGTH))
					} else {
						emit(n ":" tag)
					}
				}
			}
		}
	' "${files[@]}")
}

# split_reference <ref>: sets REF_NAME, REF_TAG, REF_DIGEST (empty when absent).
split_reference() {
	local ref="$1" rest
	REF_DIGEST=""
	REF_TAG=""
	rest="$ref"
	case "$rest" in *@*)
		REF_DIGEST="${rest#*@}"
		rest="${rest%%@*}"
		;;
	esac
	case "${rest##*/}" in *:*)
		REF_TAG="${rest##*:}"
		rest="${rest%:*}"
		;;
	esac
	REF_NAME="$rest"
}

FINDINGS=0
finding() {
	echo "FAIL: $*" >&2
	FINDINGS=$((FINDINGS + 1))
}

# Reads the tree into PIN_USES / ALLOW_USED and reports every bad reference.
scan() {
	local names tmp path line ref key want nfiles=0 nrefs=0
	tmp="$(mktemp -d)" || die "mktemp failed"
	# shellcheck disable=SC2064 # expand now: $tmp is local
	trap "rm -rf '$tmp'" EXIT
	names="$tmp/names"
	{
		for key in "${PIN_ORDER[@]}"; do printf '%s\n' "${key%@*}"; done
		for key in "${ALLOW_ORDER[@]}"; do
			split_reference "${key#* }"
			printf '%s\n' "$REF_NAME"
		done
	} | LC_ALL=C sort -u >"$names"
	scanned_files >"$tmp/files" || die "could not list the files under ${SCAN_PATHS[*]}"
	nfiles="$(wc -l <"$tmp/files" | tr -d ' ')"
	# A `skip` whose path holds no file the scan would read excuses nothing
	# (a README or a BUILD file under it is never read anyway).
	SKIP_UNUSED=()
	for key in "${SKIP[@]}"; do
		while IFS= read -r path; do
			is_scanned_file "$path" || continue
			case "$path" in "$key"*) continue 2 ;; esac
		done <"$tmp/files"
		SKIP_UNUSED+=("$key")
	done
	extract_references "$names" <"$tmp/files" >"$tmp/refs" || die "the scan itself failed"
	while IFS=$'\t' read -r path line ref; do
		nrefs=$((nrefs + 1))
		if [ -n "${ALLOW["$path $ref"]:-}" ]; then
			ALLOW_USED["$path $ref"]=1
			continue
		fi
		split_reference "$ref"
		if [ -z "$REF_DIGEST" ]; then
			if [ -n "$REF_TAG" ]; then
				finding "$path:$line: $ref is pinned by tag only; write $REF_NAME:$REF_TAG@sha256:<index digest> and list it in ${INVENTORY#"$ROOT"/} ($0 resolve $ref)"
			else
				finding "$path:$line: $ref names no tag and no digest; write $REF_NAME:<tag>@sha256:<index digest> and list it in ${INVENTORY#"$ROOT"/}"
			fi
			continue
		fi
		if ! [[ "$REF_DIGEST" =~ $DIGEST_RE ]]; then
			finding "$path:$line: $ref: '$REF_DIGEST' is not sha256:<64 hex>"
			continue
		fi
		key="$REF_NAME@$REF_DIGEST"
		want="${PIN_TAG[$key]:-}"
		if [ -z "$want" ]; then
			finding "$path:$line: $ref is not in ${INVENTORY#"$ROOT"/}; add 'pin $REF_NAME <tag> $REF_DIGEST', or use the digest it lists for $REF_NAME"
			continue
		fi
		if [ -n "$REF_TAG" ] && [ "$REF_TAG" != "$want" ]; then
			finding "$path:$line: $ref says tag $REF_TAG, but ${INVENTORY#"$ROOT"/} lists this digest as $REF_NAME:$want"
			continue
		fi
		PIN_USES[$key]+="$path:$line"$'\n'
	done <"$tmp/refs"
	SCANNED_FILES="$nfiles"
	SCANNED_REFS="$nrefs"
}

cmd_check() {
	load_inventory
	scan
	local key
	for key in "${PIN_ORDER[@]}"; do
		[ -n "${PIN_USES[$key]:-}" ] ||
			finding "${INVENTORY#"$ROOT"/}: pin ${key%@*} ${PIN_TAG[$key]} ${key#*@} is used by no file under ${SCAN_PATHS[*]}; remove the line"
	done
	for key in "${ALLOW_ORDER[@]}"; do
		[ -n "${ALLOW_USED[$key]:-}" ] ||
			finding "${INVENTORY#"$ROOT"/}: 'allow $key' matches nothing any more; remove the line"
	done
	for key in "${SKIP_UNUSED[@]}"; do
		finding "${INVENTORY#"$ROOT"/}: 'skip $key' matches no file any more; remove the line"
	done
	# A scan that read nothing would pass by default: say so instead.
	[ "$SCANNED_FILES" -gt 0 ] || finding "no file found under ${SCAN_PATHS[*]} in $ROOT; nothing was checked"
	[ "$SCANNED_REFS" -gt 0 ] || finding "no image reference found under ${SCAN_PATHS[*]} in $ROOT; the scan reads nothing"
	if [ "$FINDINGS" -gt 0 ]; then
		echo "third-party-images: $FINDINGS finding(s)." >&2
		return 1
	fi
	echo "OK: $SCANNED_REFS image reference(s) in $SCANNED_FILES file(s): ${#PIN_ORDER[@]} pinned image(s), ${#ALLOW_ORDER[@]} allowed exception(s), ${#SKIP[@]} skipped path(s)."
}

cmd_list() {
	load_inventory
	scan 2>/dev/null
	local key
	for key in "${PIN_ORDER[@]}"; do
		printf '%s:%s@%s\n' "${key%@*}" "${PIN_TAG[$key]}" "${key#*@}"
		printf '%s' "${PIN_USES[$key]:-}" | sed 's/^/    /'
	done
	for key in "${ALLOW_ORDER[@]}"; do
		printf 'NOT PINNED %s (%s)\n' "${key#* }" "${key% *}"
	done
}

# --- the registry (anonymous, read-only) ---------------------------------------

ACCEPT='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

# registry_of <name>: sets REG_HOST and REG_REPO the way a container runtime
# reads the name (no registry host means Docker Hub; one path segment there
# means library/<name>).
registry_of() {
	local name="$1" first="${1%%/*}"
	if [ "$first" != "$name" ] && [[ "$first" == *.* || "$first" == *:* || "$first" == localhost ]]; then
		REG_HOST="$first"
		REG_REPO="${name#*/}"
	else
		REG_HOST="registry-1.docker.io"
		REG_REPO="$name"
		[[ "$name" == */* ]] || REG_REPO="library/$name"
	fi
	[ "$REG_HOST" = docker.io ] && REG_HOST="registry-1.docker.io"
	return 0
}

# One GET. No credential is ever sent: -q ignores ~/.curlrc, and nothing here
# passes --netrc, -u or a Docker config. A bearer token, when there is one, is
# the anonymous pull token the registry itself handed out. HTTPS only, for the
# request and for every redirect -L follows: over plain HTTP an on-path answer
# could supply both the manifest and the digest header it is checked against.
http_get() { # <url> <body out> <headers out> [<bearer token>]
	local args=(-q -sS -L --proto '=https' --proto-redir '=https' --max-time 60 --retry 2 -D "$3" -o "$2" -w '%{http_code}' -H "Accept: $ACCEPT")
	[ -n "${4:-}" ] && args+=(-H "Authorization: Bearer $4")
	"$CURL" "${args[@]}" "$1"
}

header_value() { # <headers file> <lower-case name>: the value in the LAST response
	tr -d '\r' <"$1" | awk -v want="$2" '
		/^HTTP\// { v = "" }
		{ k = tolower($0); if (index(k, want ":") == 1) { v = substr($0, length(want) + 2); sub(/^[ \t]+/, "", v) } }
		END { print v }'
}

# registry_get <host> <repo> <url> <body out> <headers out>: GET with the
# anonymous token dance. Prints nothing; returns 0 on HTTP 200, 1 otherwise
# with the reason in REG_ERROR.
registry_get() {
	local host="$1" repo="$2" url="$3" body="$4" hdr="$5" code challenge realm service scope token
	REG_ERROR=""
	code="$(http_get "$url" "$body" "$hdr")" || {
		REG_ERROR="no answer from $host"
		return 1
	}
	if [ "$code" = 401 ]; then
		challenge="$(header_value "$hdr" www-authenticate)"
		realm="$(printf '%s' "$challenge" | sed -n 's/.*realm="\([^"]*\)".*/\1/p')"
		service="$(printf '%s' "$challenge" | sed -n 's/.*service="\([^"]*\)".*/\1/p')"
		scope="$(printf '%s' "$challenge" | sed -n 's/.*scope="\([^"]*\)".*/\1/p')"
		[ -n "$scope" ] || scope="repository:$repo:pull"
		case "$realm" in
		https://*) ;;
		*)
			REG_ERROR="$host answered 401 without a token endpoint (private repository?)"
			return 1
			;;
		esac
		code="$(http_get "$realm?service=$service&scope=$scope" "$body.token" "$hdr.token")" || code=000
		token=""
		[ "$code" = 200 ] && token="$("$JQ" -r '.token // .access_token // empty' "$body.token" 2>/dev/null)"
		[ -n "$token" ] || {
			REG_ERROR="$realm gave no anonymous pull token for $repo (HTTP $code; private repository?)"
			return 1
		}
		code="$(http_get "$url" "$body" "$hdr" "$token")" || {
			REG_ERROR="no answer from $host"
			return 1
		}
	fi
	[ "$code" = 200 ] || {
		REG_ERROR="$url answered HTTP $code"
		return 1
	}
}

# tag_digest <name> <tag> <work dir>: sets TAG_DIGEST and TAG_PLATFORMS (a
# space-separated os/arch list; empty for a single-platform manifest).
tag_digest() {
	local name="$1" tag="$2" work="$3" got header
	TAG_DIGEST=""
	TAG_PLATFORMS=""
	registry_of "$name"
	registry_get "$REG_HOST" "$REG_REPO" "https://$REG_HOST/v2/$REG_REPO/manifests/$tag" "$work/manifest" "$work/headers" || return 1
	"$JQ" -e 'type == "object"' "$work/manifest" >/dev/null 2>&1 || {
		REG_ERROR="$REG_HOST answered 200 for $name:$tag with something that is not a manifest"
		return 1
	}
	got="sha256:$(sha256sum <"$work/manifest" | awk '{print $1}')"
	header="$(header_value "$work/headers" docker-content-digest)"
	if [ -n "$header" ] && [ "$header" != "$got" ]; then
		REG_ERROR="$name:$tag: the registry says $header but the manifest it sent hashes to $got"
		return 1
	fi
	TAG_DIGEST="$got"
	TAG_PLATFORMS="$("$JQ" -r '[.manifests[]?.platform | select(. != null and .os != "unknown") | "\(.os)/\(.architecture)"] | unique | join(" ")' "$work/manifest")"
}

# Says what is wrong with a platform list, or nothing.
platform_gap() { # <platform list>
	local missing="" want
	[ -n "$1" ] || {
		echo "a single-platform manifest, not a multi-arch index"
		return
	}
	for want in linux/amd64 linux/arm64; do
		case " $1 " in *" $want "*) ;; *) missing+=" $want" ;; esac
	done
	[ -z "$missing" ] || echo "the index does not list${missing}"
}

# newer_tags <name> <tag> <work dir>: the registry's tags with the pinned tag's
# shape (digits may differ, everything else must not) that sort after it.
newer_tags() {
	local name="$1" tag="$2" work="$3" url shape next pages=0
	registry_of "$name"
	: >"$work/tags"
	url="https://$REG_HOST/v2/$REG_REPO/tags/list?n=1000"
	while [ -n "$url" ]; do
		pages=$((pages + 1))
		[ "$pages" -le 50 ] || {
			REG_ERROR="$name: more than 50 pages of tags"
			return 1
		}
		registry_get "$REG_HOST" "$REG_REPO" "$url" "$work/taglist" "$work/tagheaders" || return 1
		"$JQ" -r '.tags[]?' "$work/taglist" >>"$work/tags" || {
			REG_ERROR="$name: the tag list is not JSON"
			return 1
		}
		next="$(header_value "$work/tagheaders" link | sed -n 's/^<\([^>]*\)>;[ ]*rel="next".*/\1/p')"
		case "$next" in
		"") url="" ;;
		https://*) url="$next" ;;
		/*) url="https://$REG_HOST$next" ;;
		*) url="" ;;
		esac
	done
	shape="$(printf '%s' "$tag" | sed -e 's/[.]/\\./g' -e 's/[0-9][0-9]*/[0-9]+/g')"
	{
		grep -E -x -- "$shape" "$work/tags" || true
		printf '%s\n' "$tag"
	} | LC_ALL=C sort -u -V | awk -v pinned="$tag" 'found { print } $0 == pinned { found = 1 }'
}

need_network_tools() {
	command -v "$JQ" >/dev/null 2>&1 || die "jq not found (JQ=$JQ)"
	command -v "$CURL" >/dev/null 2>&1 || die "curl not found (CURL=$CURL)"
	command -v sha256sum >/dev/null 2>&1 || die "sha256sum not found"
}

cmd_outdated() {
	local with_newer=0 only=() key name tag digest gap newer work behind=0 errors=0 seen=0
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--newer-tags) with_newer=1 ;;
		-*) die "outdated: unknown option $1" ;;
		*) only+=("$1") ;;
		esac
		shift
	done
	need_network_tools
	load_inventory
	work="$(mktemp -d)" || die "mktemp failed"
	# shellcheck disable=SC2064 # expand now: $work is local
	trap "rm -rf '$work'" EXIT
	wanted() {
		[ "${#only[@]}" -eq 0 ] && return 0
		local o
		for o in "${only[@]}"; do [ "$o" = "$1" ] && return 0; done
		return 1
	}
	for key in "${PIN_ORDER[@]}"; do
		name="${key%@*}"
		digest="${key#*@}"
		tag="${PIN_TAG[$key]}"
		wanted "$name" || continue
		seen=$((seen + 1))
		if ! tag_digest "$name" "$tag" "$work"; then
			echo "ERROR    $name:$tag  $REG_ERROR"
			errors=$((errors + 1))
			continue
		fi
		gap="$(platform_gap "$TAG_PLATFORMS")"
		if [ "$TAG_DIGEST" != "$digest" ]; then
			echo "MOVED    $name:$tag  pinned $digest, the tag now points at $TAG_DIGEST"
			behind=$((behind + 1))
			[ -z "$gap" ] || echo "         $name:$tag  note: $gap"
		elif [ -n "$gap" ]; then
			# The pin itself is not an index of both architectures: not current,
			# whatever the tag does.
			echo "NOT-MULTI-ARCH $name:$tag  $digest: $gap"
			behind=$((behind + 1))
		else
			echo "current  $name:$tag  $digest"
		fi
		if [ "$with_newer" = 1 ]; then
			if newer="$(newer_tags "$name" "$tag" "$work")"; then
				[ -z "$newer" ] || echo "         $name:$tag  newer tags: $(printf '%s' "$newer" | tr '\n' ' ')"
			else
				echo "ERROR    $name:$tag  tags: $REG_ERROR"
				errors=$((errors + 1))
			fi
		fi
	done
	# What is not pinned at all is behind by definition; say where its tag is.
	for key in "${ALLOW_ORDER[@]}"; do
		split_reference "${key#* }"
		wanted "$REF_NAME" || continue
		[ -n "$REF_TAG" ] && [ -z "$REF_DIGEST" ] || continue
		seen=$((seen + 1))
		if tag_digest "$REF_NAME" "$REF_TAG" "$work"; then
			echo "UNPINNED $REF_NAME:$REF_TAG  (${key% *}) the tag points at $TAG_DIGEST"
		else
			echo "ERROR    $REF_NAME:$REF_TAG  $REG_ERROR"
			errors=$((errors + 1))
		fi
	done
	[ "$seen" -gt 0 ] || die "outdated: no pin matches ${only[*]:-the inventory}"
	echo "$seen checked: $behind behind, $errors could not be checked."
	[ "$errors" -eq 0 ] || return 2
	[ "$behind" -eq 0 ] || return 1
}

cmd_resolve() {
	[ "$#" -gt 0 ] || die "resolve: want <name>:<tag>..."
	need_network_tools
	local work ref gap rc=0
	work="$(mktemp -d)" || die "mktemp failed"
	# shellcheck disable=SC2064 # expand now: $work is local
	trap "rm -rf '$work'" EXIT
	for ref in "$@"; do
		split_reference "$ref"
		[ -n "$REF_TAG" ] && [ -z "$REF_DIGEST" ] || die "resolve: '$ref' is not <name>:<tag>"
		if ! tag_digest "$REF_NAME" "$REF_TAG" "$work"; then
			echo "ERROR $ref: $REG_ERROR" >&2
			rc=2
			continue
		fi
		gap="$(platform_gap "$TAG_PLATFORMS")"
		if [ -n "$gap" ]; then
			echo "# $ref: $gap" >&2
			[ "$rc" -ne 0 ] || rc=1
		fi
		echo "pin $REF_NAME $REF_TAG $TAG_DIGEST"
	done
	return "$rc"
}

case "${1:-}" in
check)
	shift
	cmd_check "$@"
	;;
list)
	shift
	cmd_list "$@"
	;;
outdated)
	shift
	cmd_outdated "$@"
	;;
resolve)
	shift
	cmd_resolve "$@"
	;;
*)
	sed -n '2,/^set -uo/p' "${BASH_SOURCE[0]}" | sed -e '$d' -e 's/^# \{0,1\}//' >&2
	exit 2
	;;
esac
