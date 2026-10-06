#!/usr/bin/env bash
# Per-component line-coverage table (Markdown) from an LCOV tracefile.
# docs/runbook.md, "Code coverage".
#
#   scripts/coverage-summary.sh <report.lcov> [<tests-only.lcov>]
#
# <report.lcov> is the report that is uploaded: the combined tracefile of the
# unit suite, including a zero-hit record for every first-party file no test
# links. <tests-only.lcov>, when given, is the same run without those records
# (the tests' own tracefiles, concatenated), and adds the columns that show
# what the correction is worth: a component whose untested packages are
# invisible looks better covered than it is.
#
# A component is the first path segment (agent, cni, common, ...). A line is
# covered when its hit counts, added over every record that names it, are
# above zero, so concatenated tracefiles need no merge step. Same parsing rules
# as scripts/lcov-to-cobertura.sh, and the totals of the two agree
# (//scripts:coverage_summary_test).
#
# The last line of the output is machine-readable, for a caller that wants the
# numbers without parsing Markdown:
#   <!-- coverage-total lines=<n> covered=<n> percent=<p> -->
set -euo pipefail
export LC_ALL=C

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
	echo "usage: $0 <report.lcov> [<tests-only.lcov>]" >&2
	exit 2
fi
for f in "$@"; do
	[ -f "$f" ] || {
		echo "coverage-summary: no such file: $f" >&2
		exit 2
	}
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# "<component>\t<files>\t<lines>\t<covered>" per component, sorted.
per_component() { # lcov
	awk '
		{ sub(/\r$/, "") }
		/^SF:/ {
			file = substr($0, 4)
			while (substr(file, 1, 2) == "./") file = substr(file, 3)
			next
		}
		/^DA:/ {
			if (file == "") next
			n = split(substr($0, 4), part, ",")
			if (n < 2) next
			hits[file SUBSEP (part[1] + 0)] += part[2]
			next
		}
		/^end_of_record$/ { file = "" }
		END {
			for (key in hits) {
				split(key, k, SUBSEP)
				component = k[1]
				sub(/\/.*/, "", component)
				if (!((component, k[1]) in seen)) {
					seen[component, k[1]] = 1
					files[component]++
				}
				lines[component]++
				if (hits[key] > 0) covered[component]++
			}
			for (component in lines)
				printf "%s\t%d\t%d\t%d\n", component, files[component], lines[component], covered[component]
		}
	' "$1" | sort -t "$(printf '\t')" -k1,1
}

per_component "$1" >"$work/report"
if [ ! -s "$work/report" ]; then
	echo "coverage-summary: no DA record in $1: refusing to summarise an empty report" >&2
	exit 1
fi
if [ "$#" -eq 2 ]; then
	per_component "$2" >"$work/tests_only"
else
	: >"$work/tests_only"
fi

awk -F '\t' -v with_tests_only="$(($# - 1))" '
	function pct(hit, valid) { return valid ? sprintf("%.2f%%", 100 * hit / valid) : "n/a" }
	phase == 1 {
		t_lines[$1] = $3
		t_covered[$1] = $4
		tt_lines += $3
		tt_covered += $4
		next
	}
	FNR == 1 {
		if (with_tests_only) {
			print "| Component | Files | Lines | Covered | Coverage | Tests-only lines | Tests-only covered | Tests-only coverage |"
			print "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"
		} else {
			print "| Component | Files | Lines | Covered | Coverage |"
			print "| --- | ---: | ---: | ---: | ---: |"
		}
	}
	{
		row = sprintf("| `%s` | %d | %d | %d | %s |", $1, $2, $3, $4, pct($4, $3))
		if (with_tests_only)
			row = row sprintf(" %d | %d | %s |", t_lines[$1], t_covered[$1], pct(t_covered[$1], t_lines[$1]))
		print row
		files += $2
		lines += $3
		covered += $4
	}
	END {
		row = sprintf("| **Total** | %d | %d | %d | **%s** |", files, lines, covered, pct(covered, lines))
		if (with_tests_only)
			row = row sprintf(" %d | %d | %s |", tt_lines, tt_covered, pct(tt_covered, tt_lines))
		print row
		print ""
		printf "<!-- coverage-total lines=%d covered=%d percent=%.2f -->\n", lines, covered, lines ? 100 * covered / lines : 0
	}
' phase=1 "$work/tests_only" phase=2 "$work/report"
