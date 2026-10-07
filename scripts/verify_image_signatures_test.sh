#!/usr/bin/env bash
# Hermetic test of the signature verifier's handling of answers that are not
# answers (#1316): scripts/verify-image-signatures.sh and the registry lookups
# it makes through scripts/registry-lib.sh, against a fake `curl`, a fake
# `cosign` and a fake `sleep`. No network, no real waiting.
#
# publish-verify run 37522284996: quay.io answered one pull-token request with
# a 502, the JSON reader was handed an empty string, and the step died with a
# Python traceback. What this pins:
#   - a 5xx, a 429, no connection at all, an empty 200, a 200 that is HTML and
#     a 200 cut short are each retried, with a doubling wait, and the run then
#     passes;
#   - the retries are bounded, and the final error names the URL and the HTTP
#     status (or how the body starts) -- never a traceback, never a token
#     reply's bytes, never the registry password;
#   - the registry's own final answers (401, 404, a JSON document that is not
#     an index) are NOT retried;
#   - a failed `cosign verify` is re-run, except an identity mismatch, within a
#     per-run budget, and a manifest that keeps failing is FAILED with cosign's
#     last line and the attempt count while the others are still checked;
#   - the tag listing (registry_all_tags, #1337) rides the same retry, page by
#     page, prints nothing when a page cannot be read, and under
#     registry_commit_tag's own 13 listings each page is asked ONCE, so the
#     two retries do not multiply (60 s of waiting, not 242 s);
#   - sourcing registry-lib.sh says nothing on stderr (#1338): the setting
#     reader it calls at source time, scripts/image-registry.sh, is beside it.
#
# Run: bazel test //scripts:verify_image_signatures_test
# shellcheck disable=SC2016 # single-quoted $names belong to the fakes' own shells.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/verify-image-signatures.sh"
for f in "$SCRIPT" "$HERE/registry-lib.sh" "$HERE/image-registry.sh"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

index="sha256:$(printf '1%.0s' {1..64})"
kid_a="sha256:$(printf 'a%.0s' {1..64})"
kid_b="sha256:$(printf 'b%.0s' {1..64})"
repo=acme/widget
ref="quay.io/${repo}@${index}"
token_url="https://quay.io/v2/auth?service=quay.io&scope=repository:${repo}:pull"
index_url="https://quay.io/v2/${repo}/manifests/${index}"
commit="0123456789abcdef0123456789abcdef01234567"
tags_url="https://quay.io/v2/${repo}/tags/list?n=1000"
tags_url2="${tags_url}&last=b"
bearer="b34r3r-t0k3n-bytes"

# The registry setting registry-lib.sh reads at source time, through
# scripts/image-registry.sh (#1338). A fixture, so the test does not depend on
# where Bazel puts the real one.
cat >"$TMP/registry.bzl" <<'BZL'
IMAGE_REGISTRY = "quay.io"
IMAGE_NAMESPACE = "acme"
IMAGE_NAME_OVERRIDES = {}
CHART_REPOSITORY_PREFIX = "chart-"
SIGNATURE_LAYOUT = "referrer"
BZL

# --- the fakes -------------------------------------------------------------------
# curl: each request takes the next line of $FAKE/token.seq, $FAKE/manifest.seq
# or $FAKE/tags.seq (by URL); with none left it answers correctly. The tag
# listing is two pages: `a b` with a `Link: rel="next"` header (written to the
# -D file), then, for `last=b`, `c dev-<$FAKE_COMMIT>`. A line is
#   <http status>            an error status: with -f, exit 22 and curl's own
#                            "The requested URL returned error: <status>"
#   000                      no connection: exit 7
#   200 <empty|html|cut|notoken|manifest|secret|array>
#                            a 200 with that body instead of the right one
# Every URL asked is appended to $FAKE/curl.log.
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
url="" fail=0 hdr=/dev/null link=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	-D)
		hdr="$2"
		shift
		;;
	-H | -u | -o | -w | --retry) shift ;;
	-f*) fail=1 ;;
	https://*) url="$1" ;;
	esac
	shift
