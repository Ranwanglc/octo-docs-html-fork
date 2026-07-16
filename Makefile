# octo-doc — developer tasks. Run `make help` for the list.

GO        ?= go
BINARY    ?= octo-doc
PKG       := ./...
BUILD_DIR := bin
# VERSION defaults to `git describe` (tag + commits-since + short sha), falling
# back to "dev" outside a git checkout. Release CI overrides it with the tag.
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)

# Storage backends for integration/e2e tests (override as needed).
export OCTO_TEST_DATABASE_URL ?= postgres://octo:octo@localhost:55432/octodoc
export OCTO_TEST_S3_BUCKET    ?= octo-test
export OCTO_TEST_S3_ENDPOINT  ?= http://localhost:59000
export OCTO_TEST_S3_ACCESS_KEY_ID     ?= minioadmin
export OCTO_TEST_S3_SECRET_ACCESS_KEY ?= minioadmin

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into bin/
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/octo-doc

# Cross-compile the server for release. Emits bin/dist/octo-doc_<os>_<arch>[.exe]
# and a SHA256SUMS, attached to the GitHub Release by .github/workflows/release.yml.
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
.PHONY: release
release: ## Cross-compile the server for all release platforms + checksums
	@rm -rf $(BUILD_DIR)/dist && mkdir -p $(BUILD_DIR)/dist
	@for p in $(RELEASE_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=$(BINARY)_$${os}_$${arch}; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
		echo "  building $$out"; \
		GOOS=$$os GOARCH=$$arch $(GO) build -ldflags "$(LDFLAGS)" \
			-o $(BUILD_DIR)/dist/$$out ./cmd/octo-doc || exit 1; \
	done
	@cd $(BUILD_DIR)/dist && (command -v sha256sum >/dev/null 2>&1 && sha256sum $(BINARY)_* || shasum -a 256 $(BINARY)_*) > SHA256SUMS
	@echo "  wrote $(BUILD_DIR)/dist/SHA256SUMS"

.PHONY: run
run: ## Run the server locally
	$(GO) run ./cmd/octo-doc serve

.PHONY: test
test: ## Run all tests
	$(GO) test $(PKG)

.PHONY: test-race
test-race: ## Run tests with the race detector
	$(GO) test -race $(PKG)

.PHONY: cover
cover: ## Run tests with coverage, print a summary
	$(GO) test -coverprofile=coverage.out $(PKG)
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: cover-html
cover-html: cover ## Open the HTML coverage report
	$(GO) tool cover -html=coverage.out

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format all Go code
	gofmt -w .
	$(GO) mod tidy

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKG)

.PHONY: check
check: fmt vet lint test ## Run the full local quality gate

.PHONY: golden
golden: ## Regenerate golden fixtures (see docs/PORTING.md — requires the archived TS reference)
	@echo "Golden fixtures are frozen. See docs/PORTING.md for regeneration."

.PHONY: docker
docker: ## Build the Docker image
	docker build -f deploy/Dockerfile -t octo-doc:dev .

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BUILD_DIR) coverage.out
