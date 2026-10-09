#!/usr/bin/env bash
# Hermetic test of scripts/third-party-images.sh (#1400, #1401): no network, no
# registry, no credentials. Three halves:
#
#   1. `check` over throwaway trees: a tree whose every reference agrees with
#      its inventory passes, and each way of being unpinned or unlisted fails
#      with the file and line: a tag alone, no tag at all, a digest the
#      inventory does not list, a tag that disagrees with it, in every spelling
#      the scan reads (YAML, --image, a shell *_IMAGE default, Go), a known
#      image by tag behind a key the scan does not read, a pin or an exception
#      nothing uses, and a scan that read nothing. A YAML file goes through
#      the YAML parser (#1525): what a line reader could not follow, a file no
#      parser can read, and a reader that fails. The Bazel module files: the
#      two pulls, the files they include() and a rule of an image ruleset the
#      reader does not know (#1571).
#   2. `outdated` and `resolve` against a fake `curl` that plays the registries:
#      a tag that still points at the pin, one that moved, a registry that is
#      down or answers with something that is not a manifest (never "current"),
#      the anonymous token dance, an index short of an architecture, newer tags
#      across two pages of a tag list.
#   3. no credential: every fake-registry call is logged, and none carries one.
#
# Run: bazel test //scripts:third_party_images_test (jq and the YAML reader's
# PyYAML are the Bazel-pinned ones), or bash scripts/third_party_images_test.sh
# with jq and a python3 that has PyYAML on PATH.
#
# File-wide: the fixture lines below are literal shell, YAML and Markdown written
# into throwaway files ('C_IMAGE="${C_IMAGE:-x/y:1.2}"'); none is meant to expand
# here.
# shellcheck disable=SC2016
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/third-party-images.sh"
REAL_INVENTORY="$HERE/third-party-images.txt"

if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}
export JQ

# The YAML reader `check` hands every YAML file to: under Bazel the py_binary
# with the hash-checked PyYAML wheel, otherwise the script's own default
# (python3 on PATH and its yaml module).
if [ -n "${YAML_READER_RLOCATIONPATH:-}" ]; then
	THIRD_PARTY_YAML_READER="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${YAML_READER_RLOCATIONPATH}"
	[ -x "$THIRD_PARTY_YAML_READER" ] || {
		echo "FAIL: no YAML reader (THIRD_PARTY_YAML_READER=${THIRD_PARTY_YAML_READER})"
		exit 1
	}
	export THIRD_PARTY_YAML_READER
fi

TMP="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
CASES=0
ok() {
	CASES=$((CASES + 1))
	echo "ok   $*"
}
bad() {
	CASES=$((CASES + 1))
	FAILS=$((FAILS + 1))
	echo "FAIL $*"
}

D1="sha256:$(printf '1%.0s' {1..64})"
D2="sha256:$(printf '2%.0s' {1..64})"
D3="sha256:$(printf '3%.0s' {1..64})"
D9="sha256:$(printf '9%.0s' {1..64})"

# --- 1. check ------------------------------------------------------------------

# new_tree <dir>: a tree in which every reference is pinned and listed.
new_tree() {
	local t="$1"
	rm -rf "$t"
	mkdir -p "$t/scripts" "$t/charts/x/templates" "$t/e2e/sub" "$t/test/e2e" "$t/registry/etcdtest" "$t/other"
	cat >"$t/scripts/third-party-images.txt" <<EOF
# a comment
pin a/b 1.0 $D1   # trailing comment
pin quay.io/c/d v2 $D2
pin registry.k8s.io/pause 3.10 $D3
EOF
	cat >"$t/charts/x/values.yaml" <<EOF
image:
  ref: "{@//x:image_push}"
client:
  # a/b:1.0 is the tag; a comment may say so
  image: a/b:1.0@$D1
EOF
	cat >"$t/charts/x/templates/x.yaml" <<'EOF'
          image: {{ .Values.client.image }}
          imagePullPolicy: {{ .Values.image.pullPolicy }}
EOF
	cat >"$t/e2e/run.sh" <<EOF
#!/usr/bin/env bash
D_IMAGE="\${D_IMAGE:-quay.io/c/d:v2@$D2}"
EVICT_IMAGE="\${X_EVICT_IMAGE:-1}"
echo "(X_EVICT_IMAGE=\$EVICT_IMAGE: the image stays cached)"
err "\$CFG: \$nodes node(s) but \$images image: line(s)"
kubectl run p --image=registry.k8s.io/pause:3.10@$D3 --restart=Never
kind create cluster --image "\$KIND_NODE_IMAGE"
cat <<YAML
      image: \$D_IMAGE
      image: \${IMAGE_REGISTRY}/local:latest
YAML
EOF
	cat >"$t/test/e2e/x_test.go" <<EOF
package e2e

var agentImage = envOrDefault("AGENT_IMAGE", defaultAgentImage)

// Image: "nginx" in a comment is not read.
var c = Container{
	Image: "a/b@$D1",
}
var d = Container{Name: "x", Image: agentImage}
EOF
	# Not scanned: Bazel files, a shell test's patterns, prose, and anything
	# outside the scan paths.
	echo 'patterns = ["image: \"a/b:9\""]' >"$t/e2e/BUILD.bazel"
	echo 'want_re="^a/b:${V}@sha256:[0-9a-f]{64}$"' >"$t/e2e/pin_test.sh"
	echo 'Use `image: nginx` for a quick try.' >"$t/e2e/README.md"
	echo 'image: nginx' >"$t/other/pod.yaml"
}

run_check() { # <tree>: sets RC and OUT
	OUT="$(THIRD_PARTY_ROOT="$1" "$SCRIPT" check 2>&1)"
	RC=$?
}

# expect_fail <name> <tree> <needle>...: check fails (1) and says every needle.
expect_fail() {
	local name="$1" tree="$2" needle
	shift 2
	run_check "$tree"
	if [ "$RC" -ne 1 ]; then
		bad "$name: exit $RC, want 1"$'\n'"$OUT"
		return
	fi
	for needle in "$@"; do
		case "$OUT" in *"$needle"*) ;; *)
			bad "$name: output lacks '$needle'"$'\n'"$OUT"
			return
			;;
		esac
	done
	ok "$name"
}

T="$TMP/tree"
new_tree "$T"
run_check "$T"
if [ "$RC" -eq 0 ] && [[ "$OUT" == "OK: 4 image reference(s) in "*": 3 pinned image(s), 0 allowed exception(s), 0 skipped path(s)." ]]; then
	ok "a tree whose references all agree with the inventory passes"
else
	bad "clean tree: exit $RC"$'\n'"$OUT"
fi

# list: every pin with its uses.
OUT="$(THIRD_PARTY_ROOT="$T" "$SCRIPT" list 2>&1)"
want="a/b:1.0@$D1
    charts/x/values.yaml:5
    test/e2e/x_test.go:7
quay.io/c/d:v2@$D2
    e2e/run.sh:2
registry.k8s.io/pause:3.10@$D3
    e2e/run.sh:6"
if [ "$OUT" = "$want" ]; then ok "list prints every pin with the files and lines that use it"; else bad "list:"$'\n'"$OUT"; fi

# Each mutation: one more line in one file of a fresh clean tree.
mutate() { # <name> <relative file> <line> <needle>...
	local name="$1" file="$2" line="$3"
	shift 3
	new_tree "$T"
	mkdir -p "$(dirname "$T/$file")"
	printf '%s\n' "$line" >>"$T/$file"
	expect_fail "$name" "$T" "$@"
}

mutate "YAML image by tag only" charts/x/values.yaml "  image: x/y:1.2" \
	"charts/x/values.yaml:6: x/y:1.2 is pinned by tag only"
mutate "YAML list item, quoted, by tag only" e2e/sub/pod.yaml '  - image: "x/y:1.2"' \
	"e2e/sub/pod.yaml:1: x/y:1.2 is pinned by tag only"
mutate "YAML image with no tag at all" e2e/sub/pod.yaml "      image: nginx" \
	"e2e/sub/pod.yaml:1: nginx names no tag and no digest"
mutate "a digest the inventory does not list" e2e/sub/pod.yaml "      image: x/y:1.2@$D9" \
	"e2e/sub/pod.yaml:1: x/y:1.2@$D9 is not in scripts/third-party-images.txt"
mutate "a listed name under another digest" e2e/sub/pod.yaml "      image: a/b:1.0@$D9" \
	"e2e/sub/pod.yaml:1: a/b:1.0@$D9 is not in scripts/third-party-images.txt"
mutate "a tag that disagrees with the inventory" e2e/sub/pod.yaml "      image: a/b:1.1@$D1" \
	"says tag 1.1, but scripts/third-party-images.txt lists this digest as a/b:1.0"
mutate "a malformed digest" e2e/sub/pod.yaml "      image: a/b:1.0@sha256:abc" \
	"is not sha256:<64 hex>"
mutate "shell *_IMAGE assignment by tag" e2e/run.sh 'C_IMAGE="x/y:1.2"' \
	"e2e/run.sh:12: x/y:1.2 is pinned by tag only"
