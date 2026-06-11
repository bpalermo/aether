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
	@bazel run //agent/cmd/agent:image_push

.PHONY: build-cni-install
build-cni-install:
	@bazel build //cni/cmd/cni-install/...

.PHONY: load-cni-install-image
load-cni-install-image:
	@bazel run //cni/cmd/cni-install:image_load

.PHONY: push-cni-install-image
push-cni-install-image:
	@bazel run //cni/cmd/cni-install:image_push

.PHONY: build-registrar
build-registrar:
	@bazel build //registrar/cmd/registrar/...

.PHONY: load-registrar-image
load-registrar-image:
	@bazel run //registrar/cmd/registrar:image_load

.PHONY: push-registrar-image
push-registrar-image:
	@bazel run //registrar/cmd/registrar:image_push

.PHONY: load-all
load-all: load-agent-image load-cni-install-image load-registrar-image

.PHONY: push-all
push-all: push-agent-image push-cni-install-image push-registrar-image

# Publish everything in the order that works: image manifests FIRST, then the
# charts (chart-only push targets). The combined `:*.push` targets race the
# chart's digest reference against the image manifest upload and fail with
# `Tag ... not found` (observed repeatedly); pushing images first avoids it.
.PHONY: publish
publish: push-all
	@bazel run //charts/agent:agent.push_registry --stamp
	@bazel run //charts/registrar:registrar.push_registry --stamp
