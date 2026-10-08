#!/usr/bin/env bash
# Hermetic test of scripts/publish-sign.sh (#1378): publish.yaml signs an
# artifact only when it is not signed already, and "signed" means the manifest
# and, for an image, every child manifest. A fake `curl` is the registry, a fake
# `bazel` stands in for `bazel run //bazel/cosign...` (its verify target runs
# the REAL scripts/verify-image-signatures.sh against a fake `cosign`, so the
# child walk under test is the one production runs), a throwaway git history
# holds the Chart.yaml files. No network, no real waiting.
#
# What it pins:
#   - every digest new (what each publish is while images carry their commit):
#     the same nine `cosign sign --recursive` calls, in the same order, as the
#     inline step this script replaced, and not one `cosign verify` spent;
#   - a re-run of the same commit signs NOTHING and still records every
#     reference for the verify step;
#   - some signed, some not: only the unsigned ones are signed;
#   - an index that is signed while one CHILD is not is signed again, whole;
#   - a signature by another identity is not "signed": signed now, and the
#     question leaves no ::error:: annotation in a run that is healthy;
#   - a lookup that goes unanswered is not "signed" either: signed now;
#   - a registry without the Referrers API: cosign alone decides, both ways;
#   - a `cosign sign` that fails ends the script non-zero, and the next run
#     signs only what the failed one did not;
#   - a commit tag the registry does not list fails before anything is signed;
#   - charts: one `cosign sign` per chart manifest, no `--recursive`, and a
#     re-run signs none.
#
# Run: bazel test //scripts:publish_sign_test
# shellcheck disable=SC2016 # single-quoted $names belong to the fakes' own shells.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/publish-sign.sh"
for f in "$SCRIPT" "$HERE/registry-lib.sh" "$HERE/image-registry.sh" \
	"$HERE/verify-image-signatures.sh" "$HERE/published-chart-refs.sh"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/repo" "$TMP/fake"

cat >"$TMP/registry.bzl" <<'BZL'
IMAGE_REGISTRY = "quay.io"
IMAGE_NAMESPACE = "acme"
IMAGE_NAME_OVERRIDES = {}
CHART_REPOSITORY_PREFIX = "chart-"
SIGNATURE_LAYOUT = "referrer"
BZL

# --- the fixture's arithmetic, shared by the fakes and the assertions -------------
# A tag's digest is the sha256 of "<repo>:<tag>"; an index's two children are
# the sha256 of "<index digest>:a" and ":b". Nothing has to be stored to know
# what the registry would answer.
cat >"$TMP/bin/fixture.sh" <<'LIB'
digest_of() { printf 'sha256:%s' "$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"; }
kid() { digest_of "$1:$2"; }
listed() { [ -f "$2" ] && grep -qxF -- "$1" "$2"; }
LIB
# shellcheck source=/dev/null
. "$TMP/bin/fixture.sh"

# --- the fakes -------------------------------------------------------------------
# curl: the registry.
#   token                 always answers
#   tags/list             `dev` and `dev-<$FAKE_COMMIT>` (none of the second for
#                         a repository listed in $FAKE/untagged)
#   HEAD manifests/<tag>  the digest of "<repo>:<tag>"
#   GET  manifests/<dig>  an index of two children for an image repository's tag
#                         digest, a plain manifest otherwise (a chart, a child)
#   referrers/<dig>       one signature referrer when the digest is in
#                         $FAKE/signed or $FAKE/foreign, an empty index when it
#                         is in neither; 503 for a digest in $FAKE/unanswered;
#                         404 for everything when $FAKE/noapi exists
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
. "$(dirname "$0")/fixture.sh"
url="" head=0 fail=0 out="" fmt="" hdr=/dev/null
while [ "$#" -gt 0 ]; do
	case "$1" in
	-D)
		hdr="$2"
		shift
		;;
	-o)
		out="$2"
		shift
		;;
	-w)
		fmt="$2"
		shift
		;;
	-H | -u | --retry) shift ;;
	-I | -fsSI) head=1 fail=1 ;;
	-f*) fail=1 ;;
	https://*) url="$1" ;;
	esac
	shift
