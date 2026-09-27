#!/usr/bin/env bash
# Exercise the direct tag lookup (ghcr_tag_exists, #985) — and the listing race
# it replaced — against a fake registry. No network.
#
# THE RACE. verify-published-artifacts.sh used to page through every tag of a
# repository and grep for the ones it expected. A tag listing is not a snapshot:
# on 2026-09-27 a sweep that overlapped a publish reported cni-install's
# d526bf2 signature MISSING, and a re-run minutes later passed (#985). Case 1
# reproduces the shape with a registry whose listing cursor is positional (the
# page after `last=X` starts at the offset X was served at) while a publish
# writes tags between page 1 and page 2: it appends the new commit's tags and
# re-points the mutable `dev` tag, which ghcr lists FIRST (observed). Every
# entry after it moves up one place, the page boundary slides past one existing
# tag, and the walk never sees it. Case 1 asserts the OLD lookup (walk + grep)
# reports that present tag missing — if the fake ever stops reproducing the
# race, this fails instead of case 2 passing vacuously. Case 2 asserts the NEW
# lookup finds the same tag in the same mid-publish registry.
#
# Then ghcr_tag_exists' three answers (200 present / 404 absent / anything else
# inconclusive — never "absent"), ghcr_signature_layout_direct over every
# layout, and the real verifier end to end on this checkout's HEAD: a complete
# registry passes with the exact lookup count, one missing signature is exit 1
# with a MISSING line, an unanswered lookup is exit 2, an absent image carries
# its witness (a listed tag answering 200), and a lookup that 404s even a
# listed tag is exit 2 rather than MISSING.
#
# The fake is a `curl` shell function: ghcr-lib.sh's functions call `curl` by
# name, so it replaces the network for them. The verifier sources its libraries
# from its own directory, so for the end-to-end cases it runs from a temp copy
# whose ghcr-lib.sh has the same function appended (the #983 seam).
#
# SC2016 is off on purpose: the fake's text is single-quoted so it reaches the
# generated library unexpanded.
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
# shellcheck disable=SC1091
. scripts/ghcr-lib.sh

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export FAKE="$tmp/registry"
mkdir -p "$FAKE"

# --- the fake registry -------------------------------------------------------
#
# State lives in $FAKE because ghcr-lib.sh calls curl inside $(...) subshells.
#   tags         the repository's tag list, in registry order
#   pagesize     entries per /tags/list page (ghcr silently caps `n`)
#   after_page1  if present, REPLACES tags once page 1 has been served: the
#                publish that runs while the walk is in flight
#   everything   if present, every tag exists except `*.sig` ones (a complete
#                cosign-3 registry for the end-to-end cases) and those in
#   absent       tags that 404 regardless
#   nothing      if present, EVERY manifest lookup 404s (a lookup that has lost
#                its Accept header, say) — the listing still works
#   codes        `<tag> <http code>` overrides (000 = no connection)
#   accept       the Accept header of the last manifest request
fake_curl='
curl() {
	local url="" hdr_out="" wfmt="" head=0 fail=0 accept="" a
	while [ "$#" -gt 0 ]; do
		a="$1"
		shift
		case "$a" in
		-D) hdr_out="$1"; shift ;;
		-o | -u | --retry) shift ;;
		-w) wfmt="$1"; shift ;;
		-H)
			case "$1" in [Aa]ccept:*) accept="${1#*: }" ;; esac
			shift
			;;
		-I) head=1 ;;
		-f*) fail=1; [[ "$a" == *I* ]] && head=1 ;;
		https://*) url="$a" ;;
		esac
	done
	case "$url" in
	*/token\?*) printf "{\"token\":\"fake\"}\n"; return 0 ;;
	*/tags/list*)
		local last="" off=0 ps total
		[[ "$url" == *"last="* ]] && last="${url##*last=}" && last="${last%%&*}"
		if [ -n "$last" ]; then
			off="$(awk -v t="$last" "\$1 == t { print \$2 }" "$FAKE/cursors" | tail -1)"
		else
			: >"$FAKE/cursors"
			echo 0 >"$FAKE/pages"
		fi
		ps="$(cat "$FAKE/pagesize")"
		total="$(grep -c . "$FAKE/tags")"
		local page
		page="$(sed -n "$((off + 1)),$((off + ps))p" "$FAKE/tags")"
		printf "%s %s\n" "$(printf "%s\n" "$page" | tail -1)" "$((off + ps))" >>"$FAKE/cursors"
		: >"$hdr_out"
		[ "$((off + ps))" -lt "$total" ] && printf "Link: </v2/x/tags/list?n=0>; rel=\"next\"\r\n" >"$hdr_out"
		printf "%s\n" "$page" | python3 -c "import sys,json;print(json.dumps({\"tags\":[l.strip() for l in sys.stdin if l.strip()]}))"
		echo "$(($(cat "$FAKE/pages") + 1))" >"$FAKE/pages"
		if [ "$(cat "$FAKE/pages")" = 1 ] && [ -f "$FAKE/after_page1" ]; then
			cp "$FAKE/after_page1" "$FAKE/tags"
		fi
		return 0
		;;
	*/manifests/*)
		local ref="${url##*/manifests/}" code
		printf "%s\n" "$accept" >"$FAKE/accept"
		code="$(awk -v t="$ref" "\$1 == t { print \$2 }" "$FAKE/codes" 2>/dev/null | tail -1)"
		if [ -z "$code" ]; then
			if [ -f "$FAKE/nothing" ] || grep -qxF -- "$ref" "$FAKE/absent" 2>/dev/null; then
				code=404
			elif [ -f "$FAKE/everything" ]; then
				case "$ref" in *.sig) code=404 ;; *) code=200 ;; esac
			elif grep -qxF -- "$ref" "$FAKE/tags" 2>/dev/null; then
				code=200
			else
				code=404
			fi
		fi
		if [ "$code" = 000 ]; then
			[ -n "$wfmt" ] && printf "000"
			return 7
		fi
		if [ -n "$wfmt" ]; then
			printf "%s" "$code"
			return 0
		fi
		[ "$code" != 200 ] && [ "$fail" = 1 ] && return 22
		if [ "$head" = 1 ]; then
			printf "HTTP/2 %s\r\ndocker-content-digest: sha256:%064d\r\n\r\n" "$code" 1
		else
			printf "{\"manifests\":[{\"digest\":\"sha256:%064d\"},{\"digest\":\"sha256:%064d\"}]}\n" 2 3
		fi
		return 0
		;;
	esac
	echo "fake curl: unexpected url ${url}" >&2
	return 7
}
'
eval "$fake_curl"

