# GitHub Actions Docker Runners Makefile
# Build and push Docker images for KMS and Runner services

# Configuration
REGISTRY ?= registry.digitalocean.com
NAMESPACE ?= turix
PROJECT_NAME = gha-docker-runners

# Image names
KMS_IMAGE = $(REGISTRY)/$(NAMESPACE)/gha-runner-kms
RUNNER_IMAGE = $(REGISTRY)/$(NAMESPACE)/gha-runner

# Tags
TAG ?= latest
COMMIT_SHA = $(shell git rev-parse --short HEAD)
BRANCH = $(shell git rev-parse --abbrev-ref HEAD)

# Build arguments
BUILD_DATE = $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
VERSION = $(shell git describe --tags --always --dirty)

# Docker build options
DOCKER_BUILDKIT = 1
PLATFORM = linux/amd64

.PHONY: help build build-kms build-runner push push-kms push-runner tag clean test info

## Default target
all: build

## Show help
help:
	@echo "GitHub Actions Docker Runners - Build System"
	@echo ""
	@echo "Available targets:"
	@echo "  build          - Build both KMS and Runner images"
	@echo "  build-kms      - Build only KMS image"
	@echo "  build-runner   - Build only Runner image"
	@echo "  push           - Push both images to registry"
	@echo "  push-kms       - Push only KMS image"
	@echo "  push-runner    - Push only Runner image"
	@echo "  tag            - Tag images with additional tags"
	@echo "  clean          - Remove local images"
	@echo "  test           - Test built images locally"
	@echo "  info           - Show build information"
	@echo ""
	@echo "Variables:"
	@echo "  REGISTRY       - Container registry (default: $(REGISTRY))"
	@echo "  NAMESPACE      - Registry namespace (default: $(NAMESPACE))"
	@echo "  TAG            - Image tag (default: $(TAG))"
	@echo ""
	@echo "Examples:"
	@echo "  make build                    # Build both images"
	@echo "  make build-kms TAG=v1.0.0     # Build KMS with specific tag"
	@echo "  make push                     # Push both images"
	@echo "  make TAG=develop push-kms     # Build and push KMS with develop tag"

## Build both images
build: build-kms build-runner

