#!/usr/bin/env bash
# Hermetic test of scripts/published-chart-refs.sh: which chart references one
# commit's publish is signed and attested under. A throwaway git history holds
# the Chart.yaml files, a fake `curl` plays the registry, a fake `sleep` records
# the waits. No network.
#
# What it pins:
#   - every chart of REGISTRY_CHARTS is listed under its commit tag, by digest;
#   - a chart with a bare version (aether) is listed TWICE, bare tag and commit
#     tag, and a chart that stamps {GIT_COMMIT} itself once;
#   - a tag the registry does not serve fails the script after bounded retries,
#     names the tag, and prints NO partial list (a short list would sign less
#     than it reads as);
#   - an argument that is not a full sha is refused.
#
# Run: bazel test //scripts:published_chart_refs_test
# shellcheck disable=SC2016 # single-quoted $names belong to the fakes' own shells.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/published-chart-refs.sh"
for f in "$SCRIPT" "$HERE/registry-lib.sh" "$HERE/image-registry.sh"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/repo"

cat >"$TMP/registry.bzl" <<'BZL'
IMAGE_REGISTRY = "quay.io"
IMAGE_NAMESPACE = "acme"
IMAGE_NAME_OVERRIDES = {}
CHART_REPOSITORY_PREFIX = "chart-"
SIGNATURE_LAYOUT = "referrer"
BZL

# curl: a pull token for any repository; for a manifest HEAD, the headers with a
# digest that is the sha256 of "<repo>:<tag>" -- unless the tag is listed in
# $FAKE/missing, which is a 404. Every URL asked is appended to $FAKE/curl.log.
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
url=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	-H | -u | -o | -w | -D | --retry) shift ;;
	https://*) url="$1" ;;
	esac
	shift
done
printf '%s\n' "$url" >>"$FAKE/curl.log"
case "$url" in
*/v2/auth\?*) printf '{"token":"fake"}\n' ;;
*/manifests/*)
	rt="${url#https://quay.io/v2/}"
	repo="${rt%%/manifests/*}" tag="${rt##*/manifests/}"
	if grep -qxF -- "${repo}:${tag}" "$FAKE/missing" 2>/dev/null; then
		echo "curl: (22) The requested URL returned error: 404" >&2
		exit 22
	fi
	printf 'HTTP/2 200\r\ndocker-content-digest: sha256:%s\r\n\r\n' \
		"$(printf '%s' "${repo}:${tag}" | sha256sum | cut -d' ' -f1)"
	;;
*)
	echo "fake curl: unexpected url ${url}" >&2
	exit 2
	;;
esac
FAKE
cat >"$TMP/bin/sleep" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$1" >>"$FAKE/sleep.log"
FAKE
chmod +x "$TMP/bin/curl" "$TMP/bin/sleep"

# The history: aether carries a bare version, the others stamp the commit.
(
	cd "$TMP/repo" || exit 1
	git init -q .
	for c in aether crds prober udsecho; do mkdir -p "charts/$c"; done
	printf 'name: aether\nversion: 2.4.12\n' >charts/aether/Chart.yaml
	printf 'name: crds\nversion: "1.3.0-{GIT_COMMIT}"\n' >charts/crds/Chart.yaml
	printf 'name: prober\nversion: "1.0.4-{GIT_COMMIT}"\n' >charts/prober/Chart.yaml
	printf 'name: udsecho\nversion: "0.2.0-{GIT_COMMIT}"\n' >charts/udsecho/Chart.yaml
	git add -A
	git -c user.name=t -c user.email=t@example.invalid commit -q -m charts
) || {
	echo "FAIL: could not build the fixture history"
	exit 1
}
sha="$(git -C "$TMP/repo" rev-parse HEAD)"
digest_of() { printf 'sha256:%s' "$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"; }

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
has() { grep -qxF -- "$1" "$2"; }
says() { grep -qF -- "$1" "$2"; }

