#!/usr/bin/env bash
# The aether-proxy chart pin, as the signature sweep sees it (#984).
#
# This file is SOURCED, never executed: `. scripts/proxy-pin-lib.sh`. It is
# sourced by scripts/verify-published-artifacts.sh and exercised, with no
# network, by scripts/check-proxy-pin.sh.
#
# WHY THE PROXY IS DIFFERENT FROM THE OTHER NINE IMAGES
#
# The nine images in REGISTRY_IMAGE_REPOS are built by publish.yaml on every push
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
# PROXY_SIGNING_CUTOVER is the commit that pinned the FIRST signed aether-proxy
# image: #988 (2026-09-27), index sha256:574d5211…, signed by proxy-release.yml
# and hand-verified (index + both children) under the proxy-release identity.
# Every pin introduced after it came from a proxy-release run that had the sign
# job. Pins at or before it with no signature are history and are skipped (see
# proxy_pin_verdict). It moved forward once, from #986's base 8422b46 to this
# pin's merge commit; it must never move backwards.
#
# shellcheck disable=SC2034  # consumed by whoever sources this file.
PROXY_SIGNING_CUTOVER="${PROXY_SIGNING_CUTOVER:-857e65988acda3c84ec33ae2e8eace2fdbf0407a}"
#
# The proxy's repository and host-qualified image come from the single registry
# setting, bazel/img/registry.bzl (proposal 040), never a literal.
#
# WHICH REGISTRY A PIN IS ON is whatever the pin says. The pin is data only the
# bump-chart job writes, so the Quay cut-over (proposal 040 phase 2) could not
# move it: until the first proxy release after the flip re-pins it, it names
# the ghcr.io image. PROXY_PIN_REFS is every reference a pin may name --
# image_reference("proxy") first, then PROXY_PIN_LEGACY_REFERENCES (both from
# registry.bzl, via `image-registry.sh proxy-pin-refs`) -- and the sweep looks
# the pinned digest up on the host the pin itself names.
proxy_pin__here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC2034  # consumed by whoever sources this file.
PROXY_REPO="$("${proxy_pin__here}/image-registry.sh" repo proxy)" || PROXY_REPO=""
PROXY_IMAGE="$("${proxy_pin__here}/image-registry.sh" ref proxy)" || PROXY_IMAGE=""
PROXY_PIN_REFS=()
if proxy_pin__refs="$("${proxy_pin__here}/image-registry.sh" proxy-pin-refs)"; then
	mapfile -t PROXY_PIN_REFS <<<"$proxy_pin__refs"
fi
unset proxy_pin__refs

# The chart values file whose pin is checked. The path is the bump-chart job's.
# shellcheck disable=SC2034  # consumed by whoever sources this file.
PROXY_VALUES_PATH=charts/aether/values.yaml

