.PHONY: build run test clean docker docker-build docker-run fmt lint tidy help

# Binary name
BINARY_NAME=rampardos
BUILD_DIR=bin
GO_DIR=rampardos

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GORUN=$(GOCMD) run
GOTEST=$(GOCMD) test
GOCLEAN=$(GOCMD) clean
GOMOD=$(GOCMD) mod
GOFMT=gofmt

# Build flags
LDFLAGS=-ldflags="-w -s"

# Default target
all: build

## build: Build the binary
build:
	@echo "Building..."
	@mkdir -p $(BUILD_DIR)
	cd $(GO_DIR) && $(GOBUILD) $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME) ./cmd/server

## build-linux: Build for Linux (useful for Docker)
build-linux:
	@echo "Building for Linux..."
	@mkdir -p $(BUILD_DIR)
	cd $(GO_DIR) && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/server

## build-all: Build for multiple platforms
build-all: build-linux
	@echo "Building for macOS..."
	cd $(GO_DIR) && CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/server
	cd $(GO_DIR) && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o ../$(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/server

## run: Run the application
run:
	@echo "Running..."
	cd $(GO_DIR) && $(GORUN) ./cmd/server

## test: Run tests
test:
	@echo "Testing..."
	cd $(GO_DIR) && $(GOTEST) -v ./...

## test-coverage: Run tests with coverage
test-coverage:
	@echo "Testing with coverage..."
	cd $(GO_DIR) && $(GOTEST) -v -coverprofile=coverage.out ./...
	cd $(GO_DIR) && $(GOCMD) tool cover -html=coverage.out -o coverage.html

## clean: Clean build artifacts
clean:
	@echo "Cleaning..."
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html

## tidy: Tidy and verify dependencies
tidy:
	@echo "Tidying dependencies..."
	cd $(GO_DIR) && $(GOMOD) tidy
	cd $(GO_DIR) && $(GOMOD) verify

## fmt: Format code
fmt:
	@echo "Formatting..."
	$(GOFMT) -s -w $(GO_DIR)

## lint: Run linter (requires golangci-lint)
lint:
	@echo "Linting..."
	@which golangci-lint > /dev/null || (echo "golangci-lint not installed. Run: brew install golangci-lint" && exit 1)
	cd $(GO_DIR) && golangci-lint run

## docker-build: Build Docker image
docker-build:
	@echo "Building Docker image..."
	docker build --build-arg GIT_COMMIT=$$(git rev-parse HEAD) -t $(BINARY_NAME):latest .

## docker-run: Run Docker container
docker-run:
	@echo "Running Docker container..."
	docker run -p 9000:9000 -e TILE_SERVER_URL=http://host.docker.internal:8080 $(BINARY_NAME):latest

## docker-compose-up: Start with docker-compose
docker-compose-up:
	docker-compose up -d

## docker-compose-down: Stop docker-compose
docker-compose-down:
	docker-compose down

## docker-compose-logs: View docker-compose logs
docker-compose-logs:
	docker-compose logs -f

## setup-dirs: Create required directories
setup-dirs:
	@echo "Creating directories..."
	mkdir -p Cache/Tile Cache/Static Cache/StaticMulti Cache/Marker Cache/Regeneratable
	mkdir -p TileServer/Fonts TileServer/Styles TileServer/Datasets
	mkdir -p Templates Markers Temp

## help: Show this help
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