done
printf '%s\n' "$url" >>"$FAKE/curl.log"
case "$url" in
*/v2/auth\?*) seq="$FAKE/token.seq" good='{"token":"fake"}' ;;
*/manifests/sha256:*) seq="$FAKE/manifest.seq" good="$(cat "$FAKE/index.json")" ;;
*/tags/list\?n=1000) seq="$FAKE/tags.seq" good='{"name":"acme/widget","tags":["a","b"]}' link=1 ;;
*/tags/list\?n=1000\&last=b) seq="$FAKE/tags.seq" good="{\"tags\":[\"c\",\"dev-${FAKE_COMMIT:-none}\"]}" ;;
*)
	echo "fake curl: unexpected url ${url}" >&2
	exit 2
	;;
esac
line=""
if [ -s "$seq" ]; then
	line="$(head -n 1 "$seq")"
	sed -i 1d "$seq"
fi
read -r code body <<<"${line:-200 good}"
printf 'HTTP/2 %s\r\n' "$code" >"$hdr"
case "$code" in
000)
	echo "curl: (7) Failed to connect to quay.io port 443 after 21 ms: Could not connect to server" >&2
	exit 7
	;;
200) ;;
*)
	if [ "$fail" = 1 ]; then
		echo "curl: (22) The requested URL returned error: ${code}" >&2
		exit 22
	fi
	printf '<html><body>%s</body></html>\n' "$code"
	exit 0
	;;
esac
case "$body" in
good)
	[ -z "$link" ] || printf 'link: <%s&last=b>; rel="next"\r\n' "${url#https://quay.io}" >>"$hdr"
	printf '%s\n' "$good"
	;;
array) printf '["a","b"]\n' ;;
empty) ;;
html) printf '<html>\n<head><title>502 Bad Gateway</title></head>\n<body>nginx</body></html>\n' ;;
cut) printf '%s' "${good:0:24}" ;;
notoken) printf '{"errors":[{"code":"TOOMANYREQUESTS"}]}\n' ;;
manifest) printf '{"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}\n' ;;
secret) printf '{"token":"s3cr3t-half-a-tok' ;;
*)
	echo "fake curl: unknown body ${body}" >&2
	exit 2
	;;
esac
FAKE

# cosign: each `verify` takes the next line of $FAKE/cosign.seq; with none left
# it verifies. The messages are the pinned cosign's own (v3.1.2, observed).
#   ok        exit 0
#   flake     a registry error under cosign: exit 1
#   nosig     "no signatures found": exit 10
#   identity  the signature was fetched and its identity is another's: exit 1
# Every ref asked is appended to $FAKE/cosign.log.
cat >"$TMP/bin/cosign" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
[ "$1" = verify ] || { echo "fake cosign: unexpected $*" >&2; exit 2; }
printf '%s\n' "${!#}" >>"$FAKE/cosign.log"
line=ok
if [ -s "$FAKE/cosign.seq" ]; then
	line="$(head -n 1 "$FAKE/cosign.seq")"
	sed -i 1d "$FAKE/cosign.seq"
fi
case "$line" in
ok) echo '[{"critical":{}}]' ;;
flake)
	echo 'Error: GET https://quay.io/v2/acme/widget/referrers/sha256:x: unexpected status code 502 Bad Gateway' >&2
	echo 'error during command execution: GET https://quay.io/v2/acme/widget/referrers/sha256:x: unexpected status code 502 Bad Gateway' >&2
	exit 1
	;;
nosig)
	echo 'Error: no signatures found' >&2
	echo 'error during command execution: no signatures found' >&2
	exit 10
	;;