mutate "shell *_IMAGE default by tag" e2e/run.sh 'C_IMAGE="${C_IMAGE:-x/y:1.2}"' \
	"e2e/run.sh:12: x/y:1.2 is pinned by tag only"
mutate "--image=<tag>" e2e/run.sh 'kubectl run q --image=x/y:1.2' \
	"e2e/run.sh:12: x/y:1.2 is pinned by tag only"
mutate "--image <tag>" e2e/run.sh 'kubectl run q --image x/y:1.2 --restart=Never' \
	"e2e/run.sh:12: x/y:1.2 is pinned by tag only"
mutate "Go field by tag" test/e2e/x_test.go '	Image: "x/y:1.2",' \
	"test/e2e/x_test.go:10: x/y:1.2 is pinned by tag only"
mutate "Go field in the middle of a line" test/e2e/x_test.go 'var e = Container{Name: "x", Image: "x/y:1.2"}' \
	"test/e2e/x_test.go:10: x/y:1.2 is pinned by tag only"
mutate "Go constant by tag" registry/etcdtest/etcdtest.go 'const Image = "gcr.io/x/etcd:v3"' \
	"registry/etcdtest/etcdtest.go:1: gcr.io/x/etcd:v3 is pinned by tag only"
mutate "a registry with a port" e2e/sub/pod.yaml "      image: localhost:5000/x/y:1" \
	"localhost:5000/x/y:1 is pinned by tag only"
mutate "a known image by tag behind a key the scan does not read" e2e/run.sh 'docker run --rm a/b:1.0 true' \
	"e2e/run.sh:12: a/b:1.0 is pinned by tag only"
mutate "a known image under a computed tag" e2e/run.sh 'docker run --rm "a/b:${B_TAG}" true' \
	'e2e/run.sh:12: a/b:${B_TAG} is pinned by tag only'
mutate "an unknown image under a computed tag, YAML" e2e/sub/pod.yaml '      image: new/tool:${TOOL_TAG}' \
	'e2e/sub/pod.yaml:1: new/tool:${TOOL_TAG} is pinned by tag only'
mutate "an unknown image under a computed tag, quoted flag" e2e/run.sh 'kubectl run q --image="new/tool:$TOOL_TAG"' \
	'e2e/run.sh:12: new/tool:$TOOL_TAG is pinned by tag only'
mutate "an unknown image under a templated tag" charts/x/values.yaml '  image: new/tool:{{ .Values.tag }}' \
	'charts/x/values.yaml:6: new/tool:{{ is pinned by tag only'
# A computed DIGEST is no pin either, with or without a written-out tag.
mutate "an unknown image under a computed digest, YAML" e2e/sub/pod.yaml '      image: new/tool:2@${TOOL_DIGEST}' \
	'e2e/sub/pod.yaml:1: new/tool:2@${TOOL_DIGEST}' "is not sha256:<64 hex>"
mutate "an unknown image under a computed digest, no tag, quoted flag" e2e/run.sh 'kubectl run q --image="new/tool@$TOOL_DIGEST"' \
	'e2e/run.sh:12: new/tool@$TOOL_DIGEST' "is not sha256:<64 hex>"
mutate "an unknown image under a templated digest" charts/x/values.yaml '  image: new/tool:2@{{ .Values.digest }}' \
	'charts/x/values.yaml:6: new/tool:2@{{' "is not sha256:<64 hex>"
# A known name by digest alone, behind a key the scan does not read.
mutate "a known image by an unlisted digest, no tag, behind no image key" e2e/run.sh "docker run --rm a/b@$D9 true" \
	"e2e/run.sh:12: a/b@$D9 is not in scripts/third-party-images.txt"
mutate "a known image by a computed digest, no tag, behind no image key" e2e/run.sh 'docker run --rm "a/b@${B_DIGEST}" true' \
	'e2e/run.sh:12: a/b@${B_DIGEST' "is not sha256:<64 hex>"
mutate "a known image by an unlisted digest under a child key of image:" charts/x/values.yaml $'engine:\n  image:\n    ref: a/b@'"$D9" \
	"charts/x/values.yaml:8: a/b@$D9 is not in scripts/third-party-images.txt"
# ...which counts as a use when the digest is the listed one; a longer name that
# ends in a known one, and a comment, are not that image.
new_tree "$T"
echo "pin x/positional 2 $D9" >>"$T/scripts/third-party-images.txt"
cat >>"$T/e2e/run.sh" <<EOF
docker run --rm x/positional@$D9 true
docker run --rm zx/positional@$D1 true # x/positional@$D1
EOF
run_check "$T"
if [ "$RC" -eq 0 ] && [[ "$OUT" == "OK: 5 image reference(s) in "*": 4 pinned image(s)"* ]]; then
	ok "a known image by its listed digest alone, behind no image key, is a use"
else
	bad "digest-only positional use: exit $RC"$'\n'"$OUT"
fi

# Spellings that once read as prose or were not read at all (review of #1476).
mutate "YAML flow mapping, unquoted, by tag only" e2e/sub/pod.yaml '  containers: [{name: probe, image: x/y:latest}]' \
	"e2e/sub/pod.yaml:1: x/y:latest is pinned by tag only"
mutate "YAML flow mapping, image first, by tag only" e2e/sub/pod.yaml '  containers: [{image: x/y:latest, name: probe}]' \
	"e2e/sub/pod.yaml:1: x/y:latest is pinned by tag only"
mutate "YAML flow mapping, quoted, by tag only" e2e/sub/pod.yaml '  containers: [{name: probe, "image": "x/y:latest"}]' \
	"e2e/sub/pod.yaml:1: x/y:latest is pinned by tag only"
mutate "YAML folded block scalar by tag only" e2e/sub/pod.yaml $'  - image: >-\n      x/y:latest' \
	"e2e/sub/pod.yaml:2: x/y:latest is pinned by tag only"
mutate "YAML literal block scalar by tag only" e2e/sub/pod.yaml $'    image: |\n\n      x/y:latest' \
	"e2e/sub/pod.yaml:3: x/y:latest is pinned by tag only"
mutate "YAML list item with a trailing comment" e2e/sub/pod.yaml '  - image: x/y:latest # the probe' \
	"e2e/sub/pod.yaml:1: x/y:latest is pinned by tag only"
mutate "JSON manifest by tag only" e2e/sub/pod.json '{"spec":{"containers":[{"name":"p","image":"x/y:latest"}]}}' \
	"e2e/sub/pod.json:1: x/y:latest is pinned by tag only"
mutate "JSON in a kubectl --overrides string" e2e/run.sh "kubectl run r --overrides='{\"spec\":{\"containers\":[{\"name\":\"r\",\"image\":\"x/y:latest\"}]}}'" \
	"e2e/run.sh:12: x/y:latest is pinned by tag only"
mutate "--image=\"<tag>\", quoted" e2e/run.sh 'kubectl run s --image="x/y:latest"' \
	"e2e/run.sh:12: x/y:latest is pinned by tag only"
mutate "YAML anchor before the value" e2e/sub/pod.yaml '    image: &probe x/y:latest' \
	"e2e/sub/pod.yaml:1: x/y:latest is pinned by tag only"
mutate "YAML tag and anchor before the value" e2e/sub/pod.yaml '  - image: !!str &probe "x/y:latest"' \
	"e2e/sub/pod.yaml:1: x/y:latest is pinned by tag only"
mutate "YAML anchor in a flow mapping" e2e/sub/pod.yaml '  containers: [{name: p, image: &probe x/y:latest}]' \
	"e2e/sub/pod.yaml:1: x/y:latest is pinned by tag only"
# An alias with no anchor is no YAML: the file is refused, never passed over.
# (Before #1525 the line reader refused the alias itself, as `*probe names no
# tag`; an alias that HAS its anchor is followed, below.)
mutate "YAML alias with no anchor as the image" e2e/sub/pod.yaml '    image: *probe' \
	"e2e/sub/pod.yaml:1: is not YAML a parser can read (found undefined alias"

# Second review of #1476. JSON inside a DOUBLE-quoted shell argument: every
# quote of it is written `\"`.
mutate "JSON in a double-quoted kubectl --overrides string" e2e/run.sh 'kubectl run r --overrides="{\"spec\":{\"containers\":[{\"name\":\"r\",\"image\":\"x/y:latest\"}]}}"' \
	"e2e/run.sh:12: x/y:latest is pinned by tag only"
# A flow mapping broken across lines: in a YAML file a key that follows `{`,
# `[` or `,` is a flow key wherever the brace was opened.
mutate "YAML flow mapping broken across lines" e2e/sub/pod.yaml $'  containers: [{name: probe,\n    command: [sleep], image: new/tool:latest}]' \
	"e2e/sub/pod.yaml:2: new/tool:latest is pinned by tag only"
mutate "YAML flow mapping broken across lines, quoted key" e2e/sub/pod.yml $'  containers: [{name: probe,\n    command: [sleep], "image": new/tool:latest}]' \
	"e2e/sub/pod.yml:2: new/tool:latest is pinned by tag only"
mutate "YAML single-pair mapping in a flow sequence" e2e/sub/pod.yaml '  containers: [image: new/tool:latest]' \
	"e2e/sub/pod.yaml:1: new/tool:latest is pinned by tag only"
