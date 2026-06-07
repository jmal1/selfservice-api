.PHONY: build test test-race lint fmt vet tidy clean help

GO            ?= go
GOFLAGS       ?=
BUILD_FLAGS   ?= -trimpath
BIN_DIR       ?= bin
CMDS          := api-gateway provision-worker crucible-engine crucible-runner synthetic-api-monitor

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build all command binaries into ./bin
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		echo "==> building $$cmd"; \
		$(GO) build $(BUILD_FLAGS) -o $(BIN_DIR)/$$cmd ./cmd/$$cmd || exit 1; \
	done

test: ## Run unit tests with the race detector (matches CI)
	$(GO) test ./... -race -count=1

test-short: ## Run tests excluding -tags=integration
	$(GO) test ./... -short -race -count=1

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Run gofmt against the tree
	$(GO) fmt ./...

tidy: ## Tidy go.mod / go.sum
	$(GO) mod tidy

lint: vet ## Run static analysis (vet today; golangci-lint when installed)
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint not installed; ran go vet only"

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
