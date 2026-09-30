#!/usr/bin/env bash
# Exercise the publish-verify expected-red control (#930) without the network.
#
# scripts/publish-verify-control.sh is what shows, on every publish-verify run,
# that the artifact gate can still fail. A control is itself a gate, and the
# #853 lesson applies to it too: it must be shown to fail. This pins both
# halves, offline:
#
#   - the REAL verifier, on a fake registry that holds tags but none for the
#     control sha, goes red and the control accepts that red (exit 0). This is
#     #930's option 1: verify_commit driven against a commit with no artifacts,
#     asserting exit 1 plus the MISSING lines, on every CI pass.
#   - the control REJECTS every wrong red and every green:
#       * a mutated verifier that still prints MISSING but no longer counts it
#         (so exits 0) — the vacuous gate;
#       * a red whose absences carry no witness (#985) — a 404 never shown to
#         come from a lookup that can answer "present";
#       * a verifier with a MISSING line naming some other commit;
#       * a verifier that reports one artifact present;
#       * a verifier that prints a perfect red but exits 0;
#       * a verifier that could not complete (exit 2) -> inconclusive, 2.
#     A hand-written CORRECT red is accepted, so each rejection above is for its
#     own defect and not because every stub is rejected.
#   - the real verifier on unreadable repositories, or behind a lookup that
#     404s every tag, never goes red at all: it exits 2 and the control reports
#     inconclusive — a red that proves nothing is not available to accept.
#   - THE SPLIT (proposal 040 phase 2): the verifier reads bazel/img/registry.bzl
#     AS OF the control commit, so a control built on a PRE-cut-over tree goes
#     red on the old registry (ghcr.io, charts/<name>) and one built on a
#     POST-cut-over tree on the new one (quay.io, chart-<name>), and the control
#     accepts each only when every MISSING line names its tree's registry. A
#     perfect red printed against the OTHER registry is rejected, and a POST
#     tree whose registry is unreadable while the old one answers is
#     inconclusive — never a red borrowed from the old registry.
#
# The fake registry is scripts/registry-lib.sh with its network functions
# overridden, placed next to UNMODIFIED copies of the verifier,
# push-heads-lib.sh, proxy-pin-lib.sh and image-registry.sh in a temp dir (the
# verifier sources its libraries from its own directory; IMAGE_REGISTRY_BZL
# points image-registry.sh back at this checkout's bazel/img/registry.bzl for the
# source-time lists; the verifier reads each commit's own). The control commit is
# written to this checkout's object store by the control itself; no ref ever
# points at it, and neither does any PRE/POST base built below.
#
# SC2016 is off for the whole file on purpose: every single-quoted `$…` here is
# the text of a sed pattern or of a generated stub script, which must reach its
# consumer unexpanded.
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --- the fake registry -------------------------------------------------------
reg="$tmp/registry"
mkdir -p "$reg"
cp scripts/verify-published-artifacts.sh scripts/push-heads-lib.sh scripts/proxy-pin-lib.sh scripts/image-registry.sh "$reg/"
cp scripts/registry-lib.sh "$reg/registry-lib.sh"
IMAGE_REGISTRY_BZL="$PWD/bazel/img/registry.bzl"
export IMAGE_REGISTRY_BZL
cat >>"$reg/registry-lib.sh" <<'FAKE'