done
printf '%s\n' "$url" >>"$FAKE/curl.log"
# answer <status> <body>: the body to stdout or -o, the status to -w or, for a
# failing status under -f, curl's own error and exit 22.
answer() {
	local code="$1" body="$2"
	printf 'HTTP/2 %s\r\n' "$code" >"$hdr"
	if [ -n "$fmt" ]; then
		printf '%s' "$body" >"${out:-/dev/null}"
		printf '%s' "$code"
		exit 0
	fi
	if [ "$code" != 200 ] && [ "$fail" = 1 ]; then
		echo "curl: (22) The requested URL returned error: ${code}" >&2
		exit 22
	fi
	printf '%s\n' "$body"
	exit 0
}
path="${url#https://quay.io/v2/}"
case "$url" in
*/v2/auth\?*) answer 200 '{"token":"fake"}' ;;
*/tags/list\?*)
	repo="${path%%/tags/list*}"
	if listed "$repo" "$FAKE/untagged"; then
		answer 200 '{"tags":["dev"]}'
	fi
	answer 200 "{\"tags\":[\"dev\",\"dev-${FAKE_COMMIT}\"]}"
	;;
*/manifests/sha256:*)
	repo="${path%%/manifests/*}" digest="${path##*/manifests/}"
	case "$repo" in
	acme/chart-*) answer 200 '{"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}' ;;
	esac
	if [ "$digest" = "$(digest_of "${repo}:dev-${FAKE_COMMIT}")" ]; then
		answer 200 "{\"manifests\":[{\"digest\":\"$(kid "$digest" a)\"},{\"digest\":\"$(kid "$digest" b)\"}]}"
	fi
	answer 200 '{"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}'
	;;
*/manifests/*)
	repo="${path%%/manifests/*}" tag="${path##*/manifests/}"
	[ "$head" = 1 ] || {
		echo "fake curl: a tag is only ever asked for with HEAD: ${url}" >&2
		exit 2
	}
	printf 'HTTP/2 200\r\ndocker-content-digest: %s\r\n\r\n' "$(digest_of "${repo}:${tag}")"
	exit 0
	;;
*/referrers/sha256:*)
	digest="${path##*/referrers/}"
	[ -e "$FAKE/noapi" ] && answer 404 '{"errors":[{"code":"NOT_FOUND"}]}'
	listed "$digest" "$FAKE/unanswered" && answer 503 '<html>503</html>'
	if listed "$digest" "$FAKE/signed" || listed "$digest" "$FAKE/foreign"; then
		answer 200 '{"schemaVersion":2,"manifests":[{"digest":"sha256:feed","artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json","annotations":{"dev.sigstore.bundle.predicateType":"https://sigstore.dev/cosign/sign/v1","org.opencontainers.image.created":"2026-10-01T08:00:00Z"}},{"digest":"sha256:beef","artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json","annotations":{"dev.sigstore.bundle.predicateType":"https://slsa.dev/provenance/v1","org.opencontainers.image.created":"2026-09-01T08:00:00Z"}}]}'
	fi
	answer 200 '{"schemaVersion":2,"manifests":[]}'
	;;
esac
echo "fake curl: unexpected url ${url}" >&2
exit 2
FAKE

# cosign: `verify` succeeds for a digest in $FAKE/signed, fails as an identity
# mismatch for one in $FAKE/foreign, and finds no signature otherwise.
cat >"$TMP/bin/cosign" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
. "$(dirname "$0")/fixture.sh"
ref="${*: -1}"
digest="${ref##*@}"
printf '%s\n' "$*" >>"$FAKE/cosign.log"
if listed "$digest" "$FAKE/signed"; then
	exit 0
fi
if listed "$digest" "$FAKE/foreign"; then
	echo 'Error: no matching attestations: failed to verify certificate identity: no matching CertificateIdentity found, last error: expected SAN value to match regex "x", got "https://github.com/acme/widget/.github/workflows/other.yml@refs/heads/main"' >&2
	exit 1
fi
echo "Error: no signatures found" >&2
exit 10
FAKE

