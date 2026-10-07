#!/usr/bin/env bash
# Test of scripts/go-build-plain.sh against a throwaway git repository and a
# fake `go` that records its arguments: no Go toolchain, no Bazel, no network.
#
# What it pins (#1311):
#   - the build is ONE `go build ./...` and, with --vet, ONE `go vet ./...`:
#     the whole module, no package list, no second invocation for packages that
#     are expected to fail. An exclusion cannot come back without this failing;
#   - a package the go command refuses (the internal-package error is the
#     fixture) fails the script, for build and for vet;
#   - the fixed environment reaches the go command (-mod=readonly,
#     GOTOOLCHAIN=local, GOWORK=off, GOOS=linux, CGO_ENABLED=0);
#   - the wrong Go version, a tree without the materialized proto packages and
#     an unknown flag are refused before anything is built;
#   - a go command that edits go.sum fails the script.
#
# The real thing (the pinned Go over the real module) is `make go-build-plain`
# and the `go` job of .github/workflows/codeql.yaml on every pull request.
#
# Run: bazel test //scripts:go_build_plain_test, or
#      bash scripts/go_build_plain_test.sh
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/go-build-plain.sh"
[ -f "$SCRIPT" ] || {
	echo "FAIL: $SCRIPT not found"
	exit 1
}
command -v git >/dev/null || {
	echo "FAIL: git is not on PATH"
	exit 1
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}
check() {
	local what="$1"
	shift
	if "$@"; then pass "$what"; else fail "$what"; fi
}
# refuse <description> <expected output substring> <command...>: the command
# must fail and say why.
refuse() {
	local what="$1" want="$2" out
	shift 2
	if out="$("$@" 2>&1)"; then
		fail "$what: succeeded, wanted a failure"
	elif [[ "$out" != *"$want"* ]]; then
		fail "$what: failed without '$want': $out"
	else
		pass "$what"
	fi
}

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid

REPO="$TMP/repo"
FAKE="$TMP/fake"
mkdir -p "$REPO" "$FAKE/bin"

# --- the fake go --------------------------------------------------------------
# Logs "<subcommand and arguments>" to $FAKE_DIR/calls and the environment the
# script fixed to $FAKE_DIR/env. $FAKE_DIR/fail-<subcommand>, when present, is
# printed on stderr and makes that subcommand exit 1.
cat >"$FAKE/bin/go" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_DIR/calls"
case "$1" in
env)
	case "$2" in
	GOVERSION) cat "$FAKE_DIR/goversion" ;;
	GOARCH) echo amd64 ;;
	esac
	;;
list) printf 'example.test/mod/a\nexample.test/mod/b\nexample.test/mod/agent/test/x\n' ;;
build | vet)
	echo "GOFLAGS=${GOFLAGS:-} GOTOOLCHAIN=${GOTOOLCHAIN:-} GOWORK=${GOWORK:-} GOOS=${GOOS:-} CGO_ENABLED=${CGO_ENABLED:-}" >>"$FAKE_DIR/env"
	if [ -f "$FAKE_DIR/fail-$1" ]; then
		cat "$FAKE_DIR/fail-$1" >&2
		exit 1
	fi
	if [ -f "$FAKE_DIR/touch-sum" ]; then
		echo "example.test/sneaked v1.0.0 h1:xyz=" >>go.sum
	fi
	;;
*)
	echo "fake go: unexpected command $1" >&2
	exit 64
	;;
esac
EOF
chmod +x "$FAKE/bin/go"
export FAKE_DIR="$FAKE"
PATH="$FAKE/bin:$PATH"

reset_fake() {
	rm -f "$FAKE"/fail-* "$FAKE/touch-sum"
	: >"$FAKE/calls"
	: >"$FAKE/env"
	echo go1.27.1 >"$FAKE/goversion"
}