# --- test overrides: no network ---------------------------------------------
# A registry that has published ONE other commit, completely: every repository
# holds tags, so a missing control tag is a real absence beside a witness that
# answers 200 (#985: the verifier looks tags up by name, registry_tag_exists,
# and backs each 404 with registry_any_tag). No Referrers API (a 404, as on
# ghcr.io), so the tag layouts decide every signature.
# FAKE_EMPTY=1: every repository lists nothing (unreadable).
# FAKE_BROKEN=1: the lookup answers 404 for EVERY tag, even listed ones — the
# shape of a manifest HEAD whose Accept the registry does not like.
# FAKE_ONLY_HOST=<host>: only that registry's repositories are readable; every
# other host lists nothing (the split: the old registry answering, the new one
# not).
# Every lookup must present the token the fake issued, `fake-token`; any other
# value is a 401, which the library reports as inconclusive (#999: a fake that
# took any token let a tag list passed as the token through green).
fake_other=0123456789abcdef0123456789abcdef01234567
registry_registry_token() { printf 'fake-token\n'; }
fake_authorized() {
	[ "$1" = fake-token ] && return 0
	echo "fake registry: 401 for bearer '${1:0:40}'" >&2
	return 2
}
registry_all_tags() {
	fake_authorized "$2" || return 2
	[ "${FAKE_EMPTY:-0}" = 1 ] && return 0
	[ -n "${FAKE_ONLY_HOST:-}" ] && [ "$REGISTRY_HOST" != "$FAKE_ONLY_HOST" ] && return 0
	case "$1" in
	*/charts/* | */chart-*) printf '0.1.0-%s\n' "$fake_other" ;;
	*) printf 'dev-%s\nsha256-%064d.sig\n' "$fake_other" 0 ;;
	esac
}
registry_any_tag() { registry_all_tags "$1" "$2" | head -1; }
registry_tag_exists() {
	fake_authorized "$3" || return 2
	[ "${FAKE_BROKEN:-0}" = 1 ] && return 1
	registry_all_tags "$1" "$3" | grep -qxF -- "$2"
}
registry_manifest_digest() { fake_authorized "$3" && printf 'sha256:%064d\n' 0; }
registry_referrers() {
	fake_authorized "$3" || return 2
	return 1
}
FAKE

