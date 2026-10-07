#!/usr/bin/env bash
# The coverage gate: compare a pull request's line coverage with the baseline
# from main and fail when the total drops by more than a threshold.
# docs/runbook.md, "Code coverage". Run by the `gate` job of
# .github/workflows/coverage.yaml; scripts/coverage-baseline.sh finds the
# baseline it is given.
#
#   scripts/coverage-compare.sh --baseline <main.lcov> --head <pr.lcov>
#       [--baseline-label <text>] [--changed-files <list>]
#       [--max-drop <points>] [--min <percent>] [--top-drops <n>]
#
# Both inputs are the `coverage.lcov` scripts/coverage.sh writes: the whole unit
# suite, with a zero-hit record for every first-party file no test links.
#
# WHAT GATES
#
#   - The TOTAL line coverage may not drop by more than --max-drop percentage
#     points (default: the COVERAGE_MAX_DROP environment variable, else
#     DEFAULT_MAX_DROP below). A drop of exactly the threshold passes.
#   - With --min (default: the COVERAGE_MIN environment variable, else off) the
#     total may also not be below that percentage.
# An empty COVERAGE_MAX_DROP / COVERAGE_MIN means "not set" (an unset repository
# variable expands to the empty string in a workflow). Anything else that is
# not a plain decimal number between 0 and 100 is refused: a mistyped variable
# must not quietly become the default, or quietly switch the gate off.
#
# WHAT IS SHOWN AND DOES NOT GATE
#
#   - a per-component table (a component is the first path segment) with the
#     delta of each, including components only one side has;
#   - with --changed-files (one repo-relative path per line: what
#     `git diff --name-only` prints), the coverage of each of those files that
#     either report names, with its delta. Paths neither report names (tests,
#     non-Go files, generated code) are counted, not listed;
#   - when the total dropped, the --top-drops (default DEFAULT_TOP_DROPS) files
#     whose COVERED-LINE COUNT fell most, whatever the diff touched, and the
#     files that left the report altogether (`removed`). This is the table that
#     names the cause when no measured file changed (#1319): a test disabled or
#     dropped in a BUILD file changes no Go file, so the changed-files table is
#     empty and only a component's delta hints at it. Ranked by covered lines
#     lost, then by path; a line says how many more there are. Open when the
#     gate fails, collapsed (<details>) when the drop is within the threshold,
#     absent when the total did not drop: a clean pull request stays short.
# Patch coverage is deliberately not a gate: it punishes the pull request that
# touches old untested code and rewards the one that leaves it alone.
#
# OUTPUT
#
#   stdout  the report, as Markdown (the job summary);
#   stderr  exactly one workflow command: `::notice` when the gate passes,
#           `::error` when it fails or cannot be evaluated.
#
# EXIT
#
#   0  the gate passes;
#   1  the gate fails (the drop is beyond the threshold, or below --min);
#   2  the gate could not be evaluated: no baseline, an unreadable or empty
#      report, a malformed threshold, bad usage. Never a pass.
set -euo pipefail
export LC_ALL=C

# The maximum drop of the total, in percentage points, when neither --max-drop
# nor COVERAGE_MAX_DROP says otherwise. Run-to-run noise of the suite is at
# most 0.06 points (docs/runbook.md), so this is not a noise margin: it is how
# much a single pull request may cost.
DEFAULT_MAX_DROP=1.0

# How many rows the "largest per-file drops" table shows at most.
DEFAULT_TOP_DROPS=10

baseline=""
head=""
label="main"
changed=""
max_drop="${COVERAGE_MAX_DROP:-}"
max_drop_source="the repository variable COVERAGE_MAX_DROP"
min="${COVERAGE_MIN:-}"
min_source="the repository variable COVERAGE_MIN"
top_drops="$DEFAULT_TOP_DROPS"

# One `::error` on stderr and exit 2: the gate was not evaluated.
refuse() { # title, message
	echo "::error title=$1::$2" >&2
	exit 2
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--baseline | --head | --baseline-label | --changed-files | --max-drop | --min | --top-drops)
		[ "$#" -ge 2 ] || refuse "Coverage gate: usage" "$1 needs a value"
		case "$1" in
		--baseline) baseline="$2" ;;
		--head) head="$2" ;;
		--baseline-label) label="$2" ;;
		--changed-files) changed="$2" ;;
		--max-drop)
			max_drop="$2"
			max_drop_source="--max-drop"
			;;
		--min)
			min="$2"
			min_source="--min"
			;;
		--top-drops) top_drops="$2" ;;
		esac
		shift 2
		;;
	*) refuse "Coverage gate: usage" "unknown argument: $1" ;;
	esac