identity)
	echo 'Error: no matching attestations: failed to verify certificate identity: no matching CertificateIdentity found, last error: expected SAN value to match regex "^x", got "https://example.invalid/other"' >&2
	echo 'error during command execution: no matching attestations: failed to verify certificate identity: no matching CertificateIdentity found, last error: expected SAN value to match regex "^x", got "https://example.invalid/other"' >&2
	exit 1
	;;
*)
	echo "fake cosign: unknown outcome ${line}" >&2
	exit 2
	;;
esac
FAKE

# sleep: record the wait, do not wait.
cat >"$TMP/bin/sleep" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$1" >>"$FAKE/sleep.log"
FAKE
chmod +x "$TMP/bin/curl" "$TMP/bin/cosign" "$TMP/bin/sleep"

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
has() { grep -qF -- "$1" "$2"; }
lacks() { [ -f "$2" ] && ! grep -qF -- "$1" "$2"; }
# How many times a URL (or a ref) was asked, and the waits, space-separated.
asked() { grep -cxF -- "$1" "$TMP/fake/$2" 2>/dev/null || true; }
waits() { [ ! -f "$TMP/fake/sleep.log" ] || tr '\n' ' ' <"$TMP/fake/sleep.log"; }

# run <want exit> <name> <token seq> <manifest seq> <cosign seq> [VAR=value...]
# Sequences are comma-separated ("502,200 html"); "" is "always the right
# answer". stdout lands in $TMP/out, stderr in $TMP/err.
run() {
	local want="$1" name="$2" rc
	rm -rf "$TMP/fake"
	mkdir -p "$TMP/fake"
	printf '{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"%s"},{"digest":"%s"}]}' \
		"$kid_a" "$kid_b" >"$TMP/fake/index.json"
	[ -z "$3" ] || tr ',' '\n' <<<"$3" >"$TMP/fake/token.seq"
	[ -z "$4" ] || tr ',' '\n' <<<"$4" >"$TMP/fake/manifest.seq"
	[ -z "$5" ] || tr ',' '\n' <<<"$5" >"$TMP/fake/cosign.seq"
	shift 5
	env "${CLEAN_ENV[@]}" COSIGN="$TMP/bin/cosign" "$@" \
		bash "$SCRIPT" "$ref" >"$TMP/out" 2>"$TMP/err"
	rc=$?
	judge "$want" "$name" "$rc"
}

# The environment every case starts from: no knob inherited from the caller,
# the fakes first on PATH, the fixture as the registry setting.
CLEAN_ENV=(
	-u REGISTRY_USERNAME -u REGISTRY_PASSWORD -u REGISTRY_FETCH_ATTEMPTS -u REGISTRY_FETCH_INTERVAL
	-u REGISTRY_COMMIT_TAG_ATTEMPTS -u REGISTRY_COMMIT_TAG_INTERVAL -u REGISTRY_COMMIT_TAG_FETCH_ATTEMPTS
	-u VERIFY_ATTEMPTS -u VERIFY_RETRY_INTERVAL -u VERIFY_RETRY_BUDGET -u CERT_IDENTITY_REGEXP
	-u IMAGE_REGISTRY_HOST -u REGISTRY_CREDENTIAL_HOST
	"PATH=$TMP/bin:$PATH" "FAKE=$TMP/fake" "FAKE_COMMIT=$commit" REGISTRY_HOST=quay.io
	"IMAGE_REGISTRY_BZL=$TMP/registry.bzl"
)

judge() {
	local want="$1" name="$2" rc="$3"
	if [ "$rc" -ne "$want" ]; then
		fail "$name: exit $rc, wanted $want"
		return
	fi
	# Whatever happened, nobody reads a traceback.
	if grep -q 'Traceback\|JSONDecodeError' "$TMP/out" "$TMP/err"; then
		fail "$name: a Python traceback reached the log"
		return
	fi
	# Nor a library that could not find its own sibling (#1338).
	if grep -q 'No such file or directory' "$TMP/out" "$TMP/err"; then
		fail "$name: registry-lib.sh could not run something it sources or calls"
		return
	fi
	pass "$name"
}

