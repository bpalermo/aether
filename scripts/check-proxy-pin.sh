#!/usr/bin/env bash
# Exercise the aether-proxy pin logic the signature sweep relies on (#984):
# proxy_pinned_digest / proxy_pinned_ref (which digest a values.yaml pins, and
# under which reference), proxy_pin_verdict (whether that pin is checked or
# skipped as pre-signing history), and proxy_pin_rewrite (the bump-chart job's
# edit).
#
# The pin may name exactly image_reference("proxy") and nothing else. During the
# Quay cut-over (proposal 040 phase 2) it could also name the pre-cut-over
# image; phase 4 removed that allowance, so a pin on the pre-cut-over reference
# (derived below from registry.bzl's own history note, never typed here) is
# refused by the reader AND by the rewrite -- never looked up on the old
# registry.
#
# The two dangerous failures are both SILENT:
#   - reading the wrong digest (the supervisor image sits directly below the
#     proxy's and has a `digest:` line of its own), which would check the
#     signature of an image the chart does not deploy for the proxy; and
#   - a skip that is too wide. The cut-over exists so pins that predate proxy
#     signing do not keep the sweep red forever; if it also swallowed a NEW
#     unsigned pin, the gate would be vacuous (#853). So the cases below insist
#     that a post-cut-over pin with no signature is `check` (which the sweep
#     reports as MISSING), including a revert to an old unsigned digest.
#
# No registry access: tag lists are literals, and the git history is a throw-
# away repository built here.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
# shellcheck disable=SC1091
. scripts/registry-lib.sh
unset PROXY_SIGNING_CUTOVER # read the COMMITTED value, not an override
# shellcheck disable=SC1091
. scripts/proxy-pin-lib.sh
committed_cutover="$PROXY_SIGNING_CUTOVER"

d1="sha256:$(printf '1%.0s' {1..64})"
d2="sha256:$(printf '2%.0s' {1..64})"
fail=0
n=0

ok() {
	printf '  ok    %s\n' "$1"
}
bad() {
	printf '  FAIL  %s\n' "$1"
	fail=1
}

# The pre-cut-over proxy reference, from the setting's history note in
# bazel/img/registry.bzl (the four assignments it quotes, indented four spaces).
pre_bzl="$(mktemp)"
sed -nE 's/^    (IMAGE_REGISTRY|IMAGE_NAMESPACE|IMAGE_NAME_OVERRIDES|CHART_REPOSITORY_PREFIX) = /\1 = /p' \
	bazel/img/registry.bzl >"$pre_bzl"
old_ref="$(IMAGE_REGISTRY_BZL="$pre_bzl" scripts/image-registry.sh ref proxy 2>/dev/null)" || old_ref=""
rm -f "$pre_bzl"
if [ -z "$PROXY_IMAGE" ] || [ -z "$old_ref" ] || [ "$old_ref" = "$PROXY_IMAGE" ]; then
	echo "::error::need image_reference(\"proxy\") and the pre-cut-over proxy reference (bazel/img/registry.bzl's history note) to exercise the refusal cases; got [${PROXY_IMAGE}] and [${old_ref}]" >&2
	exit 2
fi

# values <digest line> [<proxy repository>] — a values.yaml shaped like
# charts/aether/values.yaml around the proxy block, including the supervisor's
# own digest right below it.
values() {
	local repo="${2:-$PROXY_IMAGE}"
	cat <<EOF
agent:
  image:
    repository: "{@//agent/cmd/agent:image_push.repository}"
    digest: "{@//agent/cmd/agent:image_push.digest}"
proxy:
  image:
    repository: ${repo}
    tag: 0123456789abcdef0123456789abcdef01234567
    # Digest-pinned (option A): content-addressed, tamper-proof. The aether.image
    # helper prefers digest over tag. Multi-arch index for 0123456.
$1
    pullPolicy: Always
  supervisor:
    image:
      repository: "{@//agent/cmd/proxy-supervisor:image_push.repository}"
      digest: "sha256:$(printf 'f%.0s' {1..64})"
EOF
}

# want_digest <name> <expected> <values text>
want_digest() {
	local name="$1" want="$2" got
	n=$((n + 1))
	if got="$(printf '%s\n' "$3" | proxy_pinned_digest)" && [ "$got" = "$want" ]; then
		ok "$name"
	else
		bad "$name: want [$want] got [$got]"
	fi
}

# want_no_digest <name> <values text>
want_no_digest() {
	local name="$1" got
	n=$((n + 1))
	if got="$(printf '%s\n' "$2" | proxy_pinned_digest)"; then
		bad "$name: accepted, printed [$got]"
	else
		ok "$name (refused)"
	fi
}

