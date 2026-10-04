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
# EVERY PIN IS CHECKED
#
# The sweep checks the pin at every commit it is given: the digest must exist,
# and the index and every child must carry a signature. There is no exemption.
# The pre-signing pins (before #988) all named the pre-cut-over registry, which the reader
# below refuses, and the sweep refuses any commit that predates the Quay
# cut-over (proposal 040 phase 4), so the old PROXY_SIGNING_CUTOVER skip could
# no longer be reached and was removed (#1191).
#
# The proxy's repository and host-qualified image come from the single registry
# setting, bazel/img/registry.bzl (proposal 040), never a literal.
#
# THE PIN NAMES EXACTLY PROXY_IMAGE, image_reference("proxy"). The Quay
# cut-over (proposal 040 phase 2) briefly let it name the pre-cut-over image
# too, until the first proxy release after the flip re-pinned it; phase 4
# removed that allowance. A pin on any other reference is unreadable here (the
# sweep exits 2, the bump-chart rewrite refuses), never looked up elsewhere.
proxy_pin__here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC2034  # consumed by whoever sources this file.
PROXY_REPO="$("${proxy_pin__here}/image-registry.sh" repo proxy)" || PROXY_REPO=""
PROXY_IMAGE="$("${proxy_pin__here}/image-registry.sh" ref proxy)" || PROXY_IMAGE=""

# The chart values file whose pin is checked. The path is the bump-chart job's.
# shellcheck disable=SC2034  # consumed by whoever sources this file.
PROXY_VALUES_PATH=charts/aether/values.yaml

# The pinned proxy reference in a values.yaml read on stdin: `<repository>
# <digest>` on one line.
#
# Scoped to the block opened by `repository: <PROXY_IMAGE>` and closed by the
# next `repository:` line (the supervisor image sits right below it and carries
# a digest of its own, which must never be picked up) or by six lines, the
# window the bump-chart rewrite covers.
#
# Prints exactly one `<PROXY_IMAGE> sha256:<64 hex>` and succeeds, or prints
# nothing and fails: no proxy block, no digest in it, a placeholder, or more
# than one proxy block.
proxy_pinned_ref() {
	local out
	[ -n "$PROXY_IMAGE" ] || return 1
	out="$(awk -v want="$PROXY_IMAGE" '
		/^[[:space:]]*repository:/ {
			v = $0
			sub(/^[[:space:]]*repository:[[:space:]]*/, "", v)
			sub(/[[:space:]]*$/, "", v)
			gsub(/"/, "", v)
			if (v == want) { blocks++; inblk = 1; n = 0; img = v; next }
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
# `repository:` naming PROXY_IMAGE -- and its repository line is rewritten to
# <image>. Inside that block (up to the next `repository:` or six lines) the
# `tag:`, the `digest:` and the "Multi-arch index for" provenance comment are
# rewritten; nothing outside it is touched (the supervisor's digest sits right
# below). Fails, leaving the file unchanged, unless exactly one block was found
# and it got a new tag AND a new digest.
#
# Usage: proxy_pin_rewrite <values file> <image> <tag> <digest>
proxy_pin_rewrite() {
	local file="$1" image="$2" tag="$3" digest="$4" tmp
	[ -n "$PROXY_IMAGE" ] || return 1
	[[ "$image" =~ ^[a-z0-9][a-z0-9.:/_-]*$ ]] || return 1
	[[ "$tag" =~ ^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$ ]] || return 1
	[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
	tmp="$(mktemp)"
	if ! awk -v want="$PROXY_IMAGE" -v image="$image" -v tag="$tag" -v digest="$digest" '
		/^[[:space:]]*repository:/ {
			v = $0
			sub(/^[[:space:]]*repository:[[:space:]]*/, "", v)
			sub(/[[:space:]]*$/, "", v)
			gsub(/"/, "", v)
			if (v == want) {
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
