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

Design settled. M0 (the gateway seam), M1 (the Slack app, phase 0), M2 (repo prep), M3
(the vertical slice), **M4 (the phase-1 MVP)** and **M4.6 (the agent surface) are done**:
in Slack, a mention or a command starts a pi session in a thread, the answer streams back,
and the thread's state survives a restart. Phase 1 has a user allowlist, durable thread
state, project directories with the worktree convention, `/pi help|status|resume`,
`@pi /<command>` pass-through, idle close and cold resume. Declared as Slack's agent
messaging experience (the recommended manifest), the session also shows a real status
while it works and a stop button that aborts the turn. Still to come (phase 2):
`@pi /delete`, `/abort`, `/model`, interactive approvals, and the warm-session cap with
eviction. Details: `PLAN.md`.

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

> **What works today:** the mention forms and `/pi help`, `/pi status`, `/pi resume`,
> `@pi /status`, and pass-through of pi's own commands. `/delete`, `/abort`, `/model` and
> `/stop` are part of the grammar but answer "not in this build yet" until phase 2 — they
> are never forwarded to the agent as text.

| Where | Form | Example |
|---|---|---|
| Channel or DM root | `/pi <command>` | `/pi status`, `/pi resume`, `/pi help` |
| Channel or DM thread | `@pi /<command>` | `@pi /status`, `@pi /skill:grill-me` |
| Channel root | `@pi <text>` | starts a thread and a session |
| Channel thread | `@pi <text>` | continues that thread's session |
| DM thread | plain text | continues that thread's session |

A session is always thread-scoped; a root command never touches one. `/pi help`
and `@pi /help` print the current list.

In a **direct message**, mention the bot to start a thread (`@pi <prompt>`); after that,
plain text in that DM thread continues the session — no mention needed. Plain text in a
channel or DM *root* is ignored: roots are session-less, which is what keeps a busy
channel from turning every later message into a prompt. Plain text in a **channel thread**
is a prompt only if that thread already has a session.

### Images

Attach a PNG, JPEG, GIF or WebP to a mention (or a DM message) and the agent
receives the actual image, including image-only requests. Screenshots posted earlier
in the thread are included as conversation context when you next address the bot.
A turn carries up to four images, at most 5 MiB each and 10 MiB total; unavailable,
oversized and unsupported attachments produce a visible warning. Other file types
are not supported yet. The bot needs the `files:read` scope (included in both app
manifests); reinstall the Slack app if your existing token does not have it.

State lives in SQLite at `paths.db_path` (created on first run, inside a 0700 directory). It
holds thread keys, session identities, cursors and handled event IDs — never
transcripts, prompts, or answers.

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
