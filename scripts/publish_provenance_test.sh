#!/usr/bin/env bash
# Hermetic test of scripts/publish-provenance.sh (#1378): publish.yaml attests
# only the digests that have no provenance yet, verifies EVERY digest
# afterwards (a new one against this commit, an unchanged one against "this
# repository's publish workflow attested it"), and says in the run summary which
# is which. A fake `gh` is GitHub's attestation store and `gh attestation
# verify`; `attest` below stands in for actions/attest. No network, no waiting.
#
# What it pins:
#   - every digest new: every one is a subject, in sha256sum format, and each
#     verifies against this commit;
#   - a re-run of the same commit: NO subject, an EMPTY checksums file,
#     `subjects=0` for the workflow's `if:`, and verification still passes;
#   - the next commit, some digests unchanged: the checksums file holds only
#     the new ones; an unchanged digest is verified WITHOUT `--source-digest`
#     (its attestation names the older commit, which the line prints) and a new
#     one WITH it;
#   - the store does not answer (5xx, a 200 that is not a list, nothing): the
#     step FAILS after bounded retries, nothing is skipped and nothing is
#     handed to the attest step; one bad answer followed by a good one passes;
#   - the store lists an attestation that does not verify as this workflow:
#     attested now, with a warning;
#   - verify fails when a new digest has no attestation from this commit, and
#     when a skipped digest has none that verifies; every call passes --limit;
#   - the summary names component, digest, new or unchanged, the commit the
#     provenance names and the tag;
#   - publish.yaml is wired to all of it: the sign steps run publish-sign.sh,
#     the attest step is skipped when `subjects` is 0, no step pins
#     `--source-digest` by itself.
#
# Run: bazel test //scripts:publish_provenance_test
# shellcheck disable=SC2016 # single-quoted $names belong to the fakes' own shells.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/publish-provenance.sh"
# The workflow: a runfile under Bazel (//:ci_definitions), the checkout otherwise.
WORKFLOW="${TEST_SRCDIR:-}/${TEST_WORKSPACE:-_main}/.github/workflows/publish.yaml"
[ -f "$WORKFLOW" ] || WORKFLOW="$HERE/../.github/workflows/publish.yaml"
for f in "$SCRIPT" "$WORKFLOW"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/work" "$TMP/fake"

SLUG=acme/widget
SIGNER="${SLUG}/.github/workflows/publish.yaml"
A=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
B=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

# --- the fake gh -----------------------------------------------------------------
# The store is $FAKE/store, one attestation per line: `<digest> <commit> <signer>`.
#
#   gh api -i repos/<slug>/attestations/<digest>?...
#       200 and a list of one when the store has a line for the digest, 404
#       when it has none. A digest in $FAKE/api.down is a 503 every time, in
#       $FAKE/api.once a 502 the first time only, in $FAKE/api.html a 200 with
#       an HTML body, in $FAKE/api.silent no output at all.
#   gh attestation verify oci://<ref> --repo --signer-workflow --predicate-type
#       --limit --format json [--source-digest <sha>]
#       exit 0 and the JSON of the store's lines for that digest whose signer
#       is the one asked for and, with --source-digest, whose commit is too;
#       exit 1 when there is none.
cat >"$TMP/bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"$FAKE/gh.log"
listed() { [ -f "$2" ] && grep -qxF -- "$1" "$2"; }
if [ "${1:-}" = api ]; then
	[ "${2:-}" = "-i" ] || {
		echo "fake gh: api is only ever asked with -i: $*" >&2
		exit 2
	}
	path="$3"
	case "$path" in
	"repos/${FAKE_SLUG}/attestations/sha256:"*"?per_page=1&predicate_type=https%3A%2F%2Fslsa.dev%2Fprovenance%2Fv1") ;;
	*)
		echo "fake gh: unexpected api path ${path}" >&2
		exit 2
		;;
	esac
	digest="${path#*/attestations/}"
	digest="${digest%%\?*}"
	if listed "$digest" "$FAKE/api.silent"; then
		echo "gh: connection reset" >&2
		exit 1
	fi
	if listed "$digest" "$FAKE/api.down"; then
		printf 'HTTP/2.0 503 Service Unavailable\r\nContent-Type: text/html\r\n\r\n<html>503</html>\n'
		echo "gh: HTTP 503" >&2
		exit 1
	fi
	if listed "$digest" "$FAKE/api.once" && ! listed "$digest" "$FAKE/api.once.seen"; then
		echo "$digest" >>"$FAKE/api.once.seen"
		printf 'HTTP/2.0 502 Bad Gateway\r\n\r\n<html>502</html>\n'
		echo "gh: HTTP 502" >&2
		exit 1
	fi
	if listed "$digest" "$FAKE/api.html"; then
		printf 'HTTP/2.0 200 OK\r\nContent-Type: text/html\r\n\r\n<html>maintenance</html>\n'
		exit 0
	fi
	if grep -q -- "^${digest} " "$FAKE/store" 2>/dev/null; then
		printf 'HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n{"attestations":[{"bundle":{},"repository_id":1}]}\n'
		exit 0
	fi
	printf 'HTTP/2.0 404 Not Found\r\nContent-Type: application/json\r\n\r\n{"message":"Not Found","status":"404"}\n'
	echo "gh: Not Found (HTTP 404)" >&2
	exit 1