# lib <want exit> <name> <tags seq> <shell using registry-lib.sh> [VAR=value...]
# Source the library the way a workflow step does and run one line against it.
lib() {
	local want="$1" name="$2" code="$4" rc
	rm -rf "$TMP/fake"
	mkdir -p "$TMP/fake"
	[ -z "$3" ] || tr ',' '\n' <<<"$3" >"$TMP/fake/tags.seq"
	shift 4
	env "${CLEAN_ENV[@]}" LIB="$HERE/registry-lib.sh" "$@" \
		bash -c 'set -euo pipefail; . "$LIB"; '"$code" >"$TMP/out" 2>"$TMP/err"
	rc=$?
	judge "$want" "$name" "$rc"
}
# <n> copies of one answer, comma-separated.
times() {
	local n="$1" out="" i
	for ((i = 0; i < n; i++)); do out="${out}${out:+,}$2"; done
	printf '%s' "$out"
}
# The waits added up, in seconds.
sum_waits() {
	local total=0 w
	[ ! -f "$TMP/fake/sleep.log" ] || while read -r w; do total=$((total + w)); done <"$TMP/fake/sleep.log"
	echo "$total"
}

# --- nothing goes wrong ----------------------------------------------------------
run 0 "index and both children verify" "" "" ""
check "clean: PASS line counts all three" has 'PASS: 3 manifest(s) verified (1 index(es) + their children)' "$TMP/out"
check "clean: one token request, one index request" test "$(asked "$token_url" curl.log) $(asked "$index_url" curl.log)" = "1 1"
check "clean: three cosign runs" test "$(wc -l <"$TMP/fake/cosign.log")" = 3
check "clean: no waiting" test ! -e "$TMP/fake/sleep.log"
check "clean: no retry and no error on stderr" test "$(grep -c -e "registry-lib: GET" -e "^::error" "$TMP/err")" = 0

# --- the pull token (run 37522284996) --------------------------------------------
run 0 "#1316: one 502 from the token endpoint is retried and the run passes" "502" "" ""
check "502: the retry names the URL, the status and the wait" \
	has "registry-lib: GET ${token_url} answered HTTP 502; attempt 1 of 4, retrying in 2s" "$TMP/err"
check "502: asked twice, waited 2 s once" test "$(asked "$token_url" curl.log):$(waits)" = "2:2 "
check "502: everything was still verified" has 'PASS: 3 manifest(s) verified' "$TMP/out"

run 0 "an empty 200 is retried" "200 empty" "" ""
check "empty: says the body was empty" \
	has "answered 200 with a body that is not a JSON object with a token (empty); attempt 1 of 4" "$TMP/err"
run 0 "429, 503 and no connection are retried in turn" "429,503,000" "" ""
check "three flakes: 2 s, 4 s, 8 s" test "$(asked "$token_url" curl.log):$(waits)" = "4:2 4 8 "
check "no connection: curl's own error is quoted" \
	has "got no HTTP answer (curl exit 7: curl: (7) Failed to connect to quay.io port 443" "$TMP/err"

run 2 "a token endpoint that keeps answering 502 ends the run, exit 2" "502,502,502,502" "" ""
check "502 x4: the final line names the URL, the status and the attempts" \
	has "registry-lib: GET ${token_url} answered HTTP 502; giving up after 4 attempt(s)" "$TMP/err"
check "502 x4: bounded at four requests and three waits" test "$(asked "$token_url" curl.log):$(waits)" = "4:2 4 8 "
check "502 x4: the workflow error names the repository and the registry" \
	has "::error::could not obtain a pull token for ${repo} on quay.io" "$TMP/err"
check "502 x4: nothing was verified, and nothing says PASS" lacks 'PASS' "$TMP/out"

