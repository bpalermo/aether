#!/usr/bin/env bash
# //e2e:go_pin_test (#1283): every workflow job and composite action that runs
# a bare `go` command builds with the Go toolchain the repository pins, not the
# runner image's preinstalled Go (which moves whenever GitHub updates the image).
#
# The authoritative pin is MODULE.bazel's `go_sdk.download(version = ...)`: the
# SDK rules_go downloads and that builds and tests everything Bazel ships. A
# workflow cannot read MODULE.bazel, so it reads go.mod through
# actions/setup-go's `go-version-file: go.mod`, and this test keeps the two
# files saying the same thing.
#
# Hermetic: reads only its data files (MODULE.bazel, go.mod, the workflows and
# the composite actions), from the runfiles root of the main repository where
# they sit at their source paths.
#
# Checks:
#   1. go.mod's effective toolchain version (its `toolchain goX.Y.Z` line when it
#      has one, which setup-go prefers, else its `go` line) is a full X.Y.Z and
#      equals go_sdk.download's version. A `go 1.27` line would let setup-go
#      pick whatever 1.27.x is newest.
#   2. every actions/setup-go step is SHA-pinned, reads `go-version-file:
#      go.mod`, and sets neither `go-version:` nor `check-latest: true`, either
#      of which would select something other than the pin.
#   3. every workflow job, and every composite action, that runs `go <cmd>`
#      sets up Go with actions/setup-go before its first such command. A job that
#      reaches Go only through a local composite action is covered by check 3
#      on that action.
set -euo pipefail

fail=0
err() {
	echo "FAIL: $*" >&2
	fail=1
}

for f in MODULE.bazel go.mod; do
	[ -f "$f" ] || {
		echo "FAIL: $f not in runfiles" >&2
		exit 1
	}
done