echo "proxy_pinned_digest:"
want_digest "quoted digest (the bump-chart shape)" "$d1" "$(values "    digest: \"$d1\"")"
want_digest "unquoted digest with a trailing comment" "$d1" "$(values "    digest: $d1 # pinned")"
want_digest "the real charts/aether/values.yaml parses" \
	"$(sed -nE '/^proxy:/,/^[[:space:]]*digest:/ s/^[[:space:]]*digest:[[:space:]]*"(sha256:[0-9a-f]{64})"$/\1/p' charts/aether/values.yaml)" \
	"$(cat charts/aether/values.yaml)"
# The supervisor's digest must never stand in for a missing proxy one.
want_no_digest "proxy block without a digest (supervisor digest below)" "$(values "    # no digest")"
want_no_digest "empty digest" "$(values '    digest: ""')"
want_no_digest "stamp placeholder instead of a digest" "$(values '    digest: "{@//proxy:image_push.digest}"')"
want_no_digest "truncated digest" "$(values '    digest: "sha256:938c5a57"')"
want_no_digest "no proxy block at all" "$(printf 'agent:\n  image:\n    digest: "%s"\n' "$d1")"
want_no_digest "two proxy blocks" "$(values "    digest: \"$d1\"")"$'\n'"$(values "    digest: \"$d2\"" | sed -n '5,$p')"

# --- the cut-over: which reference the pin may name -------------------------
# want_ref <name> <expected "<ref> <digest>"> <values text>
want_ref() {
	local name="$1" want="$2" got
	n=$((n + 1))
	if got="$(printf '%s\n' "$3" | proxy_pinned_ref)" && [ "$got" = "$want" ]; then
		ok "$name"
	else
		bad "$name: want [$want] got [$got]"
	fi
}
echo "proxy_pinned_ref (only image_reference(\"proxy\"); proposal 040 phase 4):"
want_ref "pin on image_reference(\"proxy\")" "${PROXY_IMAGE} ${d1}" "$(values "    digest: \"$d1\"")"
want_no_digest "pin on the pre-cut-over reference ${old_ref}, never looked up there" \
	"$(values "    digest: \"$d1\"" "$old_ref")"
want_no_digest "pin on a registry nobody named (not accepted)" "$(values "    digest: \"$d1\"" "registry.invalid/someone/aether-proxy")"
want_ref "a stray pre-cut-over block beside the current one: only the current pin is read" \
	"${PROXY_IMAGE} ${d2}" \
	"$(values "    digest: \"$d1\"" "$old_ref")"$'\n'"$(values "    digest: \"$d2\"" | sed -n '5,$p')"

# --- proxy_pin_rewrite (the bump-chart edit) --------------------------------
echo "proxy_pin_rewrite:"
rw_dir="$(mktemp -d)"
sup="sha256:$(printf 'f%.0s' {1..64})"
new_tag=fedcba9876543210fedcba9876543210fedcba98

# rewrite_case <name> <from repository> — the next release pins d2 under
# image_reference("proxy").
rewrite_case() {
	local name="$1" from="$2" f="${rw_dir}/values.yaml" got
	values "    digest: \"$d1\"" "$from" >"$f"
	n=$((n + 1))
	if ! proxy_pin_rewrite "$f" "$PROXY_IMAGE" "$new_tag" "$d2"; then
		bad "$name: the rewrite failed"
		return
	fi
	got="$(proxy_pinned_ref <"$f")" || got=""
	if [ "$got" = "${PROXY_IMAGE} ${d2}" ] &&
		grep -qxF "    tag: ${new_tag}" "$f" &&
		grep -qF "Multi-arch index for ${new_tag}." "$f" &&
		grep -qxF "      digest: \"${sup}\"" "$f" &&
		[ "$(grep -c "$d1" "$f")" = 0 ]; then
		ok "$name -> ${got%% *} @ new digest, tag + provenance moved, supervisor digest untouched"
	else
		bad "$name: pinned [$got]"
		sed 's/^/        | /' "$f"
	fi
}
rewrite_case "current pin is re-pinned in place" "$PROXY_IMAGE"

# refuse_rewrite <name> <values text> <image> <tag> <digest>
refuse_rewrite() {
	local name="$1" f="${rw_dir}/values.yaml" before
	printf '%s\n' "$2" >"$f"
	before="$(cat "$f")"
	n=$((n + 1))
	if proxy_pin_rewrite "$f" "$3" "$4" "$5"; then
		bad "$name: accepted"
	elif [ "$(cat "$f")" != "$before" ]; then
		bad "$name: refused but CHANGED the file"
	else
		ok "$name (refused, file unchanged)"
	fi
}
refuse_rewrite "no accepted proxy block" "$(values "    digest: \"$d1\"" "registry.invalid/someone/aether-proxy")" "$PROXY_IMAGE" "$new_tag" "$d2"
refuse_rewrite "a pin on the pre-cut-over reference is not moved (no fallback)" "$(values "    digest: \"$d1\"" "$old_ref")" "$PROXY_IMAGE" "$new_tag" "$d2"
refuse_rewrite "a proxy block with no digest" "$(values "    # no digest")" "$PROXY_IMAGE" "$new_tag" "$d2"
refuse_rewrite "a truncated digest to pin" "$(values "    digest: \"$d1\"")" "$PROXY_IMAGE" "$new_tag" "sha256:938c5a57"
rm -rf "$rw_dir"

