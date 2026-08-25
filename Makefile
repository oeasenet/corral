# oease GitHub Actions runners: build, test and deploy helpers.
# Images are built and pushed by CI (.github/workflows/docker-build-push.yml);
# build/push targets are for local development and emergencies.

REGISTRY       ?= ghcr.io
NAMESPACE      ?= oeasenet/gha-docker-runner
TAG            ?= latest
PLATFORMS      ?= linux/amd64,linux/arm64
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
RUNNER_VERSION ?= $(shell sed -n 's/^ARG GITHUB_RUNNER_VERSION=//p' runner/Dockerfile | head -n1)

CONTROLLER_IMAGE = $(REGISTRY)/$(NAMESPACE)/controller
RUNNER_IMAGE     = $(REGISTRY)/$(NAMESPACE)/runner

.DEFAULT_GOAL := help
.PHONY: help up down update ps logs test test-controller test-runner build build-controller build-runner login push runner-version

help: ## Show this help
	@echo "oease GitHub Actions runners"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Variables: TAG=$(TAG)  RUNNER_VERSION=$(RUNNER_VERSION)  PLATFORMS=$(PLATFORMS)"

## --- deploy (on the runner host) --------------------------------------------

up: ## Start the controller (needs .env, see .env.example)
	docker compose up -d

down: ## Stop the controller (runners keep running; scale to 0 in the UI first to remove them)
	docker compose down

update: ## Pull the latest controller image and restart it
	docker compose pull
	docker compose up -d

ps: ## Show the controller and its runners
	docker compose ps
	@docker ps --filter label=dev.oease.gha.managed=true --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'

logs: ## Tail controller logs
	docker compose logs -f --tail=100

## --- develop -----------------------------------------------------------------

test: test-controller test-runner ## Run all tests

test-controller: ## Go unit tests for the controller
	cd controller && go vet ./... && go test -race ./...

test-runner: ## Hermetic tests for the runner entrypoint
	bash runner/test/entrypoint_test.sh

build: build-controller build-runner ## Build both images for the local platform

build-controller: ## Build the controller image
	docker buildx build --load --build-arg VERSION=$(VERSION) -t $(CONTROLLER_IMAGE):$(TAG) controller

build-runner: ## Build the runner image (RUNNER_VERSION=x.y.z to override)
	docker buildx build --load --build-arg GITHUB_RUNNER_VERSION=$(RUNNER_VERSION) -t $(RUNNER_IMAGE):$(TAG) runner

login: ## Log in to GHCR using the gh CLI (needs the write:packages scope)
	gh auth token | docker login $(REGISTRY) -u "$$(gh api user --jq .login)" --password-stdin

push: ## Build multi-arch images and push them to GHCR
	docker buildx build --platform $(PLATFORMS) --push --build-arg VERSION=$(VERSION) -t $(CONTROLLER_IMAGE):$(TAG) controller
	docker buildx build --platform $(PLATFORMS) --push --build-arg GITHUB_RUNNER_VERSION=$(RUNNER_VERSION) -t $(RUNNER_IMAGE):$(TAG) runner

runner-version: ## Print the latest actions/runner release
	@gh api repos/actions/runner/releases/latest --jq .tag_name
