#!/usr/bin/env bash
# Hermetic test of scripts/check-image-digest-stability.sh (#1378): no Bazel and
# no git repository. It runs a copy of the script inside a throwaway tree, with
# a fake `bazel` and a fake `git` on PATH. The fake bazel "builds" two images:
# it runs the workspace-status script it is handed, like the real one, and
# writes each image's digest and each push's tags from what that script
# reported, in the way $FAKE_MODE says.
#
#   1. ok             digests ignore the commit, tags follow it   -> exit 0
#   2. stamped        one image's digest follows the commit        -> exit 1,
#                     naming that image and not the other
#   3. stale-tag      tags keep the first commit on the 2nd build  -> exit 1
#   4. ignore-status  the status never reaches the build           -> exit 1
#                     (the digests agree, and must not be believed)
#   5. no-images      the query returns nothing                    -> exit 1
#   6. no-digest      a build that writes no digest                -> exit 1
#   7. the real status script reports no STABLE_GIT_COMMIT         -> exit 1
#
# Run: bazel test //scripts:check_image_digest_stability_test, or
#      bash scripts/check_image_digest_stability_test.sh
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/check-image-digest-stability.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}

HEAD_SHA="0123456789abcdef0123456789abcdef01234567"

repo="$TMP/repo"
mkdir -p "$repo/scripts" "$repo/bazel" "$TMP/bin"
cp "$SCRIPT" "$repo/scripts/check-image-digest-stability.sh"

# The "real" status script of the throwaway tree.
write_status() { # write_status <line>...
	{
		echo '#!/usr/bin/env bash'
		printf 'echo "%s"\n' "$@"
	} >"$repo/bazel/workspace_status.sh"
}

cat >"$TMP/bin/git" <<EOF
#!/usr/bin/env bash
[ "\$1 \$2" = "rev-parse HEAD" ] || { echo "fake git: unexpected \$*" >&2; exit 1; }
echo "$HEAD_SHA"
EOF

# Fake bazel. State that must survive from the first build to the second (the
# "warm server") lives in $FAKE_STATE.
cat >"$TMP/bin/bazel" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cmd="$1"
shift
pkgs=(agent/cmd/agent other/image)
bin="bazel-out/k8-fastbuild/bin"

case "$cmd" in
query)
	[ "$FAKE_MODE" = no-images ] && exit 0
	for p in "${pkgs[@]}"; do echo "//$p:image_index"; done
	;;
build)
	status=""
	for a in "$@"; do
		case "$a" in --workspace_status_command=*) status="${a#*=}" ;; esac
	done
	[ -n "$status" ] || { echo "fake bazel: build without a status command" >&2; exit 1; }
	commit="$("$status" | sed -n 's/^STABLE_GIT_COMMIT //p')"
	[ -f "$FAKE_STATE/first" ] || echo "$commit" >"$FAKE_STATE/first"
	first="$(cat "$FAKE_STATE/first")"
	for p in "${pkgs[@]}"; do
		mkdir -p "$bin/$p"
		digest="sha256:$(printf '%s' "$p" | sha256sum | cut -d' ' -f1)"
		tag="dev-$commit"
		case "$FAKE_MODE" in
		stamped) [ "$p" = other/image ] && digest="sha256:$(printf '%s' "$p$commit" | sha256sum | cut -d' ' -f1)" ;;
		stale-tag) tag="dev-$first" ;;
		ignore-status) tag="dev" ;;
		esac
		if [ "$FAKE_MODE" = no-digest ]; then
			: >"$bin/$p/image_index_digest"
		else
			echo "$digest" >"$bin/$p/image_index_digest"
		fi
		echo "{\"tags\":[\"dev\",\"$tag\"]}" >"$bin/$p/image_push.json"
	done
	;;
cquery)
	for p in "${pkgs[@]}"; do
		echo "$bin/$p/image_index_digest"
		echo "$bin/$p/image_push.json"
	done
	;;
*)
	echo "fake bazel: unexpected $cmd" >&2
	exit 1
	;;
esac
EOF
chmod +x "$TMP/bin/git" "$TMP/bin/bazel"

run_check() { # run_check <mode> -> rc, output in $TMP/out
	rm -rf "$TMP/state" "$repo/bazel-out"
	mkdir -p "$TMP/state"
	PATH="$TMP/bin:$PATH" FAKE_MODE="$1" FAKE_STATE="$TMP/state" BAZEL=bazel \
		bash "$repo/scripts/check-image-digest-stability.sh" >"$TMP/out" 2>&1
}

expect() { # expect <name> <mode> <want rc> <must contain>... [-- <must not contain>...]
	local name="$1" mode="$2" want="$3" rc absent=0 s
	shift 3
	run_check "$mode"
	rc=$?
	if [ "$rc" -ne "$want" ]; then
		fail "$name: exit $rc, want $want"
		sed 's/^/      /' "$TMP/out"
		return
	fi
	for s in "$@"; do
		if [ "$s" = -- ]; then
			absent=1
		elif [ "$absent" -eq 0 ] && ! grep -qF -- "$s" "$TMP/out"; then
			fail "$name: output lacks '$s'"
			sed 's/^/      /' "$TMP/out"
			return
		elif [ "$absent" -eq 1 ] && grep -qF -- "$s" "$TMP/out"; then
			fail "$name: output must not contain '$s'"
			sed 's/^/      /' "$TMP/out"
			return
		fi
	done
	pass "$name"
}

write_status "STABLE_GIT_COMMIT $HEAD_SHA" "GIT_COMMIT $HEAD_SHA"

expect "stable digests and tags that follow the commit pass" ok 0 \
	"OK: 2 of 2 image digests" "ok: //agent/cmd/agent:image_index sha256:" -- "FAIL"
expect "a digest that follows the commit fails, and only that image is named" stamped 1 \
	"FAIL: //other/image:image_index: the digest depends on the commit" "ok: //agent/cmd/agent:image_index" "1 check(s) failed" \
	-- "FAIL: //agent/cmd/agent"
expect "a tag that keeps the previous commit fails" stale-tag 1 \
	"image_push: the per-commit tag did not follow the commit" -- "OK:"
expect "a status that never reaches the build fails although the digests agree" ignore-status 1 \
	"the workspace status did not reach the build" -- "OK:"
expect "a query that returns no image fails" no-images 1 \
	"it cannot be trusted" -- "OK:"
expect "a build that writes no digest fails" no-digest 1 \
	"no digest read" -- "OK:"

write_status "STABLE_GIT_VERSION v1" "GIT_COMMIT $HEAD_SHA"
expect "a real status script with no STABLE_GIT_COMMIT fails" ok 1 \
	"bazel/workspace_status.sh reports STABLE_GIT_COMMIT ''" -- "OK:"

if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS case(s) failed"
	exit 1
fi
echo "all cases passed"