# The pinned proxy reference in a values.yaml read on stdin: `<repository>
# <digest>` on one line.
#
# Scoped to the block opened by `repository: <ref>` for a <ref> in
# PROXY_PIN_REFS and closed by the next `repository:` line (the supervisor image
# sits right below it and carries a digest of its own, which must never be
# picked up) or by six lines, the window the bump-chart rewrite covers.
#
# Prints exactly one `<ref> sha256:<64 hex>` and succeeds, or prints nothing and
# fails: no proxy block, no digest in it, a placeholder, or more than one proxy
# block (under any accepted reference).
proxy_pinned_ref() {
	local out refs
	[ "${#PROXY_PIN_REFS[@]}" -gt 0 ] || return 1
	refs="$(printf '%s\n' "${PROXY_PIN_REFS[@]}")"
	out="$(awk -v refs="$refs" '
		BEGIN { n = split(refs, r, "\n"); for (i = 1; i <= n; i++) if (r[i] != "") ok[r[i]] = 1 }
		/^[[:space:]]*repository:/ {
			v = $0
			sub(/^[[:space:]]*repository:[[:space:]]*/, "", v)
			sub(/[[:space:]]*$/, "", v)
			gsub(/"/, "", v)
			if (v in ok) { blocks++; inblk = 1; n = 0; img = v; next }
		}
		inblk {
			n++
			if ($0 ~ /^[[:space:]]*repository:/ || n > 6) { inblk = 0; next }
			if ($0 ~ /^[[:space:]]*digest:/) {
				v = $0
				sub(/^[[:space:]]*digest:[[:space:]]*/, "", v)
				gsub(/"/, "", v)
				sub(/[[:space:]]*(#.*)?$/, "", v)
				print img " " v
				inblk = 0
			}
		}
		END { if (blocks != 1) exit 1 }
	')" || return 1
	[[ "$out" =~ ^[a-z0-9][a-z0-9.:/_-]*\ sha256:[0-9a-f]{64}$ ]] || return 1
	printf '%s\n' "$out"
}

# The digest alone (see proxy_pinned_ref).
proxy_pinned_digest() {
	local out
	out="$(proxy_pinned_ref)" || return 1
	printf '%s\n' "${out#* }"
}

# Rewrite the proxy pin in <values file> to <image>:<tag> @ <digest>, in place:
# the bump-chart job's edit (proxy-release.yml).
#
# The block is found exactly as proxy_pinned_ref finds it -- the ONE
# `repository:` naming a PROXY_PIN_REFS entry -- so the first release after the
# Quay cut-over finds the ghcr.io pin and moves it, repository line included,
# to image_reference("proxy"). Inside that block (up to the next `repository:`
# or six lines) the `tag:`, the `digest:` and the "Multi-arch index for"
# provenance comment are rewritten; nothing outside it is touched (the
# supervisor's digest sits right below). Fails, leaving the file unchanged,
# unless exactly one block was found and it got a new tag AND a new digest.
#
# Usage: proxy_pin_rewrite <values file> <image> <tag> <digest>
proxy_pin_rewrite() {
	local file="$1" image="$2" tag="$3" digest="$4" refs tmp
	[ "${#PROXY_PIN_REFS[@]}" -gt 0 ] || return 1
	[[ "$image" =~ ^[a-z0-9][a-z0-9.:/_-]*$ ]] || return 1
	[[ "$tag" =~ ^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$ ]] || return 1
	[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
	refs="$(printf '%s\n' "${PROXY_PIN_REFS[@]}")"
	tmp="$(mktemp)"
	if ! awk -v refs="$refs" -v image="$image" -v tag="$tag" -v digest="$digest" '
		BEGIN { n = split(refs, r, "\n"); for (i = 1; i <= n; i++) if (r[i] != "") ok[r[i]] = 1 }
		/^[[:space:]]*repository:/ {
			v = $0
			sub(/^[[:space:]]*repository:[[:space:]]*/, "", v)
			sub(/[[:space:]]*$/, "", v)
			gsub(/"/, "", v)
			if (v in ok) {
				blocks++; inblk = 1; n = 0
				ind = $0; sub(/repository:.*/, "", ind)
				print ind "repository: " image
				next
			}
			inblk = 0
		}
		inblk {
			n++
			if (n > 6) { inblk = 0 }
			else if ($0 ~ /^[[:space:]]*tag:/) {
				ind = $0; sub(/tag:.*/, "", ind); print ind "tag: " tag; tags++; next
			} else if ($0 ~ /^[[:space:]]*digest:/) {
				ind = $0; sub(/digest:.*/, "", ind); print ind "digest: \"" digest "\""; digests++; next
			} else if ($0 ~ /helper prefers digest over tag\./) {
				sub(/helper prefers digest over tag\..*/, "helper prefers digest over tag. Multi-arch index for " tag ".")
			}
		}
		{ print }
		END { if (blocks != 1 || tags != 1 || digests != 1) exit 1 }
	' "$file" >"$tmp"; then
		rm -f "$tmp"
		return 1
	fi
	cat "$tmp" >"$file"
	rm -f "$tmp"
}

# The commit that introduced <digest> into values.yaml, as seen from <sha>: the
# newest commit reachable from <sha> that changed the digest's occurrence count
# AND leaves it present (an ADD, not a removal). Prints nothing when <sha> does
# not pin <digest> at all.
# `grep -c`, never `grep -q`, after a pipe: under a caller's pipefail (the
# verifier sets it) `grep -q` exits on the first match, the writer can take a
# SIGPIPE, and the pipeline reports a HIT as a miss -- seen as "no commit
# introduced <digest>" on a merge-commit HEAD.
proxy_pin_introduced_by() {
	local sha="$1" digest="$2" c
	git show "${sha}:${PROXY_VALUES_PATH}" 2>/dev/null | grep -cF -- "$digest" >/dev/null || return 0
	while read -r c; do
		if git show "${c}:${PROXY_VALUES_PATH}" 2>/dev/null | grep -cF -- "$digest" >/dev/null; then
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
	# Tag layouts only (below): every pin from before PROXY_SIGNING_CUTOVER lives
	# on ghcr.io, which has no Referrers API, so a referrer can neither exempt nor
	# condemn one.
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
		[ "$(registry_signature_layout "$digest" "$tags")" = none ]; then
		printf 'skip %s\n' "$intro"
	else
		printf 'check\n'
	fi
}
