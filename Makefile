.PHONY: gazelle
gazelle:
	@bazel run //:gazelle

.PHONY: tidy
tidy:
	@bazel mod tidy

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
load-all: load-agent-image load-mesh-dns-image load-cni-install-image load-registrar-image

# Every push target passes --stamp so the released artifacts carry the git
# version information (charts, x_defs). The GNU build-IDs do NOT depend on it:
# //tools/buildid derives each one from the binary's own content (#651, #653),
# in every build configuration.
.PHONY: push-all
push-all: push-agent-image push-mesh-dns-image push-cni-install-image push-registrar-image

# Print (and assert) the GNU build-ID of every binary that ships in a released
# image. Each must hash that binary's own content and no two may be equal — the
# collision that made Pyroscope symbol upload unsafe (#653).
.PHONY: check-build-id
check-build-id:
	@bazel build //tools/buildid:release_build_ids
	@cat bazel-bin/tools/buildid/release_build_ids.txt

# Publish everything in the order that works: image manifests FIRST, then the
# charts (chart-only push targets). The combined `:*.push` targets race the
# chart's digest reference against the image manifest upload and fail with
# `Tag ... not found` (observed repeatedly); pushing images first avoids it.
.PHONY: publish
publish: push-all
	@bazel run //charts/agent:agent.push_registry --stamp
	@bazel run //charts/registrar:registrar.push_registry --stamp

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
# These run inside proxy/ so its own Bazel version (.bazelversion=7.7.1) is used.
# NOTE: building the proxy compiles Envoy from source (multi-hour); use a warm
# cache / CI.
.PHONY: load-proxy-image
load-proxy-image:
	@cd proxy && bazel run --config=release //:load

.PHONY: push-proxy-image
push-proxy-image:
	@cd proxy && bazel run --config=release //:push --stamp
