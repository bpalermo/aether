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
# A base image is third-party too (#1477): the image every aether image is
# built on is pulled by the Bazel module files, where it is the attributes of a
# pull and never one reference. MODULE_FILES, and the module files they
# `include()`, are read for exactly that construct (extract_module_pulls) and
# what they name is held to the same list.
#
# Usage:
#   scripts/third-party-images.sh check
#       Offline; the test (.github/workflows/ci.yaml, `shell` job). Scans the
#       tracked and untracked-but-not-ignored files under SCAN_PATHS, and the
#       base-image pulls in MODULE_FILES, and fails on
#         - an image referenced by tag only, or by no tag at all
#         - a digest the inventory does not list for that name
#         - a tag that disagrees with the inventory's tag for that digest
#         - a pin, or an `allow` or `skip` line, that nothing uses any more
#         - a YAML file no YAML parser can read, a module file's include() it
#           cannot follow, and a rule of an image ruleset it does not know
#       so a new image cannot arrive unpinned or unlisted. YAML files are read
#       with a YAML parser (#1525): this needs python3 with PyYAML, and stops
#       with exit 2, having checked nothing, when it is not there.
#   scripts/third-party-images.sh list
#       Offline. Every pin and the files that use it.
#   scripts/third-party-images.sh outdated [--newer-tags] [<name>...]
#       Asks each pin's registry what its tag points at now (daily, by
#       .github/workflows/third-party-images.yaml through
#       scripts/third-party-images-report.sh). Anonymous: public
#       repositories only, no credential is read from anywhere. Reports, per pin,
#       `current`, `MOVED` (the tag was re-pushed: the new digest is printed),
#       `NOT-MULTI-ARCH` (the pinned index lacks linux/amd64 or linux/arm64) or
#       `ERROR` (the registry did not answer; never reported as current), and
#       says when the index no longer lists linux/amd64 and linux/arm64. With
#       --newer-tags it also lists the registry's tags that sort after the
#       pinned one and have its shape (8.22.0 -> 8.23.0, not `latest`).
#       Exit 0: every pin is current. 1: a pin is behind. 2: a pin could not be
#       checked.
#   scripts/third-party-images.sh newer [<name>...]
#       Asks each pin's registry for its tag list and nothing else, and prints
#       `NEWER <name>:<tag>  <n> newer: <tags>` for a pin that has tags of the
#       pinned tag's shape that sort after it (the newest NEWER_SHOWN of them),
#       `ERROR <name>:<tag>  tags: <why>` for a list it could not read, and a
#       summary line. The scheduled report shows it as a section of its issue
#       (#1569). Exit 0: every list was read, newer tags or not. 2: one was not.
#   scripts/third-party-images.sh resolve <name>:<tag>...
#       Prints the `pin` line for each, after checking the index lists both
#       architectures. How a pin is added or moved; see docs/runbook.md,
#       "Refreshing third-party image pins".
#
# Environment (tests set these; a normal run needs none):
#   THIRD_PARTY_ROOT       the tree to scan (default: this script's repository)
#   THIRD_PARTY_INVENTORY  the inventory (default: scripts/third-party-images.txt
#                          under the root)
#   JQ, CURL               the jq and curl to run (`outdated`, `newer`, `resolve`
#                          only)
#   THIRD_PARTY_YAML_READER  the YAML reader `check` and `list` run (default:
#                          python3 scripts/third_party_images_yaml.py; the test
#                          passes the Bazel-built one, with a pinned PyYAML)
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${THIRD_PARTY_ROOT:-$(cd -- "$HERE/.." && pwd)}"
INVENTORY="${THIRD_PARTY_INVENTORY:-$ROOT/scripts/third-party-images.txt}"
JQ="${JQ:-jq}"
CURL="${CURL:-curl}"
if [ -n "${THIRD_PARTY_YAML_READER:-}" ]; then
	YAML_READER=("$THIRD_PARTY_YAML_READER")
else
	YAML_READER=(python3 "$HERE/third_party_images_yaml.py")