# The value may stand alone on the line after its key, behind a node property
# or not (third review of #1476).
mutate "YAML value on the next line" e2e/sub/pod.yaml $'    image:\n      new/tool:latest' \
	"e2e/sub/pod.yaml:2: new/tool:latest is pinned by tag only"
mutate "YAML anchored value on the next line" e2e/sub/pod.yaml $'  - image:\n      &probe new/tool:latest # why' \
	"e2e/sub/pod.yaml:2: new/tool:latest is pinned by tag only"
mutate "YAML anchor on the key line, quoted value after a comment line" e2e/sub/pod.yaml $'    image: !!str &probe\n      # the probe\n      "new/tool:latest"' \
	"e2e/sub/pod.yaml:3: new/tool:latest is pinned by tag only"
# A `#` is a comment only after white space and outside quotes: what follows a
# quoted one, a `${var#pattern}` or a URL fragment is still read.
mutate "a known image after a quoted #" e2e/run.sh 'echo "step # 1" && docker run --rm a/b:1.0 true' \
	"e2e/run.sh:12: a/b:1.0 is pinned by tag only"
mutate "a known image after a single-quoted #" e2e/run.sh "echo 'step # 1' && docker run --rm a/b:1.0 true" \
	"e2e/run.sh:12: a/b:1.0 is pinned by tag only"
mutate "a known image after an escaped quote and a #" e2e/run.sh 'echo "a \" # b" && docker run --rm a/b:1.0 true' \
	"e2e/run.sh:12: a/b:1.0 is pinned by tag only"
mutate "an image after a \${var#pattern}" e2e/run.sh 'short=${full#docker.io/} C_IMAGE="x/y:1.2"' \
	"e2e/run.sh:12: x/y:1.2 is pinned by tag only"
mutate "an image after a URL fragment" e2e/run.sh 'curl -s https://example.test/doc#pins; kubectl run q --image=x/y:1.2' \
	"e2e/run.sh:12: x/y:1.2 is pinned by tag only"
mutate "a Go field after a // inside a string" test/e2e/x_test.go 'var f = Container{Name: "see // below", Image: "x/y:1.2"}' \
	"test/e2e/x_test.go:10: x/y:1.2 is pinned by tag only"
mutate "a Go field after a URL" test/e2e/x_test.go 'var g = Container{Name: "https://example.test", Image: "x/y:1.2"} // why' \
	"test/e2e/x_test.go:10: x/y:1.2 is pinned by tag only"

# A pin whose only occurrence is in a trailing comment is used by nothing.
stale_in_comment() { # <name> <relative file> <line>
	new_tree "$T"
	echo "pin x/stale 2 $D9" >>"$T/scripts/third-party-images.txt"
	mkdir -p "$(dirname "$T/$2")"
	printf '%s\n' "$3" >>"$T/$2"
	expect_fail "$1" "$T" "pin x/stale 2 $D9 is used by no file"
}
stale_in_comment "a pin named only in a YAML trailing comment" e2e/sub/pod.yaml "note: history # x/stale:2@$D9"
stale_in_comment "a pin named only in a shell trailing comment" e2e/run.sh "true # was x/stale:2@$D9"
stale_in_comment "a pin named only after a quoted string and a comment" e2e/run.sh "echo \"it's\" # was x/stale:2@$D9"
stale_in_comment "a pin named only in a Go trailing comment" test/e2e/x_test.go "var h = 1 // was x/stale:2@$D9"
stale_in_comment "a pin named only in a Go comment with no space before it" test/e2e/x_test.go "var h = 1// was x/stale:2@$D9"
# ...and a comment may still name the tag alone, as a comment line may.
new_tree "$T"
echo "  - image: a/b:1.0@$D1 # a/b:1.0, was a/b:0.9" >"$T/e2e/sub/pod.yaml"
run_check "$T"
if [ "$RC" -eq 0 ] && [[ "$OUT" == "OK: 5 image reference(s) in "* ]]; then
	ok "a trailing comment may name a tag"
else
	bad "tag in a trailing comment: exit $RC"$'\n'"$OUT"
fi

# The same spellings pass when pinned and listed, and prose stays prose: an
# `image:` in mid-line inside a quoted string, in a shell script with no `{`
# before it, and a block scalar's value that is not an image.
new_tree "$T"
cat >"$T/e2e/sub/pod.yaml" <<EOF
spec:
  containers: [{name: probe, image: a/b:1.0@$D1}]
  initContainers: [{name: i, "image": "a/b:1.0@$D1"}]
  more:
    - image: >-
        a/b:1.0@$D1
    - image: a/b:1.0@$D1 # the probe
    - image: &anchored a/b:1.0@$D1
  split: [{name: probe,
    command: [sleep], image: a/b:1.0@$D1}]
  description: "no tag, image: latest is what a reader would write"
  other: 'one, image: latest'
  escaped: "a \" b, image: latest"
  note: {text: "see the image: line", other: 1}
  image: |
    not an image, a paragraph
  next:
    image:
      &next a/b:1.0@$D1
  nested:
    image:
      repository: local
      tag: dev
EOF
cat >>"$T/e2e/run.sh" <<'EOF'
err "one, two, image: missing"
log "[image: missing], image: gone"
echo nodes, image: missing
EOF
run_check "$T"
if [ "$RC" -eq 0 ] && [[ "$OUT" == "OK: 11 image reference(s) in "* ]]; then
	ok "flow mappings, block scalars and trailing comments pass when pinned; prose is not read"
else
	bad "pinned flow/block spellings: exit $RC"$'\n'"$OUT"
fi

# --- a YAML file is read with a YAML parser (#1525, #1557) ----------------------
#
# What a line reader could not follow in a YAML file: each is a reference by
# tag only and must fail, at the line the value is written on.
mutate "YAML alias, followed to its anchor" e2e/sub/pod.yaml $'probe: &probe new/tool:latest\nspec:\n  image: *probe' \
	"e2e/sub/pod.yaml:1: new/tool:latest is pinned by tag only"
mutate "YAML block scalar behind node properties" e2e/sub/pod.yaml $'    image: &probe >-\n      new/tool:latest' \
	"e2e/sub/pod.yaml:2: new/tool:latest is pinned by tag only"
mutate "YAML flow mapping broken between the key and its value" e2e/sub/pod.yaml $'  containers: [{name: probe, image:\n      new/tool:latest}]' \
	"e2e/sub/pod.yaml:2: new/tool:latest is pinned by tag only"
# A whole reference under a child key of `image:` (the Helm values spelling
# `image:` / `  ref: x`), of a name no pin knows yet (#1557).
mutate "YAML reference under a child key of image:, a name no pin knows" charts/x/values.yaml $'engine:\n  image:\n    ref: new/tool:latest' \
	"charts/x/values.yaml:8: new/tool:latest is pinned by tag only"
mutate "YAML reference under a child key of image:, flow mapping" charts/x/values.yaml 'engine: {image: {ref: new/tool:latest, pullPolicy: Always}}' \
	"charts/x/values.yaml:6: new/tool:latest is pinned by tag only"
# A mapping reached once as it stands and once, through an alias, as the value
# of an image key is judged the second time too.
mutate "YAML alias of a mapping, under an image key" e2e/sub/pod.yaml $'shared: &shared {ref: new/tool:latest}\nengine:\n  image: *shared' \
	"e2e/sub/pod.yaml:1: new/tool:latest is pinned by tag only"
# Every document of a multi-document file is read, however alike they are.
new_tree "$T"
for _ in 1 2 3 4 5 6; do
	printf -- '---\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - name: echo\n          image: a/b:1.0@%s\n' "$D1"
done >"$T/e2e/sub/many.yaml"
run_check "$T"
if [ "$RC" -eq 0 ] && [[ "$OUT" == "OK: 10 image reference(s) in "* ]]; then
	ok "every document of a multi-document YAML file is read"
else
	bad "multi-document YAML: exit $RC"$'\n'"$OUT"
fi
printf -- '---\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - name: echo\n          image: new/tool:latest\n' >>"$T/e2e/sub/many.yaml"
expect_fail "an image by tag in the last document of a multi-document YAML file" "$T" \
	"e2e/sub/many.yaml:56: new/tool:latest is pinned by tag only"
# A file under a YAML name that no parser can read is a finding: it is not
# passed over, and it is not handed to the line reader as if it were a script.
mutate "a YAML file no parser can read" e2e/sub/broken.yaml $'spec:\n  ports: [1, 2\n  image: 3' \
	"e2e/sub/broken.yaml:3: is not YAML a parser can read"
# What a string of a YAML file holds is text in another language, and is read
# like a here-document: a manifest, a script (its comments are comments), JSON.
mutate "a manifest embedded in a YAML block scalar" e2e/sub/cm.yaml $'data:\n  pod.yaml: |\n    spec:\n      containers:\n        - image: new/tool:latest' \
	"e2e/sub/cm.yaml:5: new/tool:latest is pinned by tag only"
