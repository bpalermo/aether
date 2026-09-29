#!/usr/bin/env bash
# //bazel/helm:chart_push_test — chart_push hands oras exactly the artifact
# `helm push` would write, at the repository and tag it names (proposal 040).
#
# No network: CHART_PUSH_ORAS points the runner at a recorder that saves its
# argv and the files it was handed. What is pinned:
#   - the repository is chart_registry_url(<chart>) from //bazel/img:registry.bzl
#     and the tag is the PACKAGED version, with SemVer's '+' written as '_'
#     (helm's rule; an OCI tag cannot hold '+');
#   - the config is Chart.yaml as JSON under helm's config media type, and the
#     one layer is the packaged .tgz, byte for byte, under helm's chart-content
#     media type, named <name>-<version>.tgz like `helm package` names it;
#   - extra runner arguments reach `oras push`;
#   - a target whose chart_name is not the package's chart refuses to push.
#
# Usage: chart_push_test.sh <runner> <package .tgz> <wrong-name runner> <expected repository>
set -euo pipefail

runner="$1" pkg="$2" wrong="$3" want_repo="$4"
scratch="${TEST_TMPDIR:?}/chart_push"
mkdir -p "$scratch"

cat >"${scratch}/fake-oras" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$@" >"${scratch}/argv"
cp config.json "${scratch}/config.json"
cp ./*.tgz "${scratch}/"
EOF
chmod +x "${scratch}/fake-oras"
export CHART_PUSH_ORAS="${scratch}/fake-oras"

fail=0
check() {
	if [ "$2" = "$3" ]; then
		printf '  ok    %s\n' "$1"
	else
		printf '  FAIL  %s\n        want: %s\n        got:  %s\n' "$1" "$2" "$3"
		fail=1
	fi
}

"$runner" --plain-http >"${scratch}/out" 2>&1 || {
	cat "${scratch}/out" >&2
	echo "FAIL: the runner exited non-zero against a recording oras" >&2
	exit 1
}
mapfile -t argv <"${scratch}/argv"

check "oras subcommand" "push" "${argv[0]}"
check "extra runner args reach oras push" "--plain-http" "${argv[1]}"
joined="$(printf '%s\n' "${argv[@]}")"
check "config under helm's config media type" "1" \
	"$(grep -cxF 'config.json:application/vnd.cncf.helm.config.v1+json' <<<"$joined")"
check "title annotation" "1" "$(grep -cxF 'org.opencontainers.image.title=pushtest' <<<"$joined")"
check "version annotation" "1" "$(grep -cxF 'org.opencontainers.image.version=1.2.3+build.7' <<<"$joined")"
n=${#argv[@]}
check "target: chart_registry_url(chart):<version, '+' as '_'>" "${want_repo}:1.2.3_build.7" "${argv[n - 2]}"
check "the only layer: the package under helm's chart-content media type" \
	"pushtest-1.2.3+build.7.tgz:application/vnd.cncf.helm.chart.content.v1.tar+gzip" "${argv[n - 1]}"
if cmp -s "$pkg" "${scratch}/pushtest-1.2.3+build.7.tgz"; then
	check "the layer is the packaged chart, byte for byte" same same
else
	check "the layer is the packaged chart, byte for byte" same different
fi
config="$(cat "${scratch}/config.json")"
check "config names the chart" 1 "$(grep -c '"name":"pushtest"' <<<"$config")"
check "config carries the packaged version" 1 "$(grep -c '"version":"1.2.3+build.7"' <<<"$config")"
check "config is Chart.yaml (apiVersion kept)" 1 "$(grep -c '"apiVersion":"v2"' <<<"$config")"

rm -f "${scratch}/argv"
rc=0
"$wrong" >"${scratch}/out" 2>&1 || rc=$?
check "a target for another chart refuses to push (exit 1)" 1 "$rc"
check "…and never ran oras" absent "$([ -e "${scratch}/argv" ] && echo present || echo absent)"
check "…naming the mismatch" 1 "$(grep -c "the package is chart 'pushtest', but this target pushes chart 'not-pushtest'" "${scratch}/out")"

if [ "$fail" -ne 0 ]; then
	echo "chart_push: see failures above" >&2
	exit 1
fi
echo "chart_push: oras is handed exactly what helm push writes"
