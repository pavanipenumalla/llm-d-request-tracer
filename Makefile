# Build/test the request-tracer module and its EPP image.
# Image conventions mirror llm-d-router's Makefile so the flags feel familiar.

IMAGE_REGISTRY   ?= ghcr.io/d-sai-venkatesh
IMAGE_NAME       ?= llm-d-router-epp-with-tracer
IMAGE_TAG        ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo latest)
IMAGE            ?= $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

CONTAINER_RUNTIME ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || echo podman)
TARGETARCH        ?= $(shell go env GOARCH)

GIT_COMMIT_SHA ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_REF      ?= $(shell git describe --tags --abbrev=0 2>/dev/null)
LDFLAGS        ?= -s -w

# BASE_IMAGE optionally overrides the runtime base (see Dockerfile).
BASE_IMAGE ?=

.PHONY: help
help: ## List targets.
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the epp-with-tracer binary to ./bin/epp.
	CGO_ENABLED=0 go build -o bin/epp ./cmd/epp-with-tracer

.PHONY: test
test: ## Run unit tests with the race detector.
	go test -race ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum.
	go mod tidy

.PHONY: image-build
image-build: ## Build the EPP container image ($(IMAGE)).
	@printf "\033[33;1m==== Building image $(IMAGE) ====\033[0m\n"
	$(CONTAINER_RUNTIME) build \
		--platform linux/$(TARGETARCH) \
		--build-arg TARGETOS=linux \
		--build-arg TARGETARCH=$(TARGETARCH) \
		--build-arg COMMIT_SHA=$(GIT_COMMIT_SHA) \
		--build-arg BUILD_REF=$(BUILD_REF) \
		--build-arg LDFLAGS="$(LDFLAGS)" \
		$(if $(BASE_IMAGE),--build-arg BASE_IMAGE="$(BASE_IMAGE)") \
		-t $(IMAGE) -f Dockerfile .

.PHONY: image-push
image-push: ## Push the EPP container image ($(IMAGE)).
	$(CONTAINER_RUNTIME) push $(IMAGE)
