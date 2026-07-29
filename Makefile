BINARY := grafana-dashboard-extractor
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/felixbarny/grafana-dashboard-extractor/internal/cli.version=$(VERSION)

# Number of dashboards the scale test extracts.
SCALE ?= 50000
# Grafana image the integration tests run against.
GRAFANA_IMAGE ?= grafana/grafana:latest
# Number of community dashboards the corpus test validates against.
CORPUS ?= 1000

.PHONY: help build install test test-race test-integration test-scale test-corpus test-throughput test-all fmt vet lint snapshot clean clean-corpus

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into ./bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

install: ## Install the binary into GOBIN
	go install -trimpath -ldflags "$(LDFLAGS)" .

test: ## Run unit tests
	go test -timeout 5m ./...

test-race: ## Run unit tests with the race detector
	go test -race -timeout 5m ./...

test-integration: ## Run integration tests against a dockerized Grafana
	GRAFANA_IMAGE=$(GRAFANA_IMAGE) go test -tags=integration -timeout 15m -v ./integration/

test-scale: ## Extract SCALE synthetic dashboards and check memory stays flat
	EXTRACTOR_SCALE_DASHBOARDS=$(SCALE) go test -timeout 15m -v -run TestScale ./internal/cli/

test-corpus: ## Validate against the CORPUS most downloaded grafana.com dashboards (downloads once into .cache)
	CORPUS_DASHBOARDS=$(CORPUS) go test -tags=corpus -timeout 60m -v ./corpus/

test-throughput: ## Time extracting the cached community dashboards from a dockerized Grafana
	CORPUS_DASHBOARDS=$(CORPUS) GRAFANA_IMAGE=$(GRAFANA_IMAGE) \
		go test -tags="integration corpus" -count=1 -timeout 60m -v -run TestCorpusThroughput ./integration/

test-all: test-race test-integration test-scale ## Run every test tier that needs no third-party service

fmt: ## Format the code
	gofmt -w .

vet: ## Run go vet, including the tagged builds
	go vet -tags=integration ./...
	go vet -tags=corpus ./...
	go vet -tags="integration corpus" ./...

lint: fmt vet ## Format and vet

snapshot: ## Cross compile release artifacts without publishing
	goreleaser release --snapshot --clean

clean: ## Remove build output
	rm -rf bin dist

clean-corpus: ## Drop the cached community dashboards
	rm -rf .cache/grafana-com