fi

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
# The Bazel module files, read for the base-image pulls and nothing else (see
# extract_module_pulls): the root workspace's and the //proxy workspace's.
MODULE_FILES=(MODULE.bazel proxy/MODULE.bazel)

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

declare -A YAML_PARSED=() # "<path>" -> 1: read from its records, not line by line

# read_yaml_files <work dir> <problems out> <path>...: hands the YAML files to
# the YAML reader (YAML_READER; scripts/third_party_images_yaml.py). Sets
# YAML_PARSED for each file it parsed, whose records are then in
# <work dir>/records/<path>; appends "<path>\t<line>\t<what is wrong>" to
# <problems out> for a file that is neither YAML nor a template. Returns 1,
# with the reason on stderr, when the reader failed or did not answer for every
# file: the caller then stops, because a YAML file nothing read is not a file
# with no image in it.
read_yaml_files() {
	local work="$1" problems="$2" status path line what n=0
	shift 2
	mkdir -p "$work/records" || return 1
	printf '%s\n' "$@" | (cd "$ROOT" && "${YAML_READER[@]}" "$work/records") >"$work/yaml-status" || {
		echo "third-party-images: the YAML reader failed (${YAML_READER[*]}): it needs python3 with PyYAML (THIRD_PARTY_YAML_READER names another)" >&2
		return 1
	}
	while IFS=$'\t' read -r status path line what; do
		case "$status" in
		parsed)
			[ -f "$work/records/$path" ] || continue
			YAML_PARSED["$path"]=1
			;;
		template) ;;
		problem) printf '%s\t%s\t%s\n' "$path" "$line" "$what" >>"$problems" ;;
		*) continue ;;
		esac
		n=$((n + 1))
	done <"$work/yaml-status"
	[ "$n" -eq "$#" ] || {
		echo "third-party-images: the YAML reader (${YAML_READER[*]}) answered for $n of $# YAML file(s)" >&2
		return 1
	}
}

