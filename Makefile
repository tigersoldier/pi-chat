GO ?= go

.PHONY: build vet test tidy check probe config-example install install-unit uninstall fmt live-test clean

build: ## build the daemon into bin/
	$(GO) build -o bin/pi-chatd ./cmd/pi-chatd

vet: ## go vet ./...
	$(GO) vet ./...

test: ## go test ./...
	$(GO) test ./...

fmt: ## gofmt every source file
	$(GO) fmt ./...

tidy: ## go mod tidy
	$(GO) mod tidy

check: ## validate the live configuration and print a redacted summary
	$(GO) run ./cmd/pi-chatd --check

live-test: ## one real turn through a running pi-gatewayd (spends a few tokens)
	PI_CHAT_LIVE=1 $(GO) test ./internal/bot -run Live -v -timeout 5m

install: build ## install the daemon into ~/.local/bin
	install -Dm755 bin/pi-chatd "$(HOME)/.local/bin/pi-chatd"
	@echo "installed $(HOME)/.local/bin/pi-chatd"

install-unit: ## install and reload the systemd user unit (does not enable it)
	install -Dm644 packaging/pi-chatd.service "$(HOME)/.config/systemd/user/pi-chatd.service"
	systemctl --user daemon-reload
	@echo "now: systemctl --user enable --now pi-chatd"

uninstall: ## remove the installed daemon and unit
	rm -f "$(HOME)/.local/bin/pi-chatd" "$(HOME)/.config/systemd/user/pi-chatd.service"
	systemctl --user daemon-reload

probe: ## run the pi-gateway seam smoke test (needs pi-gatewayd running)
	cd tools/gateway-probe && $(GO) run .

config-example: ## show where the example configuration lives
	@echo "example:  $(CURDIR)/pi-chat.toml.example"
	@echo "live:     ~/.config/pi-chat/config.toml (0600)"

clean: ## remove build output
	rm -rf bin
