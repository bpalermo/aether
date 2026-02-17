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

.PHONY: build-registrar
build-registrar:
	@bazel build //registrar/cmd/registrar/...

.PHONY: load-all
load-all: load-agent-image

.PHONY: push-all
push-all: push-agent-image
