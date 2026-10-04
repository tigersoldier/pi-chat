GO ?= go

.PHONY: build vet test tidy check probe config-example install install-unit install-unit-oauth uninstall fmt live-test clean

build: ## build the daemon and the OAuth broker into bin/
	$(GO) build -o bin/pi-chatd ./cmd/pi-chatd
	$(GO) build -o bin/pi-chat-oauth ./cmd/pi-chat-oauth

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

install: build ## install the daemon and the broker into ~/.local/bin
	install -Dm755 bin/pi-chatd "$(HOME)/.local/bin/pi-chatd"
	install -Dm755 bin/pi-chat-oauth "$(HOME)/.local/bin/pi-chat-oauth"
	@echo "installed $(HOME)/.local/bin/pi-chatd and pi-chat-oauth"

install-unit: ## install and reload the systemd user unit (does not enable it)
	install -Dm644 packaging/pi-chatd.service "$(HOME)/.config/systemd/user/pi-chatd.service"
	systemctl --user daemon-reload
	@echo "now: systemctl --user enable --now pi-chatd"

install-unit-oauth: ## install and reload the optional OAuth broker unit (does not enable it)
	install -Dm644 packaging/pi-chat-oauth.service "$(HOME)/.config/systemd/user/pi-chat-oauth.service"
	systemctl --user daemon-reload
	@echo "now: create ~/.config/pi-chat/oauth.env (0600), then: systemctl --user enable --now pi-chat-oauth"

uninstall: ## remove the installed daemon, broker and units
	rm -f "$(HOME)/.local/bin/pi-chatd" "$(HOME)/.local/bin/pi-chat-oauth" \
	      "$(HOME)/.config/systemd/user/pi-chatd.service" \
	      "$(HOME)/.config/systemd/user/pi-chat-oauth.service"
	systemctl --user daemon-reload

probe: ## run the pi-gateway seam smoke test (needs pi-gatewayd running)
	cd tools/gateway-probe && $(GO) run .

config-example: ## show where the example configuration lives
	@echo "example:  $(CURDIR)/pi-chat.toml.example"
	@echo "live:     ~/.config/pi-chat/config.toml (0600)"

clean: ## remove build output
	rm -rf bin
