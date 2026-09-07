GO_DIR      := go
COMPOSE     := docker compose -f deploy/compose/docker-compose.yml
SERVICE     ?= gorge-render
BIN_DIR     := bin

# gorge-render serves both the render and diff domains on one port, so those
# two scripts share a base URL. gorge-notification is a second binary on two
# listeners that answer the same request differently, so it needs one URL per
# port rather than a single BASE_URL.
BASE_URL          ?= http://127.0.0.1:8140
NOTIFY_ADMIN_URL  ?= http://127.0.0.1:22281
NOTIFY_CLIENT_URL ?= http://127.0.0.1:22280
MAILER_URL        ?= http://127.0.0.1:8110
SEARCH_URL        ?= http://127.0.0.1:8120
FILESTORAGE_URL   ?= http://127.0.0.1:8100

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
e2e: ## Run the e2e smoke tests against BASE_URL and the notification ports
	# render and diff share one binary on one port, so those two are two
	# scripts against one BASE_URL rather than two deployments. notification is
	# a separate binary and needs both of its ports; every service under test
	# has to be running already, since none of these scripts start anything.
	BASE_URL=$(BASE_URL) bash tests/e2e/render.sh
	BASE_URL=$(BASE_URL) bash tests/e2e/diff.sh
	ADMIN_URL=$(NOTIFY_ADMIN_URL) CLIENT_URL=$(NOTIFY_CLIENT_URL) bash tests/e2e/notification.sh
	# gorge-mailer needs a backend configured as well as a listener, or its
	# /readyz scenario fails: GORGE_MAILER_CONFIG='[{"key":"test","type":"test"}]'
	# is what the compose service ships with.
	BASE_URL=$(MAILER_URL) bash tests/e2e/mailer.sh
	# gorge-search drops and recreates its index, so this runs against the
	# compose stack's default "test" backend, which holds it in memory. The
	# script detects that backend and skips the two scenarios that only a real
	# Elasticsearch can answer — the CJK analyser ones, which are the reason it
	# exists. Point BASE_URL at a service backed by a scratch cluster to get
	# them, and never at one holding a real install's documents.
	BASE_URL=$(SEARCH_URL) bash tests/e2e/search.sh
	# gorge-file-storage needs a backend too, for the same reason. Local disk
	# is the one that needs nothing external:
	# GORGE_FILE_LOCAL_DISK_PATH=/tmp/gorge-files make run SERVICE=gorge-file-storage
	BASE_URL=$(FILESTORAGE_URL) bash tests/e2e/file-storage.sh

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) $(GO_DIR)/coverage.out $(GO_DIR)/coverage.html