# bazel: `run //bazel/cosign -- sign ...` records the call and marks the digest
# (with --recursive, its two children as well) signed, or fails for a digest in
# $FAKE/sign.fail. `run //bazel/cosign:verify_image_signatures -- ...` runs the
# real verifier with the fake cosign.
cat >"$TMP/bin/bazel" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
. "$(dirname "$0")/fixture.sh"
printf '%s\n' "$*" >>"$FAKE/bazel.log"
[ "${1:-}" = run ] || {
	echo "fake bazel: only run is expected: $*" >&2
	exit 2
}
target="$2"
shift 3
case "$target" in
//bazel/cosign)
	ref="${*: -1}"
	digest="${ref##*@}"
	if listed "$digest" "$FAKE/sign.fail"; then
		echo "Error: signing [${ref}]: getting keypair and token: retrieving ID token" >&2
		exit 1
	fi
	echo "$digest" >>"$FAKE/signed"
	case " $* " in
	*" --recursive "*)
		kid "$digest" a >>"$FAKE/signed"
		echo >>"$FAKE/signed"
		kid "$digest" b >>"$FAKE/signed"
		echo >>"$FAKE/signed"
		;;
	esac
	;;
//bazel/cosign:verify_image_signatures)
	COSIGN="$(dirname "$0")/cosign" exec bash "$VERIFY_SCRIPT" "$@"
	;;
*)
	echo "fake bazel: unexpected target ${target}" >&2
	exit 2
	;;
esac
FAKE
cat >"$TMP/bin/sleep" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$1" >>"$FAKE/sleep.log"
FAKE
chmod +x "$TMP/bin/curl" "$TMP/bin/cosign" "$TMP/bin/bazel" "$TMP/bin/sleep"

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

# The nine image components, in the library's own order: the order the old
# inline step signed in.
components=(agent mesh-dns proxy-supervisor uds-csi cni-install registrar controller prober udsecho)
image_ref() { printf 'quay.io/acme/%s@%s' "$1" "$(digest_of "acme/$1:dev-${sha}")"; }
image_digest() { digest_of "acme/$1:dev-${sha}"; }
chart_refs=(
	"quay.io/acme/chart-aether@$(digest_of "acme/chart-aether:2.4.12-${sha}")"
	"quay.io/acme/chart-aether@$(digest_of "acme/chart-aether:2.4.12")"
	"quay.io/acme/chart-crds@$(digest_of "acme/chart-crds:1.3.0-${sha}")"
	"quay.io/acme/chart-prober@$(digest_of "acme/chart-prober:1.0.4-${sha}")"
	"quay.io/acme/chart-udsecho@$(digest_of "acme/chart-udsecho:0.2.0-${sha}")"
)

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
	sed 's/^/      | /' "$TMP/out" 2>/dev/null
}
check() {
	local what="$1"
	shift
	if "$@"; then pass "$what"; else fail "$what"; fi
}
says() { grep -qF -- "$1" "$TMP/out"; }
silent_on() { ! grep -qF -- "$1" "$TMP/out"; }
# A log a run never wrote counts as zero lines, not as an empty string.
count_in() {
	[ -f "$2" ] || {
		echo 0
		return
	}
	grep -c -- "$1" "$2" || true
}
signs() { count_in '^run //bazel/cosign -- sign ' "$TMP/fake/bazel.log"; }
verifies() { count_in '^verify ' "$TMP/fake/cosign.log"; }
# The line <out file>.state holds for a reference: `<ref> TAB <tags> TAB <state>`.
state_of() { grep -F -- "$1"$'\t' "$TMP/signed.txt.state" 2>/dev/null || true; }
# The sign calls a run made, one per line.
sign_log() { grep -- '^run //bazel/cosign -- sign ' "$TMP/fake/bazel.log" 2>/dev/null || true; }

# The registry's state survives from one run to the next unless reset: that is
# what makes "run it again" a test. The call logs do not.
reset() {
	rm -rf "$TMP/fake"
	mkdir -p "$TMP/fake"
}
# mark_signed <file> <component>...: the index and both children.
mark_tree() {
	local file="$1" c d
	shift
	for c in "$@"; do
		d="$(image_digest "$c")"
		printf '%s\n%s\n%s\n' "$d" "$(kid "$d" a)" "$(kid "$d" b)" >>"$TMP/fake/$file"
	done
}

