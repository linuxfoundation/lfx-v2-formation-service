# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT

APP_NAME := lfx-v2-formation-service
VERSION := $(shell git describe --tags --always)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GIT_COMMIT := $(shell git rev-parse HEAD)

# Docker
DOCKER_REGISTRY := linuxfoundation
DOCKER_IMAGE := $(DOCKER_REGISTRY)/$(APP_NAME)
DOCKER_TAG := $(VERSION)

# Helm variables
HELM_CHART_PATH=./charts/lfx-v2-formation-service
HELM_RELEASE_NAME=lfx-v2-formation-service
HELM_NAMESPACE=lfx
HELM_VALUES_FILE=$(HELM_CHART_PATH)/values.local.yaml

# Go
GO_VERSION := 1.25.0
GOOS := linux
GOARCH := amd64
GO_FILES := $(shell find . -name '*.go' -not -path './gen/*' -not -path './vendor/*')

# Linting
GOLANGCI_LINT_VERSION := v2.2.2
LINT_TIMEOUT := 10m
LINT_TOOL=$(shell go env GOPATH)/bin/golangci-lint

GOA_VERSION := v3.25.3

##@ Development

.PHONY: setup-dev
setup-dev: ## Setup development tools
	@echo "Installing development tools..."
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: setup
setup: ## Setup development environment
	@echo "Setting up development environment..."
	go mod download
	go mod tidy

.PHONY: deps
deps: ## Install dependencies
	@echo "Installing dependencies..."
	go install goa.design/goa/v3/cmd/goa@$(GOA_VERSION)

.PHONY: apigen
apigen: deps #@ Generate API code using Goa
	goa gen github.com/linuxfoundation/lfx-v2-formation-service/cmd/formation-api/design

.PHONY: lint
lint: ## Run golangci-lint (local Go linting)
	@echo "Running golangci-lint..."
	@which golangci-lint >/dev/null 2>&1 || (echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..." && go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION))
	@golangci-lint run ./... && echo "==> Lint OK"

.PHONY: fmt
fmt: ## Format Go sources
	@echo "Formatting Go sources..."
	go fmt ./...
	gofmt -s -w $(GO_FILES)

.PHONY: license-check
license-check: ## Verify every Go file carries the LF copyright header
	@missing_files=$$(find . \( -name "*.go" \) \
		-not -path "./vendor/*" \
		-not -path "./gen/*" \
		-exec sh -c 'head -10 "$$1" | grep -q "Copyright The Linux Foundation" || echo "$$1"' _ {} \;); \
	if [ -n "$$missing_files" ]; then \
		echo "Files missing license headers:"; echo "$$missing_files"; exit 1; \
	fi
	@echo "==> License headers OK"

.PHONY: check
check: fmt lint license-check ## Run formatting, linting, license and vet checks
	go vet ./...

.PHONY: test
test: ## Run tests
	@echo "Running tests..."
	go test -v -race -coverprofile=coverage.out ./...

.PHONY: test-integration
test-integration: ## Run integration tests against a live Postgres
	go test -v -tags integration ./internal/infrastructure/postgres/...

.PHONY: build
build: ## Build the application for local OS
	@echo "Building application for local development..."
	go build \
		-ldflags "-X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME) -X main.gitCommit=$(GIT_COMMIT)" \
		-o bin/$(APP_NAME) ./cmd/formation-api/

.PHONY: run
run: build ## Run the application for local development
	@echo "Running application for local development..."
	./bin/$(APP_NAME)

##@ Docker

.PHONY: docker-build
docker-build: ## Build Docker image
	@echo "Building Docker image..."
	docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .
	docker tag $(DOCKER_IMAGE):$(DOCKER_TAG) $(DOCKER_IMAGE):latest


.PHONY: docker-run
docker-run: ## Run Docker container locally
	@echo "Running Docker container..."
	docker run \
		--name $(APP_NAME) \
		-p 8080:8080 \
		-e OPENSEARCH_URL=http://opensearch-cluster-master.lfx.svc.cluster.local:9200 \
		-e NATS_URL=nats://lfx-platform-nats.lfx.svc.cluster.local:4222 \
		$(DOCKER_IMAGE):$(DOCKER_TAG)

##@ Helm/Kubernetes
# Install Helm chart
.PHONY: helm-install
helm-install:
	@echo "==> Installing Helm chart..."
	helm upgrade --install $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) --namespace $(HELM_NAMESPACE) --set image.tag=$(DOCKER_TAG)
	@echo "==> Helm chart installed: $(HELM_RELEASE_NAME)"

.PHONY: helm-install-local
helm-install-local: ## Install Helm chart with local values file
	@echo "==> Installing Helm chart with local values..."
	helm upgrade --force --install $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) \
		--namespace $(HELM_NAMESPACE) --create-namespace \
		--values $(HELM_VALUES_FILE)
	@echo "==> Helm chart installed: $(HELM_RELEASE_NAME)"


# Print templates for Helm chart
.PHONY: helm-templates
helm-templates:
	@echo "==> Printing templates for Helm chart..."
	helm template $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) --namespace $(HELM_NAMESPACE) --set image.tag=$(DOCKER_TAG)
	@echo "==> Templates printed for Helm chart: $(HELM_RELEASE_NAME)"

# Uninstall Helm chart
.PHONY: helm-uninstall
helm-uninstall:
	@echo "==> Uninstalling Helm chart..."
	helm uninstall $(HELM_RELEASE_NAME) --namespace $(HELM_NAMESPACE)
	@echo "==> Helm chart uninstalled: $(HELM_RELEASE_NAME)"
