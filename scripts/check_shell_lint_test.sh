#!/usr/bin/env bash
# Hermetic test of scripts/check-shell-lint.sh: no Bazel query, no git
# repository. It runs a copy of the script inside a throwaway tree, with fake
# `bazel` and `git` on PATH serving the two inputs the check compares.
#
# The case this exists for is #1297: the covered-set lookup used to be
# `printf '%s\n' "$covered" | grep -qxF "$f"` under `set -euo pipefail`. grep -q
# exits at its first match; when printf still has output left it dies of
# SIGPIPE, the pipeline's status is 141, and a COVERED file is reported as
# uncovered. That only happens when the covered set outgrows the pipe buffer and
# the match is near the top, so the fake query prints ~1 MiB of labels with the
# real ones sorting first: the old shape fails here every time, not by luck.
#
#   1. every shell file covered       -> exit 0, "OK: all 4 ..."
#   2. one file outside every target  -> exit 1, names exactly that file
#   3. the query returns nothing      -> exit 1 (the #853 state)
#   4. anti-vacuity: the same tree with the lookup put back to the old
#      `printf | grep -q` shape must report a covered file as uncovered, or this
#      test no longer exercises the trap.
#
# Run: bazel test //scripts:check_shell_lint_test, or
#      bash scripts/check_shell_lint_test.sh
# shellcheck disable=SC2016 # single-quoted "$f"/"$covered" are the checked
# script's own text, matched and rewritten, never expanded here.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/check-shell-lint.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}

# The tree: the script under scripts/ (it cd's to its parent), four shell files
# (two by extension, one by shebang only, one more by extension), a non-shell
# file, and a //proxy script the check must skip.
repo="$TMP/repo"
mkdir -p "$repo/scripts" "$repo/e2e" "$repo/tools" "$repo/proxy" "$TMP/bin"
cp "$SCRIPT" "$repo/scripts/check-shell-lint.sh"
printf '#!/usr/bin/env bash\n' >"$repo/scripts/a.sh"
printf '#!/usr/bin/env bash\n' >"$repo/e2e/b.sh"
printf '#!/bin/sh\necho hi\n' >"$repo/tools/runme"
printf 'not shell\n' >"$repo/tools/README"
printf '#!/usr/bin/env bash\n' >"$repo/proxy/skip.sh"

# Fake git: `ls-files` lists the tree (relative paths, as git does).
cat >"$TMP/bin/git" <<'EOF'
#!/usr/bin/env bash
[ "$1" = ls-files ] || { echo "fake git: unexpected $*" >&2; exit 1; }
find . -type f | sed 's|^\./||' | sort
EOF

# Fake bazel: `query` prints the labels in $FAKE_LABELS, then ~1 MiB of padding
# labels that sort after them (so grep -q matches long before printf is done).
cat >"$TMP/bin/bazel" <<'EOF'
#!/usr/bin/env bash
[ "$1" = query ] || { echo "fake bazel: unexpected $*" >&2; exit 1; }
[ -s "$FAKE_LABELS" ] || exit 0
cat "$FAKE_LABELS"
i=0
while [ "$i" -lt 40000 ]; do
	printf '//zz/padding:pad_%06d_xxxxxxxxxxxxxxxx.sh\n' "$i"
	i=$((i + 1))
done
EOF
chmod +x "$TMP/bin/git" "$TMP/bin/bazel"

run_check() { # run_check <script> <labels...> -> rc, output in $TMP/out
	local script="$1"
	shift
	printf '%s\n' "$@" | sed '/^$/d' >"$TMP/labels"
	PATH="$TMP/bin:$PATH" FAKE_LABELS="$TMP/labels" BAZEL=bazel \
		bash "$script" >"$TMP/out" 2>&1
}

all_labels=(//scripts:a.sh //scripts:check-shell-lint.sh //e2e:b.sh //tools:runme)

# --- 1. everything covered ------------------------------------------------------
run_check "$repo/scripts/check-shell-lint.sh" "${all_labels[@]}"
rc=$?
if [ "$rc" -eq 0 ] && grep -q '^OK: all 4 root-workspace shell script(s) are covered' "$TMP/out" &&
	! grep -q uncovered "$TMP/out"; then
	pass "all covered: exit 0, 4 scripts, nothing uncovered"
else
	fail "all covered (rc=$rc)"
	sed 's/^/    /' "$TMP/out"
fi

# --- 2. one file outside every target -------------------------------------------
run_check "$repo/scripts/check-shell-lint.sh" //scripts:a.sh //scripts:check-shell-lint.sh //e2e:b.sh
rc=$?
if [ "$rc" -eq 1 ] && grep -q '^uncovered: tools/runme ' "$TMP/out" &&
	[ "$(grep -c '^uncovered:' "$TMP/out")" -eq 1 ]; then
	pass "one uncovered: exit 1, names only tools/runme"
else
	fail "one uncovered (rc=$rc)"
	sed 's/^/    /' "$TMP/out"
fi

# --- 3. the query hands the aspect nothing --------------------------------------
run_check "$repo/scripts/check-shell-lint.sh"
rc=$?
if [ "$rc" -eq 1 ] && grep -q 'nothing is being linted' "$TMP/out"; then
	pass "empty query: exit 1"
else
	fail "empty query (rc=$rc)"
	sed 's/^/    /' "$TMP/out"
fi

# --- 4. anti-vacuity: the old pipe shape fails on this same input ---------------
# Put the #1297 lookup back into a copy and expect the false "uncovered". If this
# ever passes, the fixture no longer triggers SIGPIPE and case 1 proves nothing.
# A second tree with the same files, so the old copy is not itself a new,
# uncovered script that would make this case pass for the wrong reason.
cp -R "$repo" "$TMP/repo-old"
old="$TMP/repo-old/scripts/check-shell-lint.sh"
sed -e 's|grep -qxF -- "\$f" <<<"\$covered"|printf '"'"'%s\\n'"'"' "$covered" \| grep -qxF "$f"|' \
	"$SCRIPT" >"$old"
if cmp -s "$old" "$SCRIPT" || ! grep -qF '| grep -qxF "$f"' "$old"; then
	fail "anti-vacuity: could not rebuild the old printf | grep -q shape (did the lookup change?)"
else
	run_check "$old" "${all_labels[@]}"
	rc=$?
	# Every file IS covered, so any "uncovered" line is the SIGPIPE false negative.
	if [ "$rc" -ne 0 ] && grep -q '^uncovered:' "$TMP/out"; then
		pass "anti-vacuity: the old printf | grep -q shape reports a covered file as uncovered here"
	else
		fail "anti-vacuity: the old shape passed (rc=$rc), so this input no longer triggers SIGPIPE"
		sed 's/^/    /' "$TMP/out"
	fi
fi

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS failure(s)"
	exit 1
fi
echo "all passed"
