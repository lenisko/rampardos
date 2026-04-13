.PHONY: build run test test-integration test-coverage clean tidy fmt lint \
       npm-install docker-build docker-push docker-compose-up docker-compose-down \
       docker-compose-logs setup-dirs help

# Binary name
BINARY_NAME=rampardos
BUILD_DIR=bin
GO_DIR=rampardos
WORKER_DIR=rampardos-render-worker

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOCLEAN=$(GOCMD) clean
GOMOD=$(GOCMD) mod
GOFMT=gofmt

# Build flags
GIT_COMMIT=$(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
LDFLAGS=-ldflags="-w -s -X github.com/lenisko/rampardos/internal/version.gitCommitFromLdflags=$(GIT_COMMIT)"

# Docker
DOCKER_IMAGE=ghcr.io/jfberry/rampardos
DOCKER_TAG=latest
DOCKER_PLATFORMS=linux/amd64,linux/arm64

# Default target
all: build

## build: Build the Go binary and install Node worker deps
build: npm-install
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	cd $(GO_DIR) && $(GOBUILD) -trimpath $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME) ./cmd/server
	@echo "Binary: $(BUILD_DIR)/$(BINARY_NAME)"

## build-fast: Build Go binary only (skip npm install if already done)
build-fast:
	@mkdir -p $(BUILD_DIR)
	cd $(GO_DIR) && $(GOBUILD) -trimpath $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME) ./cmd/server

## build-linux: Cross-compile for Linux amd64
build-linux: npm-install
	@mkdir -p $(BUILD_DIR)
	cd $(GO_DIR) && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) -trimpath $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/server

## build-all: Build for multiple platforms
build-all: npm-install
	@mkdir -p $(BUILD_DIR)
	cd $(GO_DIR) && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) -trimpath $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/server
	cd $(GO_DIR) && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GOBUILD) -trimpath $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/server
	cd $(GO_DIR) && CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GOBUILD) -trimpath $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/server
	cd $(GO_DIR) && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GOBUILD) -trimpath $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/server

## npm-install: Install Node render worker dependencies
npm-install:
	@if [ ! -d "$(WORKER_DIR)/node_modules/@maplibre/maplibre-gl-native" ]; then \
		echo "Installing render worker deps..."; \
		cd $(WORKER_DIR) && npm install; \
	fi

## run: Build and run locally with sensible defaults
run: build
	./start.sh

## test: Run Go tests (no Node/maplibre required)
test:
	cd $(GO_DIR) && $(GOTEST) ./...

## test-integration: Run integration tests (requires npm install)
test-integration: npm-install
	cd $(GO_DIR) && $(GOTEST) -tags renderer_integration ./internal/services/renderer/ -v -timeout 60s

## test-coverage: Run tests with coverage report
test-coverage:
	cd $(GO_DIR) && $(GOTEST) -coverprofile=coverage.out ./...
	cd $(GO_DIR) && $(GOCMD) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: $(GO_DIR)/coverage.html"

## clean: Remove build artifacts and caches
clean:
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)
	rm -f $(GO_DIR)/coverage.out $(GO_DIR)/coverage.html

## tidy: Tidy and verify Go dependencies
tidy:
	cd $(GO_DIR) && $(GOMOD) tidy
	cd $(GO_DIR) && $(GOMOD) verify

## fmt: Format Go code
fmt:
	$(GOFMT) -s -w $(GO_DIR)

## lint: Run linter (requires golangci-lint)
lint:
	@which golangci-lint > /dev/null || (echo "Install: brew install golangci-lint" && exit 1)
	cd $(GO_DIR) && golangci-lint run

## docker-build: Build multi-arch Docker image locally
docker-build:
	docker buildx build \
		--platform $(DOCKER_PLATFORMS) \
		--tag $(DOCKER_IMAGE):$(DOCKER_TAG) \
		.

## docker-build-local: Build for current platform only (faster)
docker-build-local:
	docker build -t $(DOCKER_IMAGE):local .

## docker-push: Build and push multi-arch image to registry
docker-push:
	docker buildx build \
		--platform $(DOCKER_PLATFORMS) \
		--tag $(DOCKER_IMAGE):$(DOCKER_TAG) \
		--push \
		.

## docker-compose-up: Start with docker-compose
docker-compose-up:
	docker compose up -d

## docker-compose-down: Stop docker-compose
docker-compose-down:
	docker compose down

## docker-compose-logs: View docker-compose logs
docker-compose-logs:
	docker compose logs -f

## setup-dirs: Create required directories
setup-dirs:
	@mkdir -p Cache/Tile Cache/Static Cache/StaticMulti Cache/Marker Cache/Regeneratable
	@mkdir -p TileServer/Fonts TileServer/Styles TileServer/Datasets
	@mkdir -p Templates Markers Temp

## help: Show this help
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
