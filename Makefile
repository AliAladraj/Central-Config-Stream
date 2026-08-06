# central-config — task runner.
#
# Every command line here is one that CONTRIBUTING.md or
# deploy/compose/README.md already documents in prose. If you change one, change
# it in both places: a Makefile that has drifted from the walkthrough is worse
# than no Makefile, because the prose is what people read first.
#
# Requires: Go (version in go.mod), Node 20+ for the UI targets, Docker for
# `stack`, and golangci-lint for `lint`.

# Keep the shell predictable and fail on the first error in a recipe.
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

.DEFAULT_GOAL := help

# Where `build` writes the two binaries. Both are gitignored at the repo root.
SERVICE_BIN := central-config
CONSOLE_BIN := testconsole

COMPOSE_FILE := deploy/compose/docker-compose.yml
COVERAGE := coverage.out

.PHONY: help build test cover lint fmt ui run console stack clean

help: ## List the available targets
	@echo "central-config — make targets"
	@echo
	@awk 'BEGIN { FS = ":.*?## " } /^[a-zA-Z_-]+:.*?## / { printf "  %-9s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo
	@echo "First run: make ui   (the console has nothing to serve until web/ exists)"

build: ## Compile every package, then the service and console binaries
	go build ./...
	go build -o $(SERVICE_BIN) ./cmd/central-config
	go build -o $(CONSOLE_BIN) ./cmd/testconsole

test: ## Run the Go test suite with the race detector
	go test -race -count=1 ./...

cover: ## Run tests with coverage and write an HTML report
	go test -coverprofile=$(COVERAGE) ./...
	go tool cover -func=$(COVERAGE) | tail -1
	go tool cover -html=$(COVERAGE) -o coverage.html
	@echo "wrote coverage.html"

lint: ## gofmt check, go vet, golangci-lint, and the web UI's eslint
	@echo "==> gofmt"
	@# node_modules is filtered because npm packages ship Go sources of their
	@# own (flatted, today) and `gofmt -l .` would report them once `make ui`
	@# has run. The Go tool skips that directory; gofmt does not.
	@unformatted=$$(gofmt -l . | grep -v '/node_modules/' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needs to run on:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "==> go vet"
	go vet ./...
	@echo "==> golangci-lint"
	golangci-lint run
	@echo "==> eslint"
	cd webui && npm run lint

fmt: ## Rewrite Go sources with gofmt
	gofmt -w .

ui: ## Build the React console bundle into web/
	cd webui && npm ci && npm run build

run: ## Run the service on :8080 against a local SQLite database
	DB_DRIVER=sqlite CONN_STRING="file:./central-config.db" PORT=:8080 \
	NATS_URL=nats://127.0.0.1:4222 PUBLISH_ENABLED=true NATS_REPLICAS=1 \
	RECONCILE_INTERVAL=60s ADMIN_TOKEN=local-dev-token go run ./cmd/central-config

console: ## Run the test console + embedded NATS on :8090 (needs `make ui` first)
	NATS_EMBEDDED=true NATS_URL=nats://127.0.0.1:4222 ENVIRONMENT_ID=1 \
	CENTRAL_CONFIG_URL=http://127.0.0.1:8080 ADMIN_TOKEN=local-dev-token \
	PORT=:8090 WEB_DIR=web go run ./cmd/testconsole

stack: ## Bring up the whole stack in Docker (nats + service + console)
	docker compose -f $(COMPOSE_FILE) up --build

clean: ## Remove build output, coverage reports and the local SQLite database
	rm -f $(SERVICE_BIN) $(CONSOLE_BIN)
	rm -f $(COVERAGE) coverage.html
	rm -f central-config.db central-config.db-shm central-config.db-wal
	rm -rf web