## Build KMS image
build-kms:
	@echo "🔨 Building KMS image..."
	@echo "Image: $(KMS_IMAGE):$(TAG)"
	@cd kms && \
	DOCKER_BUILDKIT=$(DOCKER_BUILDKIT) docker build \
		--platform $(PLATFORM) \
		--build-arg VERSION=$(VERSION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--build-arg COMMIT_SHA=$(COMMIT_SHA) \
		-t $(KMS_IMAGE):$(TAG) \
		-t $(KMS_IMAGE):$(COMMIT_SHA) \
		.
	@echo "✅ KMS image built successfully"

## Build Runner image
build-runner:
	@echo "🔨 Building Runner image..."
	@echo "Image: $(RUNNER_IMAGE):$(TAG)"
	@cd runner && \
	DOCKER_BUILDKIT=$(DOCKER_BUILDKIT) docker build \
		--platform $(PLATFORM) \
		--build-arg VERSION=$(VERSION) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--build-arg COMMIT_SHA=$(COMMIT_SHA) \
		-t $(RUNNER_IMAGE):$(TAG) \
		-t $(RUNNER_IMAGE):$(COMMIT_SHA) \
		.
	@echo "✅ Runner image built successfully"

## Push both images
push: push-kms push-runner

## Push KMS image
push-kms:
	@echo "📤 Pushing KMS image..."
	@docker push $(KMS_IMAGE):$(TAG)
	@docker push $(KMS_IMAGE):$(COMMIT_SHA)
	@echo "✅ KMS image pushed successfully"

## Push Runner image
push-runner:
	@echo "📤 Pushing Runner image..."
	@docker push $(RUNNER_IMAGE):$(TAG)
	@docker push $(RUNNER_IMAGE):$(COMMIT_SHA)
	@echo "✅ Runner image pushed successfully"

## Tag images with branch name
tag:
	@echo "🏷️  Tagging images with branch: $(BRANCH)"
	@docker tag $(KMS_IMAGE):$(TAG) $(KMS_IMAGE):$(BRANCH)
	@docker tag $(RUNNER_IMAGE):$(TAG) $(RUNNER_IMAGE):$(BRANCH)
	@echo "✅ Images tagged successfully"

## Push branch tags
push-branch-tags: tag
	@echo "📤 Pushing branch tags..."
	@docker push $(KMS_IMAGE):$(BRANCH)
	@docker push $(RUNNER_IMAGE):$(BRANCH)
	@echo "✅ Branch tags pushed successfully"

## Clean local images
clean:
	@echo "🧹 Cleaning local images..."
	@docker rmi $(KMS_IMAGE):$(TAG) $(KMS_IMAGE):$(COMMIT_SHA) 2>/dev/null || true
	@docker rmi $(RUNNER_IMAGE):$(TAG) $(RUNNER_IMAGE):$(COMMIT_SHA) 2>/dev/null || true
	@echo "✅ Local images cleaned"

## Test images locally
test: test-kms test-runner

## Test KMS image
test-kms:
	@echo "🧪 Testing KMS image..."
	@docker run --rm -d --name kms-test -p 3001:3000 \
		-e PAT_test=dummy_token \
		$(KMS_IMAGE):$(TAG) && \
	sleep 3 && \
	curl -f http://localhost:3001/health && \
	docker stop kms-test && \
	echo "✅ KMS image test passed" || \
	(docker stop kms-test 2>/dev/null; echo "❌ KMS image test failed"; exit 1)

## Test Runner image (basic check)
test-runner:
	@echo "🧪 Testing Runner image..."
	@docker run --rm $(RUNNER_IMAGE):$(TAG) /bin/bash -c "which git && which curl && which jq" && \
	echo "✅ Runner image test passed" || \
	(echo "❌ Runner image test failed"; exit 1)

## Show build information
info:
	@echo "📋 Build Information:"
	@echo "  Registry:     $(REGISTRY)"
	@echo "  Namespace:    $(NAMESPACE)"
	@echo "  Tag:          $(TAG)"
	@echo "  Commit SHA:   $(COMMIT_SHA)"
	@echo "  Branch:       $(BRANCH)"
	@echo "  Version:      $(VERSION)"
	@echo "  Build Date:   $(BUILD_DATE)"
	@echo "  Platform:     $(PLATFORM)"
	@echo ""
	@echo "📦 Image Names:"
	@echo "  KMS:          $(KMS_IMAGE):$(TAG)"
	@echo "  Runner:       $(RUNNER_IMAGE):$(TAG)"

## Build and push everything (common workflow)
release: build test push
	@echo "🚀 Release completed successfully!"

## Build and push with branch tags
deploy: build push push-branch-tags
	@echo "🚀 Deployment completed successfully!"

## Quick build for development
dev: build-kms test-kms
	@echo "🔧 Development build completed!"

## Show Docker system information
docker-info:
	@echo "🐳 Docker Information:"
	@docker version --format "  Version: {{.Server.Version}}"
	@docker system df
	@echo ""
	@echo "📦 Local Images:"
	@docker images | grep -E "($(NAMESPACE)|gha-runner)" || echo "  No local images found"

## Login status check
check-login:
	@echo "🔐 Checking registry login status..."
	@docker system info | grep -i "$(REGISTRY)" && \
	echo "✅ Logged in to $(REGISTRY)" || \
	echo "❌ Not logged in to $(REGISTRY). Run: docker login $(REGISTRY)"

## Full workflow with checks
full: info check-login build test push
	@echo "🎉 Full build and push workflow completed!"

## Clean Docker system
docker-clean:
	@echo "🧹 Cleaning Docker system..."
	@docker system prune -f
	@echo "✅ Docker system cleaned"