# run <want exit> <name> <kind> [commit]
run() {
	local want="$1" name="$2" kind="$3" commit="${4:-$sha}" rc
	rm -f "$TMP/fake/"*.log "$TMP/signed.txt" "$TMP/signed.txt.state"
	(
		cd "$TMP/repo" || exit 99
		env -u REGISTRY_USERNAME -u REGISTRY_PASSWORD -u IMAGE_REGISTRY_HOST -u REGISTRY_CREDENTIAL_HOST \
			-u CERT_IDENTITY_REGEXP -u CERT_OIDC_ISSUER -u BAZEL -u COSIGN \
			"PATH=$TMP/bin:$PATH" "FAKE=$TMP/fake" "FAKE_COMMIT=$sha" "VERIFY_SCRIPT=$HERE/verify-image-signatures.sh" \
			"IMAGE_REGISTRY_BZL=$TMP/registry.bzl" REGISTRY_COMMIT_TAG_ATTEMPTS=2 GITHUB_REPOSITORY=acme/widget \
			bash "$SCRIPT" "$kind" "$commit" "$TMP/signed.txt" >"$TMP/out" 2>&1
	)
	rc=$?
	if [ "$rc" -ne "$want" ]; then fail "$name: exit $rc, wanted $want"; else pass "$name"; fi
}

all_image_refs="$(for c in "${components[@]}"; do
	image_ref "$c"
	echo
done)"

# --- every digest is new --------------------------------------------------------
reset
run 0 "images, every digest new" images
want="$(for c in "${components[@]}"; do echo "run //bazel/cosign -- sign --yes --recursive $(image_ref "$c")"; done)"
check "all new: the nine sign calls of the old inline step, in its order" test "$(sign_log)" = "$want"
check "all new: not one cosign verify is spent asking" test "$(verifies)" = 0
check "all new: every reference is recorded for the verify step, in order" test "$(cat "$TMP/signed.txt")" = "$all_image_refs"
check "all new: the tally" says "9 images reference(s): 9 signed now, 0 already signed"
check "all new: each line says why" says "signing now:    $(image_ref agent) (no signature attached)"
check "all new: the state file names the tag and what was done, for the summary" \
	test "$(state_of "$(image_ref agent)")" = "$(image_ref agent)"$'\t'"dev-${sha}"$'\t'"signed-now"
check "all new: one state line per reference" test "$(wc -l <"$TMP/signed.txt.state")" = 9

# --- the same commit again: nothing new is created -------------------------------
run 0 "images, a re-run of the same commit" images
check "re-run: NOTHING is signed" test "$(signs)" = 0
check "re-run: all 27 manifests were verified to say so (9 indexes, 18 children)" test "$(verifies)" = 27
check "re-run: every reference is still recorded for the verify step" test "$(cat "$TMP/signed.txt")" = "$all_image_refs"
check "re-run: the tally" says "9 images reference(s): 0 signed now, 9 already signed"
check "re-run: the line says since when" \
	says "already signed: $(image_ref agent) (first signature attached 2026-10-01T08:00:00Z; 1 signature(s) on the manifest)"
check "re-run: the state file says already signed" \
	test "$(state_of "$(image_ref agent)")" = "$(image_ref agent)"$'\t'"dev-${sha}"$'\t'"already-signed"

# --- some signed, some not, and every way of being "not" -------------------------
# One run, nine images (a signed image costs a full child walk through the real
# verifier, so the cases share a run):
#   agent, registrar   signed before, whole              -> left alone
#   mesh-dns           the index is signed, child b not  -> signed again, whole
#   prober             the index is signed by another identity
#   controller         signed, but its referrers lookup goes unanswered
#   the other four     nothing attached
reset
mark_tree signed agent registrar mesh-dns prober controller
d="$(image_digest mesh-dns)"
grep -vxF -e "$(kid "$d" b)" -e "$(image_digest prober)" "$TMP/fake/signed" >"$TMP/fake/signed.new" &&
	mv "$TMP/fake/signed.new" "$TMP/fake/signed"
image_digest prober >"$TMP/fake/foreign"
echo >>"$TMP/fake/foreign"
image_digest controller >"$TMP/fake/unanswered"
echo >>"$TMP/fake/unanswered"
run 0 "images, two of nine signed before" images
want="$(for c in mesh-dns proxy-supervisor uds-csi cni-install controller prober udsecho; do
	echo "run //bazel/cosign -- sign --yes --recursive $(image_ref "$c")"
done)"
check "mixed: exactly the seven that are not signed are signed, whole" test "$(sign_log)" = "$want"
check "mixed: all nine are recorded" test "$(wc -l <"$TMP/signed.txt")" = 9
check "mixed: the tally" says "9 images reference(s): 7 signed now, 2 already signed"
check "mixed: a signed image is left alone" says "already signed: $(image_ref agent) ("
check "mixed: the state file tells the two apart" \
	test "$(cut -f3 "$TMP/signed.txt.state" | tr '\n' ' ')" = \
	"already-signed signed-now signed-now signed-now signed-now already-signed signed-now signed-now signed-now "

