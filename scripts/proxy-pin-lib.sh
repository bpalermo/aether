#!/usr/bin/env bash
# The aether-proxy chart pin, as the signature sweep sees it (#984).
#
# This file is SOURCED, never executed: `. scripts/proxy-pin-lib.sh`. It is
# sourced by scripts/verify-published-artifacts.sh and exercised, with no
# network, by scripts/check-proxy-pin.sh.
#
# WHY THE PROXY IS DIFFERENT FROM THE OTHER EIGHT IMAGES
#
# The eight images in GHCR_IMAGE_REPOS are built by publish.yaml on every push
# to main and carry a `*-<aether sha>` tag, so "the image for commit X" is a tag
# lookup. aether-proxy is not: proxy-release.yml builds it only when proxy/
# changes, tags it with the commit that CHANGED the proxy, and a bot PR pins
# its index digest into charts/aether/values.yaml. Every later commit ships
# that same digest until the next proxy release. So for the proxy, "the artifact
# commit X deploys" is the digest pinned in X's values.yaml — nothing else.
#
# THE CUT-OVER
#
# Nothing signed the proxy before #984. Every pin introduced before signing
# existed points at an image that has no signature and never will (the images
# are immutable and nobody re-signs history), so checking them would make the
# sweep permanently red about a fact that cannot change. Those pins are SKIPPED,
# by name, when BOTH hold:
#
#   1. the commit that introduced the pinned digest into values.yaml is an
#      ancestor of (or equal to) PROXY_SIGNING_CUTOVER, and
#   2. the registry holds no signature tag of either layout for the digest.
#
# A pin introduced AFTER the cut-over is always checked — unsigned is red. A
# pre-cut-over pin that does carry a signature is checked too (it is signed, so
# it must verify). "Introduced" is the NEWEST commit reachable from X that
# changed the number of occurrences of the digest in values.yaml, so a revert
# that re-pins an old unsigned digest after the cut-over counts as a new pin and
# goes red, rather than inheriting the old pin's exemption.
#
# PROXY_SIGNING_CUTOVER is the main commit this change was based on (origin/main
# when #984's PR was cut). Any pin introduced after it came from a proxy-release
# run that had the sign job. Once the first signed pin lands, it may be moved
# forward to that pin commit; it must never move backwards.
#
# shellcheck disable=SC2034  # consumed by whoever sources this file.
PROXY_SIGNING_CUTOVER="${PROXY_SIGNING_CUTOVER:-8422b46ff3144a9f8438007909dc1b355460a7a5}"

# shellcheck disable=SC2034  # consumed by whoever sources this file.
PROXY_REPO=bpalermo/aether/aether-proxy

# The chart values file whose pin is checked. The path is the bump-chart job's.
# shellcheck disable=SC2034  # consumed by whoever sources this file.
PROXY_VALUES_PATH=charts/aether/values.yaml

# The digest pinned for the proxy in a values.yaml read on stdin.
#
# Scoped to the block opened by `repository: ghcr.io/bpalermo/aether/aether-proxy`
# and closed by the next `repository:` line (the supervisor image sits right
# below it and carries a digest of its own, which must never be picked up) or by
# six lines, the window the bump-chart sed rewrites.
#
# Prints exactly one `sha256:<64 hex>` and succeeds, or prints nothing and fails:
# no proxy block, no digest in it, a placeholder, or more than one proxy block.
proxy_pinned_digest() {
	local out
	out="$(awk '
		/^[[:space:]]*repository:[[:space:]]*"?ghcr\.io\/bpalermo\/aether\/aether-proxy"?[[:space:]]*$/ {
			blocks++; inblk = 1; n = 0; next
		}
		inblk {
			n++
			if ($0 ~ /^[[:space:]]*repository:/ || n > 6) { inblk = 0; next }
			if ($0 ~ /^[[:space:]]*digest:/) {
				v = $0
				sub(/^[[:space:]]*digest:[[:space:]]*/, "", v)
				gsub(/"/, "", v)
				sub(/[[:space:]]*(#.*)?$/, "", v)
				print v
				inblk = 0
			}
		}
		END { if (blocks != 1) exit 1 }
	')" || return 1
	[[ "$out" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
	printf '%s\n' "$out"
}

# The commit that introduced <digest> into values.yaml, as seen from <sha>: the
# newest commit reachable from <sha> that changed the digest's occurrence count
# AND leaves it present (an ADD, not a removal). Prints nothing when <sha> does
# not pin <digest> at all.
proxy_pin_introduced_by() {
	local sha="$1" digest="$2" c
	git show "${sha}:${PROXY_VALUES_PATH}" 2>/dev/null | grep -qF -- "$digest" || return 0
	while read -r c; do
		if git show "${c}:${PROXY_VALUES_PATH}" 2>/dev/null | grep -qF -- "$digest"; then
			printf '%s\n' "$c"
			return 0
		fi
	done < <(git log --format=%H -S "$digest" "$sha" -- "$PROXY_VALUES_PATH")
}

# Whether the proxy pin at <sha> is checked or skipped.
#
# Usage: proxy_pin_verdict <sha> <digest> <tags of the proxy repo>
#   prints `check`, or `skip <introducing commit>`; exit 2 when it cannot decide
#   (unknown cut-over, pin with no introducing commit) — never a silent skip.
proxy_pin_verdict() {
	local sha="$1" digest="$2" tags="$3" cut intro
	if ! cut="$(git rev-parse --verify --quiet "${PROXY_SIGNING_CUTOVER}^{commit}")"; then
		echo "::error::PROXY_SIGNING_CUTOVER ${PROXY_SIGNING_CUTOVER} is not a commit in this repository" >&2
		return 2
	fi
	intro="$(proxy_pin_introduced_by "$sha" "$digest")"
	if [ -z "$intro" ]; then
		echo "::error::no commit reachable from ${sha} introduced ${digest} into ${PROXY_VALUES_PATH}" >&2
		return 2
	fi
	if git merge-base --is-ancestor "$intro" "$cut" &&
		[ "$(ghcr_signature_layout "$digest" "$tags")" = none ]; then
		printf 'skip %s\n' "$intro"
	else
		printf 'check\n'
	fi
}
