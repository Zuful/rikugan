# Variables ──────────────────────────────────────────────────────────────────
BINARY_NAME := rikugan
GO          := go
GOFLAGS     := -v

VERSION     ?= $(shell git describe --tags --always --dirty)
LDFLAGS     := -ldflags "-X main.version=$(VERSION)"

.PHONY: all build clean test test-short benchmark lint lint-fix fmt audit run install deps-update help

all: clean fmt lint test build

## build: Build the rikugan MCP server into bin/rikugan
build:
	@echo "Building $(BINARY_NAME) $(VERSION)..."
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/rikugan

## clean: Remove build and coverage artefacts
clean:
	@echo "Cleaning..."
	rm -rf bin/
	rm -f coverage.out coverage.html

## test: Run all tests with race detector and coverage
test:
	@echo "Running tests..."
	$(GO) test -v -race -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

## test-short: Run tests without long-running ones
test-short:
	$(GO) test -v -short ./...

## benchmark: Run benchmarks across the module
benchmark:
	@echo "Running benchmarks..."
	$(GO) test -bench=. -benchmem ./...

## lint: Run golangci-lint
lint:
	@echo "Running linter..."
	golangci-lint run ./...

## lint-fix: Run golangci-lint with auto-fix
lint-fix:
	@echo "Running linter with auto-fix..."
	golangci-lint run --fix ./...

## fmt: Format code and run go vet
fmt:
	$(GO) fmt ./...
	$(GO) vet ./...

## audit: Audit dependencies for security vulnerabilities
audit:
	@echo "Auditing dependencies..."
	govulncheck ./...

## run: Build and run rikugan (expects PRIM/SENTRY/etc. env vars in your shell)
run: build
	@echo "Running $(BINARY_NAME)..."
	./bin/$(BINARY_NAME)

## install: Download and tidy module dependencies
install:
	$(GO) mod download
	$(GO) mod tidy

## deps-update: Update dependencies (patch level only, for safety)
deps-update:
	$(GO) get -u=patch ./...
	$(GO) mod tidy

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
