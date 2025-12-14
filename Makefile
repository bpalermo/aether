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
	@bazel run //agent/cmd/agent:load -- --tag latest

.PHONY: push-agent-image
push-agent-image:
	@bazel run //agent/cmd/agent:push -- --tag latest