run 2 "a token endpoint that keeps answering HTML ends the run, exit 2" "200 html,200 html,200 html,200 html" "" ""
check "html token: says it was not a token, and how big" \
	has "answered 200 with a body that is not a JSON object with a token (76 bytes, not shown); giving up after 4 attempt(s)" "$TMP/err"
run 2 "a token reply cut short is never quoted" "200 secret,200 secret,200 secret,200 secret" "" ""
check "cut token: its bytes are not in the log" lacks 's3cr3t' "$TMP/err"
check "cut token: nor on stdout" lacks 's3cr3t' "$TMP/out"
run 2 "JSON without a token is not a token" "200 notoken,200 notoken,200 notoken,200 notoken" "" ""
check "no token: four attempts" test "$(asked "$token_url" curl.log)" = 4

run 2 "a 401 from the token endpoint is the registry's answer: not retried" "401" "" ""
check "401: one request, no wait" test "$(asked "$token_url" curl.log):$(waits)" = "1:"
check "401: says so" has "registry-lib: GET ${token_url} answered HTTP 401; not retried" "$TMP/err"

run 2 "REGISTRY_FETCH_ATTEMPTS bounds the retries" "502,502,502,502" "" "" REGISTRY_FETCH_ATTEMPTS=2 REGISTRY_FETCH_INTERVAL=5
check "2 attempts: two requests, one wait of the configured length" test "$(asked "$token_url" curl.log):$(waits)" = "2:5 "
check "2 attempts: giving up after 2" has "giving up after 2 attempt(s)" "$TMP/err"

run 2 "the registry password never reaches the log" "502,200 html,000,502" "" "" \
	REGISTRY_USERNAME=aethermesh+robot REGISTRY_PASSWORD=not-a-real-secret REGISTRY_CREDENTIAL_HOST=quay.io
check "password: not on stderr" lacks 'not-a-real-secret' "$TMP/err"
check "password: not on stdout" lacks 'not-a-real-secret' "$TMP/out"

# --- the child walk --------------------------------------------------------------
run 0 "a 503 and then an index cut short are retried; the walk then succeeds" "" "503,200 cut" ""
check "cut index: the retry quotes how the body starts" \
	has "registry-lib: GET ${index_url} answered 200 with a body that is not JSON (24 bytes, starts '{\"mediaType\":\"applicatio'); attempt 2 of 4, retrying in 4s" "$TMP/err"
check "cut index: three requests" test "$(asked "$index_url" curl.log)" = 3
check "cut index: both children were walked" has '2 child manifest(s) walked' "$TMP/out"

run 2 "an index lookup that keeps answering HTML ends the run, exit 2" "" "200 html,200 html,200 html,200 html" ""
check "html index: the URL and the first bytes, on one line" \
	has "registry-lib: GET ${index_url} answered 200 with a body that is not JSON (76 bytes, starts '<html>?<head><title>502 Bad Gateway</title></head>?<body>nginx</body></html>'); giving up after 4 attempt(s)" "$TMP/err"
check "html index: the workflow error names the reference" \
	has "::error::could not enumerate the child manifests of ${ref}" "$TMP/err"
# The index itself verified before the walk; that is not a PASS.
check "html index: no PASS" lacks 'PASS' "$TMP/out"

run 2 "a 404 for the index is the registry's answer: not retried" "" "404" ""
check "404: one request, no wait" test "$(asked "$index_url" curl.log):$(waits)" = "1:"
run 2 "a manifest that is not an index is an answer, not a flake: not retried" "" "200 manifest" ""
check "not an index: one request, no wait" test "$(asked "$index_url" curl.log):$(waits)" = "1:"