# --- proxy_pin_verdict, over a throwaway history ----------------------------
#
#   c1  pins d1                      <- PROXY_SIGNING_CUTOVER
#   c2  unrelated change             (still pins d1)
#   c3  pins d2                      (a post-cut-over release)
#   c4  re-pins d1                   (a revert, post-cut-over)
echo "proxy_pin_verdict:"
repo_dir="$(mktemp -d)"
trap 'rm -rf "$repo_dir"' EXIT
(
	set -e
	cd "$repo_dir"
	git init -q -b main .
	git config user.email check@example.invalid
	git config user.name check-proxy-pin
	git config commit.gpgsign false
	mkdir -p charts/aether
	values "    digest: \"$d1\"" >charts/aether/values.yaml
	git add -A && git commit -qm c1
	echo "# unrelated" >>charts/aether/values.yaml
	git commit -qam c2
	values "    digest: \"$d2\"" >charts/aether/values.yaml
	git commit -qam c3
	values "    digest: \"$d1\"" >charts/aether/values.yaml
	git commit -qam c4
) || {
	echo "::error::could not build the throwaway history" >&2
	exit 2
}
rev() { git -C "$repo_dir" rev-parse "main~$1"; }
c1="$(rev 3)"
c2="$(rev 2)"
c3="$(rev 1)"
c4="$(rev 0)"

unsigned=(dev-0123 other)
signed_d1="$(registry_signature_tag_bundle "$d1")"
signed_d2="$(registry_signature_tag_legacy "$d2")"

# want_verdict <name> <expected> <sha> <digest> <tags...>
want_verdict() {
	local name="$1" want="$2" sha="$3" digest="$4" got rc
	shift 4
	n=$((n + 1))
	got="$(cd "$repo_dir" && PROXY_SIGNING_CUTOVER="$c1" proxy_pin_verdict "$sha" "$digest" "$(printf '%s\n' "$@")" 2>/dev/null)"
	rc=$?
	if [ "$rc" -eq 0 ] && [ "$got" = "$want" ]; then
		ok "$name -> $got"
	else
		bad "$name: want [$want] got [$got] (rc=$rc)"
	fi
}

want_verdict "pre-cut-over pin, unsigned -> skipped as history" "skip $c1" "$c1" "$d1" "${unsigned[@]}"
want_verdict "later commit still on the pre-cut-over pin -> skipped" "skip $c1" "$c2" "$d1" "${unsigned[@]}"
want_verdict "pre-cut-over pin that IS signed -> checked" check "$c2" "$d1" "${unsigned[@]}" "$signed_d1"
want_verdict "post-cut-over pin, unsigned -> checked (goes red)" check "$c3" "$d2" "${unsigned[@]}"
want_verdict "post-cut-over pin, signed -> checked" check "$c3" "$d2" "${unsigned[@]}" "$signed_d2"
want_verdict "revert to an old unsigned digest after the cut-over -> checked (goes red)" check "$c4" "$d1" "${unsigned[@]}"

# Cannot decide -> exit 2, never a skip.
n=$((n + 1))
if got="$(cd "$repo_dir" && PROXY_SIGNING_CUTOVER="$(printf 'e%.0s' {1..40})" proxy_pin_verdict "$c4" "$d1" "" 2>/dev/null)"; then
	bad "unknown cut-over commit: accepted, printed [$got]"
else
	ok "unknown cut-over commit (refused)"
fi
n=$((n + 1))
if got="$(cd "$repo_dir" && PROXY_SIGNING_CUTOVER="$c1" proxy_pin_verdict "$c4" "$d2" "" 2>/dev/null)"; then
	bad "digest no longer pinned at this commit: accepted, printed [$got]"
else
	ok "digest no longer pinned at this commit (refused)"
fi

# The committed cut-over is a FULL sha. An abbreviation would still resolve
# today and silently stop resolving once it becomes ambiguous; a typo would
# make every sweep exit 2. Existence is checked by the sweep itself (it has the
# full history; CI's shallow checkout here does not).
n=$((n + 1))
if [[ "$committed_cutover" =~ ^[0-9a-f]{40}$ ]]; then
	ok "committed PROXY_SIGNING_CUTOVER is a full 40-hex sha"
else
	bad "committed PROXY_SIGNING_CUTOVER is not a full sha: [$committed_cutover]"
fi

if [ "$n" -ne 27 ]; then
	echo "::error::ran ${n} cases, expected 27 -- a gate that checks nothing passes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "::error::aether-proxy pin logic is wrong; see cases above" >&2
	exit 1
fi
echo "aether-proxy pin: ${n}/${n} cases correct"
