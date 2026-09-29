SHELL := /bin/bash
ROOT := $(CURDIR)
export GOCACHE := $(ROOT)/.cache/go-build
export GOMODCACHE := $(ROOT)/.cache/go-mod
export TMPDIR := $(ROOT)/.cache/tmp
export HELM_CACHE_HOME := $(ROOT)/.cache/helm/cache
export HELM_CONFIG_HOME := $(ROOT)/.cache/helm/config
export HELM_DATA_HOME := $(ROOT)/.cache/helm/data
export HELM ?= helm
IMAGE ?= agentregistry-operator:dev
VERSION ?=

.PHONY: help verify test test-e2e test-k3s build chart package image demo-up demo-down clean
.DEFAULT_GOAL := help

help: ## Show available commands
	@awk 'BEGIN {FS = ":.*## "} /^[a-z-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

verify: test build chart ## Run Go tests, vet, chart checks, and documentation checks
	@test -z "$$(gofmt -l cmd internal)" || { echo 'Run gofmt -w cmd internal'; exit 1; }
	go vet ./...
	python3 scripts/verify-docs.py
	python3 -m py_compile scripts/*.py examples/local/*.py
	bash -n scripts/demo.sh

test: ## Run Go behavioral tests with the race detector
	@mkdir -p "$(TMPDIR)"
	go test -race -count=1 -coverprofile=coverage.out ./...

test-e2e: ## Test managed PostgreSQL in a new disposable k3d cluster
	@mkdir -p "$(TMPDIR)"
	python3 scripts/test-managed-postgres.py

test-k3s: test-e2e ## Alias for the disposable K3s acceptance test

build: ## Build the operator and gateway binaries
	@mkdir -p bin "$(TMPDIR)"
	CGO_ENABLED=0 go build -trimpath -o bin/operator ./cmd/operator
	CGO_ENABLED=0 go build -trimpath -o bin/gateway ./cmd/gateway

chart: ## Lint and verify the bundled Helm chart without a cluster
	@mkdir -p "$(TMPDIR)"
	python3 scripts/fetch-metacontroller.py --verify
	python3 scripts/verify-chart.py

package: chart ## Build chart, source/example bundle, and checksums under dist/
	python3 scripts/package.py $(if $(VERSION),--version "$(VERSION)") --image "$(IMAGE)"

image: ## Build the container image (override IMAGE to tag it)
	docker build --tag "$(IMAGE)" .

demo-up: ## Create a local k3d cluster and a working example registry
	bash scripts/demo.sh up

demo-down: ## Delete only this checkout's demo cluster and its demo data
	bash scripts/demo.sh down

clean: ## Remove build outputs; preserve demo credentials and kubeconfig in .local/
	rm -rf .cache/go-build .cache/go-mod .cache/tmp .cache/helm bin dist coverage.out scripts/__pycache__ examples/local/__pycache__