fi
if [ "${1:-}" = attestation ] && [ "${2:-}" = verify ]; then
	ref="${3#oci://}"
	digest="${ref##*@}"
	shift 3
	signer="" commit="" repo="" limit="" format=""
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--repo) repo="$2" ;;
		--signer-workflow) signer="$2" ;;
		--source-digest) commit="$2" ;;
		--limit) limit="$2" ;;
		--format) format="$2" ;;
		--predicate-type) ;;
		*)
			echo "fake gh: unexpected flag $1" >&2
			exit 2
			;;
		esac
		shift 2
	done
	if [ "$repo" != "$FAKE_SLUG" ] || [ -z "$limit" ] || [ "$format" != json ]; then
		echo "fake gh: verify needs --repo ${FAKE_SLUG}, --limit and --format json" >&2
		exit 2
	fi
	rows="$(awk -v d="$digest" -v s="$signer" -v c="$commit" \
		'$1 == d && $3 == s && (c == "" || $2 == c) { print $2 }' "$FAKE/store" 2>/dev/null)"
	if [ -z "$rows" ]; then
		echo "Error: verifying with issuer \"sigstore.dev\": no attestation of ${digest} matches the policy" >&2
		exit 1
	fi
	# Newest first, to show the script does not rely on the order.
	printf '%s\n' "$rows" | python3 -c '
import json, sys
out = []
for n, commit in enumerate(reversed(sys.stdin.read().split())):
    run = "https://github.com/acme/widget/actions/runs/%s/attempts/1" % commit[:4]
    out.append({"verificationResult": {
        "signature": {"certificate": {"sourceRepositoryDigest": commit}},
        "verifiedTimestamps": [{"type": "Tlog", "timestamp": "2026-10-0%dT12:00:00-03:00" % (8 - n)}],
        "statement": {"predicate": {
            "buildDefinition": {"resolvedDependencies": [{"digest": {"gitCommit": commit}}]},
            "runDetails": {"metadata": {"invocationId": run}}}}}})
print(json.dumps(out))
'
	exit 0
fi
echo "fake gh: unexpected call: $*" >&2
exit 2
FAKE
cat >"$TMP/bin/sleep" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$1" >>"$FAKE/sleep.log"
FAKE
chmod +x "$TMP/bin/gh" "$TMP/bin/sleep"

# --- the fixture -----------------------------------------------------------------
digest_of() { printf 'sha256:%s' "$(printf '%s' "$1" | sha256sum | cut -d' ' -f1)"; }
# ref <repository> <content>: the reference of that content in that repository.
ref() { printf 'quay.io/acme/%s@%s' "$1" "$(digest_of "$1:$2")"; }

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
has() { grep -qxF -- "$1" "$2"; }
lines() { wc -l <"$1" | tr -d ' '; }
count_in() {
	[ -f "$2" ] || {
		echo 0
		return
	}
	grep -c -- "$1" "$2" || true
}