# --- cosign ----------------------------------------------------------------------
run 0 "one failed cosign verify is re-run and the run passes" "" "" "flake"
check "cosign flake: says it is retrying, and why" \
	has "  retrying index ${ref} in 2s (attempt 1 of 3 failed: error during command execution: GET https://quay.io/v2/acme/widget/referrers/sha256:x: unexpected status code 502 Bad Gateway)" "$TMP/out"
check "cosign flake: the verified line says it took two" has "  verified index ${ref} (attempt 2 of 3)" "$TMP/out"
check "cosign flake: four cosign runs, one wait" test "$(wc -l <"$TMP/fake/cosign.log"):$(waits)" = "4:2 "
check "cosign flake: no FAILED line for the issue to pick up" lacks 'FAILED' "$TMP/out"

# The index verifies, the first child fails three times, the second verifies.
run 1 "a child that never verifies is FAILED after three attempts" "" "" "ok,flake,nosig,flake"
check "child: FAILED with cosign's last line and the attempts" \
	has "  FAILED   child quay.io/${repo}@${kid_a}: error during command execution: GET https://quay.io/v2/acme/widget/referrers/sha256:x: unexpected status code 502 Bad Gateway (3 attempt(s))" "$TMP/out"
check "child: 2 s then 4 s" test "$(waits)" = "2 4 "
check "child: the other child was still checked" has "  verified child quay.io/${repo}@${kid_b}" "$TMP/out"
check "child: the tally" has 'FAIL: 1 manifest(s) did not verify, 2 did, across 1 index(es)' "$TMP/out"
check "child: three runs for it" test "$(asked "quay.io/${repo}@${kid_a}" cosign.log)" = 3

run 1 "'no signatures found' is retried, then FAILED" "" "" "nosig,nosig,nosig"
check "nosig: three runs for the index" test "$(asked "$ref" cosign.log)" = 3
check "nosig: FAILED says what cosign said" \
	has "  FAILED   index ${ref}: error during command execution: no signatures found (3 attempt(s))" "$TMP/out"

run 1 "a signature by another identity is final: not retried" "" "" "identity"
check "identity: one run for the index, no wait" test "$(asked "$ref" cosign.log):$(waits)" = "1:"
check "identity: FAILED after one attempt" has '(1 attempt(s))' "$TMP/out"
check "identity: the children are still checked" has '2 child manifest(s) walked' "$TMP/out"

# Everything fails. Budget 1: one re-run in the whole invocation, so four
# cosign runs for three manifests, not nine.
run 1 "the retry budget bounds a run in which nothing verifies" "" "" \
	"flake,flake,flake,flake,flake,flake,flake,flake,flake" VERIFY_RETRY_BUDGET=1
check "budget 1: four cosign runs, one wait" test "$(wc -l <"$TMP/fake/cosign.log"):$(waits)" = "4:2 "
check "budget 1: all three are FAILED" test "$(grep -c '^  FAILED' "$TMP/out")" = 3
run 1 "the default budget of 12 covers three manifests failing three times" "" "" \
	"flake,flake,flake,flake,flake,flake,flake,flake,flake"
check "default budget: nine cosign runs" test "$(wc -l <"$TMP/fake/cosign.log")" = 9
run 1 "VERIFY_ATTEMPTS=1 is no retry" "" "" "flake" VERIFY_ATTEMPTS=1
check "1 attempt: no wait" test "$(waits)" = ""
run 0 "VERIFY_ATTEMPTS and VERIFY_RETRY_INTERVAL are honoured" "" "" "flake,flake,flake" VERIFY_ATTEMPTS=4 VERIFY_RETRY_INTERVAL=1
check "4 attempts from 1 s: 1, 2, 4" test "$(waits)" = "1 2 4 "
run 2 "a malformed VERIFY_ATTEMPTS is refused" "" "" "" VERIFY_ATTEMPTS=lots
check "malformed: no cosign run" test ! -e "$TMP/fake/cosign.log"

