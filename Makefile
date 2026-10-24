SHELL := /bin/sh
GO ?= go

.DEFAULT_GOAL := help
.PHONY: help build test test-race vet fmt-check mod-check check replay clean

help: ## List the targets
	@awk -F ':.*## ' '/^[a-z-]+:.*## / { printf "%-10s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build bin/elprovo
	$(GO) build -o bin/elprovo ./cmd/elprovo

test: ## Run the tests
	$(GO) test ./...

test-race: ## Run the tests with the race detector
	$(GO) test -race ./...

vet: ## Run go vet
	$(GO) vet ./...

fmt-check: ## Fail when Go source is not formatted
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "Run gofmt -w on:"; echo "$$out"; exit 1; fi

mod-check: ## Fail when go.mod or go.sum is not tidy
	$(GO) mod tidy -diff

check: fmt-check vet mod-check test-race build ## Run every check that CI runs

replay: build ## Replay this repository's tests against the merge base with origin/HEAD, or against BASE
	GOCACHE="$$($(GO) env GOCACHE)" GOMODCACHE="$$($(GO) env GOMODCACHE)" ./bin/elprovo replay $${BASE:+--base "$$BASE"}

clean: ## Remove the build and replay output
	rm -rf bin .elprovo
