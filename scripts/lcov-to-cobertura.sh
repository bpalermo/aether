#!/usr/bin/env bash
# LCOV tracefile -> Cobertura XML, the one format GitHub's code coverage
# (actions/upload-code-coverage) accepts. docs/runbook.md, "Code coverage".
#
#   scripts/lcov-to-cobertura.sh [--timestamp <epoch-seconds>]
#       [--strip-prefix <prefix>]... <lcov file>... >coverage.xml
#
# Line coverage only: GitHub reads nothing else from the report, and rules_go's
# LCOV carries no function or branch records to convert. Every `DA:<line>,<hits>`
# of every input becomes one `<line number= hits=>`; a line that appears in more
# than one record (the same file named by two inputs, or twice in one) has its
# hit counts added, so concatenated tracefiles convert correctly without a
# separate merge step.
#
# File names must come out repo-relative, because that is how the consumer maps
# a `<class filename=>` onto a file in the commit:
#   - each --strip-prefix is removed from the front of a name that starts with
#     it (the first match wins), then a leading `./` is dropped;
#   - a name that is still absolute, or has a `..` segment, fails the conversion.
#     Uploading it would silently attribute its lines to no file at all.
#
# Layout: one <package> per directory, one <class> per file (name = the file's
# base name, filename = its repo-relative path), sorted bytewise, so the output
# is a pure function of the input and --timestamp (default: $SOURCE_DATE_EPOCH,
# else 0). A file with no DA record at all has nothing coverable and is left
# out. An input with no coverable line at all is refused: an empty report reads
# as "0 of 0 lines", which a consumer shows as full coverage.
#
# bash, POSIX awk and sort; nothing else. //scripts:lcov_to_cobertura_test
# holds it to a golden file.
set -euo pipefail
export LC_ALL=C

usage() {
	sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}

timestamp="${SOURCE_DATE_EPOCH:-0}"
prefixes=()
inputs=()
while [ "$#" -gt 0 ]; do
	case "$1" in
	--timestamp)
		[ "$#" -ge 2 ] || usage
		timestamp="$2"
		shift 2
		;;
	--strip-prefix)
		[ "$#" -ge 2 ] || usage
		prefixes+=("$2")
		shift 2
		;;
	-h | --help) usage ;;
	--)
		shift
		inputs+=("$@")
		break
		;;
	-*)
		echo "lcov-to-cobertura: unknown option $1" >&2
		usage
		;;
	*)
		inputs+=("$1")
		shift
		;;
	esac
done
[ "${#inputs[@]}" -gt 0 ] || usage
case "$timestamp" in
'' | *[!0-9]*)
	echo "lcov-to-cobertura: --timestamp must be a non-negative integer, got '$timestamp'" >&2
	exit 2
	;;
esac
for f in "${inputs[@]}"; do
	[ -f "$f" ] || {
		echo "lcov-to-cobertura: no such file: $f" >&2
		exit 2
	}
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# One prefix per line; a prefix cannot contain a newline.
printf '%s\n' "${prefixes[@]+"${prefixes[@]}"}" >"$work/prefixes"