mutate "a script embedded in a YAML block scalar" e2e/sub/cm.yaml $'data:\n  run.sh: |\n    # kubectl run old --image=x/y:0\n    kubectl run q --image=new/tool:latest # why' \
	"e2e/sub/cm.yaml:4: new/tool:latest is pinned by tag only"
mutate "JSON embedded in a YAML string" e2e/sub/cm.yaml $'overrides: \'{"spec":{"containers":[{"name":"r","image":"new/tool:latest"}]}}\'' \
	"e2e/sub/cm.yaml:1: new/tool:latest is pinned by tag only"
mutate "a known image by tag in a YAML string" e2e/sub/cm.yaml 'command: ["docker", "run", "a/b:1.0"]' \
	"e2e/sub/cm.yaml:1: a/b:1.0 is pinned by tag only"
# A `#` inside a YAML string is text: the parser has already taken the comments.
mutate "a known image after a # inside a YAML string" e2e/sub/cm.yaml 'note: "step # 1, then a/b:1.0"' \
	"e2e/sub/cm.yaml:1: a/b:1.0 is pinned by tag only"
# A Helm template is no YAML until it is rendered: it stays with the line
# reader, like YAML embedded in any other language.
mutate "a Helm template is still read line by line" charts/x/templates/d.yaml $'{{- if .Values.x }}\n          image: new/tool:latest\n{{- end }}' \
	"charts/x/templates/d.yaml:2: new/tool:latest is pinned by tag only"
# ...and what the fail-closed line reader charged for it is gone: prose inside a
# YAML string that reads `, image: word` is prose, and a comment line inside an
# embedded script is a comment.
new_tree "$T"
cat >"$T/e2e/sub/pod.yaml" <<EOF
notes: |
  nodes, image: missing
  [image: missing], image: gone
script: |
  # docker run a/b:0.9
  true # a/b:0.8
cut:
  - |
    image: >-
  - |
    new/tool:latest
parts: &parts
  image:
    repository: local
    tag: dev
again: *parts
EOF
run_check "$T"
if [ "$RC" -eq 0 ] && [[ "$OUT" == "OK: 4 image reference(s) in "* ]]; then
	ok "prose and comments inside a YAML string are not read; an image split over repository/tag is not a reference"
else
	bad "prose in a YAML string: exit $RC"$'\n'"$OUT"
fi
# The reader failing is never "no YAML file names an image": exit 2.
for reader in false true; do
	new_tree "$T"
	OUT="$(THIRD_PARTY_ROOT="$T" THIRD_PARTY_YAML_READER="$reader" "$SCRIPT" check 2>&1)"
	RC=$?
	if [ "$RC" -eq 2 ] && [[ "$OUT" == *"YAML reader"* ]] && [[ "$OUT" != *"OK:"* ]]; then
		ok "a YAML reader that fails or answers for no file ($reader) is exit 2, never a pass"
	else
		bad "YAML reader '$reader': exit $RC"$'\n'"$OUT"
	fi
done

# A pin nothing uses.
new_tree "$T"
echo "pin x/unused 1 $D9" >>"$T/scripts/third-party-images.txt"
expect_fail "a pin that no file uses" "$T" "pin x/unused 1 $D9 is used by no file"

# allow: this reference, in this file, and nowhere else.
new_tree "$T"
echo "  image: x/y:1.2" >>"$T/charts/x/values.yaml"
echo "allow charts/x/values.yaml x/y:1.2 # cannot be pinned: reason" >>"$T/scripts/third-party-images.txt"
run_check "$T"
if [ "$RC" -eq 0 ] && [[ "$OUT" == *"1 allowed exception(s)"* ]]; then ok "an allowed exception passes"; else bad "allow: exit $RC"$'\n'"$OUT"; fi
echo "      image: x/y:1.2" >"$T/e2e/sub/pod.yaml"
expect_fail "an exception covers its own file only" "$T" "e2e/sub/pod.yaml:1: x/y:1.2 is pinned by tag only"
new_tree "$T"
echo "allow charts/x/values.yaml x/y:1.2" >>"$T/scripts/third-party-images.txt"
expect_fail "an exception that matches nothing any more" "$T" "'allow charts/x/values.yaml x/y:1.2' matches nothing any more"

# skip: nothing under the prefix is read.
new_tree "$T"
echo "      image: x/y:1.2" >"$T/e2e/sub/pod.yaml"
echo "skip e2e/sub/" >>"$T/scripts/third-party-images.txt"
run_check "$T"
if [ "$RC" -eq 0 ] && [[ "$OUT" == *"1 skipped path(s)"* ]]; then ok "a skipped path is not read"; else bad "skip: exit $RC"$'\n'"$OUT"; fi
new_tree "$T"
echo "skip e2e/gone/" >>"$T/scripts/third-party-images.txt"
expect_fail "a skip that matches no file any more" "$T" "'skip e2e/gone/' matches no file any more"
# ...and a prefix that holds only files the scan never reads excuses nothing.
new_tree "$T"
mkdir -p "$T/e2e/docs"
echo 'Use `image: nginx`.' >"$T/e2e/docs/README.md"
echo 'patterns = ["image: x"]' >"$T/e2e/docs/BUILD.bazel"
echo 'want="image: x/y:1"' >"$T/e2e/docs/pin_test.sh"
echo "skip e2e/docs/" >>"$T/scripts/third-party-images.txt"
expect_fail "a skip over files the scan never reads" "$T" "'skip e2e/docs/' matches no file any more"

# A scan that reads nothing must not pass.
rm -rf "$TMP/empty"
mkdir -p "$TMP/empty/scripts"
: >"$TMP/empty/scripts/third-party-images.txt"
expect_fail "a tree with nothing to scan" "$TMP/empty" "nothing was checked"

# A malformed inventory is an error (2), not a finding.
for line in "pin a/b 1.0" "pin a/b 1.0 sha256:abc" "pin A/B 1.0 $D1" "pin a/b 1.0 $D1 extra" "allow only-a-path" "frobnicate x"; do
	new_tree "$T"
	echo "$line" >>"$T/scripts/third-party-images.txt"
	run_check "$T"
	if [ "$RC" -eq 2 ] && [[ "$OUT" == *"scripts/third-party-images.txt:5:"* ]]; then
		ok "inventory line '$line' is refused with its line number"
	else
		bad "inventory line '$line': exit $RC"$'\n'"$OUT"
	fi
done
new_tree "$T"
echo "pin a/b 1.0 $D1" >>"$T/scripts/third-party-images.txt"
run_check "$T"
if [ "$RC" -eq 2 ] && [[ "$OUT" == *"listed twice"* ]]; then ok "a pin listed twice is refused"; else bad "duplicate pin: exit $RC"$'\n'"$OUT"; fi
# One tag, two digests (a refresh that moved the inventory and only some of the
# references): refused even though both digests are in use.
new_tree "$T"
echo "pin a/b 1.0 $D9" >>"$T/scripts/third-party-images.txt"
echo "      image: a/b:1.0@$D9" >"$T/e2e/sub/pod.yaml"
run_check "$T"
if [ "$RC" -eq 2 ] && [[ "$OUT" == *"scripts/third-party-images.txt:5: a/b:1.0 is already pinned"* ]]; then
	ok "one name:tag under two digests is refused"
else
	bad "one tag, two digests: exit $RC"$'\n'"$OUT"
fi

# A digest with anything after its 64 hex digits is not that digest.
mutate "a known image whose digest has a suffix" e2e/sub/pod.yaml "      image: a/b:1.0@$D1-x" \
	"e2e/sub/pod.yaml:1: a/b:1.0@$D1-x" "is not sha256:<64 hex>"
mutate "a known image whose digest has a suffix, behind no image key" e2e/run.sh "docker run --rm a/b:1.0@$D1.bak true" \
	"e2e/run.sh:12: a/b:1.0@$D1.bak" "is not sha256:<64 hex>"
mutate "an unknown image whose digest has a suffix" e2e/sub/pod.yaml "      image: x/y:1.2@$D9-x" \
	"e2e/sub/pod.yaml:1: x/y:1.2@$D9-x" "is not sha256:<64 hex>"

# In a git work tree the scan follows git: an untracked file counts before it is
# committed, an ignored one does not.
if command -v git >/dev/null 2>&1; then
	new_tree "$T"
	git -C "$T" init -q 2>/dev/null
	echo "ignored/" >"$T/e2e/.gitignore"
	mkdir -p "$T/e2e/ignored"
	echo "      image: x/y:1.2" >"$T/e2e/ignored/pod.yaml"
	run_check "$T"
	if [ "$RC" -eq 0 ]; then ok "git: an ignored file is not read"; else bad "git ignored: exit $RC"$'\n'"$OUT"; fi
	echo "      image: x/y:1.2" >"$T/e2e/sub/new.yaml"
	expect_fail "git: an untracked file is read before it is committed" "$T" "e2e/sub/new.yaml:1: x/y:1.2 is pinned by tag only"
	rm -rf "$T/.git"
else
	echo "skip git cases: no git on PATH"
fi

