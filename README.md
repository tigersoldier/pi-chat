# pi-chat

Chat integrations for [pi-gateway](https://github.com/tigersoldier/pi-gateway):
drive local [pi](https://pi.dev) coding-agent sessions from a chat platform.
Slack is the first integration; Google Chat is a planned second one.

`pi-chatd` runs on your machine and is fully outbound: one WebSocket to Slack
(Socket Mode) and one loopback connection to `pi-gatewayd`. No listening socket,
no tunnel, no public URL, no certificate. It does **not** own pi processes —
`pi-gatewayd` does, and is a prerequisite.

Architecture, the thread/session model, the access rules and the build phases:
**[DESIGN.md](DESIGN.md).** Current position and milestones: **[PLAN.md](PLAN.md).**

## Status

Design settled. M0 (the gateway seam), M1 (the Slack app, phase 0) and M2 (repo prep)
are done. **M3, the phase-0 vertical slice, is implemented and verified against the live
gateway**: a mention becomes a pi session, the turn streams, and the answer lands in the
thread. What is not built yet is the command grammar below beyond mentions — that is M4
(`PLAN.md`), along with the database, admission control and approvals.

## Prerequisites

1. **pi-gatewayd** running, with pi reachable:

   ```bash
   install -Dm644 ~/code/pi-gateway/packaging/pi-gatewayd.service \
       ~/.config/systemd/user/pi-gatewayd.service
   systemctl --user daemon-reload && systemctl --user enable --now pi-gatewayd
   ```

2. **Two tokens**, admin and thread, minted into `~/.config/pi-gateway/tokens.json`:

   ```bash
   pi-gatewayd --provision-token --token-name pi-chat-admin  --token-role admin \
       > ~/.config/pi-chat/gateway-admin.token
   pi-gatewayd --provision-token --token-name pi-chat-thread \
       --token-caps observe,interject,prompt,ui,control \
       > ~/.config/pi-chat/gateway-thread.token
   chmod 600 ~/.config/pi-chat/gateway-*.token
   ```

   The thread token deliberately has **no** `admin`: only the lifecycle
   operations use the admin token, and the long-lived per-thread connections
   cannot destroy sessions.

3. **A Slack app** — created from **[slack/manifest.yaml](slack/manifest.yaml)**;
   walkthrough: **[docs/slack-app-setup.md](docs/slack-app-setup.md)**.

## Configuration

The live file is `~/.config/pi-chat/config.toml`, mode 0600. Every key and its
defaults: **[pi-chat.toml.example](pi-chat.toml.example)**. Check it with:

```bash
make check          # loads, validates, prints a redacted summary
```

## Running it

```bash
make install        # build and install ~/.local/bin/pi-chatd
make install-unit   # install packaging/pi-chatd.service and daemon-reload
systemctl --user enable --now pi-chatd
journalctl --user -u pi-chatd -f
```

`pi-chatd` is deliberately single-instance: Socket Mode splits deliveries across
connections, so a second copy would answer some messages twice. The unit restarts on
failure and is safe to leave running.

## Talking to the bot

Two trigger tokens, one vocabulary. Slack forbids developer slash commands
inside message threads, which is why the two forms differ.

> **Phase 0 (today) answers mentions only.** The command forms below are the designed
grammar; `/pi …` and `@pi /…` commands arrive with M4.

| Where | Form | Example |
|---|---|---|
| Channel or DM root | `/pi <command>` | `/pi status`, `/pi resume`, `/pi help` |
| Channel or DM thread | `@pi /<command>` | `@pi /status`, `@pi /skill:grill-me` |
| Channel root | `@pi <text>` | starts a thread and a session |
| Channel thread | `@pi <text>` | continues that thread's session |
| DM thread | plain text | continues that thread's session |

A session is always thread-scoped; a root command never touches one. `/pi help`
and `@pi /help` print the current list.

## Layout

```text
cmd/pi-chatd/          the daemon: wiring, flags, logging, signals
internal/config/       configuration file loading and validation
internal/bot/          platform-independent core: threads, sessions, turns
internal/slack/        Slack adapter: Socket Mode, Web API, rendering
packaging/             systemd user unit
slack/                 Slack app manifests — the app's configuration as code
tools/gateway-probe/   nested module: pi-gateway seam smoke test
```

## Development

```bash
make test            # unit tests
make vet             # go vet
make fmt             # gofmt
make live-test       # one real turn through a running pi-gatewayd (spends a few tokens)
make probe           # exercise the gateway seam against a running pi-gatewayd
```