# The index is signed, one child is not: the line names the child.
check "child: the line names the child" \
	says "signing now:    $(image_ref mesh-dns) (child $(kid "$d" b) has no signature attached)"

# Signed, but not by this workflow.
check "foreign: the line says what was found" \
	says "signing now:    $(image_ref prober) (what is attached does not verify as this workflow's signature)"
check "foreign: the verifier's finding is shown" says "| FAILED   index $(image_ref prober)"
check "foreign: asking leaves no ::error:: annotation in a healthy run" silent_on "::error::"

# A lookup goes unanswered: not read as signed.
check "unanswered: the line says it could not tell" \
	says "signing now:    $(image_ref controller) (could not tell whether it is signed: the referrers lookup of $(image_digest controller) went unanswered"

# --- a registry without the Referrers API ----------------------------------------
reset
mark_tree signed agent
: >"$TMP/fake/noapi"
run 0 "images, no Referrers API" images
check "no API: cosign alone decides -- the signed image is left alone" silent_on "sign --yes --recursive $(image_ref agent)"
check "no API: and the other eight are signed" test "$(signs)" = 8
check "no API: no time to print, and it says so" \
	says "already signed: $(image_ref agent) (verified; the registry lists no time for its signature)"

# --- cosign sign fails part-way --------------------------------------------------
reset
image_digest uds-csi >"$TMP/fake/sign.fail"
echo >>"$TMP/fake/sign.fail"
run 1 "images, the fourth cosign sign fails" images
check "sign fails: it stopped there (three signed, the fourth tried)" test "$(signs)" = 4
check "sign fails: only what was signed is recorded" test "$(wc -l <"$TMP/signed.txt")" = 3
rm -f "$TMP/fake/sign.fail"
run 0 "images, the run after a failed one" images
want="$(for c in uds-csi cni-install registrar controller prober udsecho; do
	echo "run //bazel/cosign -- sign --yes --recursive $(image_ref "$c")"
done)"
check "after a failure: only what the failed run left unsigned is signed" test "$(sign_log)" = "$want"
check "after a failure: the tally" says "9 images reference(s): 6 signed now, 3 already signed"

# --- the commit's tag is not there -----------------------------------------------
reset
echo "acme/agent" >"$TMP/fake/untagged"
run 1 "images, a repository without this commit's tag" images
check "no tag: the error names the commit and the repository" says "::error::no tag ending in -${sha} in acme/agent"
check "no tag: nothing was signed" test "$(signs)" = 0

# --- charts ----------------------------------------------------------------------
reset
run 0 "charts, every digest new" charts
want="$(for r in "${chart_refs[@]}"; do echo "run //bazel/cosign -- sign --yes ${r}"; done)"
check "charts: one sign per chart manifest, no --recursive, the old order" test "$(sign_log)" = "$want"
check "charts: all five recorded" test "$(cat "$TMP/signed.txt")" = "$(printf '%s\n' "${chart_refs[@]}")"
check "charts: the tally" says "5 charts reference(s): 5 signed now, 0 already signed"
check "charts: the state file names aether's bare tag" \
	test "$(state_of "${chart_refs[1]}")" = "${chart_refs[1]}"$'\t'"2.4.12"$'\t'"signed-now"
check "charts: and its commit tag" \
	test "$(state_of "${chart_refs[0]}")" = "${chart_refs[0]}"$'\t'"2.4.12-${sha}"$'\t'"signed-now"
run 0 "charts, a re-run of the same commit" charts
check "charts re-run: nothing is signed" test "$(signs)" = 0
check "charts re-run: asked with --single (a chart is one manifest)" \
	grep -qF -- "run //bazel/cosign:verify_image_signatures -- --single ${chart_refs[0]}" "$TMP/fake/bazel.log"
check "charts re-run: the tally" says "5 charts reference(s): 0 signed now, 5 already signed"

# --- usage -----------------------------------------------------------------------
reset
run 2 "a short sha is refused" images "${sha:0:12}"
check "short sha: nothing was asked of the registry" test ! -e "$TMP/fake/curl.log"
run 2 "an unknown kind is refused" everything

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
