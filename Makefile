GO ?= go

.PHONY: probe build vet test setup

probe: ## run the phase-0 spike: pull one Chat event, reply, exit
	$(GO) run ./cmd/probe

build: ## build the probe binary
	$(GO) build -o bin/probe ./cmd/probe

vet: ## go vet ./...
	$(GO) vet ./...

test: ## go test ./...
	$(GO) test ./...

setup: ## provision GCP plumbing (topic, subscription, SA, config)
	./scripts/setup-gcp.sh
