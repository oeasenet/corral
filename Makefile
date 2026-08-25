# oease GitHub Actions runners: build, test and deploy helpers.
# Images are normally built and pushed by CI (.github/workflows/docker-build-push.yml);
# the build/push targets are for local development and emergencies.

REGISTRY       ?= ghcr.io
NAMESPACE      ?= oeasenet/gha-docker-runner
TAG            ?= latest
PLATFORMS      ?= linux/amd64,linux/arm64
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
RUNNER_VERSION ?= $(shell sed -n 's/^ARG GITHUB_RUNNER_VERSION=//p' runner/Dockerfile | head -n1)

KMS_IMAGE    = $(REGISTRY)/$(NAMESPACE)/kms
RUNNER_IMAGE = $(REGISTRY)/$(NAMESPACE)/runner

.DEFAULT_GOAL := help
.PHONY: help build build-kms build-runner test test-kms test-runner push login up down logs ps update runner-version

help: ## Show this help
	@echo "oease GitHub Actions runners"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Variables: TAG=$(TAG)  RUNNER_VERSION=$(RUNNER_VERSION)  PLATFORMS=$(PLATFORMS)"

## --- deploy (on the runner host) --------------------------------------------

up: ## Start the KMS and runners (needs .env, see .env.example)
	docker compose up -d

down: ## Stop everything; runners finish their current job and deregister first
	docker compose down

update: ## Pull the latest images and roll the stack
	docker compose pull
	docker compose up -d

ps: ## Show stack status
	docker compose ps

logs: ## Tail logs
	docker compose logs -f --tail=100

## --- develop -----------------------------------------------------------------

test: test-kms test-runner ## Run all tests

test-kms: ## Go unit tests for the KMS
	cd kms && go vet ./... && go test -race ./...

test-runner: ## Hermetic tests for the runner entrypoint
	bash runner/test/entrypoint_test.sh

build: build-kms build-runner ## Build both images for the local platform

build-kms: ## Build the KMS image
	docker buildx build --load --build-arg VERSION=$(VERSION) -t $(KMS_IMAGE):$(TAG) kms

build-runner: ## Build the runner image (RUNNER_VERSION=x.y.z to override)
	docker buildx build --load --build-arg GITHUB_RUNNER_VERSION=$(RUNNER_VERSION) -t $(RUNNER_IMAGE):$(TAG) runner

login: ## Log in to GHCR using the gh CLI (needs the write:packages scope)
	gh auth token | docker login $(REGISTRY) -u "$$(gh api user --jq .login)" --password-stdin

push: ## Build multi-arch images and push them to GHCR
	docker buildx build --platform $(PLATFORMS) --push --build-arg VERSION=$(VERSION) -t $(KMS_IMAGE):$(TAG) kms
	docker buildx build --platform $(PLATFORMS) --push --build-arg GITHUB_RUNNER_VERSION=$(RUNNER_VERSION) -t $(RUNNER_IMAGE):$(TAG) runner

runner-version: ## Print the latest actions/runner release
	@gh api repos/actions/runner/releases/latest --jq .tag_name
