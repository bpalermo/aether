#!/usr/bin/env bash
#
# The stats tag extractors the proxy workspace's C++ tests build must be the
# ones the chart ships.
#
# charts/aether/templates/agent-proxy-configmap.yaml (stats_config.stats_tags)
# is what the node proxy runs with. Two tests under
# proxy/source/extensions/filters/http/aether_stats/ rebuild that list by hand,
# because the proxy workspace cannot read a file outside itself. Chart 2.6.0
# made the aether.pod extractor strict and both tests kept the 2.5.x regex:
# they went on passing, against a tag producer no proxy runs any more.
#
# The comparison is textual. A YAML double-quoted scalar and a C++ string
# literal spell a backslash the same way (`\\`), and no extractor regex holds a
# double quote, so equal text is equal regex. Each C++ file must carry exactly
# the chart's (tag name, regex) pairs: none missing, none extra, none different.
#
# Recognised in a C++ file, one statement per line (clang-format keeps them so
# at the 100 column limit; a wrapped one is reported as unreadable, not passed):
#   add("<tag name>", "<regex>");
#   <x>->set_tag_name("<tag name>");  followed by  <x>->set_regex("<regex>");
#
# Usage: scripts/check-proxy-stats-tags.sh [<chart template> <c++ file>...]
#        (no arguments: the repository's own files, from anywhere)
# Exit 0 equal, 1 on a difference, 2 when the check cannot read its inputs. An
# input with no extractor in it is 2, never 0: a comparison of nothing with
# nothing proves nothing.
set -uo pipefail

if [ "$#" -eq 0 ]; then
	repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 2
	cd "$repo_root" || exit 2
	set -- charts/aether/templates/agent-proxy-configmap.yaml \
		proxy/source/extensions/filters/http/aether_stats/tag_extraction_test.cc \
		proxy/source/extensions/filters/http/aether_stats/aether_stats_integration_test.cc
fi
if [ "$#" -lt 2 ]; then
	echo "usage: $0 [<chart template> <c++ file>...]" >&2
	exit 2
fi

# chart_tags <template> — "<tag name><TAB><regex>" per extractor of the
# stats_tags list, sorted. The list ends at the first line that is neither
# blank nor a comment and is indented no deeper than `stats_tags:` itself.
chart_tags() {
	awk '
		function indent(s) { match(s, /^ */); return RLENGTH }
		!inblk && /^ *stats_tags:[[:space:]]*$/ { inblk = 1; depth = indent($0); next }
		!inblk { next }
		/^[[:space:]]*(#.*)?$/ { next }
		indent($0) <= depth { inblk = 0; next }
		/^ *- tag_name:/ {
			name = $0
			sub(/^ *- tag_name:[[:space:]]*/, "", name)
			sub(/[[:space:]]*$/, "", name)
			next
		}
		/^ *regex:[[:space:]]*"/ && name != "" {
			re = $0
			sub(/^ *regex:[[:space:]]*"/, "", re)
			sub(/"[[:space:]]*$/, "", re)
			printf "%s\t%s\n", name, re
			name = ""
		}
	' "$1" | LC_ALL=C sort
}

# cc_tags <file> — the same pairs out of a C++ test, sorted. A set_regex() with
# no tag name pending (the stats matcher's safe_regex) is not an extractor.
cc_tags() {
	awk '
		/add\("[^"]+", ".*"\);/ {
			s = $0
			sub(/^.*add\("/, "", s)
			name = s
			sub(/".*$/, "", name)
			re = s
			sub(/^[^"]*", "/, "", re)
			sub(/"\);.*$/, "", re)
			printf "%s\t%s\n", name, re
			next
		}
		/set_tag_name\("[^"]+"\);/ {
			pending = $0
			sub(/^.*set_tag_name\("/, "", pending)
			sub(/"\);.*$/, "", pending)
			next
		}
		/set_regex\(".*"\);/ && pending != "" {
			re = $0
			sub(/^.*set_regex\("/, "", re)
			sub(/"\);.*$/, "", re)
			printf "%s\t%s\n", pending, re
			pending = ""
		}
	' "$1" | LC_ALL=C sort
}

template="$1"
shift

[ -r "$template" ] || {
	echo "::error::check-proxy-stats-tags: cannot read the chart template $template" >&2
	exit 2
}
want="$(chart_tags "$template")"
if [ -z "$want" ]; then
	echo "::error::check-proxy-stats-tags: found no stats_tags extractor in $template: refusing to pass a comparison with nothing" >&2
	exit 2
fi
echo "chart stats_tags ($template):"
printf '%s\n' "$want" | sed 's/^/  /'

rc=0
for cc in "$@"; do
	[ -r "$cc" ] || {
		echo "::error::check-proxy-stats-tags: cannot read $cc" >&2
		exit 2
	}
	got="$(cc_tags "$cc")"
	if [ -z "$got" ]; then
		echo "::error::check-proxy-stats-tags: found no stats tag extractor in $cc (a statement wrapped over two lines, or a new way of adding one): refusing to pass a comparison with nothing" >&2
		exit 2
	fi
	if [ "$got" = "$want" ]; then
		echo "ok    $cc"
		continue
	fi
	rc=1
	echo "FAIL  $cc builds stats tags the chart does not ship ('<' the chart, '>' the test):"
	diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") | grep '^[<>]' | sed 's/^/  /'
done

if [ "$rc" -ne 0 ]; then
	echo "::error::check-proxy-stats-tags: bring the C++ test(s) to stats_config.stats_tags of $template, and their assertions to what those regexes extract" >&2
fi
exit "$rc"