# --- the throwaway repository -------------------------------------------------
git -C "$REPO" init -q -b main
printf 'module example.test/mod\n\ngo 1.27.1\n' >"$REPO/go.mod"
echo "example.test/dep v1.0.0 h1:abc=" >"$REPO/go.sum"
echo "/.materialized-generated-go" >"$REPO/.gitignore"
git -C "$REPO" add -A
git -C "$REPO" commit -q -m init
: >"$REPO/.materialized-generated-go"

run() { (cd "$REPO" && bash "$SCRIPT" "$@"); }
# calls <subcommand>: the argument lists the fake go saw for it, one per line.
calls() { grep -E "^$1( |\$)" "$FAKE/calls" || true; }

# --- 1. build only: exactly one `go build ./...` ------------------------------
reset_fake
out="$(run 2>&1)" || fail "plain run failed: $out"
check "one go build, over ./... and nothing else" test "$(calls build)" = "build ./..."
check "no go vet without --vet" test -z "$(calls vet)"
check "the summary says nothing is excluded" grep -q 'go build ./... ok: 3 packages, none excluded' <<<"$out"
check "the fixed environment reaches go build" grep -qxF 'GOFLAGS=-mod=readonly GOTOOLCHAIN=local GOWORK=off GOOS=linux CGO_ENABLED=0' "$FAKE/env"

# --- 2. --vet: one build and one vet, both over ./... -------------------------
reset_fake
out="$(run --vet 2>&1)" || fail "--vet run failed: $out"
check "--vet: one go build ./..." test "$(calls build)" = "build ./..."
check "--vet: one go vet ./..." test "$(calls vet)" = "vet ./..."
check "--vet: the summary says nothing is excluded" grep -q 'go vet ./... ok, none excluded' <<<"$out"
# `go list` may be asked what ./... is, but never to compute a subset from: no
# -e (which lists packages that fail to load) and no template.
check "go list is only asked for ./..." test -z "$(calls list | grep -v '^list \./\.\.\.$')"
check "the script names no package of its own" test -z "$(grep -En '(build|vet)_excluded|packages_except|use of internal package' "$SCRIPT")"

# --- 3. a package the go command refuses fails the script ---------------------
reset_fake
cat >"$FAKE/fail-build" <<'EOF'
package example.test/mod/test/x
	test/x/builders.go:9:2: use of internal package example.test/mod/agent/internal/xds/config not allowed
EOF
refuse "an internal-package error fails the build" "use of internal package" run
check "the failed build ran once and was not retried without the package" test "$(calls build)" = "build ./..."
refuse "the same error fails --vet's build" "use of internal package" run --vet
reset_fake
cat >"$FAKE/fail-vet" <<'EOF'
# example.test/mod/test/pool
package example.test/mod/test/pool_test
	test/pool/harness_test.go:27:2: use of internal package example.test/mod/agent/internal/xds/proxy not allowed
EOF
refuse "an internal-package error in a test-only package fails --vet" "use of internal package" run --vet
out="$(run 2>&1)" || fail "a vet-only failure should not fail the plain build: $out"

# --- 4. refusals before anything is built -------------------------------------
reset_fake
echo go1.26.0 >"$FAKE/goversion"
refuse "the wrong Go version is refused" "the repository pins go1.27.1" run
check "nothing was built with the wrong Go" test -z "$(calls build)"
reset_fake
rm "$REPO/.materialized-generated-go"
refuse "an unmaterialized tree is refused" "run scripts/materialize-generated-go.sh first" run
check "nothing was built without the generated packages" test -z "$(calls build)"
: >"$REPO/.materialized-generated-go"
refuse "an unknown argument is rejected" "unknown argument" run --exclude=test/x
refuse "a package argument is rejected" "unknown argument" run ./agent/...

# --- 5. go.mod / go.sum -------------------------------------------------------
reset_fake
: >"$FAKE/touch-sum"
refuse "a go command that edits go.sum fails the script" "changed go.mod or go.sum" run
git -C "$REPO" checkout -q -- go.sum

if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "PASS: go-build-plain"
