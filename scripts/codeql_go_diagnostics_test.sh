#!/usr/bin/env bash
# Hermetic test of scripts/codeql-go-diagnostics.sh, the gate that keeps a
# partial CodeQL Go analysis from being uploaded. The SARIF fixtures are built
# here, in the shape CodeQL 2.27.1 writes (taken from real runs of this
# repository with and without the generated proto packages in the tree): every
# diagnostic is a `toolExecutionNotifications` entry with a `descriptor.id`.
#
# Run: bazel test //scripts:codeql_go_diagnostics_test (the Bazel-pinned jq), or
#      bash scripts/codeql_go_diagnostics_test.sh with jq on PATH.
# shellcheck disable=SC2016 # single-quoted $names here are jq variables,
# never shell expansions.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/codeql-go-diagnostics.sh"

if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}
export JQ

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}

MANIFEST="$TMP/manifest"
printf '# written by the materialize script\napi/aether/cni/v1/cni.pb.go\napi/aether/cni/v1/service_grpc.pb.go\n' >"$MANIFEST"

# sarif <name> <notification>...: a one-run SARIF file. Each notification is
# "ok:<uri>" (a successfully extracted file), "err:<message>" (an extraction
# error), or "raw:<id>|<level>|<message>".
sarif() {
	local name="$1" n items=()
	shift
	for n in "$@"; do
		case "$n" in
		ok:*) items+=("$("$JQ" -n --arg u "${n#ok:}" '{locations:[{physicalLocation:{artifactLocation:{uri:$u,uriBaseId:"%SRCROOT%"}}}],message:{text:""},level:"none",descriptor:{id:"go/diagnostics/successfully-extracted-files",index:0}}')") ;;
		err:*) items+=("$("$JQ" -n --arg m "${n#err:}" '{message:{text:$m},level:"error",descriptor:{id:"go/diagnostics/extraction-errors",index:1}}')") ;;
		raw:*)
			local rest="${n#raw:}" id level
			id="${rest%%|*}"
			rest="${rest#*|}"
			level="${rest%%|*}"
			items+=("$("$JQ" -n --arg i "$id" --arg l "$level" --arg m "${rest#*|}" '{message:{text:$m},level:$l,descriptor:{id:$i}}')")
			;;
		esac
	done
	printf '%s\n' "${items[@]}" | "$JQ" -s '{version:"2.1.0",runs:[{tool:{driver:{name:"CodeQL"}},results:[],invocations:[{executionSuccessful:true,toolExecutionNotifications:.}]}]}' >"$TMP/$name.sarif"
}

# The extracted set of a healthy run: hand-written files, the generated ones,
# and the non-Go files CodeQL also lists.
GOOD=(
	"ok:go.mod"
	"ok:agent/cmd/agent/main.go"
	"ok:agent/internal/cni/server/server.go"
	"ok:api/aether/cni/v1/cni.pb.go"
	"ok:api/aether/cni/v1/service_grpc.pb.go"
	"raw:go/baseline/expected-extracted-files|none|"
	"raw:cli/build-mode|none|"
)
INTERNAL="err:Extraction failed in test/envoy_validate/builders.go with error use of internal package aethermesh.dev/agent/internal/xds/config not allowed"

# expect_ok <name>: the gate passes. expect_fail <name> <substring>: it fails and says so.
expect_ok() {
	local out
	if out="$(bash "$SCRIPT" "$TMP/$1.sarif" "$MANIFEST" 2>&1)"; then
		pass "$1 passes"
	else
		fail "$1 should pass: $out"
	fi
}
expect_fail() {
	local out
	if out="$(bash "$SCRIPT" "$TMP/$1.sarif" "$MANIFEST" 2>&1)"; then
		fail "$1 should fail, passed: $out"
	elif [[ "$out" != *"$2"* ]]; then
		fail "$1 failed without '$2': $out"
	else
		pass "$1 fails: $2"
	fi
}

sarif clean "${GOOD[@]}"
expect_ok clean

sarif allowed "${GOOD[@]}" "ok:test/envoy_validate/builders.go" "$INTERNAL"
expect_ok allowed
out="$(bash "$SCRIPT" "$TMP/allowed.sarif" "$MANIFEST" 2>&1)"
if [[ "$out" == *"5 Go file(s) extracted (2 generated), 1 allowed extraction error(s)"* ]]; then
	pass "the summary counts Go files, generated files and allowed errors"
else
	fail "unexpected summary: $out"
fi

# The measured failure: a proto package that is not in the tree.
sarif missing_proto "${GOOD[@]}" \
	"err:Extraction failed in agent/internal/xds/proxy/edge.go with error cannot find module providing package aethermesh.dev/api/aether/config/v1: import lookup disabled by -mod=readonly" \
	"err:Extraction failed in agent/internal/cni/server/server.go with error undefined: cniv1.CNIPod"
expect_fail missing_proto "2 Go extraction error(s)"
expect_fail missing_proto "a generated proto package did not resolve"

# Any other extraction error fails too, not only ones about this module.
sarif other_error "${GOOD[@]}" "err:Extraction failed in common/log/log.go with error could not import fmt (no metadata for fmt)"
expect_fail other_error "1 Go extraction error(s)"

# The allow-list is anchored: the same rule violation anywhere else is an error,
# and so is a different error in the allowed directory.
sarif internal_elsewhere "${GOOD[@]}" "err:Extraction failed in prober/internal/x.go with error use of internal package aethermesh.dev/agent/internal/xds/config not allowed"
expect_fail internal_elsewhere "1 Go extraction error(s)"
sarif other_in_allowed_dir "${GOOD[@]}" "err:Extraction failed in test/envoy_validate/builders.go with error undefined: registryv1.Service"
expect_fail other_in_allowed_dir "1 Go extraction error(s)"
sarif allowed_plus_real "${GOOD[@]}" "$INTERNAL" "err:Extraction failed in common/log/log.go with error undefined: x"
expect_fail allowed_plus_real "1 Go extraction error(s)"

# The default-setup diagnostic itself, should a CodeQL emit it in this mode.
sarif package_not_found "${GOOD[@]}" "raw:go/autobuilder/package-not-found|warning|6 packages could not be found: aethermesh.dev/api/aether/config/v1"
expect_fail package_not_found "go/autobuilder/package-not-found"
# ...while a note-level autobuilder message is not a failure.
sarif autobuilder_note "${GOOD[@]}" "raw:go/autobuilder/some-info|note|fyi"
expect_ok autobuilder_note

# Coverage: an empty database, an extracted test file, a generated file CodeQL
# never saw.
sarif nothing "raw:cli/build-mode|none|"
expect_fail nothing "extracted no Go file at all"
sarif with_test "${GOOD[@]}" "ok:agent/cmd/agent/main_test.go"
expect_fail with_test "test files were extracted"
sarif generated_missing "ok:go.mod" "ok:agent/cmd/agent/main.go" "ok:api/aether/cni/v1/cni.pb.go"
expect_fail generated_missing "api/aether/cni/v1/service_grpc.pb.go"

# Inputs: a directory of SARIF files works; a missing file, a missing manifest,
# an empty manifest and a non-SARIF file do not pass.
mkdir "$TMP/dir"
cp "$TMP/clean.sarif" "$TMP/dir/go.sarif"
if bash "$SCRIPT" "$TMP/dir" "$MANIFEST" >/dev/null 2>&1; then pass "a directory of SARIF files is read"; else fail "directory input"; fi
if bash "$SCRIPT" "$TMP/absent.sarif" "$MANIFEST" >/dev/null 2>&1; then fail "a missing SARIF file passed"; else pass "a missing SARIF file fails"; fi
if bash "$SCRIPT" "$TMP/clean.sarif" "$TMP/absent" >/dev/null 2>&1; then fail "a missing manifest passed"; else pass "a missing manifest fails"; fi
echo "# nothing" >"$TMP/empty-manifest"
if bash "$SCRIPT" "$TMP/clean.sarif" "$TMP/empty-manifest" >/dev/null 2>&1; then fail "an empty manifest passed"; else pass "an empty manifest fails"; fi
echo "not json" >"$TMP/garbage.sarif"
if bash "$SCRIPT" "$TMP/garbage.sarif" "$MANIFEST" >/dev/null 2>&1; then fail "a non-SARIF file passed"; else pass "a non-SARIF file fails"; fi

if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "PASS: codeql-go-diagnostics"
