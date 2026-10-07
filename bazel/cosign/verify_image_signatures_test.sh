#!/usr/bin/env bash
# //bazel/cosign:verify_image_signatures, run from its own runfiles (#1338).
#
# scripts/registry-lib.sh runs scripts/image-registry.sh WHILE IT IS BEING
# SOURCED (the registry host, the repository list) and again when a credential
# has to be matched to a host. Under `bazel run //bazel/cosign:verify_image_signatures`
# that file was not in the target's runfiles, so every publish-verify log
# began with
#
#   .../scripts/registry-lib.sh: line 36: .../scripts/image-registry.sh: No such file or directory
#
# and the library carried on with no host and an empty repository list. It
# happened not to matter (the verifier takes the host from each ref), which is
# how it stayed unnoticed: a path that started to need those definitions would
# have failed with that line as its only explanation.
#
# This runs the TARGET (not the script beside a checkout, where the file is
# always there) with a fake cosign and a fake curl, and asserts:
#   1. nothing at all on stderr before the script's own first line;
#   2. a whole verification through the wrapper is silent on stderr, and the
#      registry setting is really readable from there: the pull-token request
#      carries the credential, which registry-lib.sh sends only to the host
#      image-registry.sh names;
#   3. the assertion can fail: with the setting taken away, stderr is not empty.
#
# Network-free. Run: bazel test //bazel/cosign:verify_image_signatures_test
# shellcheck disable=SC2016 # single-quoted $names belong to the fakes' own shells.

# --- begin runfiles.bash initialization v3 ---
# shellcheck disable=SC1090
set -uo pipefail
set +e
f=bazel_tools/tools/bash/runfiles/runfiles.bash
source "${RUNFILES_DIR:-/dev/null}/$f" 2>/dev/null ||
	source "$(grep -sm1 "^$f " "${RUNFILES_MANIFEST_FILE:-/dev/null}" | cut -f2- -d' ')" 2>/dev/null ||
	source "$0.runfiles/$f" 2>/dev/null ||
	source "$(grep -sm1 "^$f " "$0.runfiles_manifest" | cut -f2- -d' ')" 2>/dev/null ||
	source "$(grep -sm1 "^$f " "$0.exe.runfiles_manifest" | cut -f2- -d' ')" 2>/dev/null ||
	{
		echo >&2 "ERROR: cannot find $f"
		exit 1
	}
f=
# --- end runfiles.bash initialization v3 ---

target="$(rlocation "${TARGET_RLOCATIONPATH:?set by the BUILD target}")"
reader="$(rlocation "${IMAGE_REGISTRY_SH_RLOCATIONPATH:?set by the BUILD target}")"
bzl="$(rlocation "${REGISTRY_BZL_RLOCATIONPATH:?set by the BUILD target}")"
for x in "$target" "$reader" "$bzl"; do
	[ -n "$x" ] && [ -e "$x" ] || {
		echo "FAIL: missing runfile '${x}'"
		exit 1
	}
done

# The registry and one repository, as the setting itself spells them: the
# credential below goes only to this host, whatever it is on the day.
host="$(IMAGE_REGISTRY_BZL="$bzl" bash "$reader" host)" || host=""
repo="$(IMAGE_REGISTRY_BZL="$bzl" bash "$reader" repo agent)" || repo=""
[ -n "$host" ] && [ -n "$repo" ] || {
	echo "FAIL: could not read the registry setting ${bzl}"
	exit 1
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
index="sha256:$(printf '1%.0s' {1..64})"
kid_a="sha256:$(printf 'a%.0s' {1..64})"
kid_b="sha256:$(printf 'b%.0s' {1..64})"
ref="${host}/${repo}@${index}"

# curl: the pull token and the index, nothing else. Records, per request,
# whether a credential (-u) came with it.
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
url="" cred=anonymous
while [ "$#" -gt 0 ]; do
	case "$1" in
	-u)
		cred="$2"
		shift
		;;
	-H | -o | -D | -w | --retry) shift ;;
	https://*) url="$1" ;;
	esac
	shift
