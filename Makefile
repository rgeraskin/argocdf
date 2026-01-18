# argocdf Makefile

BINARY_NAME := argocdf
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X main.Version=$(VERSION)"

.PHONY: all build install clean test lint fmt vet help

all: build

## Build
build: ## Build the binary
	go build $(LDFLAGS) -o $(BINARY_NAME) ./cmd/argocdf

build-all: ## Build for multiple platforms
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_NAME)-darwin-amd64 ./cmd/argocdf
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY_NAME)-darwin-arm64 ./cmd/argocdf
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_NAME)-linux-amd64 ./cmd/argocdf
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY_NAME)-linux-arm64 ./cmd/argocdf

install: ## Install to GOPATH/bin
	go install $(LDFLAGS) ./cmd/argocdf

## Test
test: ## Run tests
	go test -v ./...

test-coverage: ## Run tests with coverage
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

## Quality
lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Format code
	go fmt ./...
	goimports -w .

vet: ## Run go vet
	go vet ./...

## Dependencies
deps: ## Download dependencies
	go mod download

tidy: ## Tidy dependencies
	go mod tidy

## Clean
clean: ## Clean build artifacts
	rm -f $(BINARY_NAME)
	rm -f $(BINARY_NAME)-*
	rm -f coverage.out coverage.html
	rm -f argocdf-report.html

## Development
run: build ## Build and run with sample args
	./$(BINARY_NAME) --help

dev: ## Run in development mode
	go run ./cmd/argocdf --verbose

## Help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