# The checked-in inventory parses (exit 1 for unused pins here, never 2).
new_tree "$T"
OUT="$(THIRD_PARTY_ROOT="$T" THIRD_PARTY_INVENTORY="$REAL_INVENTORY" "$SCRIPT" check 2>&1)"
RC=$?
if [ "$RC" -eq 1 ] && [[ "$OUT" == *"is used by no file"* ]]; then ok "the checked-in inventory parses"; else bad "real inventory: exit $RC"$'\n'"$OUT"; fi

# --- 1b. the base-image pulls in the Bazel module files (#1477) ------------------
#
# The base image of every image this repository builds is named in Starlark, as
# the attributes of a pull and never as one reference: a rules_img
# `pull(registry =, repository =, tag =, digest =)` in MODULE.bazel and a
# rules_oci `oci.pull(image =, digest =)` in proxy/MODULE.bazel. `check` reads
# those two constructs and judges what they name like any other reference.

# module_tree <dir>: the clean tree plus both module files, pinned and listed.
module_tree() {
	local t="$1"
	new_tree "$t"
	mkdir -p "$t/proxy"
	cat >>"$t/scripts/third-party-images.txt" <<EOF
pin gcr.io/base/static nonroot $D9
pin gcr.io/base/cc nonroot $D1
EOF
	cat >"$t/MODULE.bazel" <<EOF
bazel_dep(name = "rules_img", version = "1.0")

# pull(
#     digest = "sha256:in-a-comment",
# )
pull = use_repo_rule("@rules_img//img:pull.bzl", "pull")

pull(
    name = "base_static",
    digest = "$D9",  # the index
    layer_handling = "lazy",
    registry = "gcr.io",
    repository = "base/static",
    tag = "nonroot",
)

http_file(
    name = "not_an_image",
    digest = "sha256:zzz",
)
EOF
	cat >"$t/proxy/MODULE.bazel" <<EOF
oci = use_extension("@rules_oci//oci:extensions.bzl", "oci")
oci.pull(
    name = "base_cc",
    digest = "$D1",
    image = "gcr.io/base/cc",
    platforms = [
        "linux/amd64",
        "linux/arm64/v8",
    ],
)
use_repo(oci, "base_cc")
EOF
}

M="$TMP/module-tree"
module_tree "$M"
run_check "$M"
if [ "$RC" -eq 0 ] && [[ "$OUT" == "OK: 6 image reference(s) in "*": 5 pinned image(s), "* ]]; then
	ok "module files: a rules_img pull and a rules_oci pull that agree with the inventory pass"
else
	bad "module tree: exit $RC"$'\n'"$OUT"
fi
OUT="$(THIRD_PARTY_ROOT="$M" "$SCRIPT" list 2>&1)"
if [[ "$OUT" == *"gcr.io/base/static:nonroot@$D9"$'\n'"    MODULE.bazel:10"* ]] &&
	[[ "$OUT" == *"gcr.io/base/cc:nonroot@$D1"$'\n'"    proxy/MODULE.bazel:4"* ]]; then
	ok "module files: list names the module file and the digest's line for each pull"
else
	bad "module list:"$'\n'"$OUT"
fi

# module_edit <name> <file> <sed program> <needle>...: one edit to one module
# file of a fresh module tree makes check fail.
module_edit() {
	local name="$1" file="$2" prog="$3"
	shift 3
	module_tree "$M"
	sed -i -e "$prog" "$M/$file"
	expect_fail "$name" "$M" "$@"
}

module_edit "MODULE.bazel: a digest the inventory does not list" MODULE.bazel "s/$D9/$D2/" \
	"MODULE.bazel:10: gcr.io/base/static:nonroot@$D2 is not in scripts/third-party-images.txt" \
	"pin gcr.io/base/static nonroot $D9 is used by no file"
module_edit "MODULE.bazel: a tag that disagrees with the inventory" MODULE.bazel 's/tag = "nonroot"/tag = "latest"/' \
	"MODULE.bazel:10: gcr.io/base/static:latest@$D9 says tag latest, but scripts/third-party-images.txt lists this digest as gcr.io/base/static:nonroot"
module_edit "MODULE.bazel: a pull by tag only" MODULE.bazel '/^    digest = /d' \
	"MODULE.bazel:8: gcr.io/base/static:nonroot is pinned by tag only"
module_edit "MODULE.bazel: another repository" MODULE.bazel 's|repository = "base/static"|repository = "base/other"|' \
	"MODULE.bazel:10: gcr.io/base/other:nonroot@$D9 is not in scripts/third-party-images.txt"
module_edit "MODULE.bazel: a pull with no repository" MODULE.bazel '/^    repository = /d' \
	"MODULE.bazel:8: this pull names no repository"
module_edit "MODULE.bazel: a digest that is not a string literal" MODULE.bazel "s/digest = \"$D9\"/digest = BASE_DIGEST/" \
	"MODULE.bazel:10: digest is not a plain string literal"
module_edit "MODULE.bazel: a pull written on one line" MODULE.bazel 's/^pull($/pull(name = "x", digest = "y")/' \
	"MODULE.bazel:8: a pull the reader cannot follow"
module_edit "MODULE.bazel: the pull rule under another name is still read" MODULE.bazel \
	"s/^pull = use_repo_rule/img_pull = use_repo_rule/; s/^pull(\$/img_pull(/; s/$D9/$D2/" \
	"MODULE.bazel:10: gcr.io/base/static:nonroot@$D2 is not in"
module_edit "MODULE.bazel: a pull rule bound in a way the reader does not know" MODULE.bazel \
	's|^pull = use_repo_rule("@rules_img//img:pull.bzl", "pull")|pull = use_repo_rule(\n    "@rules_img//img:pull.bzl",\n    "pull",\n)|' \
	"MODULE.bazel: names an image pull rule" "no pull was read"
module_edit "proxy/MODULE.bazel: an extension bound in a way the reader does not know" proxy/MODULE.bazel \
	's|^oci = use_extension("@rules_oci//oci:extensions.bzl", "oci")|oci = use_extension(\n    "@rules_oci//oci:extensions.bzl",\n    "oci",\n)|' \
	"proxy/MODULE.bazel: names an image pull rule (@rules_oci//oci:extensions.bzl) but no pull was read"
module_edit "proxy/MODULE.bazel: a digest the inventory does not list" proxy/MODULE.bazel "s/$D1/$D2/" \
	"proxy/MODULE.bazel:4: gcr.io/base/cc@$D2 is not in scripts/third-party-images.txt"
module_edit "proxy/MODULE.bazel: an oci.pull with no digest" proxy/MODULE.bazel '/^    digest = /d' \
	"proxy/MODULE.bazel:2: gcr.io/base/cc names no tag and no digest"
module_edit "proxy/MODULE.bazel: an oci.pull by tag" proxy/MODULE.bazel 's/^    digest = .*/    tag = "latest",/' \
	"proxy/MODULE.bazel:2: gcr.io/base/cc:latest is pinned by tag only"
module_edit "proxy/MODULE.bazel: an oci.pull with no image" proxy/MODULE.bazel '/^    image = /d' \
	"proxy/MODULE.bazel:2: this pull names no image"

# A comment that names the rule is not a pull that went unread.
module_tree "$M"
echo '# The base was once pulled with use_extension("@rules_oci//oci:extensions.bzl", "oci").' >"$M/proxy/MODULE.bazel"
sed -i -e '/^pin gcr.io\/base\/cc /d' "$M/scripts/third-party-images.txt"
run_check "$M"
if [ "$RC" -eq 0 ]; then
	ok "module files: a comment that names a pull rule is not read"
else
	bad "module comment: exit $RC"$'\n'"$OUT"
fi

# A pin only a module file's comment still names is used by nothing.
module_tree "$M"
sed -i -e "s/^    digest = \"$D9\".*/    # digest = \"$D9\",/" "$M/MODULE.bazel"
expect_fail "MODULE.bazel: a commented-out attribute is not read" "$M" \
	"MODULE.bazel:8: gcr.io/base/static:nonroot is pinned by tag only" \
	"pin gcr.io/base/static nonroot $D9 is used by no file"

# --- 1c. include()d module files and other pulling rules (#1571) -----------------
#
# A module file may `include()` another, and a pull moved there is still a pull:
# the reader follows the include, from the workspace the module file is the root
# of. What it cannot follow, and a rule of an image ruleset it does not know, is
# a finding.

# include_tree <dir>: the module tree with the root pull moved into a file the
# root module file includes, through a second include.
include_tree() {
	local t="$1"
	module_tree "$t"
	mkdir -p "$t/bazel/images" "$t/proxy/bazel"
	{
		echo '# The base images.'
		sed -n '/^pull = use_repo_rule/,/^)$/p' "$t/MODULE.bazel"
	} >"$t/bazel/images/base.MODULE.bazel"
	echo 'include("//bazel/images:base.MODULE.bazel")  # the pulls' >"$t/images.MODULE.bazel"
	sed -i -e '/^pull = use_repo_rule/,/^)$/d' "$t/MODULE.bazel"
	echo 'include("//:images.MODULE.bazel")' >>"$t/MODULE.bazel"
}
include_tree "$M"
run_check "$M"
if [ "$RC" -eq 0 ] && [[ "$OUT" == "OK: 6 image reference(s) in "*": 5 pinned image(s), "* ]]; then
	ok "include(): a pull in an included module file, two includes deep, is read"
