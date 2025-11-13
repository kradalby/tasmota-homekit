.PHONY: help
help: ## Show this help message
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary
	go build -o tasmota-homekit .

.PHONY: test
test: ## Run tests
	go test -v ./...

.PHONY: test-coverage
test-coverage: ## Run tests with coverage
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format code
	go fmt ./...
	gofumpt -l -w .

.PHONY: tidy
tidy: ## Tidy go modules
	go mod tidy

.PHONY: dev
dev: ## Run in development mode
	go run .

.PHONY: clean
clean: ## Clean build artifacts
	rm -f tasmota-homekit
	rm -f coverage.out coverage.html
	rm -rf result result-*

.PHONY: nix-build
nix-build: ## Build with Nix
	nix build

.PHONY: nix-check
nix-check: ## Check Nix flake
	nix flake check

.PHONY: nix-fmt
nix-fmt: ## Format Nix files
	nixpkgs-fmt flake.nix