reset_registry() {
	rm -rf "$FAKE"
	mkdir -p "$FAKE"
	: >"$FAKE/codes"
	echo 4 >"$FAKE/pagesize"
}

fail=0
n=0
ok() {
	n=$((n + 1))
	printf '  ok    %s\n' "$1"
}
bad() {
	n=$((n + 1))
	printf '  FAIL  %s\n' "$1"
	fail=1
}
# rc_of <cmd...> -> the command's exit status on stdout, its output discarded.
rc_of() {
	local rc=0
	"$@" >/dev/null 2>&1 || rc=$?
	echo "$rc"
}

repo=bpalermo/aether/cni-install
target="sha256-$(printf '2%.0s' {1..64})" # an existing signature tag, like #985's

# --- 1 + 2. the race --------------------------------------------------------
race_registry() {
	reset_registry
	# ghcr lists `dev` first (observed on the real cni-install repository).
	printf '%s\n' dev dev-c1 dev-c2 dev-c3 "$target" dev-c5 dev-c6 dev-c7 >"$FAKE/tags"
	# The publish: `dev` re-pointed at the new image, plus the new commit's tags.
	printf '%s\n' dev-c1 dev-c2 dev-c3 "$target" dev-c5 dev-c6 dev-c7 dev-c9 \
		"sha256-$(printf '9%.0s' {1..64})" dev >"$FAKE/after_page1"
}

race_registry
seen="$(ghcr_all_tags "$repo" fake)"
if printf '%s\n' "$seen" | grep -qxF -- "$target"; then
	bad "OLD lookup (walk every tag + grep) did not lose ${target} — the fake no longer reproduces #985, so case 2 would pass vacuously"
elif ! grep -qxF -- "$target" "$FAKE/tags"; then
	bad "the fake registry does not hold ${target} after the publish — the race case is malformed"
else
	ok "OLD lookup reports a PRESENT tag missing mid-publish: walked $(printf '%s\n' "$seen" | grep -c .) of $(grep -c . "$FAKE/tags") tags in $(cat "$FAKE/pages") pages, ${target} never seen"
fi

