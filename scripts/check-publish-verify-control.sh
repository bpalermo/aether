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
#
# The fake registry is scripts/ghcr-lib.sh with its network functions
# overridden, placed next to UNMODIFIED copies of the verifier and
# push-heads-lib.sh in a temp dir (the verifier sources its libraries from its
# own directory). The control commit is written to this checkout's object store
# by the control itself; no ref ever points at it.
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
cp scripts/verify-published-artifacts.sh scripts/push-heads-lib.sh scripts/proxy-pin-lib.sh "$reg/"
cp scripts/ghcr-lib.sh "$reg/ghcr-lib.sh"
cat >>"$reg/ghcr-lib.sh" <<'FAKE'

# --- test overrides: no network ---------------------------------------------
# A registry that has published ONE other commit, completely: every repository
# holds tags, so a missing control tag is a real absence beside a witness that
# answers 200 (#985: the verifier looks tags up by name, ghcr_tag_exists, and
# backs each 404 with ghcr_any_tag).
# FAKE_EMPTY=1: every repository lists nothing (unreadable).
# FAKE_BROKEN=1: the lookup answers 404 for EVERY tag, even listed ones — the
# shape of a manifest HEAD whose Accept the registry does not like.
fake_other=0123456789abcdef0123456789abcdef01234567
ghcr_registry_token() { printf 'fake-token\n'; }
ghcr_all_tags() {
	[ "${FAKE_EMPTY:-0}" = 1 ] && return 0
	case "$1" in
	*/charts/*) printf '0.1.0-%s\n' "$fake_other" ;;
	*) printf 'dev-%s\nsha256-%064d.sig\n' "$fake_other" 0 ;;
	esac
}
ghcr_any_tag() { ghcr_all_tags "$1" | head -1; }
ghcr_tag_exists() {
	[ "${FAKE_BROKEN:-0}" = 1 ] && return 1
	ghcr_all_tags "$1" | grep -qxF -- "$2"
}
ghcr_manifest_digest() { printf 'sha256:%064d\n' 0; }
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

# Hand-written verifiers for shapes the real one cannot easily be driven into.
# Each takes the control sha as $1, like the real one. Every shape keeps the
# counts right (20 lines, 12 witnesses) so exactly ONE defect is under test.
stub() {
	local name="$1" body="$2"
	printf '#!/usr/bin/env bash\nsha="$1"\n%s\n' "$body" >"$tmp/$name"
	chmod +x "$tmp/$name"
}
sigs='for i in $(seq 1 8); do echo "  MISSING ghcr.io/r$i signature for *-${sha} (no image to sign)"; done'
stub right-red 'for i in $(seq 1 12); do echo "  MISSING ghcr.io/r$i:*-${sha} (looked up directly: 404; witness dev: 200)"; done
'"$sigs"'
echo ""
echo "FAIL: 20 of 20 artifact(s) missing across 1 commit(s)"; exit 1'
stub other-sha 'for i in $(seq 1 11); do echo "  MISSING ghcr.io/r$i:*-${sha} (looked up directly: 404; witness dev: 200)"; done
echo "  MISSING ghcr.io/r12:*-ffffffffffffffffffffffffffffffffffffffff (looked up directly: 404; witness dev: 200)"
'"$sigs"'
echo "FAIL: 20 of 20 artifact(s) missing across 1 commit(s)"; exit 1'
stub one-present 'for i in $(seq 1 11); do echo "  MISSING ghcr.io/r$i:*-${sha} (looked up directly: 404; witness dev: 200)"; done
echo "  ok      ghcr.io/r12:dev-${sha}"
'"$sigs"'
echo "  MISSING ghcr.io/r12 extra (looked up directly: 404; witness dev: 200)"
echo "FAIL: 20 of 20 artifact(s) missing across 1 commit(s)"; exit 1'
# Prints a perfect red and exits 0: only the exit-code assertion can catch it.
stub exit-zero 'for i in $(seq 1 12); do echo "  MISSING ghcr.io/r$i:*-${sha} (looked up directly: 404; witness dev: 200)"; done
'"$sigs"'
echo "FAIL: 20 of 20 artifact(s) missing across 1 commit(s)"; exit 0'
stub inconclusive 'echo "::error::could not list tags for x" >&2; exit 2'
# A perfect red whose absences carry no witness (the pre-#985 line shape): the
# 404s were never shown to come from a lookup that can answer "present".
stub no-witness 'for i in $(seq 1 12); do echo "  MISSING ghcr.io/r$i:*-${sha} (scanned 5 tags)"; done
'"$sigs"'
echo "FAIL: 20 of 20 artifact(s) missing across 1 commit(s)"; exit 1'

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
if grep -qxF '  | FAIL: 20 of 20 artifact(s) missing across 1 commit(s)' "$tmp/out"; then
	printf '  ok    the real verifier printed FAIL: 20 of 20\n'
else
	printf '  FAIL  the real verifier did not print FAIL: 20 of 20\n'
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

if [ "$n" -ne 12 ]; then
	echo "::error::ran ${n} cases, expected 12 -- a gate that checks nothing passes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "::error::the publish-verify expected-red control is wrong; see cases above" >&2
	exit 1
fi
echo "publish-verify control: ${n}/${n} cases correct"