# Prints "<path>\t<line>\t<reference>" for every literal image reference.
#
# A YAML file (`*.yaml`, `*.yml`) is read by a YAML parser and never line by
# line (#1525): read_yaml_files hands the scan one record per scalar, so what
# is judged is what the file says, in whatever syntax it says it: a flow
# mapping broken across lines, a block scalar behind node properties, a value
# on the line after its key, an alias (followed to its anchor and reported
# there). A file under a YAML name that no parser can read is a finding, unless
# it holds `{{`: a Helm template is no YAML until it is rendered, and is read
# line by line with every other file.
#
# Two nets, because no single pattern knows every way an image is named:
#   1. the value of an `image` key or flag:
#      - in a YAML file, the scalar under any key that ends in `image` (any
#        case), and a scalar directly under such a key's mapping or sequence
#        (`image:` / `  ref: x`, as some Helm values files write a whole
#        reference), except under the keys an image is split over (see below);
#      - line by line everywhere else, in any of the spellings in use: YAML
#        embedded in a here-document or a template (`image: x` as a block key,
#        a list item, with a trailing comment, in a flow mapping
#        `[{name: p, image: x}]`, as a block scalar `image: >-` with the value
#        on the next line, as a plain or quoted scalar alone on the line after
#        a bare `image:`, behind an anchor or a tag `image: &a x`; an alias
#        `image: *a` is refused, since no parser followed it), JSON
#        `"image": "x"` (also inside a shell string,
#        `--overrides='{"image":"x"}'` or `--overrides="{\"image\":\"x\"}"`),
#        `--image=x` / `--image x`, a shell `FOO_IMAGE="x"` or
#        `FOO_IMAGE="${FOO_IMAGE:-x}"`, Go `Image: "x"` and `Image = "x"`. What
#        a string of a YAML file holds (a script, a manifest, JSON) is read
#        this way too: it is text in another language;
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
# What this cannot see: an image no pin names yet, written where no `image` key
# or flag introduces it (a positional `docker run <image>`, a list of bare
# names). Give such a reference a `*_IMAGE` variable, as e2e/etcd-image.sh does.
# Also not read, on purpose:
#   - an image split over sibling keys (`image:` / `  repository: x` /
#     `  tag: y`, the Helm values spelling, put together by a template): the
#     parts are not a reference, and the values that are written this way are
#     the images this repository builds. Write a third-party one as a single
#     reference;
#   - YAML embedded in another language (a here-document in a shell script, a
#     Go raw string, a Helm template) when it holds a flow mapping broken across
#     lines, with an unquoted `image:` value that is not first on its line and
#     whose `{` is on an earlier one. There an unquoted `image:` in mid-line
#     with no `{` before it on the same line is how prose reads ("3 node(s) but
#     2 image: line(s)"), and reading it brings those back as findings. A
#     quoted value is read. The same text has the line reader's other limits:
#     an alias is refused, a block scalar behind node properties
#     (`image: &a >-`) is not read, and of a block scalar only the first
#     non-blank line is taken;
#   - a comment that follows a quote left open on its line (an apostrophe in a
#     here-document's text): the line is read whole, comment included;
#   - an image assembled from parts (`"$REPO:$TAG"`, a Go `fmt.Sprintf`): not a
#     literal. A repository written out under a computed tag (`x/y:$TAG`) IS
#     read, by net 1 behind an image key or flag for any name and by net 2
#     anywhere for a known one.
extract_references() { # <names file> <work dir> <problems out>; file list on stdin
	local files=() yaml=() f records_root="$2/records/"
	: >"$3"
	while IFS= read -r f; do
		is_scanned_file "$f" || continue
		is_skipped "$f" && continue
		case "$f" in *.yaml | *.yml) yaml+=("$f") ;; esac
		files+=("$f")
	done
	[ "${#files[@]}" -gt 0 ] || return 0
	if [ "${#yaml[@]}" -gt 0 ]; then
		read_yaml_files "$2" "$3" "${yaml[@]}" || return 1
		# A parsed file is read from its records, in the place it had in the
		# list; a template stays where it is and is read line by line.
		local i
		for i in "${!files[@]}"; do
			[ -n "${YAML_PARSED["${files[$i]}"]:-}" ] && files[i]="$records_root${files[$i]}"
		done
	fi
	(cd "$ROOT" && RECORDS_ROOT="$records_root" awk -v names_file="$1" '
		BEGIN {
			records_root = ENVIRON["RECORDS_ROOT"]
			while ((getline n < names_file) > 0) if (n != "") names[n] = 1
			close(names_file)
		}
		function emit(ref) {
			if (!((path, line, ref) in seen)) {
				seen[path, line, ref] = 1
				printf "%s\t%d\t%s\n", path, line, ref
			}
		}
		# Only a literal image reference is judged: a lower-case repository, then
		# an optional :tag and @digest. That leaves out $VAR, {{ .Values.x }}, a
		# {@//label} stamp, a Go identifier, a regex, and a switch named *_IMAGE.
		function literal(ref) {
			if (ref !~ /^[a-z0-9][a-z0-9._\/-]*(:[0-9]+\/[a-z0-9._\/-]+)?(:[A-Za-z0-9_][A-Za-z0-9._-]*)?(@[^${]*)?$/) return 0
			return ref !~ /^(0|1|true|false|yes|no)$/
		}
		# Where the comment that ends line s begins (0: it has none). A comment
		# opens with `#` after white space, or with `//` anywhere when
		# slash_comments (Go and JavaScript need no space before it), outside
		# quotes; a backslash takes the next character with it, so `\"` closes
		# nothing. Quotes are not followed across lines: one left open hides a
		# comment on its own line only, and that line then stays read in full.
		function comment_start(s, slash_comments,    i, n, c, q, prev) {
			n = length(s)
			q = ""
			prev = ""
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
			return 0
		}
		FNR == 1 {
			block = 0
			# A YAML file the parser read arrives as its records, in a file of
			# the same path under records_root (see read_yaml_files).
			records = (records_root != "" && index(FILENAME, records_root) == 1)
			path = records ? substr(FILENAME, length(records_root) + 1) : FILENAME
			slash_comments = (path ~ /\.(go|js)$/)
		}
		# A record is `<line> TAB <kind> TAB <text>`; an empty one ends a group
		# of records. Kind `v` is what a scalar says: there is no comment left
		# in it, so a `#` in it is text.
		records {
			if ($0 == "") {
				block = 0
				next
			}
			line = $0 + 0
			plain = (substr($0, index($0, "\t") + 1, 1) == "v")
			$0 = substr($0, index($0, "\t") + 3)
		}
		!records {
			line = FNR
			plain = 0
		}
		# The line after `image: >-` / `image: |` is the value (a block scalar).
		# So is the line after a bare `image:` (block == 2), when it holds one
		# scalar and nothing else: there a comment line is passed over, and
		# node properties, quotes and a trailing comment are not the value. A
		# nested mapping (`image:` / `  repository: x`) is not a literal.
		block == 2 && !plain && /^[ \t]*#/ { next }
		block && !/^[ \t]*$/ {
			ref = $0
			if (block == 2) {
				if (!plain && (i = comment_start(ref, 0)) > 0) ref = substr(ref, 1, i - 1)
				sub(/^[ \t]+/, "", ref)
				while (match(ref, /^[&!][^ \t]*[ \t]+/)) ref = substr(ref, RLENGTH + 1)
				gsub(/^["\047]|["\047]?[ \t]*$/, "", ref)
			}
			block = 0
			gsub(/^[ \t]+|[ \t]+$/, "", ref)
			if (literal(ref)) emit(ref)
		}
		!plain && /^[ \t]*(#|\/\/)/ { next }
		{
			code = $0
			if (!plain && (i = comment_start(code, slash_comments)) > 0) code = substr(code, 1, i - 1)
			# JSON inside a double-quoted shell argument writes every quote as
			# `\"` (`--overrides="{\"image\":\"x\"}"`): read as the JSON it is.
			gsub(/\\"/, "\"", code)
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

# Prints "<path>\t<line>\t<reference>" for every base image a Bazel module file
# pulls, and writes "<path>\t<line>\t<what is wrong>" to <problems out> for a
# pull it cannot follow (#1477).
#
# A base image is not written as one reference: it is the attributes of a pull.
# This reads the two spellings in use and no other Starlark:
#
#   <name> = use_repo_rule("@rules_img//img:pull.bzl", "pull")    MODULE.bazel
#   <name>(
#       digest = "sha256:...",
#       registry = "gcr.io",
#       repository = "distroless/static-debian13",
#       tag = "nonroot",
#   )
#
#   <name> = use_extension("@rules_oci//oci:extensions.bzl", "oci")    proxy/MODULE.bazel
#   <name>.pull(
#       digest = "sha256:...",
#       image = "gcr.io/distroless/cc-debian12",
#   )
#
# and puts them together as `<registry>/<repository>[:<tag>][@<digest>]` or
# `<image>[:<tag>][@<digest>]`, reported at the digest's line (the pull's own
# line when it has none), for scan() to judge like any other reference.
#
# It is a line reader for the shape buildifier writes (the call opens a line,
# one attribute per line, `)` closes it), not a Starlark evaluator, so it fails
# closed: a pull in any other shape, an attribute it needs that is not a plain
# string literal, a pull with no repository, and a file that names either rule
# without one pull having been read are each a finding, never a pass. A comment
# line is not read.
#
# What else can pull an image (#1571):
#   - a module file reached by `include("//<package>:<file>.MODULE.bazel")`: it
#     is followed, from the workspace its root module file stands in (so an
#     include of proxy/MODULE.bazel is a path under proxy/), to any depth, each
#     file read once. An include() in any other shape (no string literal, not
#     alone on its line, a `..` in the label) and one that names no file are
#     findings: a module file nobody read is not a module file with no pull;
#   - another rule of a ruleset that pulls images (IMAGE_RULESETS: rules_img,
#     rules_oci, rules_docker, rules_apko): any label of one of them, other
#     than in the two bindings above, is a finding at its line, whether or not
#     the file also holds a pull the reader follows. Teach the reader the rule
#     and it passes.
# Still not seen: a pull by a ruleset that is not in IMAGE_RULESETS (a
# hand-written repository rule, an http_file of an image tarball), and a rule
# reached through a second name (`p = pull`).
IMAGE_RULESETS='rules_img|rules_oci|rules_docker|io_bazel_rules_docker|rules_apko'

# awk: the code of a Starlark line, without the comment that ends it.
AWK_CODE_OF='
	function code_of(s,    i, n, c, q) {
		n = length(s)
		q = ""
		for (i = 1; i <= n; i++) {
			c = substr(s, i, 1)
			if (c == "\\") {
				i++
				continue
			}
			if (q != "") {
				if (c == q) q = ""
			} else if (c == "\"" || c == "\047") {
				q = c
			} else if (c == "#") {
				return substr(s, 1, i - 1)
			}
		}
		return s
	}
'

# module_includes <module file>: prints "<line>\t<path within the workspace>"
# for every include() of the file, with an empty path for one it cannot follow.
module_includes() {
	awk "$AWK_CODE_OF"'
		/^[ \t]*#/ { next }
		{
			bare = code_of($0)
			gsub(/"([^"\\]|\\.)*"|\047([^\047\\]|\\.)*\047/, "\"\"", bare)
			if (bare !~ /(^|[^A-Za-z0-9_.])include[ \t]*\(/) next
			target = ""
			line = code_of($0)
			if (line ~ /^include\("\/\/[A-Za-z0-9_.\/-]*:[A-Za-z0-9_.\/-]+\.MODULE\.bazel"\)[ \t]*$/) {
				target = line
				sub(/^include\("\/\//, "", target)
				sub(/"\).*/, "", target)
				pkg = target
				sub(/:.*/, "", pkg)
				sub(/[^:]*:/, "", target)
				if (pkg != "") target = pkg "/" target
				if (("/" target "/") ~ /\/\.\.?\// || target ~ /\/\// || target ~ /^\//) target = ""
			}
			printf "%d\t%s\n", FNR, target
		}
	' "$1"
}

extract_module_pulls() { # <problems out>
	local problems="$1" files=() queue=() workspaces=() f ws line target i=0
	local -A read_once=()
	: >"$problems"
	for f in "${MODULE_FILES[@]}"; do
		[ -f "$ROOT/$f" ] || continue
		queue+=("$f")
		ws="${f%MODULE.bazel}"
		workspaces+=("$ws")
	done
	while [ "$i" -lt "${#queue[@]}" ]; do
		f="${queue[$i]}"
		ws="${workspaces[$i]}"
		i=$((i + 1))
		[ -z "${read_once[$f]:-}" ] || continue
		read_once[$f]=1
		files+=("$f")
		while IFS=$'\t' read -r line target; do
			if [ -z "$target" ]; then
				printf '%s\t%s\t%s\n' "$f" "$line" 'an include() the reader cannot follow: write it alone on its line, as include("//<package>:<file>.MODULE.bazel")' >>"$problems"
			elif [ -f "$ROOT/$ws$target" ]; then
				queue+=("$ws$target")
				workspaces+=("$ws")
			else
				printf '%s\t%s\t%s\n' "$f" "$line" "include() names $ws$target, which is not a file" >>"$problems"
			fi
		done < <(module_includes "$ROOT/$f")
	done
	[ "${#files[@]}" -gt 0 ] || return 0
	(cd "$ROOT" && awk -v problems="$problems" -v rulesets="$IMAGE_RULESETS" "$AWK_CODE_OF"'
		function problem(line, what) {
			printf "%s\t%s\t%s\n", file, line, what >> problems
		}
		function end_of_file() {
			if (file != "" && names_rule != "" && !pulls) problem(0, "names an image pull rule (" names_rule ") but no pull was read; write the binding and the call the way scripts/third-party-images.sh (extract_module_pulls) documents")
		}
		FNR == 1 {
			end_of_file()
			file = FILENAME
			names_rule = ""
			pulls = 0
			inside = ""
			split("", rule)
			split("", ext)
		}
		/^[ \t]*#/ { next }
		# Every label of an image ruleset: one of the two bindings below, or a
		# finding.
		{
			binding = (inside == "" && ($0 ~ /^[A-Za-z_][A-Za-z0-9_]*[ \t]*=[ \t]*use_repo_rule\("@rules_img\/\/img:pull\.bzl",[ \t]*"pull"\)/ || $0 ~ /^[A-Za-z_][A-Za-z0-9_]*[ \t]*=[ \t]*use_extension\("@rules_oci\/\/oci:extensions\.bzl",[ \t]*"oci"[,)]/))
			rest = code_of($0)
			while (match(rest, "[\"\047]@(" rulesets ")//[^\"\047]*[\"\047]")) {
				label = substr(rest, RSTART + 1, RLENGTH - 2)
				rest = substr(rest, RSTART + RLENGTH)
				if (label == "@rules_img//img:pull.bzl" || label == "@rules_oci//oci:extensions.bzl") {
					names_rule = label
					if (!binding) problem(FNR, "names " label " in a way the reader does not know; write the binding the way scripts/third-party-images.sh (extract_module_pulls) documents")
				} else {
					problem(FNR, "names " label ", a rule of an image ruleset the reader does not know: an image it pulls is checked by nothing; teach scripts/third-party-images.sh (extract_module_pulls) the rule")
				}
			}
		}
		inside == "" && /^[A-Za-z_][A-Za-z0-9_]*[ \t]*=[ \t]*use_repo_rule\("@rules_img\/\/img:pull\.bzl",[ \t]*"pull"\)/ {
			n = $0
			sub(/[ \t]*=.*/, "", n)
			rule[n] = 1
			next
		}
		inside == "" && /^[A-Za-z_][A-Za-z0-9_]*[ \t]*=[ \t]*use_extension\("@rules_oci\/\/oci:extensions\.bzl",[ \t]*"oci"[,)]/ {
			n = $0
			sub(/[ \t]*=.*/, "", n)
			ext[n] = 1
			next
		}
		inside == "" && /^[A-Za-z_][A-Za-z0-9_.]*\(/ {
			n = $0
			sub(/\(.*/, "", n)
			kind = ""
			if (n in rule) kind = "img"
			else if (n ~ /\.pull$/ && (substr(n, 1, length(n) - 5) in ext)) kind = "oci"
			if (kind == "") next
			pulls++
			if ($0 !~ /^[A-Za-z_][A-Za-z0-9_.]*\([ \t]*$/) {
				problem(FNR, "a pull the reader cannot follow: open the call on its own line and write one attribute per line")
				next
			}
			inside = kind
			opened = FNR
			digest_line = 0
			unreadable = 0
			split("", attr)
			next
		}
		inside != "" && /^\)/ {
			if (inside == "img") {
				if (!("repository" in attr)) {
					problem(opened, "this pull names no repository")
					unreadable = 1
				} else if (!("registry" in attr)) {
					problem(opened, "this pull names no registry")
					unreadable = 1
				} else {
					name = attr["registry"] "/" attr["repository"]
				}
			} else {
				if (!("image" in attr)) {
					problem(opened, "this pull names no image")
					unreadable = 1
				} else {
					name = attr["image"]
				}
			}
			if (!unreadable) {
				ref = name
				if ("tag" in attr) ref = ref ":" attr["tag"]
				if ("digest" in attr) ref = ref "@" attr["digest"]
				printf "%s\t%d\t%s\n", FILENAME, (digest_line ? digest_line : opened), ref
			}
			inside = ""
			next
		}
		inside != "" && /^[ \t]+[a-z_]+[ \t]*=/ {
			key = $0
			sub(/^[ \t]+/, "", key)
			sub(/[ \t]*=.*/, "", key)
			if (key != "registry" && key != "repository" && key != "image" && key != "tag" && key != "digest") next
			value = $0
			sub(/^[^=]*=[ \t]*/, "", value)
			if (value !~ /^"[^"\\]*",?[ \t]*(#.*)?$/) {
				problem(FNR, key " is not a plain string literal")
				unreadable = 1
				next
			}
			if (key == "digest") digest_line = FNR
			sub(/^"/, "", value)
			sub(/".*/, "", value)
			attr[key] = value
		}
		END { end_of_file() }
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
	extract_references "$names" "$tmp" "$tmp/yaml-problems" <"$tmp/files" >"$tmp/refs" || die "the scan itself failed; nothing was checked"
	# The base-image pulls of the Bazel module files: judged below like every
	# other reference; a pull that could not be read is a finding of its own.
	extract_module_pulls "$tmp/problems" >>"$tmp/refs" || die "the read of ${MODULE_FILES[*]} itself failed"
	while IFS=$'\t' read -r path line ref; do
		# Line 0: about the file as a whole.
		[ "$line" = 0 ] && line=""
		finding "$path${line:+:$line}: $ref"
	done < <(cat "$tmp/yaml-problems" "$tmp/problems")
	for path in "${MODULE_FILES[@]}"; do
		[ -f "$ROOT/$path" ] && nfiles=$((nfiles + 1))
	done
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
			finding "${INVENTORY#"$ROOT"/}: pin ${key%@*} ${PIN_TAG[$key]} ${key#*@} is used by no file under ${SCAN_PATHS[*]} and by no pull in ${MODULE_FILES[*]}; remove the line"
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

# newer_tags <name> <tag> <work dir>: writes to <work dir>/newer, one per line,
# the registry's tags with the pinned tag's shape (digits may differ, everything
# else must not) that sort after it. Returns 1 with the reason in REG_ERROR, so
# it is called as it stands and never in a command substitution, where
# REG_ERROR would be lost.
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
	} | LC_ALL=C sort -u -V | awk -v pinned="$tag" 'found { print } ($0 "") == (pinned "") { found = 1 }' >"$work/newer"
	# (Compared as strings: as numbers, a tag 3.1 IS the pinned 3.10, and
	# everything from 3.2 on was listed as newer than it.)
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
			if newer_tags "$name" "$tag" "$work"; then
				newer="$(tr '\n' ' ' <"$work/newer")"
				[ -z "$newer" ] || echo "         $name:$tag  newer tags: $newer"
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

# How many of a pin's newer tags `newer` prints: the newest ones.
NEWER_SHOWN=8

cmd_newer() {
	local only=() key name tag work n o with=0 errors=0 seen=0
	while [ "$#" -gt 0 ]; do
		case "$1" in
		-*) die "newer: unknown option $1" ;;
		*) only+=("$1") ;;
		esac
		shift
	done
	need_network_tools
	load_inventory
	work="$(mktemp -d)" || die "mktemp failed"
	# shellcheck disable=SC2064 # expand now: $work is local
	trap "rm -rf '$work'" EXIT
	for key in "${PIN_ORDER[@]}"; do
		name="${key%@*}"
		tag="${PIN_TAG[$key]}"
		if [ "${#only[@]}" -gt 0 ]; then
			for o in "${only[@]}"; do [ "$o" = "$name" ] && break; done
			[ "$o" = "$name" ] || continue
		fi
		seen=$((seen + 1))
		if ! newer_tags "$name" "$tag" "$work"; then
			echo "ERROR    $name:$tag  tags: $REG_ERROR"
			errors=$((errors + 1))
			continue
		fi
		n="$(wc -l <"$work/newer" | tr -d ' ')"
		[ "$n" -gt 0 ] || continue
		with=$((with + 1))
		if [ "$n" -gt "$NEWER_SHOWN" ]; then
			echo "NEWER    $name:$tag  $n newer, the newest $NEWER_SHOWN: $(tail -n "$NEWER_SHOWN" "$work/newer" | paste -sd' ' -)"
		else
			echo "NEWER    $name:$tag  $n newer: $(paste -sd' ' - <"$work/newer")"
		fi
	done
	[ "$seen" -gt 0 ] || die "newer: no pin matches ${only[*]:-the inventory}"
	echo "$seen checked: $with with newer tags, $errors could not be listed."
	[ "$errors" -eq 0 ] || return 2
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
newer)
	shift
	cmd_newer "$@"
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
