#!/usr/bin/env bash
# Fails if any Helm chart under charts/<name>/ was modified relative to BASE
# without its Chart.yaml `version:` being bumped to a strictly higher SemVer.
#
# The in-repo version is "MAJOR.MINOR.PATCH-{GIT_COMMIT}" (the suffix is a literal
# placeholder stamped only at publish time), so the SemVer base is what we compare.
#
# Usage: scripts/check-chart-version-bump.sh [BASE_REF]
#   BASE_REF defaults to origin/main. CI passes the PR base SHA.
set -euo pipefail

BASE="${1:-origin/main}"

# Extract MAJOR.MINOR.PATCH from a `version: "X.Y.Z-..."` line.
semver() { sed -nE 's/^version:[[:space:]]*"?([0-9]+\.[0-9]+\.[0-9]+).*/\1/p'; }

fail=0
shopt -s nullglob
for chartfile in charts/*/Chart.yaml; do
  chartdir="$(dirname "$chartfile")"

  # Nothing under this chart changed vs BASE -> no bump required.
  if git diff --quiet "$BASE" -- "$chartdir"; then
    continue
  fi

  new_ver="$(semver <"$chartfile")"
  # A brand-new chart (absent at BASE) has no prior version: the presence of a
  # version is enough.
  if ! old_raw="$(git show "$BASE:$chartfile" 2>/dev/null)"; then
    echo "OK: new chart $chartdir at version $new_ver"
    continue
  fi
  old_ver="$(printf '%s\n' "$old_raw" | semver)"

  if [ -z "$new_ver" ]; then
    echo "ERROR: $chartfile has no parseable 'version:' (MAJOR.MINOR.PATCH)"
    fail=1
    continue
  fi

  if [ "$old_ver" = "$new_ver" ]; then
    echo "ERROR: $chartdir changed but $chartfile version was not bumped (still $new_ver)."
    echo "       Bump the SemVer base in $chartfile (e.g. via 'make' or by hand)."
    fail=1
    continue
  fi

  # Require the new version to be strictly higher (not just different).
  highest="$(printf '%s\n%s\n' "$old_ver" "$new_ver" | sort -V | tail -1)"
  if [ "$highest" != "$new_ver" ]; then
    echo "ERROR: $chartfile version went backwards: $old_ver -> $new_ver"
    fail=1
    continue
  fi

  echo "OK: $chartdir changed and version bumped: $old_ver -> $new_ver"
done

if [ "$fail" -ne 0 ]; then
  echo
  echo "A chart was modified without bumping its Chart.yaml version. See errors above."
  exit 1
fi
echo "chart-version-bump check passed."