# --- sourcing the library (#1338) --------------------------------------------------
# registry-lib.sh runs scripts/image-registry.sh at source time, for the host
# and the repository list. With the reader beside it, sourcing is silent and
# the lists are filled; under `bazel run //bazel/cosign:verify_image_signatures`
# it was missing from the runfiles and every log began with bash's "No such file
# or directory" (//bazel/cosign:verify_image_signatures_test runs that target).
lib 0 "sourcing registry-lib.sh resolves the setting" "" \
	'echo "host=${REGISTRY_HOST} repos=${#REGISTRY_IMAGE_REPOS[@]} first=${REGISTRY_IMAGE_REPOS[0]}"' REGISTRY_HOST=
check "source: nothing on stderr" test ! -s "$TMP/err"
check "source: the host and the nine repositories come from the setting" \
	test "$(cat "$TMP/out")" = "host=quay.io repos=9 first=acme/agent"

# --- the tag listing (#1337) -------------------------------------------------------
list='registry_all_tags acme/widget "$BEARER"'
lib 0 "the listing walks both pages" "" "$list" BEARER="$bearer"
check "listing: every tag of both pages, in order" test "$(tr '\n' ' ' <"$TMP/out")" = "a b c dev-${commit} "
check "listing: one request per page" test "$(asked "$tags_url" curl.log) $(asked "$tags_url2" curl.log)" = "1 1"
check "listing: nothing on stderr" test ! -s "$TMP/err"

lib 0 "#1337: one 502 for the first page is retried and the listing is complete" "502" "$list" BEARER="$bearer"
check "listing 502: the retry names the URL, the status and the wait" \
	has "registry-lib: GET ${tags_url} answered HTTP 502; attempt 1 of 4, retrying in 2s" "$TMP/err"
check "listing 502: all four tags" test "$(wc -l <"$TMP/out")" = 4
check "listing 502: the page was asked twice, one wait of 2 s" test "$(asked "$tags_url" curl.log):$(waits)" = "2:2 "

# The first page answers; the second is empty, then HTML, then has no connection.
lib 0 "an empty 200, an HTML 200 and no connection on the SECOND page are retried" \
	"200 good,200 empty,200 html,000" "$list" BEARER="$bearer"
check "listing page 2: says the body was empty, not a traceback" \
	has "registry-lib: GET ${tags_url2} answered 200 with a body that is not JSON (empty); attempt 1 of 4" "$TMP/err"
check "listing page 2: four requests for it, 2 s, 4 s, 8 s" test "$(asked "$tags_url2" curl.log):$(waits)" = "4:2 4 8 "
check "listing page 2: all four tags" test "$(tr '\n' ' ' <"$TMP/out")" = "a b c dev-${commit} "

lib 1 "a listing that keeps answering HTML fails, with one line in place of the traceback" "$(times 4 '200 html')" "$list" BEARER="$bearer"
check "listing html: the final line names the URL and how the body starts" \
	has "registry-lib: GET ${tags_url} answered 200 with a body that is not JSON (76 bytes, starts '<html>?<head><title>502 Bad Gateway</title></head>?<body>nginx</body></html>'); giving up after 4 attempt(s)" "$TMP/err"
check "listing html: bounded at four requests and three waits" test "$(asked "$tags_url" curl.log):$(waits)" = "4:2 4 8 "
check "listing html: nothing on stdout" test ! -s "$TMP/out"

lib 1 "a second page that keeps answering 503 fails the WHOLE listing" "200 good,$(times 4 503)" "$list" BEARER="$bearer"
check "listing page 2 down: the final line names that page and the status" \
	has "registry-lib: GET ${tags_url2} answered HTTP 503; giving up after 4 attempt(s)" "$TMP/err"
check "listing page 2 down: the first page's tags are NOT printed as the listing" test ! -s "$TMP/out"