# --- 1. go.mod agrees with the SDK pin.
# go_sdk.download's version, however buildifier wrapped the call (the same
# reader scripts/go-deps-audit.sh uses).
sdk_version="$(
	awk '
		/^[ \t]*go_sdk\.download\(/ { inblk = 1 }
		inblk {
			buf = buf $0
			if (/\)/) {
				if (match(buf, /version[ \t]*=[ \t]*"[^"]+"/)) {
					v = substr(buf, RSTART, RLENGTH)
					sub(/^version[ \t]*=[ \t]*"/, "", v)
					sub(/"$/, "", v)
					print v
				}
				exit
			}
		}
	' MODULE.bazel
)"
semver='^[0-9]+\.[0-9]+\.[0-9]+$'
[[ "$sdk_version" =~ $semver ]] || err "MODULE.bazel: go_sdk.download version '$sdk_version' is not X.Y.Z"

gomod_go="$(sed -nE 's/^go[ \t]+([^ \t]+).*$/\1/p' go.mod | head -n1)"
gomod_toolchain="$(sed -nE 's/^toolchain[ \t]+go([^ \t]+).*$/\1/p' go.mod | head -n1)"
if [ -n "$gomod_toolchain" ]; then
	effective="$gomod_toolchain"
	from="toolchain directive"
else
	effective="$gomod_go"
	from="go directive"
fi
echo "pin: go_sdk.download $sdk_version; go.mod $from $effective"
[[ "$effective" =~ $semver ]] ||
	err "go.mod: $from '$effective' is not X.Y.Z (setup-go would resolve the newest matching release, not the pin)"
[ "$effective" = "$sdk_version" ] ||
	err "go.mod: $from '$effective' != MODULE.bazel go_sdk.download '$sdk_version' (setup-go installs the go.mod version; bump both together)"

# --- the files every remaining check reads.
mapfile -t ci_files < <(find -L .github/workflows .github/actions -type f \( -name '*.yaml' -o -name '*.yml' \) 2>/dev/null | sort)
[ "${#ci_files[@]}" -gt 0 ] || err "no workflow files in runfiles"

# --- 2. every actions/setup-go step.
# One record per step: "<line>|<ref>|<go-version-file>|<go-version>|<check-latest>".
# "|", not a tab: a whitespace IFS merges the empty fields of unset keys.
setup_go_steps() {
	awk '
	function indent(s) { match(s, /[^ ]/); return RSTART - 1 }
	function flush() {
		if (start) printf "%d|%s|%s|%s|%s\n", start, ref, v["go-version-file"], v["go-version"], v["check-latest"]
		start = 0; delete v
	}
	{
		line = $0
		if (start && line !~ /^[[:space:]]*(#|$)/ && indent(line) < keyind) flush()
		if (line ~ /uses:[[:space:]]*actions\/setup-go@/) {
			flush()
			start = NR
			keyind = indent(line)
			if (substr(line, keyind + 1, 2) == "- ") keyind += 2
			ref = line; sub(/.*actions\/setup-go@/, "", ref); sub(/[[:space:]].*$/, "", ref)
			next
		}
		if (start && match(line, /^[[:space:]]+(go-version-file|go-version|check-latest):/)) {
			k = line; sub(/^[[:space:]]+/, "", k); sub(/:.*/, "", k)
			val = line; sub(/^[^:]*:[[:space:]]*/, "", val); sub(/[[:space:]]+#.*$/, "", val)
			gsub(/^["\x27]|["\x27]$/, "", val)
			v[k] = (val == "" ? "<empty>" : val)
		}
	}
	END { flush() }' "$1"
}
steps=0
for f in "${ci_files[@]}"; do
	while IFS="|" read -r n ref vfile ver latest; do
		steps=$((steps + 1))
		[[ "$ref" =~ ^[0-9a-f]{40}$ ]] || err "$f:$n: actions/setup-go@$ref is not pinned to a full commit SHA"
		[ "$vfile" = "go.mod" ] || err "$f:$n: setup-go must read 'go-version-file: go.mod' (got '${vfile:-<unset>}')"
		[ -z "$ver" ] || err "$f:$n: setup-go sets 'go-version: $ver'; use go-version-file: go.mod so the version comes from the repository pin"
		case "$latest" in
		"" | false) ;;
		*) err "$f:$n: setup-go 'check-latest: $latest' resolves past the pin" ;;
		esac
	done < <(setup_go_steps "$f")
done
echo "checked $steps actions/setup-go step(s)"

# --- 3. every unit that runs `go` sets it up first.
# A bare go invocation in a run: script. `bazel run @rules_go//go -- ...` (the
# Bazel-pinned SDK) does not match: its `go` follows a `/`.
GO_CMD='(^|[[:space:];&|(`])go (build|env|generate|get|install|list|mod|run|test|tool|vet|version|work)([[:space:]]|$)'
# Workflows: per job under `jobs:`. Emits "<job>\t<first go line>\t<first setup-go line>".
job_go_use() {
	awk -v re="$GO_CMD" '
	function done_job() {
		if (job != "" && go_at) printf "%s\t%d\t%d\n", job, go_at, setup_at
		job = ""; go_at = 0; setup_at = 0
	}
	/^jobs:[[:space:]]*$/ { in_jobs = 1; next }
	in_jobs && /^[^[:space:]#]/ { done_job(); in_jobs = 0 }
	in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { done_job(); job = $1; sub(/:$/, "", job); next }
	job == "" || /^[[:space:]]*#/ { next }
	/uses:[[:space:]]*actions\/setup-go@/ { if (!setup_at) setup_at = NR }
	$0 ~ re { if (!go_at) go_at = NR }
	END { done_job() }' "$1"
}
# Composite actions: the whole file is one unit.
action_go_use() {
	awk -v re="$GO_CMD" '
	/^[[:space:]]*#/ { next }
	/uses:[[:space:]]*actions\/setup-go@/ { if (!setup_at) setup_at = NR }
	$0 ~ re { if (!go_at) go_at = NR }
	END { if (go_at) printf "(action)\t%d\t%d\n", go_at, setup_at }' "$1"
}
units=0
for f in "${ci_files[@]}"; do
	case "$f" in
	.github/workflows/*) records="$(job_go_use "$f")" ;;
	*) records="$(action_go_use "$f")" ;;
	esac
	[ -n "$records" ] || continue
	while IFS=$'\t' read -r unit go_at setup_at; do
		units=$((units + 1))
		if [ "$setup_at" -eq 0 ]; then
			err "$f:$go_at: $unit runs go with the runner's preinstalled toolchain; add actions/setup-go with go-version-file: go.mod before it"
		elif [ "$setup_at" -gt "$go_at" ]; then
			err "$f:$go_at: $unit runs go before its actions/setup-go step (line $setup_at)"
		fi
	done <<<"$records"
done
echo "checked $units job(s)/action(s) that run go"

if [ "$fail" -ne 0 ]; then
	echo "Go pin drift: CI must build with MODULE.bazel's go_sdk.download version, read through go.mod (docs/runbook.md, \"Bumping Go\")" >&2
	exit 1
fi
echo "PASS: every CI go invocation uses Go $sdk_version"
