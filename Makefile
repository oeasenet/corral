# corral: build, test and deploy helpers.
# Images are built and pushed by CI (.github/workflows/docker-build-push.yml);
# build/push targets are for local development and emergencies.

REGISTRY       ?= ghcr.io
NAMESPACE      ?= oeasenet/corral
TAG            ?= latest
PLATFORMS      ?= linux/amd64,linux/arm64
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
RUNNER_VERSION ?= $(shell sed -n 's/^ARG GITHUB_RUNNER_VERSION=//p' images/Dockerfile | head -n1)
# Flavors are the directories under images/ with a flavor.json (scripts/flavors.sh).
FLAVORS        := $(shell scripts/flavors.sh list)
FLAVOR         ?= $(firstword $(FLAVORS))
BASE_IMAGE     ?= $(shell scripts/flavors.sh get $(FLAVOR) base)
DOCKERFILE     ?= $(shell scripts/flavors.sh get $(FLAVOR) dockerfile)

CONTROLLER_IMAGE = $(REGISTRY)/$(NAMESPACE)
RUNNER_IMAGE     = $(REGISTRY)/$(NAMESPACE)/runner

.DEFAULT_GOAL := help
.PHONY: help up down update ps logs test test-controller test-runner test-flavors build build-controller build-runner build-runners smoke-runner login push push-runner runner-version

help: ## Show this help
	@echo "corral"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Variables: TAG=$(TAG)  RUNNER_VERSION=$(RUNNER_VERSION)  PLATFORMS=$(PLATFORMS)  FLAVORS=$(FLAVORS)"

## --- deploy (on the runner host) --------------------------------------------

up: ## Start the controller (needs .env, see .env.example)
	docker compose up -d

down: ## Stop the controller (runners keep running; scale pools to 0 in the UI first to remove them)
	docker compose down

update: ## Pull the latest controller image and restart it
	docker compose pull
	docker compose up -d

ps: ## Show the controller and its runners
	docker compose ps
	@docker ps --filter label=corral.managed=true --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'

logs: ## Tail controller logs
	docker compose logs -f --tail=100

## --- develop -----------------------------------------------------------------

test: test-controller test-runner test-flavors ## Run all tests

test-controller: ## Go unit tests for the controller
	cd controller && go vet ./... && go test -race ./...

test-runner: ## Hermetic tests for the runner entrypoint
	bash images/test/entrypoint_test.sh

test-flavors: ## Tests for the flavor catalog script
	bash scripts/test/flavors_test.sh

build: build-controller build-runner ## Build the controller and the default runner flavor for the local platform

build-controller: ## Build the controller image (the flavor catalog is baked in from images/)
	docker buildx build --load -f controller/Dockerfile --build-arg VERSION=$(VERSION) -t $(CONTROLLER_IMAGE):$(TAG) .

build-runner: ## Build one runner flavor for the local platform (FLAVOR=<name>, RUNNER_VERSION=x.y.z)
	docker buildx build --load -f $(DOCKERFILE) --build-arg GITHUB_RUNNER_VERSION=$(RUNNER_VERSION) \
		--build-arg BASE_IMAGE=$(BASE_IMAGE) --build-arg FLAVOR=$(FLAVOR) -t $(RUNNER_IMAGE):$(FLAVOR) images

build-runners: ## Build every runner flavor for the local platform
	@for f in $(FLAVORS); do $(MAKE) --no-print-directory build-runner FLAVOR=$$f || exit 1; done

smoke-runner: ## Start FLAVOR's image with registration skipped; the listener must come up and the OS labels must be right
	docker run --rm -e RUNNER_REGISTER_TO=example-org --entrypoint bash $(RUNNER_IMAGE):$(FLAVOR) -c \
		'echo "{}" > .runner; timeout 20 /usr/local/bin/entrypoint.sh > /tmp/out 2>&1; s=$$?; \
		 grep -q "Started listener process" /tmp/out && [ $$s -eq 124 ] && echo "smoke ok: $(FLAVOR) listener started under $$(./externals/node24/bin/node --version)" || { cat /tmp/out; exit 1; }'
	docker run --rm --entrypoint bash $(RUNNER_IMAGE):$(FLAVOR) -c \
		'RUNNER_TOKEN=bogus RUNNER_REGISTER_TO=example-org bash -x /usr/local/bin/entrypoint.sh 2>&1 | grep -- "^+ ./config.sh" | grep -o -- "--labels [^ ]*"'

login: ## Log in to GHCR using the gh CLI (needs the write:packages scope)
	gh auth token | docker login $(REGISTRY) -u "$$(gh api user --jq .login)" --password-stdin

push: ## Build multi-arch images and push them to GHCR (controller + every runner flavor)
	docker buildx build --platform $(PLATFORMS) --push -f controller/Dockerfile --build-arg VERSION=$(VERSION) -t $(CONTROLLER_IMAGE):$(TAG) .
	@for f in $(FLAVORS); do $(MAKE) --no-print-directory push-runner FLAVOR=$$f || exit 1; done

push-runner: ## Build and push one runner flavor (multi-arch); the default flavor is also tagged latest
	docker buildx build --platform $(PLATFORMS) --push -f $(DOCKERFILE) --build-arg GITHUB_RUNNER_VERSION=$(RUNNER_VERSION) \
		--build-arg BASE_IMAGE=$(BASE_IMAGE) --build-arg FLAVOR=$(FLAVOR) -t $(RUNNER_IMAGE):$(FLAVOR) \
		$(if $(filter true,$(shell scripts/flavors.sh get $(FLAVOR) default)),-t $(RUNNER_IMAGE):latest,) images

runner-version: ## Print the latest actions/runner release
	@gh api repos/actions/runner/releases/latest --jq .tag_name
