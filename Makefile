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

# The throwaway PostgreSQL `test-postgres` runs the integration suite against.
# It holds no state worth keeping — internal/pgintegration migrates and seeds a
# schema of its own per test — so the container is created and destroyed around
# a single run rather than left up. PG_TEST_PORT is overridable because 5432 is
# very often already taken by whatever else a developer has running; the default
# is deliberately not 5432 for the same reason.
PG_TEST_IMAGE := postgres:17-alpine
PG_TEST_NAME := central-config-test-postgres
PG_TEST_PORT ?= 55432
PG_TEST_DSN := postgres://postgres:postgres@127.0.0.1:$(PG_TEST_PORT)/central_config_test?sslmode=disable

# The golangci-lint `lint` expects, pinned to the version .github/workflows/ci.yml
# runs. Two things depend on the pin rather than on "whatever is installed": a
# new release adds checks, and that should turn a build red on the commit that
# bumps this line instead of under someone unrelated; and .golangci.yml is a
# `version: "2"` schema, which a v1 binary rejects outright. Someone who guesses
# and installs the v1 that most search results still point at gets a schema
# error rather than a lint run, so `lint` checks the major version and says so.
GOLANGCI_LINT_VERSION := v2.12.2
GOLANGCI_LINT_PKG := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Build identity, stamped into both binaries by `build` so a running process can
# say what it is. VERSION, COMMIT and DATE are all overridable — `make build
# VERSION=v1.2.3` — because a release pipeline knows the version it is cutting
# better than git does, and a build from a tarball has no git at all, which is
# what the `|| echo` fallbacks are for.
#
# They stay recursively expanded (`?=` and `=`, never `:=`) so git and date run
# only for the targets that use them, not on every `make help`.
MODULE := github.com/AliAladraj/Central-Config-Stream
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
          -X $(MODULE)/internal/buildinfo.Commit=$(COMMIT) \
          -X $(MODULE)/internal/buildinfo.Date=$(DATE)

.PHONY: help build version test test-postgres cover lint tools fmt ui run console stack clean

help: ## List the available targets
	@echo "central-config — make targets"
	@echo
	@awk 'BEGIN { FS = ":.*?## " } /^[a-zA-Z_-]+:.*?## / { printf "  %-13s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo
	@echo "First run: make ui   (the console has nothing to serve until web/ exists)"

build: ## Compile every package, then the service and console binaries
	go build ./...
	go build -ldflags "$(LDFLAGS)" -o $(SERVICE_BIN) ./cmd/central-config
	go build -ldflags "$(LDFLAGS)" -o $(CONSOLE_BIN) ./cmd/testconsole

version: ## Print the build identity `build` stamps into the binaries
	@echo "version $(VERSION)"
	@echo "commit  $(COMMIT)"
	@echo "date    $(DATE)"
	@echo
	@echo "Both binaries report it: ./$(SERVICE_BIN) --version"

test: ## Run the Go test suite with the race detector
	go test -race -count=1 ./...

