.PHONY: gazelle
gazelle:
	@bazel run //:gazelle

.PHONY: tidy
tidy:
	@bazel mod tidy

.PHONY: deps-audit
deps-audit:
	@scripts/go-deps-audit.sh

.PHONY: build
build:
	@bazel build //...

.PHONY: test
test:
	@bazel test --test_output=errors //...

.PHONY: test-unit
test-unit:
	@bazel test --test_output=errors --test_tag_filters=-integration //...

.PHONY: test-integration
test-integration:
	@bazel test --test_output=errors --test_tag_filters=integration //...

.PHONY: test-race
test-race:
	@bazel test --test_output=errors --@rules_go//go/config:race //...

.PHONY: format
format:
	@bazel run //:format

.PHONY: format-check
format-check:
	@bazel run //:format.check

.PHONY: lint
lint:
	@bazel build --config=lint //...

# Is every shell script actually reachable by the shellcheck aspect? `make lint`
# can only lint what an sh_* target hands it, and for this repository's whole
# life that was nothing (#853). A script in a directory with no sh_* target is
# still silently outside the gate, so assert the coverage rather than assume it.
# Also a required CI job (`shell` in .github/workflows/ci.yaml).
.PHONY: check-shell-lint
check-shell-lint:
	@scripts/check-shell-lint.sh

.PHONY: build-agent
build-agent:
	@bazel build //agent/cmd/agent/...

.PHONY: load-agent-image
load-agent-image:
	@bazel run //agent/cmd/agent:image_load

.PHONY: push-agent-image
push-agent-image:
	@bazel run --stamp //agent/cmd/agent:image_push

# The slim mesh-DNS daemon (#583) — its own binary AND its own image, so the
# aether-mesh-dns DaemonSet no longer ships the full agent.
.PHONY: build-mesh-dns
build-mesh-dns:
	@bazel build //agent/cmd/mesh-dns/...

.PHONY: load-mesh-dns-image
load-mesh-dns-image:
	@bazel run //agent/cmd/mesh-dns:image_load

.PHONY: push-mesh-dns-image
push-mesh-dns-image:
	@bazel run --stamp //agent/cmd/mesh-dns:image_push

# The Envoy hot-restart supervisor (#772 phase B2) — its own binary AND its own
# image, so the aether-proxy pod no longer stages and runs the full agent.
.PHONY: build-proxy-supervisor
build-proxy-supervisor:
	@bazel build //agent/cmd/proxy-supervisor/...

.PHONY: load-proxy-supervisor-image
load-proxy-supervisor-image:
	@bazel run //agent/cmd/proxy-supervisor:image_load

.PHONY: push-proxy-supervisor-image
push-proxy-supervisor-image:
	@bazel run --stamp //agent/cmd/proxy-supervisor:image_push

.PHONY: build-cni-install
build-cni-install:
	@bazel build //cni/cmd/cni-install/...

.PHONY: load-cni-install-image
load-cni-install-image:
	@bazel run //cni/cmd/cni-install:image_load

.PHONY: push-cni-install-image
push-cni-install-image:
	@bazel run --stamp //cni/cmd/cni-install:image_push

.PHONY: build-registrar
build-registrar:
	@bazel build //registrar/cmd/registrar/...

.PHONY: load-registrar-image
load-registrar-image:
	@bazel run //registrar/cmd/registrar:image_load

.PHONY: push-registrar-image
push-registrar-image:
	@bazel run --stamp //registrar/cmd/registrar:image_push

.PHONY: load-all
load-all: load-agent-image load-mesh-dns-image load-proxy-supervisor-image load-cni-install-image load-registrar-image

# Every push target passes --stamp so the released artifacts carry the git
# version information (charts, x_defs). The GNU build-IDs do NOT depend on it:
# //tools/buildid derives each one from the binary's own content (#651, #653),
# in every build configuration.
.PHONY: push-all
push-all: push-agent-image push-mesh-dns-image push-proxy-supervisor-image push-cni-install-image push-registrar-image

# Print (and assert) the GNU build-ID of every binary that ships in a released
# image. Each must hash that binary's own content and no two may be equal — the
# collision that made Pyroscope symbol upload unsafe (#653).
.PHONY: check-build-id
check-build-id:
	@bazel build //tools/buildid:release_build_ids
	@cat bazel-bin/tools/buildid/release_build_ids.txt

# Did a commit on main actually publish? Read-only registry query — no
# credentials needed for our public packages, and it cannot push anything.
#
#   make check-published                  # the last day of main
#   make check-published COMMIT=<sha>     # one commit (any commit-ish; git
#                                         # expands it to the full 40 chars)
#
# Answers the question `gh run list` cannot: a superseded publish run ends
# `cancelled`, not `failure`, so "the workflow was fine" and "nothing was
# pushed for this commit" look identical from GitHub's side (#880).
.PHONY: check-published
check-published:
	@scripts/verify-published-artifacts.sh $(if $(COMMIT),$(COMMIT),--recent)

# NOTE: there is deliberately no `publish` target. Releases are published by
# .github/workflows/publish.yaml, which is serialised (#692) and pushes the
# charts that actually exist. The old target named //charts/agent and
# //charts/registrar — neither package exists — after its `push-all`
# prerequisite had already pushed four images, so following it produced a
# half-publish.

# --- Website (aethermesh.dev; see //website) ---
# `website` produces bazel-bin/website/site.tar, exactly what the pages workflow
# deploys. `website-test` is the strict build the `ci` gate runs on PRs.
.PHONY: website
website:
	@bazel build //website:site

.PHONY: website-test
website-test:
	@bazel test --test_output=errors //website:all

.PHONY: website-serve
website-serve:
	@bazel run //website:mkdocs -- serve -f "$(CURDIR)/website/mkdocs.yml"

# --- Custom proxy (separate Bazel workspace under proxy/; see proposal 010) ---
# These run inside proxy/ so its own Bazel version (.bazelversion=8.7.0) and its
# own module graph (proxy/MODULE.bazel, pinned to the envoy bazel-registry) are
# used.
# NOTE: building the proxy compiles Envoy from source (multi-hour); use a warm
# cache / CI.
#
# Only the local `load` has a target: pushing the proxy image is owned end to end
# by .github/workflows/proxy-release.yml (`bazel run //:push` on a native arm64
# runner for that leg), which is fully automated (#703/#727). A local
# `push-proxy-image` was a second, unaudited way to write the released tag.
.PHONY: load-proxy-image
load-proxy-image:
	@cd proxy && bazel run --config=release //:load