# reset: an empty store and an empty working directory.
reset() {
	rm -rf "$TMP/fake" "$TMP/work"
	mkdir -p "$TMP/fake" "$TMP/work"
}
# signed <images|charts> <ref> <tag>...: what the sign step would have written.
signed() {
	local kind="$1" r="$2" tag="$3"
	echo "$r" >>"$TMP/work/signed-${kind}.txt"
	printf '%s\t%s\tsigned-now\n' "$r" "$tag" >>"$TMP/work/signed-${kind}.txt.state"
}
# run <want exit> <name> <args...>: the script, in the working directory.
run() {
	local want="$1" name="$2" rc
	shift 2
	rm -f "$TMP/fake/"*.log "$TMP/github-output"
	(
		cd "$TMP/work" || exit 99
		env -u SIGNER_WORKFLOW -u PROVENANCE_ATTEMPTS -u PROVENANCE_INTERVAL -u PROVENANCE_LIMIT -u GH_TOKEN \
			"PATH=$TMP/bin:$PATH" "FAKE=$TMP/fake" "FAKE_SLUG=$SLUG" "GITHUB_REPOSITORY=$SLUG" \
			"GITHUB_OUTPUT=$TMP/github-output" \
			bash "$SCRIPT" "$@" >"$TMP/out" 2>&1
	)
	rc=$?
	if [ "$rc" -ne "$want" ]; then fail "$name: exit $rc, wanted $want"; else pass "$name"; fi
}
subjects() { run "$1" "$2" subjects signed-images.txt signed-charts.txt; }
# attest <commit>: actions/attest, as the workflow runs it -- only when the
# subjects step said there is something to attest, and never on an empty file.
ATTEST_RAN=0
attest() {
	local commit="$1" hex name
	ATTEST_RAN=0
	if grep -qx 'subjects=0' "$TMP/github-output" 2>/dev/null; then
		return 0
	fi
	ATTEST_RAN=1
	if [ ! -s "$TMP/work/provenance-subjects.sha256" ]; then
		fail "the attest step was handed an empty or missing checksums file"
		return 1
	fi
	while read -r hex name; do
		[ -n "$name" ] || continue
		echo "sha256:${hex} ${commit} ${SIGNER}" >>"$TMP/fake/store"
	done <"$TMP/work/provenance-subjects.sha256"
}
verify_calls() { grep -- '^attestation verify ' "$TMP/fake/gh.log" 2>/dev/null || true; }

# --- commit A: every digest is new -----------------------------------------------
reset
agent_a="$(ref agent v1)"
dns_a="$(ref mesh-dns v1)"
reg_a="$(ref registrar v1)"
chart_a="$(ref chart-aether "2.4.12-$A")"
bare_a="$(ref chart-aether "2.4.12 at A")"
signed images "$agent_a" "dev-$A"
signed images "$dns_a" "dev-$A"
signed images "$reg_a" "dev-$A"
signed charts "$chart_a" "2.4.12-$A"
signed charts "$bare_a" "2.4.12"

subjects 0 "A: subjects, every digest new"
check "all new: five subjects for the workflow's if:" has "subjects=5" "$TMP/github-output"
check "all new: none skipped" has "existing=0" "$TMP/github-output"
check "all new: the checksums file is sha256sum format, name without a tag" \
	has "$(digest_of agent:v1 | cut -d: -f2)  quay.io/acme/agent" "$TMP/work/provenance-subjects.sha256"
check "all new: one checksum line per reference, in order" \
	test "$(cut -d' ' -f3 "$TMP/work/provenance-subjects.sha256" | tr '\n' ' ')" = \
	"quay.io/acme/agent quay.io/acme/mesh-dns quay.io/acme/registrar quay.io/acme/chart-aether quay.io/acme/chart-aether "
check "all new: the tally" says "5 signed reference(s): 5 new provenance subject(s), 0 already attested (skipped)"
check "all new: no verify is spent on a digest the store does not know" test "$(count_in '^attestation verify ' "$TMP/fake/gh.log")" = 0
attest "$A"
check "all new: the attest step ran" test "$ATTEST_RAN" = 1
run 0 "A: verify" verify "$A"
check "all new: each of the five is verified against this commit" \
	test "$(verify_calls | grep -c -- "--source-digest $A")" = 5
check "all new: the tally" says "verified the provenance of 5 artifact(s)"
check "all new: every verify fetches up to 1000 attestations, not gh's 30" \
	test "$(verify_calls | grep -c -- '--limit 1000 ')" = 5
check "all new: the signer workflow is pinned" \
	test "$(verify_calls | grep -c -- "--signer-workflow $SIGNER ")" = 5

# --- commit A again: a re-run finds everything attested --------------------------
subjects 0 "A again: subjects"
check "re-run: NOTHING to attest" has "subjects=0" "$TMP/github-output"
check "re-run: five skipped" has "existing=5" "$TMP/github-output"
check "re-run: the checksums file is EMPTY" test ! -s "$TMP/work/provenance-subjects.sha256"
check "re-run: it says so, as a notice" says "::notice::provenance: nothing new to attest -- all 5 artifact(s)"
check "re-run: each skipped line names the commit that built the digest" \
	says "already attested: ${agent_a} (built first by commit ${A} ("