# Stage 1: every (file, line) with its summed hits, as
# "<dir>\t<base>\t<line>\t<hits>". The directory is its own sort key so that a
# package's files stay together: sorted by whole path, "a/b.go" < "a/b/c.go" <
# "a/d.go" would split package "a" around package "a/b".
awk -v prefix_file="$work/prefixes" '
	function fail(msg) {
		printf "lcov-to-cobertura: %s:%d: %s\n", FILENAME, FNR, msg > "/dev/stderr"
		failed = 1
		exit 1
	}
	function normalise(name,    i, p, n, seg) {
		for (i = 1; i <= nprefix; i++) {
			p = prefix[i]
			if (substr(name, 1, length(p)) == p) {
				name = substr(name, length(p) + 1)
				break
			}
		}
		while (substr(name, 1, 2) == "./") name = substr(name, 3)
		if (name == "") fail("empty source file name")
		if (substr(name, 1, 1) == "/") fail("source file is not repo-relative: " name)
		if (index(name, "\t")) fail("source file name contains a tab: " name)
		n = split(name, seg, "/")
		for (i = 1; i <= n; i++)
			if (seg[i] == "..") fail("source file name has a .. segment: " name)
		return name
	}
	BEGIN {
		while ((getline line < prefix_file) > 0)
			if (line != "") prefix[++nprefix] = line
		close(prefix_file)
	}
	{ sub(/\r$/, "") }
	/^SF:/ {
		file = normalise(substr($0, 4))
		next
	}
	/^DA:/ {
		if (file == "") fail("DA record outside an SF record")
		n = split(substr($0, 4), part, ",")
		if (n < 2 || part[1] !~ /^[0-9]+$/ || part[2] !~ /^[0-9]+$/ || part[1] + 0 < 1)
			fail("malformed DA record: " $0)
		hits[file SUBSEP (part[1] + 0)] += part[2]
		next
	}
	/^end_of_record$/ { file = "" }
	END {
		if (failed) exit 1
		for (key in hits) {
			split(key, k, SUBSEP)
			dir = "."
			base = k[1]
			if (match(k[1], /.*\//)) {
				dir = substr(k[1], 1, RLENGTH - 1)
				base = substr(k[1], RLENGTH + 1)
			}
			printf "%s\t%s\t%d\t%.0f\n", dir, base, k[2], hits[key]
		}
	}
' "${inputs[@]}" >"$work/lines.unsorted"

sort -t "$(printf '\t')" -k1,1 -k2,2 -k3,3n "$work/lines.unsorted" >"$work/lines"

if [ ! -s "$work/lines" ]; then
	echo "lcov-to-cobertura: no DA record in the input: refusing to write an empty report" >&2
	exit 1
fi

# Stage 2: the sorted lines, read twice: totals first (the root element and
# every <package>/<class> open with their own rate), then the document.
awk -F '\t' -v timestamp="$timestamp" '
	function esc(s) {
		gsub(/&/, "\\&amp;", s)
		gsub(/</, "\\&lt;", s)
		gsub(/>/, "\\&gt;", s)
		gsub(/"/, "\\&quot;", s)
		gsub(/\047/, "\\&apos;", s)
		return s
	}
	function rate(hit, valid) { return valid ? sprintf("%.4f", hit / valid) : "0" }
	function path(dir, base) { return dir == "." ? base : dir "/" base }
	function close_class() {
		if (in_class) print "          </lines>\n        </class>"
		in_class = 0
	}
	function close_package() {
		close_class()
		if (in_package) print "      </classes>\n    </package>"
		in_package = 0
	}
	NR == FNR {
		valid++
		pkg_valid[$1]++
		file_valid[$1, $2]++
		if ($4 + 0 > 0) {
			covered++
			pkg_covered[$1]++
			file_covered[$1, $2]++
		}
		next
	}
	FNR == 1 {
		print "<?xml version=\"1.0\" encoding=\"UTF-8\"?>"
		print "<!DOCTYPE coverage SYSTEM \"http://cobertura.sourceforge.net/xml/coverage-04.dtd\">"
		printf "<coverage line-rate=\"%s\" branch-rate=\"0\" lines-covered=\"%d\" lines-valid=\"%d\" branches-covered=\"0\" branches-valid=\"0\" complexity=\"0\" version=\"aether-lcov-to-cobertura-1\" timestamp=\"%s\">\n", rate(covered, valid), covered, valid, timestamp
		print "  <sources>\n    <source>.</source>\n  </sources>"
		print "  <packages>"
	}
	{
		if (!in_package || $1 != cur_dir) {
			close_package()
			cur_dir = $1
			cur_base = ""
			printf "    <package name=\"%s\" line-rate=\"%s\" branch-rate=\"0\" complexity=\"0\">\n", esc($1), rate(pkg_covered[$1], pkg_valid[$1])
			print "      <classes>"
			in_package = 1
		}
		if (!in_class || $2 != cur_base) {
			close_class()
			cur_base = $2
			printf "        <class name=\"%s\" filename=\"%s\" line-rate=\"%s\" branch-rate=\"0\" complexity=\"0\">\n", esc($2), esc(path($1, $2)), rate(file_covered[$1, $2], file_valid[$1, $2])
			print "          <methods/>\n          <lines>"
			in_class = 1
		}
		printf "            <line number=\"%d\" hits=\"%s\"/>\n", $3, $4
	}
	END {
		close_package()
		print "  </packages>\n</coverage>"
	}
' "$work/lines" "$work/lines"
