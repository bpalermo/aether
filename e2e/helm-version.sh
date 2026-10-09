# shellcheck shell=bash
# Sourced, not run: the Helm every workflow job runs (#1580).
#
# A runner image ships one Helm and moves it without notice, and
# azure/setup-helm without `version:` installs whatever is newest: either way
# the Helm MAJOR under a job can change with no commit, and the two majors
# differ in what the suites assert (rollback, #1514/#1515; `helm list`, #1581).
# .github/actions/setup-helm installs one of the two releases below, and it is
# the only way a workflow gets Helm (//e2e:helm_pin_test).
#
#   HELM_V3_VERSION     the Helm 3 release
#   HELM_V4_VERSION     the Helm 4 release. The same release as the rules_helm
#                       toolchain (MODULE.bazel, `helm.host_tools()`), which
#                       renders every chart template test: the test holds the
#                       two equal, so a rules_helm bump that moves its Helm
#                       moves this line too
#   HELM_DEFAULT_MAJOR  which of the two a job gets when it does not ask
#
# The default is Helm 3: it is the major every nightly suite has run on (the
# runner image's), so pinning it changes what no job tests. Helm 4 already
# renders every chart in the Bazel template tests, and e2e/first-install.sh,
# the suite that asserts install, upgrade and rollback behaviour, runs under
# BOTH majors every night (#1543). Moving the default to 4 is this one line,
# to be proven by a workflow_dispatch of e2e.yaml on the branch that does it.
#
# Keep the three values as plain single-line assignments: //e2e:helm_pin_test
# and .github/actions/setup-helm read them by sourcing this file. See
# docs/runbook.md, "Bumping Helm".
HELM_V3_VERSION="v3.22.0"
HELM_V4_VERSION="v4.2.0"
HELM_DEFAULT_MAJOR="3"

# helm_pin_version [<major>]: the pinned release of that Helm major, or of the
# default major when the argument is empty. Any other value is refused: a job
# names a major, never a version, so it cannot run a Helm this file does not pin.
helm_pin_version() {
	local major="${1:-$HELM_DEFAULT_MAJOR}"
	case "$major" in
	3) printf '%s\n' "$HELM_V3_VERSION" ;;
	4) printf '%s\n' "$HELM_V4_VERSION" ;;
	*)
		echo "helm major '$major' is not pinned (e2e/helm-version.sh pins 3 and 4)" >&2
		return 1
		;;
	esac
}

# helm_list_all_flags: the flags that make `helm list` show every release in
# every namespace WHATEVER its status, for the `helm` on PATH. Helm 3 hides
# failed and pending releases without -a; Helm 4 lists them all and no longer
# has the flag, so `helm list -A -a` is an error there (#1543, #1581). Use it
# unquoted: helm --kube-context "$ctx" list $(helm_list_all_flags)
helm_list_all_flags() {
	case "$(helm version --short 2>/dev/null || true)" in
	v3.*) printf '%s\n' "-A -a" ;;
	*) printf '%s\n' "-A" ;;
	esac
}
