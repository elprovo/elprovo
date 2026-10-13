SHELL := /bin/sh

# Go modules in this repository: the agent, gateway and CLI; the session
# protocol; and the local simulations of outside services.
MODULES := . protocol sim

# The ACME integration test runs against Pebble, a small ACME test server.
# Pebble resolves the test host names to the Docker host, where the test
# answers the challenges on the ports in internal/gateway/testdata. Keep the
# names in step with internal/gateway/acme_integration_test.go.
PEBBLE_IMAGE := ghcr.io/letsencrypt/pebble:2.10.1
PEBBLE_CONTAINER := elprovo-pebble
PEBBLE_HOSTS := tls-alpn.previews.example.com http.previews.example.com

.DEFAULT_GOAL := help

# Terraform bootstrap modules, one folder per cloud.
TF_MODULES := $(shell find terraform -mindepth 1 -maxdepth 1 -type d)

# Golden image templates, one folder per cloud.
IMAGES := $(shell find images -mindepth 1 -maxdepth 1 -type d)

.PHONY: help fmt fmt-check lint test test-race test-acme mod-check vuln build build-hosts dist tf-check images-check check clean

help:
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "%-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format Go source and Terraform modules
	@set -e; for m in $(MODULES); do (cd $$m && golangci-lint fmt); done
	tofu fmt -recursive terraform
	packer fmt -recursive images

fmt-check: ## Fail when Go source is not formatted
	@set -e; for m in $(MODULES); do (cd $$m && golangci-lint fmt --diff); done

lint: ## Run static analysis, including the import rules
	@set -e; for m in $(MODULES); do echo "lint $$m"; (cd $$m && golangci-lint run ./...); done

test: ## Run unit tests
	@set -e; for m in $(MODULES); do (cd $$m && go test ./...); done

test-race: ## Run unit tests with the race detector
	@set -e; for m in $(MODULES); do (cd $$m && go test -race ./...); done

test-acme: ## Run the ACME integration test against Pebble in Docker
	@set -e; \
	docker rm -f $(PEBBLE_CONTAINER) >/dev/null 2>&1 || true; \
	tmp=$$(mktemp -d); \
	trap 'docker rm -f $(PEBBLE_CONTAINER) >/dev/null 2>&1 || true; rm -rf "$$tmp"' EXIT; \
	docker create --name $(PEBBLE_CONTAINER) \
		-p 127.0.0.1:14000:14000 -p 127.0.0.1:15000:15000 \
		-e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
		$(foreach h,$(PEBBLE_HOSTS),--add-host=$(h):host-gateway) \
		$(PEBBLE_IMAGE) -config /pebble-config.json >/dev/null; \
	docker cp internal/gateway/testdata/pebble-config.json $(PEBBLE_CONTAINER):/pebble-config.json >/dev/null; \
	docker cp $(PEBBLE_CONTAINER):/test/certs/pebble.minica.pem "$$tmp/pebble.minica.pem" >/dev/null; \
	docker start $(PEBBLE_CONTAINER) >/dev/null; \
	i=0; until curl -fs --cacert "$$tmp/pebble.minica.pem" https://localhost:14000/dir >/dev/null; do \
		i=$$((i + 1)); [ $$i -lt 50 ] || { echo "Pebble did not start"; exit 1; }; sleep 0.2; \
	done; \
	ELPROVO_PEBBLE_CA="$$tmp/pebble.minica.pem" go test -tags acme -count=1 -run '^TestACME' ./internal/gateway/

mod-check: ## Fail when go.mod or go.sum are untidy or modules fail verification
	@set -e; for m in $(MODULES); do (cd $$m && go mod tidy -diff && go mod verify); done

vuln: ## Check dependencies for known vulnerabilities
	@set -e; for m in $(MODULES); do echo "vuln $$m"; (cd $$m && govulncheck ./...); done

tf-check: ## Check that Terraform modules are formatted and valid
	@set -e; tofu fmt -check -recursive terraform; \
	mkdir -p "$${TF_PLUGIN_CACHE_DIR:=$$HOME/.cache/elprovo/tofu-plugins}"; export TF_PLUGIN_CACHE_DIR; \
	for m in $(TF_MODULES); do echo "validate $$m"; (cd $$m && tofu init -backend=false -input=false >/dev/null && tofu validate); done

images-check: ## Check that golden image templates are formatted and valid
	@set -e; packer fmt -check -recursive images; \
	for i in $(IMAGES); do echo "validate $$i"; (cd $$i && packer init . >/dev/null && packer validate -var-file=check.pkrvars.hcl .); done

build: ## Build elprovo, elprovo-agent, elprovo-gateway and elprovo-sim into bin/
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/elprovo ./cmd/elprovo-agent ./cmd/elprovo-gateway
	cd sim && CGO_ENABLED=0 go build -trimpath -o ../bin/ ./cmd/elprovo-sim

build-hosts: ## Build elprovo, elprovo-agent and elprovo-gateway for Linux into bin/linux-amd64/ and bin/linux-arm64/
	for arch in amd64 arm64; do mkdir -p bin/linux-$$arch && CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -o bin/linux-$$arch/ ./cmd/elprovo ./cmd/elprovo-agent ./cmd/elprovo-gateway || exit 1; done

dist: ## Package elprovo, elprovo-agent and elprovo-gateway for each release platform into dist/, with SHA-256 checksums
	rm -rf dist && mkdir -p dist
	for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os=$${platform%/*}; arch=$${platform#*/}; dir=dist/elprovo_$${os}_$${arch}; \
		mkdir -p $$dir && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -o $$dir/ ./cmd/elprovo ./cmd/elprovo-agent ./cmd/elprovo-gateway || exit 1; \
		tar -czf $$dir.tar.gz -C $$dir . && rm -rf $$dir; \
	done
	cd dist && shasum -a 256 *.tar.gz > checksums.txt

check: fmt-check lint test test-race mod-check vuln tf-check images-check build ## Run every local quality gate

clean: ## Remove build output
	rm -rf bin dist
