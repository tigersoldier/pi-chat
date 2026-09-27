GO ?= go

.PHONY: build vet test tidy check config-example clean

build: ## build the daemon into bin/
	$(GO) build -o bin/pi-chatd ./cmd/pi-chatd

vet: ## go vet ./...
	$(GO) vet ./...

test: ## go test ./...
	$(GO) test ./...

tidy: ## go mod tidy
	$(GO) mod tidy

check: ## validate the live configuration and print a redacted summary
	$(GO) run ./cmd/pi-chatd --check

probe: ## run the pi-gateway seam smoke test (needs pi-gatewayd running)
	cd tools/gateway-probe && $(GO) run .

config-example: ## show where the example configuration lives
	@echo "example:  $(CURDIR)/pi-chat.toml.example"
	@echo "live:     ~/.config/pi-chat/config.toml (0600)"

clean: ## remove build output
	rm -rf bin