else
	bad "include tree: exit $RC"$'\n'"$OUT"
fi
OUT="$(THIRD_PARTY_ROOT="$M" "$SCRIPT" list 2>&1)"
if [[ "$OUT" == *"gcr.io/base/static:nonroot@$D9"$'\n'"    bazel/images/base.MODULE.bazel:6"* ]]; then
	ok "include(): list names the included file and the digest's line"
else
	bad "include list:"$'\n'"$OUT"
fi
include_tree "$M"
sed -i -e "s/$D9/$D2/" "$M/bazel/images/base.MODULE.bazel"
expect_fail "include(): a digest the inventory does not list, in an included file" "$M" \
	"bazel/images/base.MODULE.bazel:6: gcr.io/base/static:nonroot@$D2 is not in scripts/third-party-images.txt" \
	"pin gcr.io/base/static nonroot $D9 is used by no file"
# An include of the //proxy workspace is a path under proxy/.
include_tree "$M"
{
	echo 'oci = use_extension("@rules_oci//oci:extensions.bzl", "oci")'
	sed -n '/^oci.pull(/,/^)$/p' "$M/proxy/MODULE.bazel" | sed -e "s/$D1/$D2/"
} >"$M/proxy/bazel/base.MODULE.bazel"
echo 'include("//bazel:base.MODULE.bazel")' >"$M/proxy/MODULE.bazel"
expect_fail "include(): an included file of the proxy workspace is read from under proxy/" "$M" \
	"proxy/bazel/base.MODULE.bazel:4: gcr.io/base/cc@$D2 is not in scripts/third-party-images.txt"
# Two files that include each other are each read once.
include_tree "$M"
echo 'include("//:images.MODULE.bazel")' >>"$M/bazel/images/base.MODULE.bazel"
run_check "$M"
if [ "$RC" -eq 0 ] && [[ "$OUT" == "OK: 6 image reference(s) in "* ]]; then
	ok "include(): a cycle of includes ends"
else
	bad "include cycle: exit $RC"$'\n'"$OUT"
fi
# An include() the reader cannot follow is a finding at its line.
include_edit() { # <name> <include line> <needle>...
	local name="$1" line="$2"
	shift 2
	include_tree "$M"
	sed -i -e '$d' "$M/MODULE.bazel"
	printf '%s\n' "$line" >>"$M/MODULE.bazel"
	expect_fail "$name" "$M" "$@"
}
include_edit "include(): a file that is not there" 'include("//bazel:gone.MODULE.bazel")' \
	"MODULE.bazel:11: include() names bazel/gone.MODULE.bazel, which is not a file"
include_edit "include(): a label that is not a string literal" 'include(IMAGES)' \
	"MODULE.bazel:11: an include() the reader cannot follow"
include_edit "include(): broken across lines" $'include(\n    "//:images.MODULE.bazel",\n)' \
	"MODULE.bazel:11: an include() the reader cannot follow"
include_edit "include(): behind another statement" 'x = 1; include("//:images.MODULE.bazel")' \
	"MODULE.bazel:11: an include() the reader cannot follow"
include_edit "include(): a label out of the workspace" 'include("//../other:images.MODULE.bazel")' \
	"MODULE.bazel:11: an include() the reader cannot follow"
# ...and a comment or an attribute that says include( is not an include.
include_tree "$M"
cat >>"$M/MODULE.bazel" <<'EOF'
# include("//bazel:gone.MODULE.bazel") was here once
archive_override(
    module_name = "x",
    patch_cmds = ["sed -i 's/#include(a)//' x.h"],
)
EOF
run_check "$M"
if [ "$RC" -eq 0 ]; then
	ok "include(): a comment and a string that say include( are not read"
else
	bad "include in a comment: exit $RC"$'\n'"$OUT"
fi

# A rule of an image ruleset the reader does not know pulls an image the check
# never sees: a finding, whether or not the file also holds a pull it can read.
module_edit "MODULE.bazel: a repository rule of an image ruleset the reader does not know" MODULE.bazel \
	'$a oci_pull = use_repo_rule("@rules_oci//oci:pull.bzl", "oci_pull")' \
	"MODULE.bazel:21: names @rules_oci//oci:pull.bzl, a rule of an image ruleset the reader does not know"
module_edit "proxy/MODULE.bazel: an extension of an image ruleset the reader does not know" proxy/MODULE.bazel \
	'$a images = use_extension("@rules_img//img:extensions.bzl", "images")' \
	"proxy/MODULE.bazel:12: names @rules_img//img:extensions.bzl, a rule of an image ruleset the reader does not know"
module_edit "MODULE.bazel: rules_docker's container_pull" MODULE.bazel \
	'$a container_pull = use_repo_rule("@io_bazel_rules_docker//container:pull.bzl", "container_pull")' \
	"MODULE.bazel:21: names @io_bazel_rules_docker//container:pull.bzl, a rule of an image ruleset the reader does not know"
# The known .bzl under another symbol, next to a pull that IS read: the file is
# not excused by its one readable pull.
module_edit "MODULE.bazel: a second binding of the pull rule that the reader cannot read" MODULE.bazel \
	'$a other = use_repo_rule("@rules_img//img:pull.bzl", "pull_tarball")' \
	"MODULE.bazel:21: names @rules_img//img:pull.bzl in a way the reader does not know"
# A dependency on an image ruleset is not a pull.
module_tree "$M"
echo 'bazel_dep(name = "rules_oci", version = "2.0")' >>"$M/MODULE.bazel"
run_check "$M"
if [ "$RC" -eq 0 ]; then
	ok "module files: a bazel_dep on an image ruleset is not a pull"
else
	bad "bazel_dep on rules_oci: exit $RC"$'\n'"$OUT"
fi

# --- 2. the registry -----------------------------------------------------------

FAKE="$TMP/fake"
BIN="$TMP/bin"
mkdir -p "$BIN"
# The fake registry. Serves $FAKE/<host>/<repo>/manifests/<tag> and
# .../tags/<page>, per-host behaviour from $FAKE/<host>/mode:
#   open    no authentication
#   token   401 + a Bearer challenge until the anonymous token is presented
#   closed  401 + a challenge whose token endpoint refuses
#   down    HTTP 503
# Logs every call (arguments and all) to $FAKE/calls.
cat >"$BIN/curl" <<'FAKECURL'
#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >>"$FAKE/calls"
out="" hdr="" auth="" url=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	-o) out="$2"; shift ;;
	-D) hdr="$2"; shift ;;
	-w | --max-time | --retry | --proto | --proto-redir) shift ;;
	-H) case "$2" in Authorization:*) auth="${2#Authorization: }" ;; esac; shift ;;
	-*) ;;
	*) url="$1" ;;
	esac
	shift
done
rest="${url#https://}"
host="${rest%%/*}"
path="/${rest#*/}"
reply() { # <code> [<header>...]; body on stdin
	local code="$1"
	shift
	{
		printf 'HTTP/2 %s\r\n' "$code"
		for h in "$@"; do printf '%s\r\n' "$h"; done
		printf '\r\n'
	} >"$hdr"
	cat >"$out"
	printf '%s' "$code"
	exit 0
}
case "$host" in
auth.*)
	registry="${host#auth.}"
	[ "$(cat "$FAKE/$registry/mode" 2>/dev/null)" = closed ] && reply 401 <<<'{"errors":[]}'
	scope="${path#*scope=}"
	reply 200 <<<"{\"token\":\"anon:${scope}\"}"
	;;
esac
mode="$(cat "$FAKE/$host/mode" 2>/dev/null || echo open)"
[ "$mode" = unreachable ] && exit 6
[ "$mode" = down ] && reply 503 <<<'Service Unavailable'
repo="${path#/v2/}"
case "$repo" in
*/manifests/*) kind=manifests; item="${repo##*/manifests/}"; repo="${repo%/manifests/*}" ;;
*/tags/list*) kind=tags; item="${repo##*/tags/list}"; repo="${repo%/tags/list*}" ;;
*) reply 404 <<<'not found' ;;
esac
if [ "$mode" = token ] || [ "$mode" = closed ]; then
	if [ "$auth" != "Bearer anon:repository:${repo}:pull" ]; then
		reply 401 <<<'{"errors":[{"code":"UNAUTHORIZED"}]}' \
			"Www-Authenticate: Bearer realm=\"https://auth.${host}/token\",service=\"${host}\",scope=\"repository:${repo}:pull\""
	fi
fi
if [ "$kind" = manifests ]; then
	f="$FAKE/$host/$repo/manifests/$item"
	[ -f "$f" ] || reply 404 <<<'{"errors":[{"code":"MANIFEST_UNKNOWN"}]}'
	digest="sha256:$(sha256sum <"$f" | awk '{print $1}')"
	[ -f "$f.digest" ] && digest="$(cat "$f.digest")"
	reply 200 "Docker-Content-Digest: $digest" <"$f"