race_registry
cp "$FAKE/after_page1" "$FAKE/tags" # the same mid-publish registry
rc="$(rc_of ghcr_tag_exists "$repo" "$target" fake)"
if [ "$rc" = 0 ]; then
	ok "NEW lookup (HEAD /v2/<repo>/manifests/<tag>) finds ${target} in the same registry"
else
	bad "NEW lookup did not find ${target}: rc ${rc}"
fi

# --- 3. ghcr_tag_exists answers ---------------------------------------------
reset_registry
printf '%s\n' present-tag >"$FAKE/tags"
printf '%s\n' "flaky 503" "denied 401" "limited 429" "gone 000" >"$FAKE/codes"
want_rc() {
	local name="$1" want="$2" tag="$3" got
	got="$(rc_of ghcr_tag_exists "$repo" "$tag" fake)"
	if [ "$got" = "$want" ]; then ok "$name (rc $got)"; else bad "$name: want rc ${want}, got ${got}"; fi
}
want_rc "200 is present" 0 present-tag
want_rc "404 is absent" 1 no-such-tag
want_rc "503 is inconclusive, not absent" 2 flaky
want_rc "401 is inconclusive, not absent" 2 denied
want_rc "429 is inconclusive, not absent" 2 limited
want_rc "no connection is inconclusive, not absent" 2 gone

ghcr_tag_exists "$repo" present-tag fake
accept="$(cat "$FAKE/accept")"
missing_types=""
for mt in application/vnd.oci.image.index.v1+json \
	application/vnd.docker.distribution.manifest.list.v2+json \
	application/vnd.oci.image.manifest.v1+json \
	application/vnd.docker.distribution.manifest.v2+json; do
	[[ ",${accept}," == *",${mt},"* ]] || missing_types="${missing_types} ${mt}"
done
if [ -z "$missing_types" ]; then
	ok "HEAD accepts the OCI and Docker index + manifest media types"
else
	bad "HEAD Accept is missing:${missing_types}"
fi

# --- 4. signature layout, by direct lookup ----------------------------------
d="sha256:abc123"
layout_case() {
	local want="$1" got rc=0
	shift
	reset_registry
	printf '%s\n' "$@" >"$FAKE/tags"
	got="$(ghcr_signature_layout_direct "$repo" "$d" fake 2>/dev/null)" || rc=$?
	if [ "$rc" = 0 ] && [ "$got" = "$want" ]; then
		ok "signature layout ${want} <- [$*]"
	else
		bad "signature layout: want ${want}, got '${got}' rc ${rc} <- [$*]"
	fi
}
layout_case legacy sha256-abc123.sig other
layout_case bundle sha256-abc123 other
layout_case both sha256-abc123.sig sha256-abc123
layout_case none other

reset_registry
printf '%s\n' sha256-abc123.sig >"$FAKE/tags"
printf '%s\n' "sha256-abc123 503" >"$FAKE/codes"
rc=0
got="$(ghcr_signature_layout_direct "$repo" "$d" fake 2>/dev/null)" || rc=$?
if [ "$rc" = 2 ] && [ -z "$got" ]; then
	ok "one unanswered signature lookup is inconclusive, not 'legacy'"
else
	bad "one unanswered signature lookup: want rc 2 and no layout, got '${got}' rc ${rc}"
fi

# --- 5. the real verifier, end to end ---------------------------------------
lib="$tmp/lib"
mkdir -p "$lib"
cp scripts/verify-published-artifacts.sh scripts/push-heads-lib.sh scripts/proxy-pin-lib.sh scripts/ghcr-lib.sh "$lib/"
printf '\n# --- test override: no network ---\n%s\n' "$fake_curl" >>"$lib/ghcr-lib.sh"