# The vacuous gate: the same verifier with absent() no longer counting. It still
# prints MISSING, so only the exit code gives it away.
mut="$tmp/mutated"
mkdir -p "$mut"
cp "$reg"/* "$mut/"
sed -i 's/^\tmissing_total=\$((missing_total + 1))$/\t:/' "$mut/verify-published-artifacts.sh"
if cmp -s "$reg/verify-published-artifacts.sh" "$mut/verify-published-artifacts.sh"; then
	echo "::error::the mutation did not apply — the vacuous-gate case would test nothing" >&2
	exit 2
fi

# The registry HEAD's own tree names (what the control, built on HEAD, expects
# every MISSING line to point at), and the other one of the cut-over pair.
head_bzl="$tmp/head-registry.bzl"
git show HEAD:bazel/img/registry.bzl >"$head_bzl"
pre_bzl="$tmp/pre-registry.bzl"
sed -E \
	-e 's|^IMAGE_REGISTRY = .*|IMAGE_REGISTRY = "ghcr.io"|' \
	-e 's|^IMAGE_NAMESPACE = .*|IMAGE_NAMESPACE = "bpalermo/aether"|' \
	-e 's|^IMAGE_NAME_OVERRIDES = .*|IMAGE_NAME_OVERRIDES = {"proxy": "aether-proxy"}|' \
	-e 's|^CHART_REPOSITORY_PREFIX = .*|CHART_REPOSITORY_PREFIX = "charts/"|' \
	-e '/^SIGNATURE_LAYOUT = /d' \
	-e '/^PROXY_PIN_LEGACY_REFERENCES = /d' \
	bazel/img/registry.bzl >"$pre_bzl"
post_bzl="$PWD/bazel/img/registry.bzl"
prefix_of() { IMAGE_REGISTRY_BZL="$1" scripts/image-registry.sh prefix; }
head_prefix="$(prefix_of "$head_bzl")"
pre_prefix="$(prefix_of "$pre_bzl")"
post_prefix="$(prefix_of "$post_bzl")"
if [ "$pre_prefix" = "$post_prefix" ]; then
	echo "::error::the split cases need two different registries; both settings say ${pre_prefix}" >&2
	exit 2
fi
other_prefix="$pre_prefix"
[ "$head_prefix" = "$pre_prefix" ] && other_prefix="$post_prefix"
export STUB_PREFIX="$head_prefix" STUB_OTHER_PREFIX="$other_prefix"

# base_with <registry.bzl> <message> -> a commit (never referenced) whose tree is
# HEAD's with that registry.bzl: a PRE or POST base for the control.
base_with() {
	local blob tree
	blob="$(git hash-object -w "$1")"
	export GIT_INDEX_FILE="$tmp/index"
	git read-tree HEAD
	git update-index --cacheinfo "100644,${blob},bazel/img/registry.bzl"
	tree="$(git write-tree)"
	unset GIT_INDEX_FILE
	GIT_AUTHOR_NAME=c GIT_AUTHOR_EMAIL=c@invalid GIT_COMMITTER_NAME=c GIT_COMMITTER_EMAIL=c@invalid \
		git commit-tree "$tree" -p HEAD -m "harness: $2"
}
pre_base="$(base_with "$pre_bzl" "a tree from before the Quay cut-over")"
post_base="$(base_with "$post_bzl" "a tree from after the Quay cut-over")"

# Hand-written verifiers for shapes the real one cannot easily be driven into.
# Each takes the control sha as $1, like the real one. Every shape keeps the
# counts right (22 lines, 13 witnesses) so exactly ONE defect is under test.
stub() {
	local name="$1" body="$2"
	printf '#!/usr/bin/env bash\nsha="$1"\n%s\n' "$body" >"$tmp/$name"
	chmod +x "$tmp/$name"
}
sigs='for i in $(seq 1 9); do echo "  MISSING ${STUB_PREFIX}/r$i signature for *-${sha} (no image to sign)"; done'
stub right-red 'for i in $(seq 1 13); do echo "  MISSING ${STUB_PREFIX}/r$i:*-${sha} (looked up directly: 404; witness dev: 200)"; done
'"$sigs"'
echo ""
echo "FAIL: 22 of 22 artifact(s) missing across 1 commit(s)"; exit 1'
stub other-sha 'for i in $(seq 1 12); do echo "  MISSING ${STUB_PREFIX}/r$i:*-${sha} (looked up directly: 404; witness dev: 200)"; done
echo "  MISSING ${STUB_PREFIX}/r13:*-ffffffffffffffffffffffffffffffffffffffff (looked up directly: 404; witness dev: 200)"
'"$sigs"'
echo "FAIL: 22 of 22 artifact(s) missing across 1 commit(s)"; exit 1'
stub one-present 'for i in $(seq 1 12); do echo "  MISSING ${STUB_PREFIX}/r$i:*-${sha} (looked up directly: 404; witness dev: 200)"; done
echo "  ok      ${STUB_PREFIX}/r13:dev-${sha}"
'"$sigs"'
echo "  MISSING ${STUB_PREFIX}/r13 extra (looked up directly: 404; witness dev: 200)"
echo "FAIL: 22 of 22 artifact(s) missing across 1 commit(s)"; exit 1'
# Prints a perfect red and exits 0: only the exit-code assertion can catch it.
stub exit-zero 'for i in $(seq 1 13); do echo "  MISSING ${STUB_PREFIX}/r$i:*-${sha} (looked up directly: 404; witness dev: 200)"; done
'"$sigs"'
echo "FAIL: 22 of 22 artifact(s) missing across 1 commit(s)"; exit 0'
stub inconclusive 'echo "::error::could not list tags for x" >&2; exit 2'
# A perfect red — right sha, right count, every absence witnessed — reported
# against the OTHER registry of the cut-over pair: the gate would be checking
# where the commit never published.
stub wrong-registry 'for i in $(seq 1 13); do echo "  MISSING ${STUB_OTHER_PREFIX}/r$i:*-${sha} (looked up directly: 404; witness dev: 200)"; done
for i in $(seq 1 9); do echo "  MISSING ${STUB_OTHER_PREFIX}/r$i signature for *-${sha} (no image to sign)"; done
echo "FAIL: 22 of 22 artifact(s) missing across 1 commit(s)"; exit 1'
# A perfect red whose absences carry no witness (the pre-#985 line shape): the
# 404s were never shown to come from a lookup that can answer "present".
stub no-witness 'for i in $(seq 1 13); do echo "  MISSING ${STUB_PREFIX}/r$i:*-${sha} (scanned 5 tags)"; done
'"$sigs"'
echo "FAIL: 22 of 22 artifact(s) missing across 1 commit(s)"; exit 1'

# --- cases -------------------------------------------------------------------
control=scripts/publish-verify-control.sh
fail=0
n=0

expect_rc() {
	local name="$1" want="$2"
	shift 2
	local got=0
	"$@" >"$tmp/out" 2>&1 || got=$?
	n=$((n + 1))
	if [ "$got" = "$want" ]; then
		printf '  ok    %s (exit %s)\n' "$name" "$got"
	else
		printf '  FAIL  %s: want exit %s, got %s\n' "$name" "$want" "$got"
		sed 's/^/        | /' "$tmp/out" | tail -15
		fail=1
	fi
}

# 1. The real verifier goes red on a never-published commit and the control
#    accepts it. The verifier's own summary is checked too, not just the verdict.
expect_rc "real verifier, commit absent from a non-empty registry: control accepts" 0 \
	env VERIFIER="$reg/verify-published-artifacts.sh" "$control" HEAD
n=$((n + 1))
if grep -qxF '  | FAIL: 22 of 22 artifact(s) missing across 1 commit(s)' "$tmp/out"; then
	printf '  ok    the real verifier printed FAIL: 22 of 22\n'
else
	printf '  FAIL  the real verifier did not print FAIL: 22 of 22\n'
	fail=1
fi
first="$(sed -nE 's/^expected-red control: commit ([0-9a-f]{40})$/\1/p' "$tmp/out")"

# 2. Same base, same control sha; and the control is never the base itself.
VERIFIER="$reg/verify-published-artifacts.sh" "$control" HEAD >"$tmp/out2" 2>&1
second="$(sed -nE 's/^expected-red control: commit ([0-9a-f]{40})$/\1/p' "$tmp/out2")"
n=$((n + 1))
if [ -n "$first" ] && [ "$first" = "$second" ] && [ "$first" != "$(git rev-parse HEAD)" ]; then
	printf '  ok    control sha is deterministic per base and is not the base\n'
else
	printf '  FAIL  control sha not deterministic, or equals the base: %s vs %s\n' "$first" "$second"
	fail=1
fi

# 3. The vacuous gate: prints MISSING, exits 0.
expect_rc "mutated verifier that no longer counts MISSING: control rejects" 1 \
	env VERIFIER="$mut/verify-published-artifacts.sh" "$control" HEAD
# 4-5. Nothing readable, or a lookup that 404s everything: the real verifier
#      refuses to call that red (no witness -> exit 2), so the control is
#      inconclusive rather than accepting a red that proves nothing (#985).
expect_rc "real verifier on UNREADABLE repositories: never a red, control inconclusive" 2 \
	env FAKE_EMPTY=1 VERIFIER="$reg/verify-published-artifacts.sh" "$control" HEAD
expect_rc "real verifier whose lookup 404s EVERY tag: never a red, control inconclusive" 2 \
	env FAKE_BROKEN=1 VERIFIER="$reg/verify-published-artifacts.sh" "$control" HEAD
# 6. The same red, printed with no witness behind its absences.
expect_rc "absences without a witness: control rejects" 1 \
	env VERIFIER="$tmp/no-witness" "$control" HEAD
# 7-11. Wrong reds, a green, an inconclusive run, and the correct red.
expect_rc "a MISSING line names another commit: control rejects" 1 \
	env VERIFIER="$tmp/other-sha" "$control" HEAD
expect_rc "one artifact reported present: control rejects" 1 \
	env VERIFIER="$tmp/one-present" "$control" HEAD
expect_rc "perfect red output but exit 0: control rejects" 1 \
	env VERIFIER="$tmp/exit-zero" "$control" HEAD
expect_rc "verifier could not complete: control is inconclusive" 2 \
	env VERIFIER="$tmp/inconclusive" "$control" HEAD
expect_rc "hand-written correct red: control accepts" 0 \
	env VERIFIER="$tmp/right-red" "$control" HEAD
expect_rc "a perfect red on the OTHER registry (${other_prefix}): control rejects" 1 \
	env VERIFIER="$tmp/wrong-registry" "$control" HEAD

# 12-15. The split, with the REAL verifier (proposal 040). A control on a PRE
#        tree is red on the old registry, one on a POST tree on the new one —
#        each MISSING line naming its tree's registry (the control asserts it)
#        — and a POST tree whose registry is unreadable while the old one
#        answers is inconclusive, never a red borrowed from the old registry.
split_case() {
	local name="$1" base="$2" want_prefix="$3" n_on
	shift 3
	expect_rc "$name" 0 env "$@" VERIFIER="$reg/verify-published-artifacts.sh" "$control" "$base"
	n=$((n + 1))
	n_on="$(grep -cE "^  \| +MISSING ${want_prefix//./\\.}/" "$tmp/out" || true)"
	if [ "$n_on" = 22 ]; then
		printf '  ok    …all 22 MISSING lines on %s/\n' "$want_prefix"
	else
		printf '  FAIL  …%s of 22 MISSING lines on %s/\n' "$n_on" "$want_prefix"
		sed 's/^/        | /' "$tmp/out" | tail -8
		fail=1
	fi
}
split_case "real verifier, control on a PRE-cut-over tree: red on ${pre_prefix}, control accepts" "$pre_base" "$pre_prefix"
split_case "real verifier, control on a POST-cut-over tree: red on ${post_prefix}, control accepts" "$post_base" "$post_prefix"
expect_rc "real verifier, POST tree, only ${pre_prefix%%/*} readable: inconclusive, never a red from the old registry" 2 \
	env FAKE_ONLY_HOST="${pre_prefix%%/*}" VERIFIER="$reg/verify-published-artifacts.sh" "$control" "$post_base"

if [ "$n" -ne 18 ]; then
	echo "::error::ran ${n} cases, expected 18 -- a gate that checks nothing passes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "::error::the publish-verify expected-red control is wrong; see cases above" >&2
	exit 1
fi
# 7. The proxy pin is NOT part of the control (#984 x #930). Build a base whose
#    chart pins a digest the fake registry has never seen, introduced by a
#    commit AFTER the signing cut-over (so the pin check would look and find
#    nothing). The control must still accept: it sets PROXY_PIN_CHECK=0 and
#    asserts exactly the 22 per-commit coordinates. Then prove the switch is
#    load-bearing: the same verifier with the pin check ON reports the proxy
#    pin MISSING too, which is exactly the red the bot's first signed pin PR
#    (#988) hit in CI.
fake_pin="sha256:$(printf 'f%.0s' $(seq 1 64))"
# The registry the PIN names (it moves with the next proxy release, not with the
# registry flip): the pinned-proxy MISSING line must name it.
pin_ref="$(sed -nE '/^proxy:/,/^[[:space:]]*repository:/ s/^[[:space:]]*repository:[[:space:]]*"?([^"[:space:]]+)"?[[:space:]]*$/\1/p' charts/aether/values.yaml | head -1)"
values_blob="$(git show HEAD:charts/aether/values.yaml | sed -E "s|(aether-proxy@)?sha256:[0-9a-f]{64}|${fake_pin}|" | git hash-object -w --stdin)"
export GIT_INDEX_FILE="$tmp/index"
git read-tree HEAD
git update-index --cacheinfo "100644,${values_blob},charts/aether/values.yaml"
pin_tree="$(git write-tree)"
unset GIT_INDEX_FILE
pin_base="$(GIT_AUTHOR_NAME=c GIT_AUTHOR_EMAIL=c@invalid GIT_COMMITTER_NAME=c GIT_COMMITTER_EMAIL=c@invalid \
	git commit-tree "$pin_tree" -p HEAD -m "harness: pin an unsigned proxy digest after the cut-over")"
expect_rc "a base pinning an unsigned post-cut-over proxy digest: control still accepts (pin check is not the control's)" 0 \
	env VERIFIER="$reg/verify-published-artifacts.sh" "$control" "$pin_base"
ctl_sha="$(grep -oE 'expected-red control: commit [0-9a-f]{40}' "$tmp/out" | head -1 | awk '{print $4}')"
n=$((n + 1))
if [ -n "$ctl_sha" ] && env PROXY_PIN_CHECK=1 PROXY_SIGNING_CUTOVER=HEAD~1 "$reg/verify-published-artifacts.sh" "$ctl_sha" >"$tmp/out2" 2>&1; then
	printf '  FAIL  with the pin check ON the verifier passed a never-signed post-cut-over pin\n'
	fail=1
elif [ -z "$ctl_sha" ]; then
	printf '  FAIL  could not learn the control sha from the previous case\n'
	fail=1
elif grep -qF "MISSING ${pin_ref}@" "$tmp/out2"; then
	printf '  ok    with the pin check ON the same commit is red for the proxy pin too (the switch is load-bearing)\n'
else
	printf '  FAIL  with the pin check ON the proxy pin was not reported MISSING:\n'
	sed 's/^/        | /' "$tmp/out2" | tail -8
	fail=1
fi

echo "publish-verify control: ${n}/${n} cases correct"