check "re-run: the skip was decided without pinning a commit" \
	test "$(count_in '--source-digest' "$TMP/fake/gh.log")" = 0
before="$(lines "$TMP/fake/store")"
attest "$A"
check "re-run: the attest step is skipped cleanly" test "$ATTEST_RAN" = 0
check "re-run: the store gained nothing" test "$(lines "$TMP/fake/store")" = "$before"
run 0 "A again: verify" verify "$A"
check "re-run: all five still verify" says "verified the provenance of 5 artifact(s)"

# --- commit B: mesh-dns and the per-commit chart changed, the rest did not -------
rm -f "$TMP/work/"signed-*
dns_b="$(ref mesh-dns v2)"
chart_b="$(ref chart-aether "2.4.12-$B")"
signed images "$agent_a" "dev-$B"
signed images "$dns_b" "dev-$B"
signed images "$reg_a" "dev-$B"
signed charts "$chart_b" "2.4.12-$B"
signed charts "$bare_a" "2.4.12"

subjects 0 "B: subjects, a mixed set"
check "mixed: two subjects" has "subjects=2" "$TMP/github-output"
check "mixed: the checksums file holds ONLY the new digests" \
	test "$(cat "$TMP/work/provenance-subjects.sha256")" = \
	"$(printf '%s  quay.io/acme/mesh-dns\n%s  quay.io/acme/chart-aether' "$(digest_of mesh-dns:v2 | cut -d: -f2)" "$(digest_of "chart-aether:2.4.12-$B" | cut -d: -f2)")"
check "mixed: the new ones are listed for the verify step" \
	test "$(cat "$TMP/work/provenance-new.txt")" = "$(printf '%s\n%s' "$dns_b" "$chart_b")"
check "mixed: the unchanged ones are listed too -- skipped is not forgotten" \
	test "$(cat "$TMP/work/provenance-existing.txt")" = "$(printf '%s\n%s\n%s' "$agent_a" "$reg_a" "$bare_a")"
attest "$B"
run 0 "B: verify" verify "$B"
check "mixed: a new digest is verified against THIS commit" \
	test "$(verify_calls | grep -F -- "oci://${dns_b} " | grep -c -- "--source-digest $B")" = 1
check "mixed: an unchanged digest is verified WITHOUT a commit pinned" \
	test "$(verify_calls | grep -F -- "oci://${agent_a} " | grep -c -- '--source-digest')" = 0
check "mixed: and the line names the older commit that built it" \
	says "verified provenance of ${agent_a} (existing: built first by commit ${A} (https://github.com/acme/widget/actions/runs/aaaa/attempts/1, 2026-10-08T12:00:00-03:00))"
check "mixed: the tally" says "verified the provenance of 5 artifact(s)"

# The summary of that run: what an operator reads to know what will roll.
run 0 "B: summary" summary "$B" signed-images.txt.state signed-charts.txt.state
check "summary: the image count" says "1 of 3 image digests are new"
check "summary: the chart count" says "1 of 2 chart digests are new"
check "summary: a new image -- component, digest, new, this commit, the tag" \
	says "| \`mesh-dns\` | \`$(digest_of mesh-dns:v2)\` | **new** | signed by this run | \`${B}\` | \`dev-${B}\` |"
check "summary: an unchanged image names the commit that built it" \
	says "| \`agent\` | \`$(digest_of agent:v1)\` | unchanged | signed by this run | \`${A}\` | \`dev-${B}\` |"
check "summary: the bare chart tag, unchanged" \
	says "| \`chart-aether\` | \`$(digest_of 'chart-aether:2.4.12 at A')\` | unchanged | signed by this run | \`${A}\` | \`2.4.12\` |"

# An unchanged digest attested by B as well (a workflow that attested every
# commit): the origin is the OLDEST, whatever order gh returns them in.
echo "$(digest_of agent:v1) ${B} ${SIGNER}" >>"$TMP/fake/store"
run 0 "B: verify, a digest with two attestations" verify "$B"
check "two attestations: the oldest commit is named, and the count" \
	says "${agent_a} (existing: built first by commit ${A} (https://github.com/acme/widget/actions/runs/aaaa/attempts/1, 2026-10-07T12:00:00-03:00; 2 attestations verify, this is the oldest))"