n_charts=${#GHCR_CHARTS[@]}
n_images=${#GHCR_IMAGE_REPOS[@]}
# Two children per index in the fake: 1 + 2 signatures per image, 2 HEADs each.
want_checks=$((n_charts + n_images + 3 * n_images))
want_lookups=$((n_charts + n_images + 2 * 3 * n_images))

# PROXY_PIN_CHECK=0: these cases exercise the per-commit lookup path against a
# fake registry that knows nothing about the aether-proxy digest HEAD's chart
# pins. Once that pin is a post-cut-over signed one (#988), the #984 pin step
# would look it up here, find no signature, and add MISSING lines to cases that
# count exactly the per-commit coordinates. The pin step has its own harness
# (scripts/check-proxy-pin.sh); the control excludes it the same way (#989).
verify() {
	local rc=0
	env -u GITHUB_STEP_SUMMARY PROXY_PIN_CHECK=0 "$lib/verify-published-artifacts.sh" HEAD >"$tmp/out" 2>&1 || rc=$?
	echo "$rc"
}

reset_registry
touch "$FAKE/everything"
rc="$(verify)"
if [ "$rc" = 0 ] &&
	grep -qxF "PASS: ${want_checks} artifact(s) present across 1 commit(s)" "$tmp/out" &&
	grep -qxF "  checked ${want_lookups} expected tags directly (HEAD /v2/<repo>/manifests/<tag>; no tag listing)" "$tmp/out"; then
	ok "verifier: complete registry passes ${want_checks}/${want_checks} with ${want_lookups} direct lookups"
else
	bad "verifier on a complete registry: rc ${rc}"
	tail -5 "$tmp/out" | sed 's/^/        | /'
fi

reset_registry
touch "$FAKE/everything"
printf 'sha256-%064d\n' 3 >"$FAKE/absent" # every image's second child is unsigned
rc="$(verify)"
n_missing="$(grep -c '^  MISSING .* signature for child sha256:0*3 ' "$tmp/out" || true)"
if [ "$rc" = 1 ] && [ "$n_missing" = "$n_images" ] &&
	grep -qxF "FAIL: ${n_images} of ${want_checks} artifact(s) missing across 1 commit(s)" "$tmp/out"; then
	ok "verifier: an unsigned child is MISSING in each of ${n_images} repos, exit 1"
else
	bad "verifier with unsigned children: rc ${rc}, ${n_missing} MISSING lines"
	tail -5 "$tmp/out" | sed 's/^/        | /'
fi

reset_registry
touch "$FAKE/everything"
image_tag="$(sed -nE 's/^  ok      ghcr\.io\/bpalermo\/aether\/agent:(dev-[0-9a-f]{40})$/\1/p' "$tmp/out" | head -1)"
if [ -z "$image_tag" ]; then
	bad "verifier: could not learn the image tag from the previous run's output"
else
	printf '%s 503\n' "$image_tag" >"$FAKE/codes"
	rc="$(verify)"
	if [ "$rc" = 2 ] && ! grep -q '^  MISSING' "$tmp/out" && grep -qF "::error::inconclusive: ghcr.io/" "$tmp/out"; then
		ok "verifier: an unanswered image lookup is exit 2, not MISSING"
	else
		bad "verifier with a 503 lookup: want exit 2 and no MISSING, got rc ${rc}"
		tail -5 "$tmp/out" | sed 's/^/        | /'
	fi
fi

if [ -n "$image_tag" ]; then
	# Every image tag 404s; `dev` is listed and answers 200 — the witness.
	reset_registry
	touch "$FAKE/everything"
	printf '%s\n' dev dev-older >"$FAKE/tags"
	printf '%s\n' "$image_tag" >"$FAKE/absent"
	rc="$(verify)"
	n_witnessed="$(grep -cE "^  MISSING ghcr\.io/.*:${image_tag} \(looked up directly: 404; witness dev: 200\)$" "$tmp/out" || true)"
	if [ "$rc" = 1 ] && [ "$n_witnessed" = "$n_images" ]; then
		ok "verifier: each absent image is MISSING with a witness (${n_witnessed}/${n_images}), exit 1"
	else
		bad "verifier with absent images: rc ${rc}, ${n_witnessed} witnessed MISSING lines, want ${n_images}"
		tail -5 "$tmp/out" | sed 's/^/        | /'
	fi

	# A lookup that 404s everything — including `dev`, which the repository
	# lists. Without the witness this is 36 MISSING for a complete publish.
	reset_registry
	touch "$FAKE/nothing"
	printf '%s\n' dev >"$FAKE/tags"
	rc="$(verify)"
	if [ "$rc" = 2 ] && ! grep -q '^  MISSING' "$tmp/out" && grep -qF "the lookup is broken" "$tmp/out"; then
		ok "verifier: a lookup that 404s a listed tag is exit 2, not a wall of MISSING"
	else
		bad "verifier with a lookup that 404s everything: want exit 2 and no MISSING, got rc ${rc}"
		tail -5 "$tmp/out" | sed 's/^/        | /'
	fi
fi

if [ "$n" -ne 19 ]; then
	echo "::error::ran ${n} cases, expected 19 -- a gate that checks nothing passes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "::error::ghcr direct lookup is wrong; see cases above" >&2
	exit 1
fi
echo "ghcr lookup: ${n}/${n} cases correct"
