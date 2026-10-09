# shellcheck shell=bash
# Sourced, not run: the ONE Gateway API release every e2e surface installs (#1583).
#
# Each kind harness applies that release's CRD bundle (standard-install.yaml or
# experimental-install.yaml), and the nightly conformance jobs install its
# experimental bundle and run its conformance suite. Bump it HERE, together
# with:
#
#   1. go.mod's `sigs.k8s.io/gateway-api` requirement: the Go types the agent,
#      the controller and the registrar are built against
#   2. `GATEWAY_API_VERSION` in .github/workflows/e2e.yaml: a workflow cannot
#      source this file, so it carries a copy
#
# //e2e:gateway_api_pin_test fails until all three agree, and fails any e2e
# harness, workflow or composite action that spells a release of its own. See
# docs/runbook.md, "Bumping Gateway API".
#
# Keep it a plain single-line assignment: the test reads it by sourcing this
# file. Override per run with GWAPI_VERSION=<release> (e.g. to try the next
# release locally); CI never sets it.
# shellcheck disable=SC2034 # read by the harnesses that source this file
GWAPI_VERSION="${GWAPI_VERSION:-v1.6.2}"