# --- the store does not answer ---------------------------------------------------
reset
signed images "$agent_a" "dev-$A"
signed images "$dns_a" "dev-$A"
signed charts "$chart_a" "2.4.12-$A"
echo "$(digest_of agent:v1) ${A} ${SIGNER}" >>"$TMP/fake/store"
digest_of mesh-dns:v1 >"$TMP/fake/api.down"
echo >>"$TMP/fake/api.down"
subjects 1 "a lookup the store answers 503 to fails the step"
check "503: asked four times, waiting 5, 10 and 15 s" \
	test "$(count_in "attestations/$(digest_of mesh-dns:v1)" "$TMP/fake/gh.log"):$(tr '\n' ' ' <"$TMP/fake/sleep.log")" = "4:5 10 15 "
check "503: the error names the reference" says "::error::could not tell whether ${dns_a} has a provenance attestation"
check "503: and the answer" says "the attestation store answered HTTP 503 for $(digest_of mesh-dns:v1) (4 attempt(s))"
check "503: not read as attested -- it is in no skip list" \
	test "$(count_in "$(digest_of mesh-dns:v1)" "$TMP/work/provenance-existing.txt")" = 0
check "503: not read as new either" \
	test "$(count_in "$(digest_of mesh-dns:v1)" "$TMP/work/provenance-subjects.sha256")" = 0
check "503: nothing is handed to the workflow's if:" test ! -s "$TMP/github-output"

rm -f "$TMP/fake/api.down"
digest_of mesh-dns:v1 >"$TMP/fake/api.html"
echo >>"$TMP/fake/api.html"
subjects 1 "a 200 that is not a list of attestations fails the step"
check "html: the answer is named" says "answered 200 with a body that is not a list of attestations"

rm -f "$TMP/fake/api.html"
digest_of mesh-dns:v1 >"$TMP/fake/api.silent"
echo >>"$TMP/fake/api.silent"
subjects 1 "no HTTP answer at all fails the step"
check "silent: the answer is named" says "the attestation store gave no HTTP answer"

rm -f "$TMP/fake/api.silent"
digest_of mesh-dns:v1 >"$TMP/fake/api.once"
echo >>"$TMP/fake/api.once"
subjects 0 "one 502, then an answer: the step passes"
check "502 once: one wait" test "$(tr '\n' ' ' <"$TMP/fake/sleep.log")" = "5 "
check "502 once: the digest is new" has "$dns_a" "$TMP/work/provenance-new.txt"
check "502 once: and the attested one is skipped" has "$agent_a" "$TMP/work/provenance-existing.txt"

# --- listed, but not this workflow's ---------------------------------------------
reset
signed images "$agent_a" "dev-$B"
signed charts "$chart_b" "2.4.12-$B"
echo "$(digest_of agent:v1) ${A} ${SLUG}/.github/workflows/other.yaml" >>"$TMP/fake/store"
subjects 0 "an attestation by another workflow is not this workflow's provenance"
check "foreign: attested now" has "$agent_a" "$TMP/work/provenance-new.txt"
check "foreign: with a warning that says why" \
	says "::warning::the attestation store lists provenance for ${agent_a}, but none of it verifies as ${SIGNER}"
check "foreign: two subjects" has "subjects=2" "$TMP/github-output"
attest "$B"
run 0 "foreign: verify passes once this workflow attested it" verify "$B"
run 0 "foreign: summary" summary "$B" signed-images.txt.state signed-charts.txt.state
check "foreign: the summary does not call the digest new" \
	says "| \`agent\` | \`$(digest_of agent:v1)\` | unchanged (provenance attested again) | signed by this run | \`${B}\` | \`dev-${B}\` |"
check "foreign: nor count it" says "0 of 1 image digests are new"

# --- verify must be able to fail --------------------------------------------------
reset
signed images "$agent_a" "dev-$A"
signed charts "$chart_a" "2.4.12-$A"
subjects 0 "subjects, before an attest step that stores nothing"
run 1 "verify fails when a new digest has no attestation" verify "$A"
check "no attestation: every reference is named" \
	test "$(grep -c "::error::no verifiable provenance attestation for " "$TMP/out")" = 2
check "no attestation: asked four times each" test "$(count_in '^attestation verify ' "$TMP/fake/gh.log")" = 8
check "no attestation: the tally" says "::error::2 artifact(s) without verifiable provenance, 0 with"