# `test` above leaves internal/pgintegration skipping, because TEST_POSTGRES_DSN
# is unset and the suite refuses to invent a database. This is the target that
# gives it one: the same thing CI does, on a laptop, in about twenty seconds.
#
# The container is removed whether the tests pass or fail — a `trap` rather than
# a trailing command, because a failing `go test` aborts the recipe (.SHELLFLAGS
# carries -e) and a cleanup line after it would simply never run. The whole
# wait-run-clean sequence is therefore one shell invocation; make gives each
# recipe line its own, and a trap does not survive that.
#
# The run is verbose and the skips are counted afterwards, the same check
# ci.yml makes. The suite fails open by design — with no TEST_POSTGRES_DSN every
# test skips and the package still reports ok, which is what keeps `make test`
# green on a machine with no server. That leaves a green run and a run that
# tested something indistinguishable, so a container that never became healthy
# or a DSN pointing somewhere unexpected would pass this target having exercised
# nothing. CI refuses to call that a pass; there is no reason for `make` to be
# the more forgiving of the two.
#
# The log is a mktemp file rather than one in the repository, because the last
# CI step asserts the working tree is clean and a stray artefact here would be
# found there instead.
test-postgres: ## Run the Postgres integration suite against a throwaway container
	@docker rm -f $(PG_TEST_NAME) >/dev/null 2>&1 || true
	@echo "==> starting $(PG_TEST_IMAGE) on :$(PG_TEST_PORT)"
	@docker run -d --rm --name $(PG_TEST_NAME) \
		-e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=central_config_test \
		-p $(PG_TEST_PORT):5432 $(PG_TEST_IMAGE) >/dev/null
	@log=$$(mktemp); \
	trap 'docker rm -f $(PG_TEST_NAME) >/dev/null 2>&1 || true; rm -f "$$log"' EXIT; \
	ready=""; \
	for _ in $$(seq 1 60); do \
		if docker exec $(PG_TEST_NAME) pg_isready -U postgres -d central_config_test >/dev/null 2>&1; then ready=yes; break; fi; \
		sleep 1; \
	done; \
	if [ -z "$$ready" ]; then echo "postgres did not become ready in 60s"; exit 1; fi; \
	echo "==> go test ./internal/pgintegration"; \
	TEST_POSTGRES_DSN="$(PG_TEST_DSN)" go test -race -count=1 -v ./internal/pgintegration/ | tee "$$log"; \
	if grep -q '^--- SKIP' "$$log"; then \
		echo "the integration suite skipped rather than ran: $(PG_TEST_DSN) did not reach the container"; \
		grep '^--- SKIP' "$$log"; \
		exit 1; \
	fi; \
	echo "==> ran $$(grep -c '^--- PASS' "$$log" || true) integration tests against a live PostgreSQL"

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
	@# A clean checkout has no golangci-lint, and a missing-binary error names
	@# neither the tool's import path nor the version this configuration needs.
	@# Both are said here instead, so the fix is a line to paste rather than a
	@# search — and the major-version check catches the likelier failure, which
	@# is a v1 binary already on the PATH meeting a version:"2" .golangci.yml.
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint is not installed. Run 'make tools', or:"; \
		echo "  go install $(GOLANGCI_LINT_PKG)"; \
		exit 1; \
	fi
	@have=$$(golangci-lint version 2>&1 | grep -oE '[0-9]+\.[0-9]+' | head -1 | cut -d. -f1 || true); \
	if [ "$$have" != "2" ]; then \
		echo "golangci-lint v$${have:-?} cannot read .golangci.yml, which is a version: \"2\" schema."; \
		echo "Install the pinned version with 'make tools', or:"; \
		echo "  go install $(GOLANGCI_LINT_PKG)"; \
		exit 1; \
	fi
	golangci-lint run
	@echo "==> eslint"
	cd webui && npm run lint

# `go install` rather than the upstream install script: the toolchain is already
# a prerequisite of every other target here, and this way the version installed
# is the one pinned above and nothing fetches a shell script over the network.
# It lands in $(go env GOPATH)/bin, which has to be on PATH for `lint` to find
# it — the recipe says so rather than assuming.
tools: ## Install the pinned golangci-lint that `lint` needs
	go install $(GOLANGCI_LINT_PKG)
	@echo "installed $(GOLANGCI_LINT_VERSION) into $$(go env GOPATH)/bin — add that to PATH if it is not already"

fmt: ## Rewrite Go sources with gofmt
	gofmt -w .

ui: ## Build the React console bundle into web/
	cd webui && npm ci && npm run build

run: ## Run the service on :8080 against a local SQLite database
	DB_DRIVER=sqlite CONN_STRING="file:./central-config.db" PORT=:8080 \
	NATS_URL=nats://127.0.0.1:4222 PUBLISH_ENABLED=true NATS_REPLICAS=1 \
	RECONCILE_INTERVAL=60s ADMIN_TOKEN=local-dev-token go run ./cmd/central-config

# PORT is the bare number rather than ":8090", which the console reads the same
# way — both take their host from BIND_ADDR and so bind loopback here. The bare
# form is written because it is the one that cannot be misread as a request for
# every interface, and loopback is what this recipe means: the console proxies
# the admin token below to the service, and nothing off this machine should be
# able to spend it.
console: ## Run the test console + embedded NATS on 127.0.0.1:8090 (needs `make ui` first)
	NATS_EMBEDDED=true NATS_URL=nats://127.0.0.1:4222 ENVIRONMENT_ID=1 \
	CENTRAL_CONFIG_URL=http://127.0.0.1:8080 ADMIN_TOKEN=local-dev-token \
	PORT=8090 WEB_DIR=web go run ./cmd/testconsole

stack: ## Bring up the whole stack in Docker (nats + service + console)
	docker compose -f $(COMPOSE_FILE) up --build

clean: ## Remove build output, coverage reports and the local SQLite database
	rm -f $(SERVICE_BIN) $(CONSOLE_BIN)
	rm -f $(COVERAGE) coverage.html
	rm -f central-config.db central-config.db-shm central-config.db-wal
	rm -rf web