fi
page=1
case "$item" in *"last="*) page=2 ;; esac
f="$FAKE/$host/$repo/tags/$page"
[ -f "$f" ] || reply 404 <<<'{"errors":[]}'
if [ "$page" = 1 ] && [ -f "$FAKE/$host/$repo/tags/2" ]; then
	reply 200 "Link: </v2/${repo}/tags/list?n=1000&last=x>; rel=\"next\"" <"$f"
fi
reply 200 <"$f"
FAKECURL
chmod +x "$BIN/curl"

index() { # <os/arch>...: an image index naming those platforms
	local p first=1
	printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":['
	for p in "$@"; do
		[ "$first" = 1 ] || printf ','
		first=0
		printf '{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%064d","size":1,"platform":{"os":"%s","architecture":"%s"}}' "$RANDOM" "${p%/*}" "${p#*/}"
	done
	# An attestation manifest, as buildx pushes them: not a platform.
	printf ',{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%064d","size":1,"platform":{"os":"unknown","architecture":"unknown"}}]}\n' 7
}
serve() { # <host> <repo> <tag> <os/arch>...: prints the digest it serves
	local dir="$FAKE/$1/$2/manifests" tag="$3"
	shift 3
	mkdir -p "$dir"
	index "$@" >"$dir/$tag"
	echo "sha256:$(sha256sum <"$dir/$tag" | awk '{print $1}')"
}
tags() { # <host> <repo> <page> <tag>...
	local dir="$FAKE/$1/$2/tags" page="$3"
	shift 3
	mkdir -p "$dir"
	printf '%s\n' "$@" | "$JQ" -R . | "$JQ" -s '{name: "x", tags: .}' >"$dir/$page"
}

# A home directory holding credentials the tool must never read or send.
SENTINEL="s3cr3t-must-not-leak"
mkdir -p "$TMP/home/.docker"
echo "{\"auths\":{\"registry-1.docker.io\":{\"auth\":\"$SENTINEL\"}}}" >"$TMP/home/.docker/config.json"
echo "machine registry-1.docker.io login u password $SENTINEL" >"$TMP/home/.netrc"
echo "user = u:$SENTINEL" >"$TMP/home/.curlrc"

INV="$TMP/inventory.txt"
registry() { # outdated|resolve ...: sets RC and OUT
	OUT="$(HOME="$TMP/home" DOCKER_CONFIG="$TMP/home/.docker" PATH="$BIN:$PATH" FAKE="$FAKE" \
		THIRD_PARTY_ROOT="$TMP/tree" THIRD_PARTY_INVENTORY="$INV" "$SCRIPT" "$@" 2>&1)"
	RC=$?
}
says() { # <name> <want rc> <needle>...
	local name="$1" want="$2" needle
	shift 2
	if [ "$RC" -ne "$want" ]; then
		bad "$name: exit $RC, want $want"$'\n'"$OUT"
		return
	fi
	for needle in "$@"; do
		case "$OUT" in *"$needle"*) ;; *)
			bad "$name: output lacks '$needle'"$'\n'"$OUT"
			return
			;;
		esac
	done
	ok "$name"
}
# The scheduled report (scripts/third-party-images-report.sh, #1478) reads what
# `outdated` prints: `reported` runs it as a dry run over the REAL `outdated`
# and this fake registry, so the two cannot drift apart on a line's wording or
# an exit status (its own test plays `outdated` from canned text).
reported() { # sets RC and OUT
	OUT="$(HOME="$TMP/home" DOCKER_CONFIG="$TMP/home/.docker" PATH="$BIN:$PATH" FAKE="$FAKE" \
		THIRD_PARTY_ROOT="$TMP/tree" THIRD_PARTY_INVENTORY="$INV" "$HERE/third-party-images-report.sh" --dry-run 2>&1)"
	RC=$?
}
reset_fake() {
	rm -rf "$FAKE"
	mkdir -p "$FAKE"
	: >"$FAKE/calls"
}

BOTH=(linux/amd64 linux/arm64)

# Every pin current: Docker Hub (token), a library/ image, gcr.io (open).
reset_fake
mkdir -p "$FAKE/registry-1.docker.io" "$FAKE/gcr.io"
echo token >"$FAKE/registry-1.docker.io/mode"
d_curl="$(serve registry-1.docker.io curlimages/curl 8.22.0 "${BOTH[@]}")"
d_busy="$(serve registry-1.docker.io library/busybox 1.36 "${BOTH[@]}" linux/s390x)"
d_echo="$(serve gcr.io proj/echo v1 "${BOTH[@]}")"
cat >"$INV" <<EOF
pin curlimages/curl 8.22.0 $d_curl
pin busybox 1.36 $d_busy
pin gcr.io/proj/echo v1 $d_echo
EOF
registry outdated
says "outdated: every pin current, through the anonymous token dance" 0 \
	"current  curlimages/curl:8.22.0  $d_curl" "current  busybox:1.36  $d_busy" \
	"current  gcr.io/proj/echo:v1  $d_echo" "3 checked: 0 behind, 0 could not be checked."
reported
says "report: reads the real answer when every pin is current" 0 "DRY RUN: every pin is current"
if grep -q 'https://registry-1.docker.io/v2/library/busybox/manifests/1.36' "$FAKE/calls" &&
	grep -q 'https://auth.registry-1.docker.io/token?service=registry-1.docker.io&scope=repository:curlimages/curl:pull' "$FAKE/calls" &&
	grep -q 'https://gcr.io/v2/proj/echo/manifests/v1' "$FAKE/calls"; then
	ok "names resolve the way a runtime reads them (Docker Hub, library/, another registry)"
else
	bad "registry URLs:"$'\n'"$(cat "$FAKE/calls")"
fi

# The tag was re-pushed.
d_new="$(serve registry-1.docker.io curlimages/curl 8.22.0 "${BOTH[@]}" linux/riscv64)"
registry outdated
says "outdated: a re-pushed tag is MOVED, with the new digest, exit 1" 1 \
	"MOVED    curlimages/curl:8.22.0  pinned $d_curl, the tag now points at $d_new" "3 checked: 1 behind, 0 could not be checked."
reported
says "report: reads a real MOVED line, its summary and its exit status" 0 \
	"DRY RUN: would open or update the issue" "  | MOVED    curlimages/curl:8.22.0  pinned $d_curl, the tag now points at $d_new"
registry outdated gcr.io/proj/echo
says "outdated <name>: only that pin" 0 "1 checked: 0 behind"
registry outdated no/such
says "outdated <name>: an unknown name is an error" 2 "no pin matches no/such"

# An index short of an architecture, and a single-platform manifest.
serve gcr.io proj/echo v1 linux/amd64 >/dev/null
registry outdated gcr.io/proj/echo
says "outdated: says when the index no longer lists an architecture" 1 "note: the index does not list linux/arm64"
echo '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}' >"$FAKE/gcr.io/proj/echo/manifests/v1"
registry outdated gcr.io/proj/echo
says "outdated: says when the tag is a single-platform manifest" 1 "note: a single-platform manifest, not a multi-arch index"

# The same, when the PIN ITSELF is the short index (the digest has not moved):
# it is not "current", and the exit is 1 for the gap alone.
d_short="$(serve gcr.io proj/echo v1 linux/amd64)"
echo "pin gcr.io/proj/echo v1 $d_short" >"$INV"
registry outdated gcr.io/proj/echo
if [[ "$OUT" == *"current  gcr.io"* ]]; then
	bad "outdated: a pinned index short of an architecture was reported current"$'\n'"$OUT"
else
	says "outdated: a pin whose own index lacks an architecture is behind, exit 1" 1 \
		"NOT-MULTI-ARCH gcr.io/proj/echo:v1  $d_short: the index does not list linux/arm64" "1 checked: 1 behind, 0 could not be checked."
fi
reported
says "report: reads a real NOT-MULTI-ARCH line as a pin behind" 0 \
	"DRY RUN: would open or update the issue" "  | NOT-MULTI-ARCH gcr.io/proj/echo:v1  $d_short"

# A registry that does not answer with a manifest is never "current".
not_current() { # <name> <needle>
	registry outdated gcr.io/proj/echo
	if [[ "$OUT" == *"current  gcr.io"* ]]; then
		bad "$1: reported current"$'\n'"$OUT"
	else
		says "$1" 2 "ERROR    gcr.io/proj/echo:v1" "$2" "1 checked: 0 behind, 1 could not be checked."
	fi
}
d_echo="$(serve gcr.io proj/echo v1 "${BOTH[@]}")"
echo "pin gcr.io/proj/echo v1 $d_echo" >"$INV"
echo down >"$FAKE/gcr.io/mode"
not_current "outdated: a 503 is an error, exit 2" "answered HTTP 503"
reported
says "report: a real registry error is a warning and exit 0, never 'current'" 0 \
	"::warning title=third-party image pin not checked::ERROR    gcr.io/proj/echo:v1" "DRY RUN: 1 pin(s) could not be checked"