# run <want exit> <name> <arg> [missing repo:tag ...]
run() {
	local want="$1" name="$2" arg="$3" rc
	shift 3
	rm -rf "$TMP/fake"
	mkdir -p "$TMP/fake"
	[ "$#" -eq 0 ] || printf '%s\n' "$@" >"$TMP/fake/missing"
	(
		cd "$TMP/repo" || exit 99
		env -u REGISTRY_USERNAME -u REGISTRY_PASSWORD -u IMAGE_REGISTRY_HOST -u REGISTRY_CREDENTIAL_HOST \
			-u CHART_REF_ATTEMPTS -u CHART_REF_INTERVAL \
			"PATH=$TMP/bin:$PATH" "FAKE=$TMP/fake" "IMAGE_REGISTRY_BZL=$TMP/registry.bzl" \
			bash "$SCRIPT" "$arg" >"$TMP/out" 2>"$TMP/err"
	)
	rc=$?
	if [ "$rc" -ne "$want" ]; then fail "$name: exit $rc, wanted $want"; else pass "$name"; fi
}

run 0 "every chart resolves" "$sha"
check "five references: four commit tags and aether's bare tag" test "$(wc -l <"$TMP/out")" = 5
check "aether, commit tag" has "quay.io/acme/chart-aether@$(digest_of "acme/chart-aether:2.4.12-${sha}")" "$TMP/out"
check "aether, bare tag" has "quay.io/acme/chart-aether@$(digest_of "acme/chart-aether:2.4.12")" "$TMP/out"
check "crds, stamped once (no doubled sha)" has "quay.io/acme/chart-crds@$(digest_of "acme/chart-crds:1.3.0-${sha}")" "$TMP/out"
check "prober" has "quay.io/acme/chart-prober@$(digest_of "acme/chart-prober:1.0.4-${sha}")" "$TMP/out"
check "udsecho" has "quay.io/acme/chart-udsecho@$(digest_of "acme/chart-udsecho:0.2.0-${sha}")" "$TMP/out"
check "a stamped chart has no bare tag asked of the registry" \
	test "$(grep -c '/chart-crds/manifests/' "$TMP/fake/curl.log")" = 1
check "no waiting when everything is there" test ! -e "$TMP/fake/sleep.log"

# --tags: the same references, each with the tag it was resolved from, for the
# publish summary (#1378). Run through `env` so the flag is one more argument.
(
	cd "$TMP/repo" || exit 99
	rm -rf "$TMP/fake" && mkdir -p "$TMP/fake"
	env -u REGISTRY_USERNAME -u REGISTRY_PASSWORD -u IMAGE_REGISTRY_HOST -u REGISTRY_CREDENTIAL_HOST \
		"PATH=$TMP/bin:$PATH" "FAKE=$TMP/fake" "IMAGE_REGISTRY_BZL=$TMP/registry.bzl" \
		bash "$SCRIPT" --tags "$sha" >"$TMP/out" 2>"$TMP/err"
)
check "--tags: exit 0" test "$?" = 0
check "--tags: five lines, one per tag" test "$(wc -l <"$TMP/out")" = 5
check "--tags: aether's commit tag beside its reference" \
	has "quay.io/acme/chart-aether@$(digest_of "acme/chart-aether:2.4.12-${sha}") 2.4.12-${sha}" "$TMP/out"
check "--tags: aether's bare tag beside its reference" \
	has "quay.io/acme/chart-aether@$(digest_of "acme/chart-aether:2.4.12") 2.4.12" "$TMP/out"
check "--tags: a stamped chart" \
	has "quay.io/acme/chart-crds@$(digest_of "acme/chart-crds:1.3.0-${sha}") 1.3.0-${sha}" "$TMP/out"

run 1 "a chart tag the registry does not serve fails the script" "$sha" "acme/chart-prober:1.0.4-${sha}"
check "missing: the error names the tag and the attempts" \
	says "::error::could not resolve a digest for quay.io/acme/chart-prober:1.0.4-${sha} (6 attempt(s))" "$TMP/err"
check "missing: six lookups, five waits of 5 s" \
	test "$(grep -c '/chart-prober/manifests/' "$TMP/fake/curl.log"):$(tr '\n' ' ' <"$TMP/fake/sleep.log")" = "6:5 5 5 5 5 "
check "missing: NO partial list on stdout" test ! -s "$TMP/out"

run 1 "aether's bare tag missing fails too" "$sha" "acme/chart-aether:2.4.12"
check "bare missing: named" says "quay.io/acme/chart-aether:2.4.12 (6 attempt(s))" "$TMP/err"

run 2 "a short sha is refused" "${sha:0:12}"
check "short sha: nothing was asked of the registry" test ! -e "$TMP/fake/curl.log"

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
