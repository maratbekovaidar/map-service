# map-service - self-hosted map stack. Run `make help`.
#
# Configuration comes from .env (copy .env.example). The Makefile exports the
# few values docker compose needs that cannot live in .env:
#   REPO_DIR      absolute path of this repository
#   DATA_DIR      absolute data directory (default ./data)
#   DATA_VERSION  the live data version (target of data/current)
SHELL := /bin/bash
.DEFAULT_GOAL := help

-include .env
# .env values are read verbatim by make (no quote stripping); see .env.example.

REPO_DIR     := $(CURDIR)
DATA_DIR     := $(if $(strip $(DATA_DIR)),$(abspath $(DATA_DIR)),$(CURDIR)/data)
DATA_VERSION := $(shell readlink "$(DATA_DIR)/current" 2>/dev/null | sed 's|.*/||')
export REPO_DIR DATA_DIR DATA_VERSION

PROJECT ?= $(if $(COMPOSE_PROJECT_NAME),$(COMPOSE_PROJECT_NAME),mapsvc)
COMPOSE      := docker compose -p $(PROJECT) -f deploy/docker-compose.yml --env-file .env
COMPOSE_DEV  := $(COMPOSE) -f deploy/docker-compose.dev.yml
GOLANGCI_LINT_VERSION := v2.14.0

.PHONY: help
help: ## Show this help
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[1m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: check-env
check-env:
	@test -f .env || { echo "ERROR: .env not found. Run: cp .env.example .env  (then edit it)"; exit 1; }

.PHONY: check-data
check-data: check-env
	@test -n "$(DATA_VERSION)" || { echo "ERROR: no data yet ($(DATA_DIR)/current is missing). Run: make init-data"; exit 1; }

# ------------------------------------------------------------------ stack ---
.PHONY: up
up: check-data ## Start the whole stack (needs `make init-data` first)
	$(COMPOSE) up -d --build --remove-orphans
	@echo "Stack is starting. Check readiness:  curl -s localhost/readyz"

.PHONY: up-dev
up-dev: check-data ## Start the stack with backend ports published on 127.0.0.1
	$(COMPOSE_DEV) up -d --build --remove-orphans

.PHONY: down
down: check-env ## Stop and remove the stack containers (data and volumes are kept)
	$(COMPOSE_DEV) down --remove-orphans

.PHONY: ps
ps: check-env ## Show stack status
	$(COMPOSE) ps

.PHONY: logs
logs: check-env ## Follow the logs (make logs S=gateway)
	$(COMPOSE) logs -f --tail=100 $(S)

# ------------------------------------------------------------------- data ---
.PHONY: init-data
init-data: check-env ## First download and build of all data (OSM extract, OSRM, tiles, Nominatim, Photon)
	@mkdir -p "$(DATA_DIR)"
	$(COMPOSE) build updater
	$(COMPOSE) run --rm --no-deps -e LOG_FORMAT=text updater run

.PHONY: update-data
update-data: init-data ## Rebuild the data without downtime and switch to it (same pipeline as init-data)

# ------------------------------------------------------------------ tests ---
.PHONY: test
test: ## Unit tests
	go test -race -count=1 ./...

comma := ,
# Integration tests read these from the environment (defaults match Astana).
export CITY_CENTER ROUTING_PROFILES

.PHONY: test-integration
test-integration: check-env ## Integration tests against the running stack (GATEWAY_URL, API_KEY; defaults from .env)
	GATEWAY_URL=$(or $(GATEWAY_URL),http://localhost) \
	API_KEY=$(or $(API_KEY),$(strip $(firstword $(subst $(comma), ,$(API_KEYS))))) \
	go test -tags=integration -count=1 -v ./test/integration/...

.PHONY: lint
lint: ## go vet + golangci-lint
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 \
	  && golangci-lint run ./... \
	  || go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

.PHONY: fmt
fmt: ## gofmt
	gofmt -w cmd internal test

.PHONY: style-assets
style-assets: ## Regenerate deploy/tiles style/glyphs/sprites from the pinned upstream sources (committed; rarely needed)
	go run ./cmd/stylegen

.PHONY: build
build: ## Build the Go binaries into ./bin
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/gateway ./cmd/updater
