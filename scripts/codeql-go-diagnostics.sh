#!/usr/bin/env bash
# Fail when CodeQL's Go analysis was partial: the gate between "analyze" and
# "upload" in .github/workflows/codeql.yaml. docs/runbook.md, "CodeQL code
# scanning".
#
#   scripts/codeql-go-diagnostics.sh <results.sarif | directory of *.sarif> [manifest]
#
# CodeQL does not fail a scan it could only half do. Under default setup this
# repository's Go scan was green for months while reporting "6 packages could
# not be found" on a status page nobody is sent to, with every file that
# imports the generated proto packages analysed without their types. A partial
# database also uploads cleanly, and code scanning then closes every alert in
# the code it did not understand as "fixed". So the workflow runs this before
# the upload.
#
# It reads the SARIF file CodeQL wrote, not the log: every run records its
# diagnostics as `runs[].invocations[].toolExecutionNotifications[]`, each with
# a stable `descriptor.id`. Three checks:
#
#   1. no `go/diagnostics/extraction-errors` notification, except the ones on
#      the allow-list below. That is the diagnostic a traced build produces when
#      a package cannot be resolved or type-checked ("cannot find module
#      providing package aethermesh.dev/api/...", "undefined: cniv1.CNIPod").
#   2. no `go/autobuilder/*` notification at warning or error level. That
#      family holds `go/autobuilder/package-not-found`, the "N packages could
#      not be found" message itself; the manual build mode does not normally
#      emit it, and if a future CodeQL does, it must not pass unnoticed.
#   3. coverage, so the gate cannot pass on an empty database: at least one
#      file was extracted, no _test.go was (tests are deliberately not
#      scanned), and every file listed in the materialize manifest (second
#      argument, default .materialized-generated-go) is among the
#      `go/diagnostics/successfully-extracted-files`.
#
# The allow-list is the two packages scripts/go-build-plain.sh documents as
# unbuildable by the plain go command: test/envoy_validate imports
# agent/internal/... from outside agent/, which Bazel permits and Go's
# internal-package rule does not. Their files are extracted and analysed; the
# extractor records the rule violation once per file.
set -euo pipefail

JQ="${JQ:-jq}"

die() {
	echo "codeql-go-diagnostics: $*" >&2
	exit 1
}

[ "$#" -ge 1 ] && [ "$#" -le 2 ] || die "usage: $0 <results.sarif | directory> [manifest]"
input="$1"
manifest="${2:-.materialized-generated-go}"

sarifs=()
if [ -d "$input" ]; then
	for f in "$input"/*.sarif; do
		[ -f "$f" ] && sarifs+=("$f")
	done
elif [ -f "$input" ]; then
	sarifs=("$input")
fi
[ "${#sarifs[@]}" -gt 0 ] || die "no SARIF file at $input"
[ -f "$manifest" ] || die "no materialize manifest at $manifest (run scripts/materialize-generated-go.sh)"

# An extraction error that is expected, as an anchored regular expression over
# the notification's message.
ALLOWED='^Extraction failed in test/envoy_validate/[^ ]+ with error use of internal package aethermesh\.dev/agent/internal/[^ ]+ not allowed$'

# One line per notification of the Go extractor: "<id>\t<level>\t<uri>\t<message>".
notifications="$(
	"$JQ" -r '
		.runs[]
		| select(.tool.driver.name == "CodeQL")
		| .invocations[]?.toolExecutionNotifications[]?
		| select(.descriptor.id | startswith("go/"))
		| [
			.descriptor.id,
			(.level // "warning"),
			(.locations[0].physicalLocation.artifactLocation.uri // ""),
			((.message.text // "") | gsub("[\t\n]"; " "))
		  ]
		| @tsv' "${sarifs[@]}"
)" || die "could not read ${sarifs[*]} as SARIF"

extracted="$(awk -F'\t' '$1 == "go/diagnostics/successfully-extracted-files" { print $3 }' <<<"$notifications" | LC_ALL=C sort -u)"
errors="$(awk -F'\t' '$1 == "go/diagnostics/extraction-errors" { print $4 }' <<<"$notifications")"
autobuilder="$(awk -F'\t' '$1 ~ /^go\/autobuilder\// && ($2 == "error" || $2 == "warning") { print $1 ": " $4 }' <<<"$notifications")"

fail=0
err() {
	echo "codeql-go-diagnostics: FAIL: $*" >&2
	fail=1
}
# show <text>: at most 25 lines of it, indented, on stderr.
show() {
	local n
	n="$(grep -c '' <<<"$1")"
	awk 'NR <= 25 { print "  " $0 }' <<<"$1" >&2
	if [ "$n" -gt 25 ]; then echo "  ... and $((n - 25)) more" >&2; fi
	return 0
}

# --- 1. extraction errors.
unexpected=""
allowed_n=0
if [ -n "$errors" ]; then
	unexpected="$(grep -Ev -- "$ALLOWED" <<<"$errors" || true)"
	allowed_n="$(grep -Ec -- "$ALLOWED" <<<"$errors" || true)"
fi
if [ -n "$unexpected" ]; then
	err "$(grep -c '' <<<"$unexpected") Go extraction error(s): the database is partial, and so is every result computed from it"
	show "$unexpected"
	if grep 'aethermesh\.dev/api/' <<<"$unexpected" >/dev/null; then
		echo "  -> a generated proto package did not resolve: scripts/materialize-generated-go.sh did not run, or a new go_proto_library is not covered (docs/runbook.md, \"CodeQL code scanning\")" >&2
	fi
fi

# --- 2. the autobuilder family (package-not-found and friends).
if [ -n "$autobuilder" ]; then
	err "CodeQL reported Go build/package diagnostics"
	show "$autobuilder"
fi

# --- 3. coverage.
go_files="$(grep '\.go$' <<<"$extracted" || true)"
n_go=0
[ -z "$go_files" ] || n_go="$(grep -c '' <<<"$go_files")"
[ "$n_go" -gt 0 ] || err "CodeQL extracted no Go file at all"
tests="$(grep '_test\.go$' <<<"$go_files" || true)"
if [ -n "$tests" ]; then
	err "test files were extracted; this scan is meant to cover non-test code only"
	show "$tests"
fi
missing=""
n_generated=0
while IFS= read -r rel; do
	case "$rel" in "" | "#"*) continue ;; esac
	n_generated=$((n_generated + 1))
	grep -xF -- "$rel" <<<"$go_files" >/dev/null || missing+="$rel"$'\n'
done <"$manifest"
[ "$n_generated" -gt 0 ] || err "the materialize manifest $manifest lists no file"
if [ -n "$missing" ]; then
	err "generated Go file(s) were materialized but CodeQL did not extract them"
	show "${missing%$'\n'}"
fi

echo "codeql-go-diagnostics: $n_go Go file(s) extracted ($n_generated generated), $allowed_n allowed extraction error(s) in test/envoy_validate"
if [ "$fail" -ne 0 ]; then
	die "the Go analysis is incomplete; nothing from this run should be uploaded"
fi
echo "codeql-go-diagnostics: clean"
