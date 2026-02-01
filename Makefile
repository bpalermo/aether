.PHONY: gazelle
gazelle:
	@bazel run //:gazelle

.PHONY: tidy
tidy:
	@bazel mod tidy

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

.PHONY: load-registrar-image
load-registrar-image:
	@bazel run //registrar/cmd/registrar:image_load

.PHONY: push-registrar-image
push-registrar-image:
	@bazel run //registrar/cmd/registrar:image_push

.PHONY: load-all
load-all: load-agent-image load-registrar-image

.PHONY: push-all
load-all: push-agent-image push-registrar-image