for code in 401 403 404; do
	lib 1 "a ${code} for the listing is the registry's answer: not retried" "$code" "$list" BEARER="$bearer"
	check "listing ${code}: one request, no wait" test "$(asked "$tags_url" curl.log):$(waits)" = "1:"
	check "listing ${code}: says so" has "registry-lib: GET ${tags_url} answered HTTP ${code}; not retried" "$TMP/err"
done
for code in 408 429 500; do
	lib 0 "a ${code} for the listing is retried" "$code" "$list" BEARER="$bearer"
	check "listing ${code}: two requests" test "$(asked "$tags_url" curl.log)" = 2
done

lib 0 "a JSON document that is not a tag list lists nothing, without a traceback" "200 array" "$list" BEARER="$bearer"
check "listing array: nothing on stdout" test ! -s "$TMP/out"

lib 1 "the pull token never reaches the log" "502,200 html,000,502" "$list" BEARER="$bearer"
check "token: not on stderr" lacks "$bearer" "$TMP/err"
check "token: not on stdout" lacks "$bearer" "$TMP/out"

# --- the sign step's lookup: 13 listings around the listing's own retry (#1337) ----
# registry_commit_tag re-lists 13 times, 5 s apart, because the listing can lag
# the push. Each listing it makes asks a page ONCE: the 13 listings are the
# retry. Against a registry that is down that is 13 requests and 60 s of
# waiting, exactly what it was before a page could retry; with the page retry
# left on underneath it would be 52 requests and 242 s.
find_tag='registry_commit_tag acme/widget "$BEARER" "$FAKE_COMMIT"'
lib 0 "sign step: the commit's tag is found on the first listing" "" "$find_tag" BEARER="$bearer"
check "sign step: only the tag on stdout" test "$(cat "$TMP/out")" = "dev-${commit}"
check "sign step: says how many tags it scanned" has "resolved ${repo}: dev-${commit} (scanned 4 tags, listing 1/13)" "$TMP/err"

lib 0 "sign step: a 502 fails one listing and the next one finds the tag" "502" "$find_tag" BEARER="$bearer"
check "sign step 502: the miss says the listing FAILED, not 'scanned 0 tags'" \
	has "no tag ending in -${commit} in ${repo} yet (the listing FAILED: see the registry-lib line above, listing 1/13); the listing can lag the push, re-listing in 5s" "$TMP/err"
check "sign step 502: the page was not retried underneath (one 5 s wait, no 2 s)" test "$(waits)" = "5 "
check "sign step 502: found on the second listing" has "(scanned 4 tags, listing 2/13)" "$TMP/err"
check "sign step 502: the registry's answer is named" has "registry-lib: GET ${tags_url} answered HTTP 502; giving up after 1 attempt(s)" "$TMP/err"

lib 1 "sign step: a registry that stays down is 13 listings, then the error" "$(times 60 502)" "$find_tag" BEARER="$bearer"
check "down: 13 requests, one per listing" test "$(asked "$tags_url" curl.log)" = 13
check "down: twelve waits of 5 s and nothing else: 60 s" test "$(sort -u "$TMP/fake/sleep.log" | tr '\n' ' '):$(wc -l <"$TMP/fake/sleep.log"):$(sum_waits)" = "5 :12:60"
check "down: the final error says the listing failed" \
	has "::error::no tag ending in -${commit} in ${repo} (the listing FAILED: see the registry-lib line above); still absent after 13 listing(s) 5s apart" "$TMP/err"
check "down: nothing on stdout" test ! -s "$TMP/out"
check "down: no token in the log" lacks "$bearer" "$TMP/err"

# What the default avoids: the page retry left on under the 13 listings.
lib 1 "sign step: with the page retry nested underneath it is 52 requests" "$(times 60 502)" "$find_tag" \
	BEARER="$bearer" REGISTRY_COMMIT_TAG_FETCH_ATTEMPTS=4
check "nested: 52 requests and 242 s of waiting" test "$(asked "$tags_url" curl.log):$(sum_waits)" = "52:242"

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
