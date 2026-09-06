GO_DIR      := go
COMPOSE     := docker compose -f deploy/compose/docker-compose.yml
SERVICE     ?= gorge-render
BIN_DIR     := bin
BASE_URL    ?= http://127.0.0.1:8140

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## --- Go -------------------------------------------------------------------

.PHONY: build
build: ## Build $(SERVICE) into bin/
	cd $(GO_DIR) && go build -trimpath -o ../$(BIN_DIR)/$(SERVICE) ./cmd/$(SERVICE)

.PHONY: run
run: ## Run $(SERVICE) from source
	cd $(GO_DIR) && go run ./cmd/$(SERVICE)

.PHONY: test
test: ## Run all Go tests
	cd $(GO_DIR) && go test ./...

.PHONY: cover
cover: ## Run tests and write coverage.html
	cd $(GO_DIR) && go test -coverprofile=coverage.out -covermode=atomic ./... \
		&& go tool cover -html=coverage.out -o coverage.html \
		&& go tool cover -func=coverage.out

.PHONY: fmt
fmt: ## Rewrite Go sources with gofmt -s
	gofmt -s -w $(GO_DIR)

.PHONY: fmt-check
fmt-check: ## Fail if any Go source needs gofmt -s
	@out="$$(gofmt -s -l $(GO_DIR))"; \
	if [ -n "$$out" ]; then echo "needs gofmt -s:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	cd $(GO_DIR) && go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (must be installed)
	cd $(GO_DIR) && golangci-lint run --timeout=5m

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	cd $(GO_DIR) && go mod tidy

.PHONY: check
check: fmt-check vet test ## Run the checks CI runs, minus lint and govulncheck

## --- Docker / Compose -----------------------------------------------------

.PHONY: docker-build
docker-build: ## Build the $(SERVICE) image (context is go/)
	docker build --build-arg SERVICE=$(SERVICE) -t $(SERVICE):dev $(GO_DIR)

.PHONY: compose-config
compose-config: ## Validate the compose file
	$(COMPOSE) config --quiet

.PHONY: compose-up
compose-up: ## Build and start the service stack
	$(COMPOSE) up -d --build

.PHONY: compose-down
compose-down: ## Stop the service stack
	$(COMPOSE) down

.PHONY: compose-logs
compose-logs: ## Tail service logs
	$(COMPOSE) logs -f

## --- Tests ----------------------------------------------------------------

.PHONY: e2e
e2e: ## Run the e2e smoke tests against BASE_URL
	# Both domains are served by one binary on one port, so this is two
	# scripts against one BASE_URL rather than two deployments.
	BASE_URL=$(BASE_URL) bash tests/e2e/render.sh
	BASE_URL=$(BASE_URL) bash tests/e2e/diff.sh

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) $(GO_DIR)/coverage.out $(GO_DIR)/coverage.html
