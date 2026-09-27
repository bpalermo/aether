#!/usr/bin/env bash
# Exercise the direct tag lookup (registry_tag_exists, #985) — and the listing
# race it replaced — the Referrers API lookup (registry_referrers, proposal 040)
# and the verifier end to end, against a fake registry that can play ghcr.io or
# quay.io. No network. (Was scripts/check-ghcr-lookup.sh.)
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
# Then registry_tag_exists' three answers (200 present / 404 absent / anything
# else inconclusive — never "absent"), registry_signature_layout_direct over
# every layout, and the real verifier end to end on a pre-cut-over commit: a
# complete registry passes with the exact lookup count, one missing signature
# is exit 1 with a MISSING line, an unanswered lookup is exit 2, an absent image
# carries its witness (a listed tag answering 200), and a lookup that 404s even
# a listed tag is exit 2 rather than MISSING.
#
# QUAY (proposal 040). The fake answers the token endpoint only at the path the
# host really serves it on (ghcr.io `/token`, quay.io `/v2/auth`), and serves
# `/v2/<repo>/referrers/<digest>` in one of several modes: 404 (ghcr.io: no
# Referrers API — the default), an index (quay.io), a 5xx, a non-index body,
# and two pages joined by a `Link: rel="next"`. The cases pin: a sign-bundle
# referrer is layout `referrer`; a referrers 404 falls back to the tag layouts;
# a referrers 5xx is inconclusive, never `none`; a bundle on page 2 is found.
#
# THE TOKEN (#999). The fake issues ONE pull token, `fake`, from its token
# endpoint, and every other request must present exactly `Authorization:
# Bearer fake` or it gets a 401 — as the real registries do. A fake that took
# any bearer value let the verifier pass the proxy repo's TAG LIST as the token
# (#999) through this harness green; the proxy-pin case below drives that path.
#
# THE CREDENTIAL (proposal 040). The robot's REGISTRY_USERNAME/PASSWORD are
# bound to REGISTRY_CREDENTIAL_HOST (default: the exported IMAGE_REGISTRY_HOST):
# section 5b pins that a token request to that host carries them and one to any
# other host carries none (the fake records every `-u` it is handed).
#
# THE SPLIT (proposal 040 phase 2). The verifier reads bazel/img/registry.bzl AS
# OF EACH COMMIT it checks, so section 6 runs it against a throwaway git history
# of two commits: PRE, whose registry.bzl is the pre-cut-over setting (ghcr.io,
# charts/ prefix, the aether-proxy override, no SIGNATURE_LAYOUT line), and
# POST, whose registry.bzl is this checkout's (quay.io, chart- prefix,
# SIGNATURE_LAYOUT "referrer"). The fake plays both registries at once, each the
# way it really behaves: ghcr.io signatures as tags with a referrers 404, quay.io
# signatures as referrers only. Pinned: PRE passes on ghcr.io, POST passes on
# quay.io, both in one run pass with each commit on its own registry, and three
# REDs -- POST whose artifacts exist only on ghcr.io is MISSING on quay.io (the
# split must never fall back to the old registry); POST signed with a fallback
# TAG on quay.io is MISSING (the wrong layout for its SIGNATURE_LAYOUT); POST
# with a referrer AND a tag is MISSING (`both`, the double-write).
#
# The fake is a `curl` shell function: registry-lib.sh's functions call `curl`
# by name, so it replaces the network for them. The verifier sources its
# libraries from its own directory, so for the end-to-end cases it runs from a
# temp copy whose registry-lib.sh has the same function appended (the #983
# seam), inside the throwaway history, which is where it reads each commit's
# registry.bzl from.
#
# SC2016 is off on purpose: the fake's text is single-quoted so it reaches the
# generated library unexpanded.
# shellcheck disable=SC2016
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
IMAGE_REGISTRY_BZL="$PWD/bazel/img/registry.bzl"
export IMAGE_REGISTRY_BZL
# shellcheck disable=SC1091
. scripts/registry-lib.sh

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export FAKE="$tmp/registry"
mkdir -p "$FAKE"

