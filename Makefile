.PHONY: gazelle
gazelle:
	@bazel run //:gazelle

.PHONY: tidy
tidy:
	@bazel mod tidy

.PHONY: test
test:
	@bazel test --test_output=errors //...

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

.PHONY: load-all
load-all: load-agent-image load-cni-install-image

.PHONY: push-all
push-all: push-agent-image push-cni-install-image