done
[ -n "$baseline" ] && [ -n "$head" ] || refuse "Coverage gate: usage" "--baseline and --head are required"

# A threshold is a plain decimal number of at most 100: no sign, no exponent,
# no unit, no surrounding space.
is_threshold() {
	[[ "$1" =~ ^[0-9]+(\.[0-9]+)?$ ]] && awk -v v="$1" 'BEGIN { exit !(v + 0 <= 100) }'
}
if [ -z "$max_drop" ]; then
	max_drop="$DEFAULT_MAX_DROP"
	max_drop_source="the default"
fi
is_threshold "$max_drop" ||
	refuse "Coverage gate: malformed threshold" "the maximum coverage drop from $max_drop_source is '$max_drop': it must be a plain number of percentage points between 0 and 100 (for example 1.0). Fix the variable (Settings > Secrets and variables > Actions > Variables) or delete it to use the default of $DEFAULT_MAX_DROP. docs/runbook.md, 'Code coverage'."
if [ -n "$min" ]; then
	is_threshold "$min" ||
		refuse "Coverage gate: malformed threshold" "the minimum coverage from $min_source is '$min': it must be a plain percentage between 0 and 100 (for example 75), or unset for no minimum. docs/runbook.md, 'Code coverage'."
fi
[[ "$top_drops" =~ ^[1-9][0-9]{0,3}$ ]] ||
	refuse "Coverage gate: usage" "--top-drops is '$top_drops': it must be a whole number of rows between 1 and 9999"

[ -f "$baseline" ] ||
	refuse "Coverage gate: no baseline" "there is no baseline report to compare with ($baseline does not exist), so the gate cannot pass. Re-run the coverage workflow on main (Actions > coverage > Run workflow, or: gh workflow run coverage.yaml --ref main), then re-run this job. docs/runbook.md, 'Code coverage'."
[ -f "$head" ] || refuse "Coverage gate: no report" "the pull request's report ($head) does not exist"
if [ -n "$changed" ] && [ ! -f "$changed" ]; then
	refuse "Coverage gate: usage" "--changed-files: no such file: $changed"
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# "<file>\t<lines>\t<covered>" per file. Same parsing rules as
# scripts/coverage-summary.sh: a line is covered when its hit counts, added
# over every record that names it, are above zero.
per_file() { # lcov
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
				lines[k[1]]++
				if (hits[key] > 0) covered[k[1]]++
			}
			for (f in lines) printf "%s\t%d\t%d\n", f, lines[f], covered[f]
		}
	' "$1"
}

per_file "$baseline" >"$work/base"
[ -s "$work/base" ] ||
	refuse "Coverage gate: empty baseline" "the baseline report ($label) has no DA record, so there is nothing to compare with. Re-run the coverage workflow on main (gh workflow run coverage.yaml --ref main), then re-run this job."
per_file "$head" >"$work/head"
[ -s "$work/head" ] || refuse "Coverage gate: empty report" "the pull request's report ($head) has no DA record"

# The row order of both tables: every component, and every changed path, sorted.
cut -f1 "$work/base" "$work/head" | sed 's|/.*||' | sort -u >"$work/components"
if [ -n "$changed" ]; then
	sed -e 's/\r$//' -e 's|^\./||' -e '/^$/d' "$changed" | sort -u >"$work/changed"
else
	: >"$work/changed"
fi

# Every file that lost covered lines, and every file only the baseline names:
# "<covered lines lost>\t<file>\t<baseline covered>\t<baseline lines>\t<covered>\t<lines>",
# the last two "-" for a file that left the report. Largest loss first, then by
# path, so the order (and the cut at --top-drops) is the same on every run.
awk -F '\t' '
	phase == 1 { lines[$1] = $2; covered[$1] = $3; next }
	{
		in_head[$1] = 1
		if (($1 in covered) && $3 < covered[$1])
			printf "%d\t%s\t%d\t%d\t%d\t%d\n", covered[$1] - $3, $1, covered[$1], lines[$1], $3, $2
	}
	END {
		for (f in covered)
			if (!(f in in_head)) printf "%d\t%s\t%d\t%d\t-\t-\n", covered[f], f, covered[f], lines[f]
	}