# --- the fake registry -------------------------------------------------------
#
# State lives in $FAKE because registry-lib.sh calls curl inside $(...)
# subshells.
#   tags         the repository's tag list, in registry order
#   pagesize     entries per /tags/list page (ghcr silently caps `n`)
#   after_page1  if present, REPLACES tags once page 1 has been served: the
#                publish that runs while the walk is in flight
#   everything   if present, every tag exists except `*.sig` ones (a complete
#                cosign-3 registry for the end-to-end cases) and those in
#   absent       tags that 404 regardless
#   quaylike     with `everything`: signature TAGS (`sha256-*`) 404 too, and
#                every digest has a sign-bundle referrer (cosign 3 on quay.io).
#                Implied for host quay.io under `everything`: each registry
#                behaves as measured (ghcr.io: tags, referrers 404).
#   quaytags     with `everything` on quay.io: the fallback TAG `sha256-<hex>`
#                exists too (a tag where a referrer is promised, or with the
#                referrer: the double-write)
#   norefs       with `everything` on quay.io: the referrers index is empty
#                (so only quaytags can sign anything)
#   published_on if present, a host: on every OTHER host each manifest lookup
#                404s except the tags listed in `tags` (the witness), so
#                "published, but on the wrong registry" is expressible
#   nothing      if present, EVERY manifest lookup 404s (a lookup that has lost
#                its Accept header, say) — the listing still works
#   codes        `<tag> <http code>` overrides (000 = no connection)
#   accept       the Accept header of the last manifest request
#   token_urls   every token URL asked for, one per line
#   basic_auth   every token request that carried `-u` credentials:
#                `<host> <user>` (the user only; the fake keeps no secret)
#   unauthorized every request refused for its bearer token: `<url> <header>`
#   referrers    referrers mode: 404 (default) | index | 503 | paged | garbage
#                (valid JSON that is not an index)
#   refs_<dg>    the referrers index served for digest <dg> in `index` mode
#                (an empty index when absent); in `paged` mode, page 2
fake_curl='
curl() {
	local url="" hdr_out="" out="" wfmt="" head=0 fail=0 accept="" auth="" basic="" a
	while [ "$#" -gt 0 ]; do
		a="$1"
		shift
		case "$a" in
		-D) hdr_out="$1"; shift ;;
		-o) out="$1"; shift ;;
		-u) basic="$1"; shift ;;
		--retry) shift ;;
		-w) wfmt="$1"; shift ;;
		-H)
			case "$1" in
			[Aa]ccept:*) accept="${1#*: }" ;;
			[Aa]uthorization:*) auth="${1#*: }" ;;
			esac
			shift
			;;
		-I) head=1 ;;
		-f*) fail=1; [[ "$a" == *I* ]] && head=1 ;;
		https://*) url="$a" ;;
		esac
	done
	local host="${url#https://}"
	host="${host%%/*}"
	# Only the token this fake issued opens anything but the token endpoint.
	case "$url" in
	*/token\?* | */v2/auth\?*) ;;
	*)
		if [ "$auth" != "Bearer fake" ]; then
			printf "%s %s\n" "$url" "${auth:-<no Authorization>}" | tr "\n" " " >>"$FAKE/unauthorized"
			echo >>"$FAKE/unauthorized"
			[ -n "$hdr_out" ] && : >"$hdr_out"
			[ -n "$out" ] && printf "{\"errors\":[{\"code\":\"UNAUTHORIZED\"}]}" >"$out"
			if [ -n "$wfmt" ]; then
				printf "401"
				return 0
			fi
			[ "$fail" = 1 ] && return 22
			printf "{\"errors\":[{\"code\":\"UNAUTHORIZED\"}]}"
			return 0
		fi
		;;
	esac
	case "$url" in
	*/token\?* | */v2/auth\?*)
		printf "%s\n" "$url" >>"$FAKE/token_urls"
		[ -n "$basic" ] && printf "%s %s\n" "$host" "${basic%%:*}" >>"$FAKE/basic_auth"
		local want_path=/token
		[ "$host" = quay.io ] && want_path=/v2/auth
		case "$url" in
		"https://${host}${want_path}?"*"service=${host}"*"scope=repository:"*":pull"* | \
			"https://${host}${want_path}?"*"scope=repository:"*":pull"*"service=${host}"*)
			printf "{\"token\":\"fake\"}\n"
			return 0
			;;
		esac
		[ "$fail" = 1 ] && return 22
		printf "not found\n"
		return 0
		;;
	*/referrers/*)
		local dg="${url##*/referrers/}" mode=404 code body
		dg="${dg%%\?*}"
		[ -f "$FAKE/referrers" ] && mode="$(cat "$FAKE/referrers")"
		[ -f "$FAKE/quaylike" ] && mode=quaylike
		[ -f "$FAKE/everything" ] && [ "$host" = quay.io ] && mode=quaylike
		[ "$mode" = quaylike ] && [ -f "$FAKE/norefs" ] && mode=index
		[ -n "$hdr_out" ] && : >"$hdr_out"
		local empty="{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.index.v1+json\",\"manifests\":[]}"
		case "$mode" in
		404) code=404 body="{\"errors\":[{\"code\":\"NOT_FOUND\"}]}" ;;
		503) code=503 body="unavailable" ;;
		garbage) code=200 body="{\"name\":\"x\",\"tags\":[\"not\",\"an\",\"index\"]}" ;;
		index) code=200 body="$(cat "$FAKE/refs_${dg}" 2>/dev/null || printf "%s" "$empty")" ;;
		quaylike) code=200 body="$(cat "$FAKE/sign_index")" ;;
		paged)
			code=200
			if [[ "$url" == *"page=2"* ]]; then
				body="$(cat "$FAKE/refs_${dg}" 2>/dev/null || printf "%s" "$empty")"
			else
				body="$empty"
				printf "Link: </v2/x/referrers/%s?page=2>; rel=\"next\"\r\n" "$dg" >"$hdr_out"
			fi
			;;
		esac
		if [ -n "$out" ]; then printf "%s" "$body" >"$out"; else printf "%s" "$body"; fi
		[ -n "$wfmt" ] && printf "%s" "$code"
		return 0
		;;
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
		[ -n "$hdr_out" ] && : >"$hdr_out"
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
		local ql=0
		[ -f "$FAKE/quaylike" ] && ql=1
		[ -f "$FAKE/everything" ] && [ "$host" = quay.io ] && ql=1
		if [ -z "$code" ]; then
			if [ -f "$FAKE/published_on" ] && [ "$host" != "$(cat "$FAKE/published_on")" ]; then
				if grep -qxF -- "$ref" "$FAKE/tags" 2>/dev/null; then code=200; else code=404; fi
			elif [ -f "$FAKE/nothing" ] || grep -qxF -- "$ref" "$FAKE/absent" 2>/dev/null; then
				code=404
			elif [ -f "$FAKE/everything" ]; then
				case "$ref" in
				*.sig) code=404 ;;
				sha256-*) if [ "$ql" = 1 ] && [ ! -f "$FAKE/quaytags" ]; then code=404; else code=200; fi ;;
				*) code=200 ;;
				esac
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
		if [ "$head" = 1 ] && [[ "$ref" == sha256:* ]]; then
			# By digest (the proxy pin): the registry echoes the digest asked for.
			printf "HTTP/2 %s\r\ndocker-content-digest: %s\r\n\r\n" "$code" "$ref"
		elif [ "$head" = 1 ]; then
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