echo unreachable >"$FAKE/gcr.io/mode"
not_current "outdated: no connection is an error" "no answer from gcr.io"
echo closed >"$FAKE/gcr.io/mode"
not_current "outdated: a repository that hands out no anonymous token is an error" "gave no anonymous pull token"
echo open >"$FAKE/gcr.io/mode"
echo '<html>502 Bad Gateway</html>' >"$FAKE/gcr.io/proj/echo/manifests/v1"
not_current "outdated: a 200 that is not a manifest is an error" "something that is not a manifest"
serve gcr.io proj/echo v1 "${BOTH[@]}" >/dev/null
echo "$d_echo" >"$FAKE/gcr.io/proj/echo/manifests/v1.digest"
not_current "outdated: a digest header that disagrees with the body is an error" "hashes to"
rm -f "$FAKE/gcr.io/proj/echo/manifests/v1.digest"
rm -f "$FAKE/gcr.io/proj/echo/manifests/v1"
not_current "outdated: a tag that is gone is an error" "answered HTTP 404"

# Newer tags: the pinned tag's shape only, version order, across two pages.
d_echo="$(serve gcr.io proj/echo 8.22.0 "${BOTH[@]}")"
echo "pin gcr.io/proj/echo 8.22.0 $d_echo" >"$INV"
tags gcr.io proj/echo 1 latest 8.9.0 8.22.0 8.23.0 8.23.0-rc1 8.100.1 v8.30.0 8.22
tags gcr.io proj/echo 2 9.0.0 8.22.0.1
registry outdated --newer-tags
says "outdated --newer-tags: same shape, version order, both pages" 0 \
	"current  gcr.io/proj/echo:8.22.0" "gcr.io/proj/echo:8.22.0  newer tags: 8.23.0 8.100.1 9.0.0"
for unwanted in latest rc1 v8.30.0 "8.9.0" "8.22.0.1"; do
	[[ "${OUT#*newer tags:}" == *"$unwanted"* ]] && bad "newer tags include $unwanted: $OUT"
done
# `newer` (#1569): the same list as a command of its own, which the scheduled
# report runs next to `outdated`. It asks for tag lists and for no manifest.
: >"$FAKE/calls"
registry newer
says "newer: same shape, version order, both pages, and a summary" 0 \
	"NEWER    gcr.io/proj/echo:8.22.0  3 newer: 8.23.0 8.100.1 9.0.0" "1 checked: 1 with newer tags, 0 could not be listed."
if grep -q '/manifests/' "$FAKE/calls"; then bad "newer asked for a manifest:"$'\n'"$(cat "$FAKE/calls")"; else ok "newer reads tag lists only"; fi
reported
says "report: the newer tags are a section of the dry run, which still says every pin is current" 0 \
	"  | NEWER    gcr.io/proj/echo:8.22.0  3 newer: 8.23.0 8.100.1 9.0.0" "DRY RUN: every pin is current"
registry newer gcr.io/proj/echo
says "newer <name>: only that pin" 0 "1 checked: 1 with newer tags"
registry newer no/such
says "newer <name>: an unknown name is an error" 2 "no pin matches no/such"
tags gcr.io proj/echo 2 9.0.0 9.0.1 9.0.2 9.0.3 9.0.4 9.0.5 9.0.6 9.0.7
registry newer
says "newer: of many newer tags, the count and the newest" 0 \
	"NEWER    gcr.io/proj/echo:8.22.0  10 newer, the newest 8: 9.0.0 9.0.1 9.0.2 9.0.3 9.0.4 9.0.5 9.0.6 9.0.7"
tags gcr.io proj/echo 1 8.22.0 8.1.0
rm -f "$FAKE/gcr.io/proj/echo/tags/2"
registry outdated --newer-tags
if [ "$RC" -eq 0 ] && [[ "$OUT" != *"newer tags"* ]]; then ok "outdated --newer-tags: silent when nothing is newer"; else bad "no newer tag: exit $RC"$'\n'"$OUT"; fi
registry newer
if [ "$RC" -eq 0 ] && [[ "$OUT" != *"NEWER"* ]] && [[ "$OUT" == *"1 checked: 0 with newer tags, 0 could not be listed."* ]]; then
	ok "newer: no line for a pin with nothing newer, exit 0"
else
	bad "newer, nothing newer: exit $RC"$'\n'"$OUT"
fi
rm -rf "$FAKE/gcr.io/proj/echo/tags"
registry outdated --newer-tags
says "outdated --newer-tags: a tag list that cannot be read is an error" 2 "ERROR    gcr.io/proj/echo:8.22.0  tags:"
says "outdated --newer-tags: the error says what the registry answered" 2 \
	"ERROR    gcr.io/proj/echo:8.22.0  tags: https://gcr.io/v2/proj/echo/tags/list?n=1000 answered HTTP 404"
registry newer
says "newer: a tag list that cannot be read is an error with its reason, exit 2" 2 \
	"ERROR    gcr.io/proj/echo:8.22.0  tags: https://gcr.io/v2/proj/echo/tags/list?n=1000 answered HTTP 404" \
	"1 checked: 0 with newer tags, 1 could not be listed."
reported
says "report: a tag list that cannot be read is said in the section and changes nothing else" 0 \
	"The tag list of 1 pin(s) was not read in this run" "DRY RUN: every pin is current"

# A tag is a string: 3.1 is not the pinned 3.10, and 3.2 to 3.9 are older.
d_pause="$(serve gcr.io proj/pause 3.10 "${BOTH[@]}")"
echo "pin gcr.io/proj/pause 3.10 $d_pause" >"$INV"
tags gcr.io proj/pause 1 3.0 3.1 3.2 3.9 3.10 3.11 latest
registry newer
says "newer: tags that are equal as numbers are not the pinned tag" 0 \
	"NEWER    gcr.io/proj/pause:3.10  1 newer: 3.11" "1 checked: 1 with newer tags"
registry outdated --newer-tags
says "outdated --newer-tags: the same" 0 "gcr.io/proj/pause:3.10  newer tags: 3.11 "
[[ "$OUT" == *"newer tags: 3.2"* ]] && bad "outdated --newer-tags lists 3.2 as newer than 3.10: $OUT"
echo "pin gcr.io/proj/echo 8.22.0 $d_echo" >"$INV"

# What is allowed to stay a tag is reported with where its tag points.
cat >"$INV" <<EOF
pin gcr.io/proj/echo 8.22.0 $d_echo
allow charts/x/values.yaml gcr.io/proj/echo:8.22.0
EOF
registry outdated
says "outdated: an unpinned exception is reported with its tag's digest" 0 \
	"UNPINNED gcr.io/proj/echo:8.22.0  (charts/x/values.yaml) the tag points at $d_echo"

# resolve: the inventory line for a tag.
registry resolve gcr.io/proj/echo:8.22.0
says "resolve prints the pin line" 0 "pin gcr.io/proj/echo 8.22.0 $d_echo"
d_one="$(serve gcr.io proj/echo 1-amd linux/amd64)"
registry resolve gcr.io/proj/echo:1-amd
says "resolve: an index short of an architecture is said, exit 1" 1 \
	"# gcr.io/proj/echo:1-amd: the index does not list linux/arm64" "pin gcr.io/proj/echo 1-amd $d_one"
registry resolve gcr.io/proj/echo:nope
says "resolve: a tag that does not exist is an error, and prints no pin" 2 "ERROR gcr.io/proj/echo:nope"
[[ "$OUT" == *"pin gcr.io/proj/echo nope"* ]] && bad "resolve printed a pin for a missing tag"
registry resolve gcr.io/proj/echo
says "resolve: a name without a tag is refused" 2 "is not <name>:<tag>"

# --- 3. no credential, ever ----------------------------------------------------

if grep -q -- "$SENTINEL" "$FAKE/calls"; then
	bad "a credential from the home directory reached a registry call"
else
	ok "no credential from ~/.docker, ~/.netrc or ~/.curlrc reached a call"
fi
if grep -Ev '^-q ' "$FAKE/calls" | grep -q .; then
	bad "a curl call does not start with -q (it would read ~/.curlrc):"$'\n'"$(grep -Ev '^-q ' "$FAKE/calls" | head -3)"
else
	ok "every curl call starts with -q"
fi
# -L follows redirects: neither the request nor a redirect may leave HTTPS, or
# an on-path answer could supply both a manifest and the digest it is checked by.
if grep -Ev -- ' --proto =https --proto-redir =https ' "$FAKE/calls" | grep -q .; then
	bad "a curl call may leave HTTPS (no --proto =https --proto-redir =https):"$'\n'"$(grep -Ev -- ' --proto =https --proto-redir =https ' "$FAKE/calls" | head -3)"
else
	ok "every curl call is held to HTTPS, redirects included"
fi
if grep -Eq -- '(^| )(-u|--user|--netrc|--netrc-file|--netrc-optional|-n|--config|-K|--oauth2-bearer|--cert|-E)( |$)' "$FAKE/calls"; then
	bad "a curl call passes a credential option"
else
	ok "no curl call passes a credential option"
fi
if grep -F 'Authorization:' "$FAKE/calls" | grep -Fv 'Authorization: Bearer anon:' | grep -q .; then
	bad "an Authorization header other than the registry's own anonymous token was sent"
else
	ok "the only Authorization ever sent is the anonymous token the registry handed out"
fi

echo
echo "$CASES cases, $FAILS failed"
[ "$FAILS" -eq 0 ]
