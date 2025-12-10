.PHONY: build install test clean run help dev lint fmt build-all

# Binary name
BINARY_NAME=tako

# Detect OS
ifeq ($(OS),Windows_NT)
    # Windows-specific commands
    SHELL := powershell.exe
    .SHELLFLAGS := -NoProfile -Command
    VERSION=$(shell $$ErrorActionPreference='SilentlyContinue'; $$v = git describe --tags --always --dirty; if ($$v) { $$v } else { "dev" })
    GIT_COMMIT=$(shell git rev-parse --short HEAD 2>$$null || echo "unknown")
    BUILD_TIME=$(shell [datetime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ'))
else
    # Unix-specific commands
    VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
    GIT_COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
    BUILD_TIME=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
endif

LDFLAGS=-ldflags "-X github.com/redentordev/tako-cli/cmd.Version=$(VERSION) -X github.com/redentordev/tako-cli/cmd.GitCommit=$(GIT_COMMIT) -X github.com/redentordev/tako-cli/cmd.BuildTime=$(BUILD_TIME)"

# Build directories
BUILD_DIR=dist
BIN_DIR=bin

# Default target
.DEFAULT_GOAL := help

## help: Display this help message
help:
	@echo "Tako CLI - Makefile commands:"
	@echo ""
	@grep -E '^## ' Makefile | sed 's/## /  /' | column -t -s ':'

## build: Build the CLI for current platform
build:
ifeq ($(OS),Windows_NT)
	@Write-Host "Building $(BINARY_NAME) for current platform..."
	@if (!(Test-Path $(BIN_DIR))) { New-Item -ItemType Directory -Path $(BIN_DIR) | Out-Null }
	@go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY_NAME).exe .
	@Write-Host "Build complete: $(BIN_DIR)/$(BINARY_NAME).exe"
else
	@echo "Building $(BINARY_NAME) for current platform..."
	@mkdir -p $(BIN_DIR)
	@go build $(LDFLAGS) -o $(BIN_DIR)/$(BINARY_NAME) .
	@echo "Build complete: $(BIN_DIR)/$(BINARY_NAME)"
endif

## install: Install the CLI to GOPATH/bin
install:
	@echo "Installing $(BINARY_NAME)..."
	@go install $(LDFLAGS) .
	@echo "Installed to $(shell go env GOPATH)/bin/$(BINARY_NAME)"

## build-all: Build for all platforms (Linux, macOS, Windows)
build-all:
ifeq ($(OS),Windows_NT)
	@Write-Host "Building for all platforms..."
	@if (!(Test-Path $(BUILD_DIR))) { New-Item -ItemType Directory -Path $(BUILD_DIR) | Out-Null }
else
	@echo "Building for all platforms..."
	@mkdir -p $(BUILD_DIR)
endif

	@echo "Building for Linux (amd64)..."
	@GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 .

	@echo "Building for Linux (arm64)..."
	@GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 .

	@echo "Building for macOS (amd64)..."
	@GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 .

	@echo "Building for macOS (arm64)..."
	@GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 .

	@echo "Building for Windows (amd64)..."
	@GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe .

	@echo "All builds complete in $(BUILD_DIR)/"

## test: Run tests
test:
	@echo "Running tests..."
	@go test -v -race -coverprofile=coverage.out ./...
	@echo "Test coverage:"
	@go tool cover -func=coverage.out | grep total

## test-short: Run short tests only
test-short:
	@echo "Running short tests..."
	@go test -short -v ./...

## lint: Run linter
lint:
	@echo "Running linter..."
	@if command -v golangci-lint > /dev/null; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed. Install it from https://golangci-lint.run/"; \
	fi

## fmt: Format code
fmt:
	@echo "Formatting code..."
	@go fmt ./...
	@echo "Code formatted"

## vet: Run go vet
vet:
	@echo "Running go vet..."
	@go vet ./...

## tidy: Tidy go modules
tidy:
	@echo "Tidying go modules..."
	@go mod tidy
	@echo "Modules tidied"

## clean: Clean build artifacts
clean:
ifeq ($(OS),Windows_NT)
	@Write-Host "Cleaning build artifacts..."
	@if (Test-Path $(BUILD_DIR)) { Remove-Item -Recurse -Force $(BUILD_DIR) }
	@if (Test-Path $(BIN_DIR)) { Remove-Item -Recurse -Force $(BIN_DIR) }
	@if (Test-Path coverage.out) { Remove-Item -Force coverage.out }
	@Write-Host "Clean complete"
else
	@echo "Cleaning build artifacts..."
	@rm -rf $(BUILD_DIR) $(BIN_DIR)
	@rm -f coverage.out
	@echo "Clean complete"
endif

## run: Run the CLI
run: build
ifeq ($(OS),Windows_NT)
	@& $(BIN_DIR)/$(BINARY_NAME).exe
else
	@$(BIN_DIR)/$(BINARY_NAME)
endif

## dev: Run in development mode with hot reload (requires air)
dev:
	@if command -v air > /dev/null; then \
		air; \
	else \
		echo "air not installed. Install it with: go install github.com/air-verse/air@latest"; \
	fi

## deps: Install development dependencies
deps:
	@echo "Installing development dependencies..."
	@go get -u ./...
	@go mod tidy
	@echo "Dependencies installed"

## version: Show version information
version:
	@echo "Version: $(VERSION)"
	@echo "Build Time: $(BUILD_TIME)"

## check: Run all checks (fmt, vet, lint, test)
check: fmt vet lint test
	@echo "All checks passed!"