done
printf '%s %s\n' "$cred" "$url" >>"$FAKE_LOG"
case "$url" in
*/manifests/sha256:*) printf '{"manifests":[{"digest":"%s"},{"digest":"%s"}]}\n' "$KID_A" "$KID_B" ;;
*scope=repository:*) printf '{"token":"fake"}\n' ;;
*)
	echo "fake curl: unexpected url ${url}" >&2
	exit 2
	;;
esac
FAKE
cat >"$TMP/bin/cosign" <<'FAKE'
#!/usr/bin/env bash
[ "$1" = verify ] || { echo "fake cosign: unexpected $*" >&2; exit 2; }
echo '[{"critical":{}}]'
FAKE
chmod +x "$TMP/bin/curl" "$TMP/bin/cosign"

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
	sed 's/^/      | /' "$TMP/out" "$TMP/err" 2>/dev/null
}
check() {
	local what="$1"
	shift
	if "$@"; then pass "$what"; else fail "$what"; fi
}

# target <want exit> <name> [VAR=value...] [-- <argument>...]: run the target as
# `bazel run` does, minus the network. rlocation returns an absolute path as it
# is, which is how the fake cosign gets in.
target() {
	local want="$1" name="$2" rc
	shift 2
	local -a vars=()
	while [ "$#" -gt 0 ] && [ "$1" != -- ]; do
		vars+=("$1")
		shift
	done
	[ "$#" -eq 0 ] || shift
	: >"$TMP/curl.log"
	env -u IMAGE_REGISTRY_BZL -u IMAGE_REGISTRY_HOST -u REGISTRY_HOST -u REGISTRY_CREDENTIAL_HOST \
		-u REGISTRY_USERNAME -u REGISTRY_PASSWORD -u BUILD_WORKING_DIRECTORY -u CERT_IDENTITY_REGEXP \
		PATH="$TMP/bin:$PATH" FAKE_LOG="$TMP/curl.log" KID_A="$kid_a" KID_B="$kid_b" \
		COSIGN_RLOCATIONPATH="$TMP/bin/cosign" "${vars[@]}" \
		"$target" "$@" >"$TMP/out" 2>"$TMP/err"
	rc=$?
	if [ "$rc" -ne "$want" ]; then
		fail "$name: exit $rc, wanted $want"
		return 1
	fi
	pass "$name"
}

# 1. Source time. With no ref the script stops at its first check, so all of
#    stderr is what sourcing the library printed plus that one line.
target 2 "no refs: the script's own refusal, exit 2"
check "source time: the refusal is the ONLY line on stderr" \
	test "$(cat "$TMP/err")" = "::error::no image refs to verify"
check "source time: no 'No such file or directory'" \
	bash -c '! grep -q "No such file or directory" "$1" "$2"' _ "$TMP/out" "$TMP/err"

# 2. A whole verification, with a credential. registry-lib.sh sends it only to
#    the host image-registry.sh names -- so the request carrying it shows that
#    the reader and the setting are both where the library looks.
target 0 "one index and its two children verify through the wrapper" \
	REGISTRY_USERNAME=robot REGISTRY_PASSWORD=not-a-real-secret -- "$ref"
check "verify: nothing on stderr" test ! -s "$TMP/err"
check "verify: all three manifests" grep -qxF 'PASS: 3 manifest(s) verified (1 index(es) + their children)' "$TMP/out"
check "verify: the token request carried the credential (the setting names ${host})" \
	grep -qxF "robot:not-a-real-secret https://${host}/$([ "$host" = quay.io ] && echo v2/auth || echo token)?service=${host}&scope=repository:${repo}:pull" "$TMP/curl.log"
check "verify: the index request did not" grep -qxF "anonymous https://${host}/v2/${repo}/manifests/${index}" "$TMP/curl.log"

# 3. The control: take the setting away and the same run is NOT silent, so the
#    two "nothing on stderr" checks above can fail.
target 0 "the same run with an unreadable setting still verifies (the host is in the ref)" \
	REGISTRY_USERNAME=robot REGISTRY_PASSWORD=not-a-real-secret IMAGE_REGISTRY_BZL="$TMP/absent.bzl" -- "$ref"
check "control: stderr is not empty without the setting" test -s "$TMP/err"
check "control: and the credential is withheld (no host to match it to)" \
	bash -c '! grep -q "^robot:" "$1"' _ "$TMP/curl.log"

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