' phase=1 "$work/base" phase=2 "$work/head" | sort -t "$(printf '\t')" -k1,1nr -k2,2 >"$work/drops"

awk -F '\t' \
	-v label="$label" \
	-v top="$top_drops" \
	-v max_drop="$max_drop" -v max_drop_source="$max_drop_source" \
	-v min="$min" -v min_source="$min_source" \
	-v with_changed="$([ -n "$changed" ] && echo 1 || echo 0)" \
	-v verdict="$work/verdict" '
	function pct(hit, valid) { return valid ? sprintf("%.2f%%", 100 * hit / valid) : "n/a" }
	function rate(hit, valid) { return valid ? 100 * hit / valid : 0 }
	function signed(n) { return sprintf("%+d", n) }
	function points(d) { return sprintf("%+.2f", d) }
	# One side of a row: "lines | covered | coverage", or dashes when absent.
	function side(present, l, c) { return present ? sprintf("%d | %d | %s", l, c, pct(c, l)) : "- | - | -" }
	phase == 1 {
		component = $1
		sub(/\/.*/, "", component)
		b_lines[$1] = $2; b_covered[$1] = $3; in_base[$1] = 1
		cb_lines[component] += $2; cb_covered[component] += $3; c_in_base[component] = 1
		tb_lines += $2; tb_covered += $3
		next
	}
	phase == 2 {
		component = $1
		sub(/\/.*/, "", component)
		h_lines[$1] = $2; h_covered[$1] = $3; in_head[$1] = 1
		ch_lines[component] += $2; ch_covered[component] += $3; c_in_head[component] = 1
		th_lines += $2; th_covered += $3
		next
	}
	phase == 3 {
		if (c_in_base[$1] && c_in_head[$1])
			note = points(rate(ch_covered[$1], ch_lines[$1]) - rate(cb_covered[$1], cb_lines[$1]))
		else
			note = c_in_head[$1] ? "new" : "removed"
		component_rows = component_rows sprintf("| `%s` | %s | %s | %s |\n", $1, \
			side(c_in_base[$1], cb_lines[$1], cb_covered[$1]), \
			side(c_in_head[$1], ch_lines[$1], ch_covered[$1]), note)
		next
	}
	phase == 4 {
		if (!in_base[$1] && !in_head[$1]) { unreported++; next }
		if (in_base[$1] && in_head[$1])
			note = points(rate(h_covered[$1], h_lines[$1]) - rate(b_covered[$1], b_lines[$1]))
		else
			note = in_head[$1] ? "new" : "removed"
		changed_rows = changed_rows sprintf("| `%s` | %s | %s | %s |\n", $1, \
			side(in_base[$1], b_lines[$1], b_covered[$1]), \
			side(in_head[$1], h_lines[$1], h_covered[$1]), note)
		reported++
		f_base_lines += b_lines[$1]; f_base_covered += b_covered[$1]
		f_head_lines += h_lines[$1]; f_head_covered += h_covered[$1]
	}
	phase == 5 {
		drops++
		drop_lines += $1
		if (drops > top) { more++; more_lines += $1; next }
		drop_rows = drop_rows sprintf("| `%s` | %d / %d | %s | %s |\n", $2, $3, $4, \
			($5 == "-") ? "removed" : $5 " / " $6, $1 ? signed(-$1) : "0")
	}
	END {
		base = rate(tb_covered, tb_lines)
		head = rate(th_covered, th_lines)
		delta = head - base
		# 1e-9: the comparison is between two quotients of integers, and a
		# drop of exactly the threshold must pass whatever the rounding.
		dropped = (-delta > max_drop + 1e-9)
		below = (min != "" && head < min - 1e-9)
		failed = dropped || below

		numbers = sprintf("%s (%d of %d lines) against %s for the baseline %s (%d of %d lines): %+.3f points", \
			pct(th_covered, th_lines), th_covered, th_lines, pct(tb_covered, tb_lines), label, tb_covered, tb_lines, delta)
		limits = sprintf("maximum drop %s points (%s)", max_drop, max_drop_source)
		limits = limits (min != "" ? sprintf(", minimum %s%% (%s)", min, min_source) : ", no minimum")

		printf "## Coverage gate: %s\n\n", failed ? "FAILED" : "passed"
		if (dropped)
			printf "Total line coverage dropped by **%.3f points**, more than the **%s** allowed.\n\n", -delta, max_drop
		if (below)
			printf "Total line coverage is **%s**, below the minimum of **%s%%**.\n\n", pct(th_covered, th_lines), min
		print "| | Lines | Covered | Coverage |"
		print "| --- | ---: | ---: | ---: |"
		printf "| Baseline: %s | %d | %d | %s |\n", label, tb_lines, tb_covered, pct(tb_covered, tb_lines)
		printf "| This pull request | %d | %d | %s |\n", th_lines, th_covered, pct(th_covered, th_lines)
		printf "| **Delta** | %s | %s | **%s points** |\n\n", signed(th_lines - tb_lines), signed(th_covered - tb_covered), sprintf("%+.3f", delta)
		printf "Gate: %s. Only the total gates; the tables below show where a change came from.\n\n", limits
		if (failed)
			print "If the drop is legitimate (deleting well-tested code lowers the percentage too), see docs/runbook.md, \"Code coverage\", \"The gate failed\".\n"

		print "### By component\n"
		print "| Component | Baseline lines | Baseline covered | Baseline | Lines | Covered | Coverage | Delta (points) |"
		print "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"
		printf "%s", component_rows
		printf "| **Total** | %s | %s | **%s** |\n\n", side(1, tb_lines, tb_covered), side(1, th_lines, th_covered), points(delta)

		if (with_changed) {
			print "### Changed files\n"
			if (reported) {
				print "| File | Baseline lines | Baseline covered | Baseline | Lines | Covered | Coverage | Delta (points) |"
				print "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"
				printf "%s", changed_rows
				printf "| **%d file(s)** | %s | %s | %s |\n\n", reported, \
					side(f_base_lines, f_base_lines, f_base_covered), side(f_head_lines, f_head_lines, f_head_covered), \
					(f_base_lines && f_head_lines) ? points(rate(f_head_covered, f_head_lines) - rate(f_base_covered, f_base_lines)) : "n/a"
			} else
				print "None of the changed files is in either report.\n"
			if (unreported)
				printf "%d changed path(s) are in neither report (tests, non-Go files, generated code, or files excluded by build constraints).\n\n", unreported
		}

		# Only when the total fell. 1e-9 as above: equal reports are not a drop.
		if (delta < -1e-9) {
			if (!drops) {
				print "### Largest per-file drops\n"
				print "No file lost covered lines or left the report: the total fell because lines were added that no test covers.\n"
			} else {
				if (failed)
					print "### Largest per-file drops\n"
				else
					printf "<details>\n<summary>Largest per-file drops: %d file(s), %d covered line(s)</summary>\n\n", drops, drop_lines
				print "The files whose covered-line count fell most between the baseline and this pull request, whatever the diff touched (a test disabled in a BUILD file changes no Go file). `removed`: the file is no longer in the report.\n"
				print "| File | Baseline covered / lines | Covered / lines | Covered lines |"
				print "| --- | ---: | ---: | ---: |"
				printf "%s", drop_rows
				printf "| **%d file(s)** | | | **%s** |\n\n", drops, drop_lines ? signed(-drop_lines) : "0"
				if (more)
					printf "%d of them are not shown (%d covered line(s) between them).\n\n", more, more_lines
				if (!failed)
					print "</details>\n"
			}
		}

		print (failed ? "fail" : "pass") >verdict
		if (dropped && below)
			print "line coverage dropped and is below the minimum: " numbers "; " limits >verdict
		else if (dropped)
			print "line coverage dropped by more than " max_drop " points: " numbers "; " limits >verdict
		else if (below)
			print "line coverage is below the minimum of " min "%: " numbers "; " limits >verdict
		else
			print "line coverage " numbers "; " limits >verdict
	}
' phase=1 "$work/base" phase=2 "$work/head" phase=3 "$work/components" phase=4 "$work/changed" phase=5 "$work/drops"

{
	read -r status
	read -r message
} <"$work/verdict"
if [ "$status" = pass ]; then
	echo "::notice title=Coverage gate passed::$message" >&2
	exit 0
fi
echo "::error title=Coverage gate failed::$message. docs/runbook.md, 'Code coverage'." >&2
exit 1