# Attested, but by another commit: a new digest must name THIS commit.
echo "$(digest_of agent:v1) ${B} ${SIGNER}" >>"$TMP/fake/store"
echo "$(digest_of "chart-aether:2.4.12-$A") ${A} ${SIGNER}" >>"$TMP/fake/store"
run 1 "verify fails when a new digest is attested by another commit only" verify "$A"
check "other commit: that reference is named" says "::error::no verifiable provenance attestation for ${agent_a} from commit ${A}"
check "other commit: the other one verified" says "::error::1 artifact(s) without verifiable provenance, 1 with"

# Skipped as attested, and then the store has nothing that verifies.
reset
signed images "$agent_a" "dev-$A"
signed charts "$chart_a" "2.4.12-$A"
echo "$(digest_of agent:v1) ${A} ${SIGNER}" >>"$TMP/fake/store"
subjects 0 "subjects, one skipped"
attest "$A"
grep -v -- "^$(digest_of agent:v1) " "$TMP/fake/store" >"$TMP/fake/store.new" && mv "$TMP/fake/store.new" "$TMP/fake/store"
run 1 "verify fails when a skipped digest has no verifiable attestation" verify "$A"
check "skipped, then gone: the line says the two disagree" \
	says "::error::${agent_a} was skipped as already attested, but no attestation of it verifies as ${SIGNER}"

# --- inputs ----------------------------------------------------------------------
reset
run 1 "subjects without the sign step's files fails" subjects signed-images.txt signed-charts.txt
check "no files: the error says so" says "::error::signed-images.txt is missing or empty"
echo "quay.io/acme/agent:dev" >"$TMP/work/signed-images.txt"
run 1 "a reference that is not a digest is refused" subjects signed-images.txt
check "tag: the error names it" says "::error::not a <registry>/<repository>@sha256:<digest> reference: quay.io/acme/agent:dev"
rm -f "$TMP/work/"provenance-*
run 1 "verify without the subjects step's lists fails" verify "$A"
check "no lists: the error says so" says "provenance-new.txt / provenance-existing.txt are missing"
: >"$TMP/work/provenance-new.txt"
: >"$TMP/work/provenance-existing.txt"
run 1 "verify of two empty lists is not a pass" verify "$A"
check "empty lists: the error says so" says "::error::verified no attestation"
run 2 "a short sha is refused" verify "${A:0:12}"
run 2 "an unknown mode is refused" attest
run 1 "summary without its inputs fails" summary "$A" signed-images.txt.state signed-charts.txt.state

# --- publish.yaml is wired to it --------------------------------------------------
# step <name>: the text of that step, from its `- name:` line to the next one.
step() {
	awk -v want="      - name: $1" '
		index($0, "      - name: ") == 1 { on = ($0 == want) }
		on { print }
	' "$WORKFLOW"
}
in_step() { step "$1" | grep -qF -- "$2"; }
check "workflow: the image sign step runs publish-sign.sh" \
	in_step "Sign published images (keyless)" 'scripts/publish-sign.sh images "$COMMIT" signed-images.txt'
check "workflow: the chart sign step runs publish-sign.sh" \
	in_step "Sign published charts (keyless)" 'scripts/publish-sign.sh charts "$COMMIT" signed-charts.txt'
check "workflow: no step signs by itself any more" \
	test "$(grep -c 'bazel run //bazel/cosign -- sign' "$WORKFLOW")" = 0
check "workflow: the subjects step has the id the attest step reads" in_step "Provenance subjects" "id: provenance"
check "workflow: the subjects step runs the script on both lists" \
	in_step "Provenance subjects" "scripts/publish-provenance.sh subjects signed-images.txt signed-charts.txt"
check "workflow: the attest step is skipped when there is no subject" \
	in_step "Attest build provenance" "if: steps.provenance.outputs.subjects != '0'"
check "workflow: the attest step reads the script's checksums file" \
	in_step "Attest build provenance" "subject-checksums: provenance-subjects.sha256"
check "workflow: the verify step runs the script" \
	in_step "Verify the provenance attestations" 'scripts/publish-provenance.sh verify "$COMMIT"'
check "workflow: no step pins --source-digest by itself" test "$(grep -c -- '--source-digest "' "$WORKFLOW")" = 0
check "workflow: the summary step writes the run summary" \
	in_step "Summary of new and unchanged digests" 'scripts/publish-provenance.sh summary "$COMMIT" signed-images.txt.state signed-charts.txt.state >> "$GITHUB_STEP_SUMMARY"'

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