# A referrers index listing one cosign 3 SIGN bundle — the shape quay.io serves
# for argoproj/argocd (2026-09-27) — and one attestation that must not count.
sign_entry='{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:f0d625b5510b3feedd814dea7b2da07038b2a6f0573f9d2089c4d9c9be77ceb8","size":895,"artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json","annotations":{"dev.sigstore.bundle.content":"dsse-envelope","dev.sigstore.bundle.predicateType":"https://sigstore.dev/cosign/sign/v1"}}'
attest_entry='{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:e0d625b5510b3feedd814dea7b2da07038b2a6f0573f9d2089c4d9c9be77ceb8","size":901,"artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json","annotations":{"dev.sigstore.bundle.content":"dsse-envelope","dev.sigstore.bundle.predicateType":"https://slsa.dev/provenance/v1"}}'
index_of() {
	local IFS=,
	printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[%s]}' "$*"
}

reset_registry() {
	rm -rf "$FAKE"
	mkdir -p "$FAKE"
	: >"$FAKE/codes"
	echo 4 >"$FAKE/pagesize"
	index_of "$attest_entry" "$sign_entry" >"$FAKE/sign_index"
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

# Sections 1-4 play ghcr.io, the registry the race (#985) happened on: no
# Referrers API (the fake's default referrers answer is a 404). The library
# reads REGISTRY_HOST at call time, and this checkout's own setting is quay.io.
REGISTRY_HOST=ghcr.io
repo="$(scripts/image-registry.sh repo cni-install)"
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
seen="$(registry_all_tags "$repo" fake)"
if printf '%s\n' "$seen" | grep -qxF -- "$target"; then
	bad "OLD lookup (walk every tag + grep) did not lose ${target} — the fake no longer reproduces #985, so case 2 would pass vacuously"
elif ! grep -qxF -- "$target" "$FAKE/tags"; then
	bad "the fake registry does not hold ${target} after the publish — the race case is malformed"
else
	ok "OLD lookup reports a PRESENT tag missing mid-publish: walked $(printf '%s\n' "$seen" | grep -c .) of $(grep -c . "$FAKE/tags") tags in $(cat "$FAKE/pages") pages, ${target} never seen"
fi

race_registry
cp "$FAKE/after_page1" "$FAKE/tags" # the same mid-publish registry
rc="$(rc_of registry_tag_exists "$repo" "$target" fake)"
if [ "$rc" = 0 ]; then
	ok "NEW lookup (HEAD /v2/<repo>/manifests/<tag>) finds ${target} in the same registry"
else
	bad "NEW lookup did not find ${target}: rc ${rc}"
fi

# --- 3. registry_tag_exists answers -----------------------------------------
reset_registry
printf '%s\n' present-tag >"$FAKE/tags"
printf '%s\n' "flaky 503" "denied 401" "limited 429" "gone 000" >"$FAKE/codes"
want_rc() {
	local name="$1" want="$2" tag="$3" got
	got="$(rc_of registry_tag_exists "$repo" "$tag" fake)"
	if [ "$got" = "$want" ]; then ok "$name (rc $got)"; else bad "$name: want rc ${want}, got ${got}"; fi
}
want_rc "200 is present" 0 present-tag
want_rc "404 is absent" 1 no-such-tag
want_rc "503 is inconclusive, not absent" 2 flaky
want_rc "401 is inconclusive, not absent" 2 denied
want_rc "429 is inconclusive, not absent" 2 limited
want_rc "no connection is inconclusive, not absent" 2 gone

registry_tag_exists "$repo" present-tag fake
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

# --- 4. signature layout, by direct lookup (ghcr.io: referrers 404) ---------
d="sha256:abc123"
layout_case() {
	local want="$1" got rc=0
	shift
	reset_registry
	printf '%s\n' "$@" >"$FAKE/tags"
	got="$(registry_signature_layout_direct "$repo" "$d" fake 2>/dev/null)" || rc=$?
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
got="$(registry_signature_layout_direct "$repo" "$d" fake 2>/dev/null)" || rc=$?
if [ "$rc" = 2 ] && [ -z "$got" ]; then
	ok "one unanswered signature lookup is inconclusive, not 'legacy'"
else
	bad "one unanswered signature lookup: want rc 2 and no layout, got '${got}' rc ${rc}"
fi

# --- 5. quay.io: token endpoint and the Referrers API -----------------------
# Each case runs with REGISTRY_HOST=quay.io, the way a caller points the
# library at another registry.
quay() { REGISTRY_HOST=quay.io "$@"; }
qrepo="$(scripts/image-registry.sh repo agent)"

reset_registry
tok="$(quay registry_registry_token "$qrepo" 2>/dev/null)" || tok=""
asked="$(tail -1 "$FAKE/token_urls" 2>/dev/null)"
if [ "$tok" = fake ] && [ "$asked" = "https://quay.io/v2/auth?service=quay.io&scope=repository:${qrepo}:pull" ]; then
	ok "quay.io pull token from /v2/auth?service=quay.io&scope=repository:<repo>:pull"
else
	bad "quay.io pull token: got '${tok}' from '${asked}'"
fi
reset_registry
tok="$(REGISTRY_HOST=ghcr.io registry_registry_token "$repo" 2>/dev/null)" || tok=""
asked="$(tail -1 "$FAKE/token_urls" 2>/dev/null)"
if [ "$tok" = fake ] &&
	[ "$asked" = "https://ghcr.io/token?service=ghcr.io&scope=repository:${repo}:pull" ]; then
	ok "ghcr.io pull token from /token?service=ghcr.io&scope=repository:<repo>:pull"
else
	bad "ghcr.io pull token: got '${tok}' from '${asked}'"
fi

# quay_layout <want layout | rc2> <referrers mode> <name> [tag...]
quay_layout() {
	local want="$1" mode="$2" name="$3" got rc=0
	shift 3
	reset_registry
	echo "$mode" >"$FAKE/referrers"
	index_of "$attest_entry" "$sign_entry" >"$FAKE/refs_${d}"
	printf '%s\n' "$@" >"$FAKE/tags"
	got="$(quay registry_signature_layout_direct "$qrepo" "$d" fake 2>/dev/null)" || rc=$?
	if [ "$want" = rc2 ]; then
		if [ "$rc" = 2 ] && [ -z "$got" ]; then
			ok "${name}: inconclusive (rc 2), never a layout"
		else
			bad "${name}: want rc 2 and no layout, got '${got}' rc ${rc}"
		fi
	elif [ "$rc" = 0 ] && [ "$got" = "$want" ]; then
		ok "${name}: layout ${want}"
	else
		bad "${name}: want ${want}, got '${got}' rc ${rc}"
	fi
}
quay_layout referrer index "referrers index with a sign bundle (+ an attestation), no tags" other
quay_layout legacy 404 "referrers 404 (no API) falls back to the tag layouts" sha256-abc123.sig other
quay_layout none 404 "referrers 404 and no tags is none, not inconclusive" other
quay_layout rc2 503 "referrers 5xx" sha256-abc123.sig other
quay_layout rc2 garbage "referrers 200 whose JSON is not an index" other
quay_layout referrer paged "sign bundle on referrers page 2 (Link: rel=next)" other
quay_layout both index "a sign referrer AND a .sig tag" sha256-abc123.sig

# --- 5b. the robot credential is bound to ONE host ---------------------------
# REGISTRY_USERNAME/REGISTRY_PASSWORD go only to REGISTRY_CREDENTIAL_HOST
# (default: the exported IMAGE_REGISTRY_HOST, else the setting's host); a token
# request anywhere else is anonymous and says so on stderr.
# cred_case <name> <REGISTRY_HOST> <want: sent|anonymous> [VAR=value...]
cred_case() {
	local name="$1" host="$2" want="$3" tok err rc=0
	shift 3
	reset_registry
	err="$tmp/cred.err"
	tok="$(env -u IMAGE_REGISTRY_HOST -u REGISTRY_CREDENTIAL_HOST REGISTRY_USERNAME='aethermesh+robot' REGISTRY_PASSWORD='not-a-real-secret' \
		REGISTRY_HOST="$host" "$@" bash -c 'eval "$1"; . scripts/registry-lib.sh; registry_registry_token some/repo' \
		_ "$fake_curl" 2>"$err")" || rc=$?
	local recorded
	recorded="$(cat "$FAKE/basic_auth" 2>/dev/null)"
	if [ "$want" = sent ] && [ "$rc" = 0 ] && [ "$tok" = fake ] &&
		[ "$recorded" = "${host} aethermesh+robot" ] && ! grep -q anonymously "$err"; then
		ok "${name}: the credential goes to ${host}"
	elif [ "$want" = anonymous ] && [ "$rc" = 0 ] && [ "$tok" = fake ] && [ -z "$recorded" ] &&
		grep -qxF "registry-lib: credentials are for $(sed -nE 's/.*credentials are for ([^;]+);.*/\1/p' "$err"); reading ${host} anonymously" "$err"; then
		ok "${name}: NO credential reaches ${host} ($(cat "$err"))"
	else
		bad "${name}: want ${want} to ${host}; token '${tok}' rc ${rc}, recorded [${recorded}], stderr [$(cat "$err")]"
	fi
}
setting_host="$(scripts/image-registry.sh host)"
other_host=ghcr.io
[ "$setting_host" = ghcr.io ] && other_host=quay.io
cred_case "default (the setting's host)" "$setting_host" sent
cred_case "default, another host" "$other_host" anonymous
cred_case "IMAGE_REGISTRY_HOST exported by the workflow" "$other_host" sent IMAGE_REGISTRY_HOST="$other_host"
cred_case "REGISTRY_CREDENTIAL_HOST override, its host" "$other_host" sent REGISTRY_CREDENTIAL_HOST="$other_host"
cred_case "REGISTRY_CREDENTIAL_HOST override, the setting's host" "$setting_host" anonymous REGISTRY_CREDENTIAL_HOST="$other_host"

# The shim: ghcr-lib.sh still sources the library and the old names still work.
# A fresh bash, so nothing this file defined can stand in for the shim's.
reset_registry
printf '%s\n' present-tag >"$FAKE/tags"
if env -u REGISTRY_HOST bash -c '
	eval "$1"
	. scripts/ghcr-lib.sh
	ghcr_tag_exists "$2" present-tag fake && [ "${#GHCR_IMAGE_REPOS[@]}" -eq 8 ] &&
		[ "${GHCR_IMAGE_REPOS[0]}" = "$(scripts/image-registry.sh repo agent)" ]
' _ "$fake_curl" "$repo" >/dev/null 2>&1; then
	ok "scripts/ghcr-lib.sh shim: ghcr_* names and GHCR_IMAGE_REPOS still resolve"
else
	bad "scripts/ghcr-lib.sh shim no longer provides the ghcr_* names"
fi

# --- 6. the real verifier, end to end, across the cut-over -----------------
lib="$tmp/lib"
mkdir -p "$lib"
cp scripts/verify-published-artifacts.sh scripts/push-heads-lib.sh scripts/proxy-pin-lib.sh \
	scripts/registry-lib.sh scripts/image-registry.sh "$lib/"
printf '\n# --- test override: no network ---\n%s\n' "$fake_curl" >>"$lib/registry-lib.sh"

# The throwaway history: PRE (the pre-cut-over setting, exactly the shape the
# file had before phase 2 -- ghcr.io, charts/, the aether-proxy override, no
# SIGNATURE_LAYOUT or PROXY_PIN_LEGACY_REFERENCES line) and POST (this
# checkout's registry.bzl). Both carry the real chart versions and release-tag
# default the verifier reads per commit.
hist="$tmp/history"
mkdir -p "$hist/bazel/img"
for c in "${REGISTRY_CHARTS[@]}"; do
	mkdir -p "$hist/charts/$c"
	cp "charts/$c/Chart.yaml" "$hist/charts/$c/Chart.yaml"
done
cp bazel/img/go_multi_arch_image.bzl "$hist/bazel/img/"
pre_bzl="$tmp/pre-registry.bzl"
sed -E \
	-e 's|^IMAGE_REGISTRY = .*|IMAGE_REGISTRY = "ghcr.io"|' \
	-e 's|^IMAGE_NAMESPACE = .*|IMAGE_NAMESPACE = "bpalermo/aether"|' \
	-e 's|^IMAGE_NAME_OVERRIDES = .*|IMAGE_NAME_OVERRIDES = {"proxy": "aether-proxy"}|' \
	-e 's|^CHART_REPOSITORY_PREFIX = .*|CHART_REPOSITORY_PREFIX = "charts/"|' \
	-e '/^SIGNATURE_LAYOUT = /d' \
	-e '/^PROXY_PIN_LEGACY_REFERENCES = /d' \
	bazel/img/registry.bzl >"$pre_bzl"
if ! (
	set -e
	cd "$hist"
	git init -q -b main .
	git config user.email check@example.invalid
	git config user.name check-registry-lookup
	git config commit.gpgsign false
	cp "$pre_bzl" bazel/img/registry.bzl
	git add -A && git commit -qm "pre: published to the pre-cut-over registry"
	cp "$OLDPWD/bazel/img/registry.bzl" bazel/img/registry.bzl
	git commit -qam "post: the cut-over"
); then
	echo "::error::could not build the throwaway history" >&2
	exit 2
fi
pre="$(git -C "$hist" rev-parse HEAD~1)"
post="$(git -C "$hist" rev-parse HEAD)"
pre_host="$(IMAGE_REGISTRY_BZL="$pre_bzl" scripts/image-registry.sh host)"
pre_agent_ref="$(IMAGE_REGISTRY_BZL="$pre_bzl" scripts/image-registry.sh ref agent)"
pre_chart_ref="$(IMAGE_REGISTRY_BZL="$pre_bzl" scripts/image-registry.sh chart-ref aether)"
post_host="$(scripts/image-registry.sh host)"
post_agent_ref="$(scripts/image-registry.sh ref agent)"
post_chart_ref="$(scripts/image-registry.sh chart-ref aether)"
if [ "$pre_host" = "$post_host" ] || [ "$post_host" != quay.io ]; then
	echo "::error::the split cases need a pre-cut-over host and quay.io after it; got ${pre_host} -> ${post_host}" >&2
	exit 2
fi

n_charts=${#REGISTRY_CHARTS[@]}
n_images=${#REGISTRY_IMAGE_COMPONENTS[@]}
# Two children per index in the fake: 1 + 2 signatures per image, 2 HEADs each.
want_checks=$((n_charts + n_images + 3 * n_images))
want_lookups=$((n_charts + n_images + 2 * 3 * n_images))

# PROXY_PIN_CHECK=0: these cases exercise the per-commit lookup path against a
# fake registry that knows nothing about the aether-proxy digest HEAD's chart
# pins. The pin step has its own harness (scripts/check-proxy-pin.sh); the
# control excludes it the same way (#989).
# verify <commit>... -> the verifier's exit code; its output in $tmp/out.
verify() {
	local rc=0
	(cd "$hist" && env -u GITHUB_STEP_SUMMARY -u REGISTRY_HOST PROXY_PIN_CHECK=0 \
		"$lib/verify-published-artifacts.sh" "$@") >"$tmp/out" 2>&1 || rc=$?
	echo "$rc"
}
show() { tail -6 "$tmp/out" | sed 's/^/        | /'; }

# 6a. PRE on its own registry (tag signatures, referrers 404): the pre-040
#     cases, unchanged -- a complete registry, an unsigned child, a 503, an
#     absent image with its witness, and a lookup that 404s everything.
reset_registry
touch "$FAKE/everything"
rc="$(verify "$pre")"
if [ "$rc" = 0 ] &&
	grep -qxF "PASS: ${want_checks} artifact(s) present across 1 commit(s)" "$tmp/out" &&
	grep -qxF "  checked ${want_lookups} expected tags directly (HEAD /v2/<repo>/manifests/<tag>; no tag listing)" "$tmp/out" &&
	grep -qF "  ok      ${pre_chart_ref}:" "$tmp/out" &&
	! grep -qF " ${post_host}/" "$tmp/out"; then
	ok "verifier, PRE-cut-over head: complete ${pre_host} registry passes ${want_checks}/${want_checks} with ${want_lookups} direct lookups, nothing asked of ${post_host}"
else
	bad "verifier on a complete ${pre_host} registry for the PRE head: rc ${rc}"
	show
fi

reset_registry
touch "$FAKE/everything"
printf 'sha256-%064d\n' 3 >"$FAKE/absent" # every image's second child is unsigned
rc="$(verify "$pre")"
n_missing="$(grep -c '^  MISSING .* signature for child sha256:0*3 ' "$tmp/out" || true)"
if [ "$rc" = 1 ] && [ "$n_missing" = "$n_images" ] &&
	grep -qxF "FAIL: ${n_images} of ${want_checks} artifact(s) missing across 1 commit(s)" "$tmp/out"; then
	ok "verifier: an unsigned child is MISSING in each of ${n_images} repos, exit 1"
else
	bad "verifier with unsigned children: rc ${rc}, ${n_missing} MISSING lines"
	show
fi

reset_registry
touch "$FAKE/everything"
image_tag="dev-${pre}"
printf '%s 503\n' "$image_tag" >"$FAKE/codes"
rc="$(verify "$pre")"
if [ "$rc" = 2 ] && ! grep -q '^  MISSING' "$tmp/out" && grep -qF "::error::inconclusive: ${pre_host}/" "$tmp/out"; then
	ok "verifier: an unanswered image lookup is exit 2, not MISSING"
else
	bad "verifier with a 503 lookup: want exit 2 and no MISSING, got rc ${rc}"
	show
fi

# Every image tag 404s; `dev` is listed and answers 200 — the witness.
reset_registry
touch "$FAKE/everything"
printf '%s\n' dev dev-older >"$FAKE/tags"
printf '%s\n' "$image_tag" >"$FAKE/absent"
rc="$(verify "$pre")"
n_witnessed="$(grep -cE "^  MISSING ${pre_host//./\\.}/.*:${image_tag} \(looked up directly: 404; witness dev: 200\)$" "$tmp/out" || true)"
if [ "$rc" = 1 ] && [ "$n_witnessed" = "$n_images" ]; then
	ok "verifier: each absent image is MISSING with a witness (${n_witnessed}/${n_images}), exit 1"
else
	bad "verifier with absent images: rc ${rc}, ${n_witnessed} witnessed MISSING lines, want ${n_images}"
	show
fi

# A lookup that 404s everything — including `dev`, which the repository
# lists. Without the witness this is 36 MISSING for a complete publish.
reset_registry
touch "$FAKE/nothing"
printf '%s\n' dev >"$FAKE/tags"
rc="$(verify "$pre")"
if [ "$rc" = 2 ] && ! grep -q '^  MISSING' "$tmp/out" && grep -qF "the lookup is broken" "$tmp/out"; then
	ok "verifier: a lookup that 404s a listed tag is exit 2, not a wall of MISSING"
else
	bad "verifier with a lookup that 404s everything: want exit 2 and no MISSING, got rc ${rc}"
	show
fi

# 6b. POST on quay.io: referrer-only signatures, flat names, chart- prefix.
reset_registry
touch "$FAKE/everything"
rc="$(verify "$post")"
n_ref="$(grep -cE "^  ok      ${post_host//./\\.}/[a-z/-]+@sha256:[0-9a-f]{64} <- OCI 1\.1 referrer " "$tmp/out" || true)"
if [ "$rc" = 0 ] &&
	grep -qxF "PASS: ${want_checks} artifact(s) present across 1 commit(s)" "$tmp/out" &&
	[ "$n_ref" = "$((3 * n_images))" ] &&
	grep -qF "  ok      ${post_chart_ref}:" "$tmp/out" &&
	grep -qF "  ok      ${post_agent_ref}:dev-${post}" "$tmp/out" &&
	! grep -qF " ${pre_host}/" "$tmp/out"; then
	ok "verifier, POST-cut-over head: ${post_host} with referrer-only signatures passes ${want_checks}/${want_checks}, ${n_ref} via referrers, nothing asked of ${pre_host}"
else
	bad "verifier on ${post_host} with referrer signatures for the POST head: rc ${rc}, ${n_ref} referrer lines (want $((3 * n_images)))"
	show
fi

# 6c. Both heads in ONE run (what --recent does across the cut-over): each on
#     its own registry, each in its own layout.
reset_registry
touch "$FAKE/everything"
rc="$(verify "$pre" "$post")"
if [ "$rc" = 0 ] &&
	grep -qxF "PASS: $((2 * want_checks)) artifact(s) present across 2 commit(s)" "$tmp/out" &&
	grep -qF "  ok      ${pre_agent_ref}:dev-${pre}" "$tmp/out" &&
	grep -qF "  ok      ${post_agent_ref}:dev-${post}" "$tmp/out" &&
	[ "$(grep -c "cosign 3 layout)$" "$tmp/out")" = "$((3 * n_images))" ] &&
	[ "$(grep -c " <- OCI 1\.1 referrer " "$tmp/out")" = "$((3 * n_images))" ]; then
	ok "verifier across the cut-over: PRE on ${pre_host} (tags) + POST on ${post_host} (referrers) in one run, $((2 * want_checks))/$((2 * want_checks))"
else
	bad "verifier across the cut-over: rc ${rc}"
	show
fi

# 6d. RED: POST published only to the OLD registry. The split must never fall
#     back: every coordinate is MISSING on quay.io, each with its witness.
reset_registry
touch "$FAKE/everything"
echo "$pre_host" >"$FAKE/published_on"
printf '%s\n' witness-tag >"$FAKE/tags"
rc="$(verify "$post")"
want_red=$((n_charts + 2 * n_images))
n_red="$(grep -c "^  MISSING ${post_host//./\\.}/" "$tmp/out" || true)"
n_wit="$(grep -c '^  MISSING .*; witness witness-tag: 200)$' "$tmp/out" || true)"
if [ "$rc" = 1 ] && [ "$n_red" = "$want_red" ] && [ "$n_wit" = "$((n_charts + n_images))" ] &&
	! grep -q '^  ok ' "$tmp/out" && ! grep -qF " ${pre_host}/" "$tmp/out" &&
	grep -qxF "FAIL: ${want_red} of ${want_red} artifact(s) missing across 1 commit(s)" "$tmp/out"; then
	ok "RED: a POST head whose artifacts are only on ${pre_host} is ${want_red}/${want_red} MISSING on ${post_host}, exit 1"
else
	bad "a POST head published only to ${pre_host}: rc ${rc}, ${n_red} MISSING on ${post_host} (want ${want_red}), ${n_wit} witnessed"
	show
fi

# 6e. RED: POST signed with a fallback TAG on quay.io (no referrer): present,
#     but not what SIGNATURE_LAYOUT "referrer" promises there.
reset_registry
touch "$FAKE/everything" "$FAKE/quaytags" "$FAKE/norefs"
rc="$(verify "$post")"
n_wrong="$(grep -c "^  MISSING .*layout 'bundle', but this commit's bazel/img/registry.bzl promises SIGNATURE_LAYOUT 'referrer' there$" "$tmp/out" || true)"
if [ "$rc" = 1 ] && [ "$n_wrong" = "$((3 * n_images))" ]; then
	ok "RED: a tag-layout signature on ${post_host} (SIGNATURE_LAYOUT referrer) is MISSING for all $((3 * n_images)) signatures, exit 1"
else
	bad "a tag-layout signature on ${post_host}: rc ${rc}, ${n_wrong} wrong-layout lines (want $((3 * n_images)))"
	show
fi

# 6f. RED: a referrer AND the fallback tag on quay.io — the double-write.
reset_registry
touch "$FAKE/everything" "$FAKE/quaytags"
rc="$(verify "$post")"
n_both="$(grep -c "^  MISSING .*MORE THAN ONE layout present" "$tmp/out" || true)"
if [ "$rc" = 1 ] && [ "$n_both" = "$((3 * n_images))" ]; then
	ok "RED: a referrer AND a fallback tag on ${post_host} (the double-write) is MISSING for all $((3 * n_images)) signatures, exit 1"
else
	bad "a double-written signature on ${post_host}: rc ${rc}, ${n_both} 'both' lines (want $((3 * n_images)))"
	show
fi

# 6g. Step 5, the aether-proxy pin (#984), through the token-enforcing fake
# (#999). This checkout's HEAD, its pinned digest, with the cut-over placed just
# before the commit that introduced it, so the pin is always a post-cut-over one
# and is CHECKED, never skipped: the pinned index, its signature and both
# children's signatures, each looked up with the pull token -- on the registry
# the PIN names (proposal 040: it moves with the next proxy release, not with
# the flip). Then the #999 bug re-injected into a copy of the verifier (the tag
# list passed where check_signature takes the token): the fake must answer 401
# and the run must be exit 2, not green. Both halves, so the case cannot pass on
# a fake that stopped checking the token.
# shellcheck source=scripts/proxy-pin-lib.sh
. scripts/proxy-pin-lib.sh
pinned="$(git show "HEAD:${PROXY_VALUES_PATH}" | proxy_pinned_ref)" || pinned=""
pin="${pinned#* }"
pin_ref="${pinned%% *}"
pin_intro=""
[ -n "$pinned" ] && pin_intro="$(proxy_pin_introduced_by HEAD "$pin")"
buggy="$tmp/buggy"
mkdir -p "$buggy"
cp "$lib"/* "$buggy/"
sed -i -E 's/(check_signature "\$pin_repo" "\$(pin|child)" )"\$tok"/\1"$tags"/' "$buggy/verify-published-artifacts.sh"
# verify_pin <verifier dir> -> exit status; output in $tmp/out, refusals in $FAKE/unauthorized
verify_pin() {
	local rc=0
	reset_registry
	touch "$FAKE/everything"
	printf '%s\n' dev >"$FAKE/tags"
	env -u GITHUB_STEP_SUMMARY -u REGISTRY_HOST PROXY_PIN_CHECK=1 PROXY_SIGNING_CUTOVER="${pin_intro}~1" \
		"$1/verify-published-artifacts.sh" HEAD >"$tmp/out" 2>&1 || rc=$?
	echo "$rc"
}
if [ -z "$pinned" ] || [ -z "$pin_intro" ]; then
	bad "verifier proxy pin: could not read HEAD's pinned reference or the commit that introduced it"
elif [ "$(grep -cF '"$tags" "proxy ' "$buggy/verify-published-artifacts.sh")" != 2 ]; then
	bad "verifier proxy pin: the #999 mutation did not apply to both call sites -- the red half would test nothing"
else
	rc="$(verify_pin "$lib")"
	good_out="$(cat "$tmp/out")"
	good_refused="$(cat "$FAKE/unauthorized" 2>/dev/null)"
	# A tag-layout pin (ghcr.io) prints `<ref>:sha256-<hex> (signature of proxy
	# …`, a referrer-layout one (quay.io) `<ref>@sha256:… <- OCI 1.1 referrer
	# (signature of proxy …`.
	n_sig="$(grep -cE "^  ok      ${pin_ref//./\\.}(:sha256-[0-9a-f]{64} |@sha256:[0-9a-f]{64} <- OCI 1\.1 referrer )\(signature of proxy (index|child) " "$tmp/out" || true)"
	rc_bug="$(verify_pin "$buggy")"
	if [ "$rc" = 0 ] && [ "$n_sig" = 3 ] && [ -z "$good_refused" ] &&
		grep -qxF "  ok      ${pin_ref}@${pin} (pinned in ${PROXY_VALUES_PATH})" <<<"$good_out" &&
		grep -qxF "PASS: $((want_checks + 4)) artifact(s) present across 1 commit(s)" <<<"$good_out" &&
		[ "$rc_bug" = 2 ] && [ -s "$FAKE/unauthorized" ] &&
		grep -qF "::error::inconclusive: could not look up the signature tags of ${pin_ref}@${pin}" "$tmp/out"; then
		ok "verifier: a signed post-cut-over proxy pin (${pin_ref}) passes on the issued token ($((want_checks + 4)) artifacts, 3 proxy signatures); #999's tag list as the token is refused 401, exit 2"
	else
		bad "verifier proxy pin: fixed rc ${rc} (${n_sig}/3 proxy signatures, $(grep -c . <<<"$good_refused") request(s) refused 401), #999-mutated rc ${rc_bug} (want 0 and 2)"
		printf '%s\n' "$good_out" | tail -5 | sed 's/^/        fixed | /'
		[ -n "$good_refused" ] && printf '        fixed | fake registry: 401 <- %.160s...\n' "$(head -1 <<<"$good_refused")"
		tail -5 "$tmp/out" | sed 's/^/        buggy | /'
	fi
fi

if [ "$n" -ne 40 ]; then
	echo "::error::ran ${n} cases, expected 40 -- a gate that checks nothing passes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "::error::registry direct lookup is wrong; see cases above" >&2
	exit 1
fi
echo "registry lookup: ${n}/${n} cases correct"
