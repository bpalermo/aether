.PHONY: gazelle
gazelle:
	@bazel run //:gazelle

.PHONY: tidy
tidy:
	@bazel mod tidy

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

.PHONY: fmt
fmt:
	@gofmt -w -s .

.PHONY: fmt-check
fmt-check:
	@test -z "$$(gofmt -l -s .)" || (echo "Files need formatting:"; gofmt -l -s .; exit 1)

.PHONY: vet
vet:
	@go vet ./...

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
