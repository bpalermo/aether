#!/usr/bin/env bash
# Shim (proposal 040): the library is scripts/registry-lib.sh, which also defines every ghcr_* name.
# shellcheck source=scripts/registry-lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/registry-lib.sh"
